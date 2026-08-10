package driver

import (
	"context"
	"errors"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	modeByName := make(map[string]string, len(attrs.SourceVolumes))
	for i, sv := range attrs.SourceVolumes {
		names[i] = sv.Name
		modeByName[sv.Name] = sv.Mode
	}

	resolved, err := d.resolver.Resolve(ctx, attrs.PodNamespace, attrs.PodName, attrs.PodUID, names)
	if err != nil {
		var notReady *volsource.NotReadyError
		if errors.As(err, &notReady) {
			return nil, status.Error(codes.Aborted, err.Error())
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Kubelet retries NodePublishVolume with backoff; sibling mounts progress
	// independently, so waiting here never deadlocks.
	if err := volsource.WaitReady(ctx, d.mounter, resolved, d.config.PublishTimeout); err != nil {
		return nil, status.Error(codes.Aborted, err.Error())
	}

	sources := make([]backend.Source, len(resolved))
	for i, sp := range resolved {
		sources[i] = backend.Source{Path: sp.Path, Mode: modeByName[sp.Name]}
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

	if err := d.config.Backend.Unmount(ctx, req.GetVolumeId(), req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount: %v", err)
	}
	if err := os.RemoveAll(req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "remove target path: %v", err)
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

func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.config.NodeID,
	}, nil
}

// NodeGetCapabilities returns no capabilities: staging is not advertised (sources
// are pod-scoped, keyed by pod UID, and cannot be computed once and shared).
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}
