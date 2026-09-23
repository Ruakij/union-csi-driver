// Package volsource resolves a pod's declared volumes, by pod volume name, to
// their node-local kubelet publish paths, and waits for those paths to become
// ready. Shared by all backends - see .docs/plan.md section 2.
package volsource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NotReadyError marks a failure that is expected to resolve itself, e.g. an
// unbound PVC. Callers should surface it as a retryable gRPC status
// (codes.Aborted/Unavailable) rather than InvalidArgument.
type NotReadyError struct {
	msg string
}

func (e *NotReadyError) Error() string { return e.msg }

func notReadyf(format string, args ...interface{}) error {
	return &NotReadyError{msg: fmt.Sprintf(format, args...)}
}

// SourcePath is one resolved source: the pod volume name it came from, its
// node-local path, and whether it is CSI-backed (must be polled as a real
// mountpoint) or a plain directory (existence is enough). Root is the
// containment base the path must sit under (kubelet's pod volumes dir, or the
// host path mount dir for hostPath).
type SourcePath struct {
	Name     string
	Path     string
	CSIBased bool
	Root     string

	// hostPaths is set for hostPath sources, whose resolved path it re-checks.
	hostPaths *HostPaths
}

// Resolver maps pod volume names to paths the driver container can see.
type Resolver struct {
	client      kubernetes.Interface
	kubeletRoot string
	hostPaths   HostPaths
	// ownDriverName is this driver's own CSI driver name, used to refuse
	// referencing another instance of this driver as a source (cycle guard).
	ownDriverName string
}

// NewResolver builds a Resolver. kubeletRoot is the node's kubelet directory
// (--kubelet-root, default /var/lib/kubelet). hostPaths governs hostPath
// sources. ownDriverName is the configured --drivername, used for the cycle guard.
func NewResolver(client kubernetes.Interface, kubeletRoot string, hostPaths HostPaths, ownDriverName string) *Resolver {
	return &Resolver{client: client, kubeletRoot: kubeletRoot, hostPaths: hostPaths, ownDriverName: ownDriverName}
}

// Resolve maps each requested pod volume name to its kubelet publish path.
func (r *Resolver) Resolve(ctx context.Context, podNamespace, podName, podUID string, names []string) ([]SourcePath, error) {
	pod, err := r.client.CoreV1().Pods(podNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, notReadyf("pod %s/%s not found", podNamespace, podName)
		}
		return nil, fmt.Errorf("get pod %s/%s: %w", podNamespace, podName, err)
	}
	if string(pod.UID) != podUID {
		return nil, fmt.Errorf("pod %s/%s UID %q does not match injected UID %q", podNamespace, podName, pod.UID, podUID)
	}

	byName := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		byName[v.Name] = v
	}
	referenced := referencedVolumes(pod)

	podVolumesRoot := filepath.Join(r.kubeletRoot, "pods", podUID, "volumes")

	results := make([]SourcePath, 0, len(names))
	for _, name := range names {
		vol, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("sourceVolumes: pod volume %q not found in pod spec", name)
		}

		// Kubelet skips volumes no container references, so such a source never
		// gets set up and waiting for it can only time out.
		if _, used := referenced[name]; !used {
			return nil, fmt.Errorf("sourceVolumes: pod volume %q is not mounted by any container in this pod, so kubelet never sets it up; add a volumeMount for it in any container", name)
		}

		sp, err := r.resolveOne(ctx, pod, podVolumesRoot, vol)
		if err != nil {
			return nil, fmt.Errorf("sourceVolumes: %q: %w", name, err)
		}

		if sp.Root != "" {
			if err := assertContained(sp.Root, sp.Path); err != nil {
				return nil, fmt.Errorf("sourceVolumes: %q: %w", name, err)
			}
		}

		results = append(results, sp)
	}

	return results, nil
}

// referencedVolumes collects the pod volume names some container mounts, using
// the same containers kubelet consults when deciding which volumes to set up.
func referencedVolumes(pod *corev1.Pod) map[string]struct{} {
	refs := make(map[string]struct{}, len(pod.Spec.Volumes))
	add := func(mounts []corev1.VolumeMount, devices []corev1.VolumeDevice) {
		for _, m := range mounts {
			refs[m.Name] = struct{}{}
		}
		for _, d := range devices {
			refs[d.Name] = struct{}{}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		add(c.VolumeMounts, c.VolumeDevices)
	}
	for _, c := range pod.Spec.Containers {
		add(c.VolumeMounts, c.VolumeDevices)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		add(c.VolumeMounts, c.VolumeDevices)
	}
	return refs
}

func (r *Resolver) resolveOne(ctx context.Context, pod *corev1.Pod, podVolumesRoot string, vol corev1.Volume) (SourcePath, error) {
	switch {
	case vol.PersistentVolumeClaim != nil, vol.Ephemeral != nil:
		return r.resolvePVC(ctx, pod, podVolumesRoot, vol)

	case vol.CSI != nil:
		if vol.CSI.Driver == r.ownDriverName {
			return SourcePath{}, fmt.Errorf("refers to another %s volume in the same pod, which would create a mount cycle", r.ownDriverName)
		}
		return SourcePath{
			Name:     vol.Name,
			Path:     filepath.Join(podVolumesRoot, "kubernetes.io~csi", vol.Name, "mount"),
			CSIBased: true,
			Root:     podVolumesRoot,
		}, nil

	case vol.EmptyDir != nil:
		return SourcePath{
			Name: vol.Name,
			Path: filepath.Join(podVolumesRoot, "kubernetes.io~empty-dir", vol.Name),
			Root: podVolumesRoot,
		}, nil

	case vol.ConfigMap != nil:
		return SourcePath{Name: vol.Name, Path: filepath.Join(podVolumesRoot, "kubernetes.io~configmap", vol.Name), Root: podVolumesRoot}, nil

	case vol.Secret != nil:
		return SourcePath{Name: vol.Name, Path: filepath.Join(podVolumesRoot, "kubernetes.io~secret", vol.Name), Root: podVolumesRoot}, nil

	case vol.DownwardAPI != nil:
		return SourcePath{Name: vol.Name, Path: filepath.Join(podVolumesRoot, "kubernetes.io~downward-api", vol.Name), Root: podVolumesRoot}, nil

	case vol.Projected != nil:
		return SourcePath{Name: vol.Name, Path: filepath.Join(podVolumesRoot, "kubernetes.io~projected", vol.Name), Root: podVolumesRoot}, nil

	case vol.HostPath != nil:
		return r.hostPath(vol.Name, vol.HostPath.Path)

	case vol.NFS != nil:
		return SourcePath{
			Name:     vol.Name,
			Path:     filepath.Join(podVolumesRoot, "kubernetes.io~nfs", vol.Name),
			CSIBased: true,
			Root:     podVolumesRoot,
		}, nil

	case vol.ISCSI != nil:
		return SourcePath{
			Name:     vol.Name,
			Path:     filepath.Join(podVolumesRoot, "kubernetes.io~iscsi", vol.Name),
			CSIBased: true,
			Root:     podVolumesRoot,
		}, nil

	case vol.FC != nil:
		return SourcePath{
			Name:     vol.Name,
			Path:     filepath.Join(podVolumesRoot, "kubernetes.io~fc", vol.Name),
			CSIBased: true,
			Root:     podVolumesRoot,
		}, nil

	case vol.Image != nil:
		return SourcePath{}, fmt.Errorf("image volumes are mounted by the container runtime into the container only, and never appear on the node")

	default:
		return SourcePath{}, fmt.Errorf("unsupported volume source")
	}
}

func (r *Resolver) resolvePVC(ctx context.Context, pod *corev1.Pod, podVolumesRoot string, vol corev1.Volume) (SourcePath, error) {
	podNamespace := pod.Namespace
	var claimName string
	if vol.Ephemeral != nil {
		// The ephemeral volume controller names the claim after pod and volume.
		claimName = pod.Name + "-" + vol.Name
	} else {
		claimName = vol.PersistentVolumeClaim.ClaimName
	}
	pvc, err := r.client.CoreV1().PersistentVolumeClaims(podNamespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SourcePath{}, notReadyf("PVC %s/%s not found", podNamespace, claimName)
		}
		return SourcePath{}, fmt.Errorf("get PVC %s/%s: %w", podNamespace, claimName, err)
	}
	// A same-named claim not owned by this pod is someone else's volume; kubelet
	// refuses it too.
	if vol.Ephemeral != nil && !metav1.IsControlledBy(pvc, pod) {
		return SourcePath{}, fmt.Errorf("PVC %s/%s was not created for this pod", podNamespace, claimName)
	}
	if pvc.Spec.VolumeName == "" {
		return SourcePath{}, notReadyf("PVC %s/%s is not yet bound", podNamespace, claimName)
	}

	pv, err := r.client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SourcePath{}, notReadyf("PV %q not found", pvc.Spec.VolumeName)
		}
		return SourcePath{}, fmt.Errorf("get PV %q: %w", pvc.Spec.VolumeName, err)
	}

	// Kubelet names a PV's pod volume directory after the PV, not the pod volume.
	var pluginDir string
	switch {
	case pv.Spec.CSI != nil:
		return SourcePath{
			Name:     vol.Name,
			Path:     filepath.Join(podVolumesRoot, "kubernetes.io~csi", pv.Name, "mount"),
			CSIBased: true,
			Root:     podVolumesRoot,
		}, nil
	case pv.Spec.HostPath != nil:
		return r.hostPath(vol.Name, pv.Spec.HostPath.Path)
	case pv.Spec.Local != nil:
		pluginDir = "kubernetes.io~local-volume"
	case pv.Spec.NFS != nil:
		pluginDir = "kubernetes.io~nfs"
	case pv.Spec.ISCSI != nil:
		pluginDir = "kubernetes.io~iscsi"
	case pv.Spec.FC != nil:
		pluginDir = "kubernetes.io~fc"
	default:
		return SourcePath{}, fmt.Errorf("PV %q uses an unsupported volume plugin", pv.Name)
	}
	return SourcePath{
		Name:     vol.Name,
		Path:     filepath.Join(podVolumesRoot, pluginDir, pv.Name),
		CSIBased: true,
		Root:     podVolumesRoot,
	}, nil
}

// hostPath maps a host-absolute path under the host path mount dir, where the
// DaemonSet bind-mounts each allowed directory, so the driver container and the
// mergerfs daemon (which shares its mount namespace) can see it. Kubelet sets up
// no mount for hostPath, so the directory itself is the source.
func (r *Resolver) hostPath(name, host string) (SourcePath, error) {
	host = filepath.Join("/", host)
	if err := r.hostPaths.check(host); err != nil {
		return SourcePath{}, err
	}
	return SourcePath{Name: name, Path: filepath.Join(r.hostPaths.Root, host), Root: r.hostPaths.Root, hostPaths: &r.hostPaths}, nil
}

// assertContained ensures resolved is lexically under root. Every input is
// validated already, but this is the invariant that matters.
func assertContained(root, resolved string) error {
	root = filepath.Clean(root)
	resolved = filepath.Clean(resolved)
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return fmt.Errorf("resolved path %q escapes %q", resolved, root)
	}
	return nil
}

// maxSymlinks matches the kernel's own limit before ELOOP.
const maxSymlinks = 40

// RealPath resolves symlinks in p.Path as if Root were "/". A hostPath directory
// that is an absolute symlink on the node, e.g. /data -> /mnt/disk, must lead to
// <hostRoot>/mnt/disk, whereas the kernel would follow it inside this container.
func (p SourcePath) RealPath() (string, error) {
	if p.Root == "" {
		return p.Path, nil
	}
	rel, err := filepath.Rel(p.Root, p.Path)
	if err != nil {
		return "", err
	}
	pending := strings.Split(rel, string(filepath.Separator))
	cur := "/"
	links := 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(filepath.Join(p.Root, next))
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > maxSymlinks {
			return "", fmt.Errorf("%s: too many levels of symbolic links", p.Path)
		}
		target, err := os.Readlink(filepath.Join(p.Root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			cur = "/"
		}
		// Refuse early: a directory outside the allowed ones is not mounted here,
		// and would otherwise look like one that does not exist yet.
		if dest := filepath.Join(cur, target); p.hostPaths != nil && !p.hostPaths.reachable(dest) {
			return "", fmt.Errorf("%s links to %s: %w", p.Path, dest, ErrHostPathNotAllowed)
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	if p.hostPaths != nil {
		if err := p.hostPaths.check(cur); err != nil {
			return "", err
		}
	}
	return filepath.Join(p.Root, cur), nil
}
