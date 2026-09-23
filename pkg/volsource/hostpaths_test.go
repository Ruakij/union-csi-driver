package volsource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHostPathsCheck(t *testing.T) {
	scoped := HostPaths{Allowed: []string{"/srv", "/data"}, Denied: []string{"/srv/secret"}}
	whole := HostPaths{Allowed: []string{"/"}, Denied: []string{"/etc"}}

	for _, tc := range []struct {
		name  string
		h     HostPaths
		host  string
		allow bool
	}{
		{"allowed dir itself", scoped, "/srv", true},
		{"below an allowed dir", scoped, "/srv/a/b", true},
		{"second allowed dir", scoped, "/data", true},
		{"sibling sharing a prefix", scoped, "/srvx", false},
		{"outside", scoped, "/etc", false},
		{"denied dir itself", scoped, "/srv/secret", false},
		{"below a denied dir", scoped, "/srv/secret/a", false},
		{"disabled", HostPaths{}, "/srv", false},
		{"whole host", whole, "/opt/data", true},
		{"denied under the whole host", whole, "/etc/ssl", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.h.check(tc.host)
			if tc.allow && err != nil {
				t.Fatalf("check(%s) = %v, want nil", tc.host, err)
			}
			if !tc.allow && !errors.Is(err, ErrHostPathNotAllowed) {
				t.Fatalf("check(%s) = %v, want ErrHostPathNotAllowed", tc.host, err)
			}
		})
	}
}

func TestHostPathsValidate(t *testing.T) {
	if err := (HostPaths{Root: "/host", Allowed: []string{"/", "/srv"}, Denied: []string{"/etc"}}).Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	for _, bad := range []string{"srv", "/srv/", "/srv/../etc", ""} {
		if err := (HostPaths{Root: "/host", Allowed: []string{bad}}).Validate(); err == nil {
			t.Errorf("Validate() accepted %q", bad)
		}
	}
}

func TestResolveHostPathPolicy(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNamespace, UID: types.UID(testUID)},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/opt/data/../../etc"},
				}},
			},
		},
	}
	for _, h := range []HostPaths{
		{Root: testHostRoot},
		{Root: testHostRoot, Allowed: []string{"/opt/data"}},
		{Root: testHostRoot, Allowed: []string{"/"}, Denied: []string{"/etc"}},
	} {
		r := NewResolver(fake.NewSimpleClientset(mountAll(pod)), testKubeletRoot, h, testDriverName)
		_, err := r.Resolve(context.Background(), testNamespace, testPod, testUID, []string{"data"})
		if !errors.Is(err, ErrHostPathNotAllowed) {
			t.Errorf("Resolve() with %+v = %v, want ErrHostPathNotAllowed", h, err)
		}
	}
}

func TestRealPathHostPathPolicy(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"srv/data", "srv/secret", "mnt/disk"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"srv/escape":   "/etc",
		"srv/sneaky":   "secret",
		"srv/viamnt":   "/mnt",
		"srv/tomnt":    "/mnt/disk",
		"srv/relative": "../srv/data",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	h := &HostPaths{Root: root, Allowed: []string{"/srv", "/mnt/disk"}, Denied: []string{"/srv/secret"}}

	for _, tc := range []struct {
		path, want string
	}{
		{"srv/relative", "srv/data"},
		{"srv/tomnt", "mnt/disk"},
		// /mnt is not allowed, but it is on the way into /mnt/disk.
		{"srv/viamnt/disk", "mnt/disk"},
	} {
		got, err := SourcePath{Path: filepath.Join(root, tc.path), Root: root, hostPaths: h}.RealPath()
		if err != nil {
			t.Fatalf("RealPath(%s): %v", tc.path, err)
		}
		if want := filepath.Join(root, tc.want); got != want {
			t.Errorf("RealPath(%s) = %s, want %s", tc.path, got, want)
		}
	}

	// /etc does not exist under root, so only the policy can explain the refusal.
	for _, path := range []string{"srv/escape", "srv/sneaky", "srv/viamnt"} {
		_, err := SourcePath{Path: filepath.Join(root, path), Root: root, hostPaths: h}.RealPath()
		if !errors.Is(err, ErrHostPathNotAllowed) {
			t.Errorf("RealPath(%s) = %v, want ErrHostPathNotAllowed", path, err)
		}
	}
}

func TestWaitReadyFailsFastOnHostPathPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "srv", "escape")); err != nil {
		t.Fatal(err)
	}
	h := &HostPaths{Root: root, Allowed: []string{"/srv"}}
	paths := []SourcePath{{Name: "hp", Path: filepath.Join(root, "srv", "escape"), Root: root, hostPaths: h}}

	start := time.Now()
	err := WaitReady(context.Background(), nil, paths, 10*time.Second)
	if !errors.Is(err, ErrHostPathNotAllowed) {
		t.Fatalf("WaitReady() = %v, want ErrHostPathNotAllowed", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("WaitReady() took %s, want an immediate refusal", time.Since(start))
	}
}
