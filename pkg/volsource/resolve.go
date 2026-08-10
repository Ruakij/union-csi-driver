// Package volsource resolves a pod's declared volumes, by pod volume name, to
// their node-local kubelet publish paths, and waits for those paths to become
// ready. Shared by all backends - see .docs/plan.md section 2.
package volsource

import (
	"context"
	"fmt"
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
// host-root bind mount for hostPath).
type SourcePath struct {
	Name     string
	Path     string
	CSIBased bool
	Root     string
}

// Resolver maps pod volume names to paths the driver container can see.
type Resolver struct {
	client      kubernetes.Interface
	kubeletRoot string
	// hostRoot is where the node's real root filesystem is bind-mounted inside
	// the driver container (--host-root, default /host). hostPath volume paths
	// are mapped under it, since the DaemonSet mounts the host root there.
	hostRoot string
	// ownDriverName is this driver's own CSI driver name, used to refuse
	// referencing another instance of this driver as a source (cycle guard).
	ownDriverName string
}

// NewResolver builds a Resolver. kubeletRoot is the node's kubelet directory
// (--kubelet-root, default /var/lib/kubelet). hostRoot is where the host root
// is bind-mounted in the container (--host-root, default /host). ownDriverName
// is the configured --drivername, used for the cycle guard.
func NewResolver(client kubernetes.Interface, kubeletRoot, hostRoot, ownDriverName string) *Resolver {
	return &Resolver{client: client, kubeletRoot: kubeletRoot, hostRoot: hostRoot, ownDriverName: ownDriverName}
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

	podVolumesRoot := filepath.Join(r.kubeletRoot, "pods", podUID, "volumes")

	results := make([]SourcePath, 0, len(names))
	for _, name := range names {
		vol, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("sourceVolumes: pod volume %q not found in pod spec", name)
		}

		sp, err := r.resolveOne(ctx, podNamespace, podVolumesRoot, vol)
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

func (r *Resolver) resolveOne(ctx context.Context, podNamespace, podVolumesRoot string, vol corev1.Volume) (SourcePath, error) {
	switch {
	case vol.PersistentVolumeClaim != nil:
		return r.resolvePVC(ctx, podNamespace, podVolumesRoot, vol)

	case vol.CSI != nil, vol.Ephemeral != nil:
		if vol.CSI != nil && vol.CSI.Driver == r.ownDriverName {
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
		// hostPath paths are host-absolute; the DaemonSet bind-mounts the host
		// root at hostRoot, so map the path under it to make it visible to the
		// driver container (and to the mergerfs daemon, which shares its mount
		// namespace). Relative hostPath paths are a kubelet edge case; normalize
		// against the host root first.
		host := vol.HostPath.Path
		if !filepath.IsAbs(host) {
			host = filepath.Join("/", host)
		}
		return SourcePath{
			Name: vol.Name,
			Path: filepath.Join(r.hostRoot, host),
			Root: r.hostRoot,
		}, nil

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
		return SourcePath{
			Name:     vol.Name,
			Path:     filepath.Join(podVolumesRoot, "kubernetes.io~image", vol.Name),
			CSIBased: true,
			Root:     podVolumesRoot,
		}, nil

	default:
		return SourcePath{}, fmt.Errorf("unsupported volume source")
	}
}

func (r *Resolver) resolvePVC(ctx context.Context, podNamespace, podVolumesRoot string, vol corev1.Volume) (SourcePath, error) {
	claimName := vol.PersistentVolumeClaim.ClaimName
	pvc, err := r.client.CoreV1().PersistentVolumeClaims(podNamespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SourcePath{}, notReadyf("PVC %s/%s not found", podNamespace, claimName)
		}
		return SourcePath{}, fmt.Errorf("get PVC %s/%s: %w", podNamespace, claimName, err)
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
	if pv.Spec.CSI == nil {
		return SourcePath{}, fmt.Errorf("PV %q is not CSI-backed (in-tree plugins are not supported)", pv.Name)
	}

	return SourcePath{
		Name:     vol.Name,
		Path:     filepath.Join(podVolumesRoot, "kubernetes.io~csi", pv.Name, "mount"),
		CSIBased: true,
		Root:     podVolumesRoot,
	}, nil
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
