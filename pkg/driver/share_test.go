package driver

import (
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	shareDriver  = "overlay.csi.ruekov.eu"
	shareRoot    = "/var/lib/kubelet"
	sharePodUID  = "0000-uid"
	shareCSIPath = shareRoot + "/pods/" + sharePodUID + "/volumes/kubernetes.io~csi"
)

func unionVolume(name, sources, options string, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver:           shareDriver,
		ReadOnly:         &readOnly,
		VolumeAttributes: map[string]string{attrSourceVolumes: sources, attrOptions: options},
	}}}
}

// sharePod mounts every volume in its only container, except those named in unmounted.
func sharePod(vols []corev1.Volume, unmounted ...string) *corev1.Pod {
	skip := map[string]bool{}
	for _, n := range unmounted {
		skip[n] = true
	}
	c := corev1.Container{Name: "app"}
	for _, v := range vols {
		if !skip[v.Name] {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: v.Name, MountPath: "/" + v.Name})
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: types.UID(sharePodUID)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{c}, Volumes: vols},
	}
}

func TestSharedSource(t *testing.T) {
	d := newTestDriver(32, overlayLikeBackend())
	d.config.DriverName = shareDriver
	d.config.KubeletRoot = shareRoot
	target := func(name string) string { return filepath.Join(shareCSIPath, name, "mount") }
	emptyDir := corev1.Volume{Name: "rw", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}

	tests := []struct {
		name      string
		vols      []corev1.Volume
		unmounted []string
		target    string
		want      string
	}{
		{
			name:   "the first of two identical unions mounts it itself",
			vols:   []corev1.Volume{emptyDir, unionVolume("a", "rw,top=RO", "", false), unionVolume("b", "rw,top=RO", "", false)},
			target: target("a"),
		},
		{
			name:   "the second of two identical unions shares the first",
			vols:   []corev1.Volume{emptyDir, unionVolume("a", "rw,top=RO", "", false), unionVolume("b", "rw,top=RO", "", false)},
			target: target("b"),
			want:   "a",
		},
		{
			name:   "a bare entry equals its explicit default mode",
			vols:   []corev1.Volume{unionVolume("a", "rw=RW,top=RO", "", false), unionVolume("b", "rw,top=RO", "", false)},
			target: target("b"),
			want:   "a",
		},
		{
			name:   "option order does not matter",
			vols:   []corev1.Volume{unionVolume("a", "rw", "xino=off,index=off", false), unionVolume("b", "rw", "index=off,xino=off", false)},
			target: target("b"),
			want:   "a",
		},
		{
			name:   "a later identical union shares the first, not the second",
			vols:   []corev1.Volume{unionVolume("a", "rw", "", false), unionVolume("b", "rw", "", false), unionVolume("c", "rw", "", false)},
			target: target("c"),
			want:   "a",
		},
		{
			name:   "different source order is a different union",
			vols:   []corev1.Volume{unionVolume("a", "rw,top=RO,bottom=RO", "", false), unionVolume("b", "rw,bottom=RO,top=RO", "", false)},
			target: target("b"),
		},
		{
			name:   "different modes are a different union",
			vols:   []corev1.Volume{unionVolume("a", "rw,top=RO", "", false), unionVolume("b", "rw=RO,top=RO", "", false)},
			target: target("b"),
		},
		{
			name:   "different options are a different union",
			vols:   []corev1.Volume{unionVolume("a", "rw", "xino=off", false), unionVolume("b", "rw", "", false)},
			target: target("b"),
		},
		{
			name:   "different readOnly is a different union",
			vols:   []corev1.Volume{unionVolume("a", "rw", "", true), unionVolume("b", "rw", "", false)},
			target: target("b"),
		},
		{
			name: "another driver's volume is never shared",
			vols: func() []corev1.Volume {
				a := unionVolume("a", "rw", "", false)
				a.CSI.Driver = "mergerfs.csi.ruekov.eu"
				return []corev1.Volume{a, unionVolume("b", "rw", "", false)}
			}(),
			target: target("b"),
		},
		{
			name:      "a volume no container mounts is never published, so it is skipped",
			vols:      []corev1.Volume{unionVolume("a", "rw", "", false), unionVolume("b", "rw", "", false), unionVolume("c", "rw", "", false)},
			unmounted: []string{"a"},
			target:    target("c"),
			want:      "b",
		},
		{
			name:   "an unparseable earlier volume is skipped",
			vols:   []corev1.Volume{unionVolume("a", "../etc", "", false), unionVolume("b", "rw", "", false)},
			target: target("b"),
		},
		{
			name:   "a target outside kubelet's layout is not shared",
			vols:   []corev1.Volume{unionVolume("a", "rw", "", false), unionVolume("b", "rw", "", false)},
			target: "/tmp/csi/target",
		},
		{
			name:   "a target of another pod is not shared",
			vols:   []corev1.Volume{unionVolume("a", "rw", "", false), unionVolume("b", "rw", "", false)},
			target: shareRoot + "/pods/other-uid/volumes/kubernetes.io~csi/b/mount",
		},
		{
			name:   "a target naming no pod volume is not shared",
			vols:   []corev1.Volume{unionVolume("a", "rw", "", false)},
			target: target("gone"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, source := d.sharedSource(sharePod(tt.vols, tt.unmounted...), tt.target)
			if name != tt.want {
				t.Fatalf("sharedSource() = %q, want %q", name, tt.want)
			}
			if tt.want != "" && source != target(tt.want) {
				t.Fatalf("sharedSource() source = %q, want %q", source, target(tt.want))
			}
			if tt.want == "" && source != "" {
				t.Fatalf("sharedSource() source = %q, want none", source)
			}
		})
	}
}
