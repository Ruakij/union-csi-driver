package driver

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
	"github.com/Ruakij/union-csi-driver/pkg/volsource"
)

const (
	attrEphemeral = "csi.storage.k8s.io/ephemeral"
)

// NodePublishVolume resolves the pod's sibling volumes named in sourceVolumes,
// waits for them to become ready, and mounts the union at req.TargetPath.
// Kubelet re-issues this call freely, including after a driver restart, so it is
// idempotent.
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability missing in request")
	}
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "target path missing in request")
	}
	if req.GetVolumeContext()[attrEphemeral] != "true" {
		return nil, status.Error(codes.InvalidArgument, "this driver only supports ephemeral inline volumes")
	}

	// A target path is unique to one volume of one pod, so an existing mount there
	// is this volume's own, already published. The backend's source set is not
	// re-checked: a bind-mounted single branch does not name its source in
	// mountinfo, so the comparison would be true only for the multi-branch case.
	mounted, err := d.isMounted(req.GetTargetPath())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check target path: %v", err)
	}
	if mounted {
		klog.V(4).Infof("target path %s is already mounted, nothing to do", req.GetTargetPath())
		return &csi.NodePublishVolumeResponse{}, nil
	}

	attrs, err := d.parseAttributes(req.GetVolumeContext())
	if err != nil {
		return nil, err
	}
	if attrs.PodNamespace == "" || attrs.PodName == "" || attrs.PodUID == "" {
		return nil, status.Error(codes.InvalidArgument, "pod identity missing from volumeAttributes (requires CSIDriver.spec.podInfoOnMount)")
	}

	options, err := d.config.Policy.Resolve(attrs.Options)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	names := make([]string, len(attrs.SourceVolumes))
	branches := make([]string, len(attrs.SourceVolumes))
	modeByName := make(map[string]string, len(attrs.SourceVolumes))
	for i, sv := range attrs.SourceVolumes {
		names[i] = sv.Name
		branches[i] = sv.Name + "=" + sv.Mode
		modeByName[sv.Name] = sv.Mode
	}

	pod, err := d.resolver.GetPod(ctx, attrs.PodNamespace, attrs.PodName, attrs.PodUID)
	if err != nil {
		return nil, resolveError(err)
	}

	if d.config.ReuseMounts {
		if name, source := d.sharedSource(pod, req.GetTargetPath()); source != "" {
			return d.publishShared(ctx, req, pod, name, source)
		}
	}

	resolved, err := d.resolver.ResolveIn(ctx, pod, names)
	if err != nil {
		return nil, resolveError(err)
	}

	// Kubelet retries NodePublishVolume with backoff; sibling mounts progress
	// independently, so waiting here never deadlocks.
	if err := volsource.WaitReady(ctx, d.mounter, resolved, d.config.PublishTimeout); err != nil {
		return nil, status.Error(codes.Aborted, err.Error())
	}

	sources := make([]backend.Source, len(resolved))
	for i, sp := range resolved {
		path, err := sp.RealPath()
		if err != nil {
			return nil, status.Errorf(codes.Aborted, "source volume %q: %v", sp.Name, err)
		}
		sources[i] = backend.Source{Path: path, Mode: modeByName[sp.Name]}
	}

	if err := os.MkdirAll(req.GetTargetPath(), 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create target path: %v", err)
	}

	spec := backend.MountSpec{
		VolumeID: req.GetVolumeId(),
		Target:   req.GetTargetPath(),
		Sources:  sources,
		Options:  options,
		ReadOnly: req.GetReadonly(),
	}
	if err := d.config.Backend.Mount(ctx, spec); err != nil {
		return nil, status.Errorf(codes.Internal, "mount: %v", err)
	}
	klog.Infof("mounted %s for pod %s/%s from %s at %s", req.GetVolumeId(), attrs.PodNamespace, attrs.PodName,
		strings.Join(branches, ":"), req.GetTargetPath())

	return &csi.NodePublishVolumeResponse{}, nil
}

func resolveError(err error) error {
	var notReady *volsource.NotReadyError
	if errors.As(err, &notReady) {
		return status.Error(codes.Aborted, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}

// sharedSource returns the first volume of pod that kubelet sets up and that
// declares the same union as target, or empty strings if that is target itself.
func (d *Driver) sharedSource(pod *corev1.Pod, target string) (string, string) {
	// Any other layout than kubelet's is not shared rather than guessed at.
	csiDir := filepath.Join(d.config.KubeletRoot, "pods", string(pod.UID), "volumes", "kubernetes.io~csi")
	volumeDir := filepath.Dir(target)
	if filepath.Base(target) != "mount" || filepath.Dir(volumeDir) != csiDir {
		return "", ""
	}
	self := filepath.Base(volumeDir)

	var want *unionDecl
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == self {
			want = d.declaredUnion(&pod.Spec.Volumes[i])
		}
	}
	if want == nil {
		return "", ""
	}

	referenced := volsource.ReferencedVolumes(pod)
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.Name == self {
			return "", ""
		}
		// Never published, so it would never mount the union for the others.
		if _, used := referenced[v.Name]; !used {
			continue
		}
		if got := d.declaredUnion(v); got != nil && got.equal(want) {
			return v.Name, filepath.Join(csiDir, v.Name, "mount")
		}
	}
	return "", ""
}

// unionDecl is what makes two volumes of a pod the same union.
type unionDecl struct {
	sources  []SourceVolume
	options  map[string]string
	readOnly bool
}

func (d *Driver) declaredUnion(v *corev1.Volume) *unionDecl {
	if v.CSI == nil || v.CSI.Driver != d.config.DriverName {
		return nil
	}
	attrs, err := d.parseAttributes(v.CSI.VolumeAttributes)
	if err != nil {
		return nil
	}
	return &unionDecl{
		sources:  attrs.SourceVolumes,
		options:  attrs.Options,
		readOnly: v.CSI.ReadOnly != nil && *v.CSI.ReadOnly,
	}
}

func (u *unionDecl) equal(o *unionDecl) bool {
	return slices.Equal(u.sources, o.sources) && maps.Equal(u.options, o.options) && u.readOnly == o.readOnly
}

func (d *Driver) publishShared(ctx context.Context, req *csi.NodePublishVolumeRequest, pod *corev1.Pod, name, source string) (*csi.NodePublishVolumeResponse, error) {
	first := volsource.SourcePath{Name: name, Path: source, CSIBased: true}
	if err := volsource.WaitReady(ctx, d.mounter, []volsource.SourcePath{first}, d.config.PublishTimeout); err != nil {
		return nil, status.Errorf(codes.Aborted, "volume %q declares the same union and mounts it for this one: %v", name, err)
	}

	if err := os.MkdirAll(req.GetTargetPath(), 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create target path: %v", err)
	}
	if err := d.config.Backend.Share(ctx, req.GetVolumeId(), source, req.GetTargetPath(), req.GetReadonly()); err != nil {
		return nil, status.Errorf(codes.Internal, "share: %v", err)
	}
	klog.Infof("mounted %s for pod %s/%s as a view of volume %q at %s", req.GetVolumeId(), pod.Namespace, pod.Name,
		name, req.GetTargetPath())

	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts req.TargetPath and removes it. Idempotent:
// succeeds if the target is already gone.
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "target path missing in request")
	}

	// Kubelet repeats unpublishes for targets that are already gone.
	mounted, err := d.isMounted(req.GetTargetPath())
	mounted = mounted || err != nil
	if err := d.config.Backend.Unmount(ctx, req.GetVolumeId(), req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount: %v", err)
	}
	// Not RemoveAll: if the target is still mounted, that would delete through the
	// merge into the source volumes. Failing lets kubelet retry the unpublish.
	if err := os.Remove(req.GetTargetPath()); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "remove target path: %v", err)
	}
	if mounted {
		klog.Infof("unmounted %s at %s", req.GetVolumeId(), req.GetTargetPath())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// isMounted reports whether path is a mountpoint, treating a missing path as not
// mounted and a corrupted mount as mounted so it gets unpublished rather than
// silently republished over.
func (d *Driver) isMounted(path string) (bool, error) {
	mounted, err := d.mounter.IsMountPoint(path)
	switch {
	case err == nil:
		return mounted, nil
	case os.IsNotExist(err):
		return false, nil
	case mount.IsCorruptedMnt(err):
		return true, nil
	default:
		return false, err
	}
}

func (d *Driver) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.config.NodeID,
	}, nil
}

// NodeGetCapabilities returns no capabilities: staging is not advertised (sources
// are pod-scoped, keyed by pod UID, and cannot be computed once and shared).
func (d *Driver) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}
