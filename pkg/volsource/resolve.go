// Package volsource resolves a pod's declared volumes, by pod volume name, to
// their node-local kubelet publish paths, and waits for those paths to become
// ready. Shared by all backends - see .docs/plan.md section 2.
package volsource

import (
	"context"

	"k8s.io/client-go/kubernetes"
)

// SourcePath is one resolved source: the pod volume name it came from, its
// node-local path, and whether it is CSI-backed (must be polled as a real
// mountpoint) or a plain directory (existence is enough).
type SourcePath struct {
	Name     string
	Path     string
	CSIBased bool
}

// Resolver maps pod volume names to kubelet's on-disk publish paths.
type Resolver struct {
	client      kubernetes.Interface
	kubeletRoot string
}

// NewResolver builds a Resolver. kubeletRoot is the node's kubelet directory
// (--kubelet-root, default /var/lib/kubelet).
func NewResolver(client kubernetes.Interface, kubeletRoot string) *Resolver {
	return &Resolver{client: client, kubeletRoot: kubeletRoot}
}

// Resolve maps each requested pod volume name to its kubelet publish path.
//
// TODO implement per .docs/plan.md section 2:
//  1. GET the Pod, verify pod.UID matches podUID (closes the name-reuse window).
//  2. Find each name in pod.Spec.Volumes; InvalidArgument if absent.
//  3. Map to <kubeletRoot>/pods/<podUID>/volumes/...: PVC via PV name
//     (kubernetes.io~csi/<pvName>/mount, PV must have Spec.CSI != nil), inline csi
//     via pod volume name, emptyDir/configMap/secret/downwardAPI/projected via their
//     plugin dirs. Anything else: InvalidArgument.
//  4. filepath.Clean and assert containment under .../pods/<podUID>/volumes/.
//  5. Refuse references to this driver's own target path or sibling volumes
//     (cycle guard).
func (r *Resolver) Resolve(ctx context.Context, podNamespace, podName, podUID string, names []string) ([]SourcePath, error) {
	panic("not implemented")
}
