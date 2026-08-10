package volsource

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testNamespace   = "default"
	testPod         = "app"
	testUID         = "pod-uid-1"
	testKubeletRoot = "/var/lib/kubelet"
	testDriverName  = "mergerfs.csi.ruekov.eu"
)

func podVolumesRoot() string {
	return filepath.Join(testKubeletRoot, "pods", testUID, "volumes")
}

func TestResolvePVC(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "base-data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "base"},
				}},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "base", Namespace: testNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-base-xyz"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-base-xyz"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "some.csi.driver"}}},
	}

	client := fake.NewSimpleClientset(pod, pvc, pv)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	got, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"base-data"})
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := filepath.Join(podVolumesRoot(), "kubernetes.io~csi", "pv-base-xyz", "mount")
	if len(got) != 1 || got[0].Path != want || !got[0].CSIBased {
		t.Fatalf("Resolve() = %+v, want path %q CSIBased=true", got, want)
	}
}

func TestResolveInlineCSI(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "scratch", VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{Driver: "other.csi.driver"},
				}},
			},
		},
	}
	client := fake.NewSimpleClientset(pod)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	got, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"scratch"})
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	// Inline CSI volumes are named by pod volume name, not PV name - the
	// asymmetry the plan calls out as the most likely bug in the project.
	want := filepath.Join(podVolumesRoot(), "kubernetes.io~csi", "scratch", "mount")
	if len(got) != 1 || got[0].Path != want || !got[0].CSIBased {
		t.Fatalf("Resolve() = %+v, want path %q CSIBased=true", got, want)
	}
}

func TestResolveEmptyDir(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}
	client := fake.NewSimpleClientset(pod)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	got, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"cache"})
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := filepath.Join(podVolumesRoot(), "kubernetes.io~empty-dir", "cache")
	if len(got) != 1 || got[0].Path != want || got[0].CSIBased {
		t.Fatalf("Resolve() = %+v, want path %q CSIBased=false", got, want)
	}
}

func TestResolveConfigMap(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
			},
		},
	}
	client := fake.NewSimpleClientset(pod)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	got, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"cfg"})
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := filepath.Join(podVolumesRoot(), "kubernetes.io~configmap", "cfg")
	if len(got) != 1 || got[0].Path != want {
		t.Fatalf("Resolve() = %+v, want path %q", got, want)
	}
}

func TestResolveUnboundPVCIsRetryable(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "base-data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "base"},
				}},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "base", Namespace: testNamespace},
		// VolumeName intentionally empty: unbound.
	}
	client := fake.NewSimpleClientset(pod, pvc)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	_, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"base-data"})
	var notReady *NotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("Resolve() error = %v, want *NotReadyError", err)
	}
}

func TestResolveNonCSIPVRejected(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "base-data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "base"},
				}},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "base", Namespace: testNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-intree"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-intree"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: "nfs.example.com", Path: "/export"},
		}},
	}
	client := fake.NewSimpleClientset(pod, pvc, pv)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	_, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"base-data"})
	if err == nil {
		t.Fatal("Resolve() = nil, want error for non-CSI PV")
	}
	var notReady *NotReadyError
	if errors.As(err, &notReady) {
		t.Fatal("Resolve() returned *NotReadyError for a non-CSI PV, want a non-retryable error")
	}
}

func TestResolveMissingVolumeName(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{}},
	}
	client := fake.NewSimpleClientset(pod)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	_, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"missing"})
	if err == nil {
		t.Fatal("Resolve() = nil, want error for a name absent from pod.Spec.Volumes")
	}
}

func TestResolveContainment(t *testing.T) {
	// A PV name containing path separators (however it got there) must not let
	// the resolved path escape the pod's volumes directory.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "base-data", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "base"},
				}},
			},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "base", Namespace: testNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "../../../etc"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "../../../etc"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "some.csi.driver"}}},
	}
	client := fake.NewSimpleClientset(pod, pvc, pv)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	_, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"base-data"})
	if err == nil {
		t.Fatal("Resolve() = nil, want containment error for an escaping resolved path")
	}
}

func TestResolvePodUIDMismatch(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID("different-uid")},
		Spec:       corev1.PodSpec{Volumes: []corev1.Volume{}},
	}
	client := fake.NewSimpleClientset(pod)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	_, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"x"})
	if err == nil {
		t.Fatal("Resolve() = nil, want error for pod UID mismatch (closes the name-reuse window)")
	}
}

func TestResolveCycleGuard(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "other-merge", VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{Driver: testDriverName},
				}},
			},
		},
	}
	client := fake.NewSimpleClientset(pod)
	r := NewResolver(client, testKubeletRoot, testDriverName)

	_, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"other-merge"})
	if err == nil {
		t.Fatal("Resolve() = nil, want error referencing another of this driver's own volumes as a source")
	}
}
