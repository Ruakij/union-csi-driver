//go:build linux && mounttest

// Real overlayfs mounts against real directories. Needs a Linux kernel and
// CAP_SYS_ADMIN, so it is behind the mounttest build tag: see "make test-mount".
package overlay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

// newWorkspace returns a directory backed by its own tmpfs. A container's own
// root is usually overlayfs, which the kernel refuses as an overlay upperdir.
func newWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		t.Fatalf("mount tmpfs on %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = unix.Unmount(dir, unix.MNT_DETACH) })
	return dir
}

func makeSource(t *testing.T, root, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, content := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func makeTarget(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unmountUnion(dir) })
	return dir
}

func wantContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func newBackend(t *testing.T) backend.Backend {
	t.Helper()
	return New()
}

func TestMountReadOnlyMerge(t *testing.T) {
	ws := newWorkspace(t)
	a := makeSource(t, ws, "a", map[string]string{"a.txt": "from-a", "both.txt": "from-a"})
	b := makeSource(t, ws, "b", map[string]string{"b.txt": "from-b", "both.txt": "from-b"})
	target := makeTarget(t, ws)

	be := newBackend(t)
	spec := backend.MountSpec{
		VolumeID: "vol-ro",
		Target:   target,
		Sources:  []backend.Source{{Path: a, Mode: modeRO}, {Path: b, Mode: modeRO}},
		Options:  be.DefaultOptions(),
	}
	if err := be.Mount(context.Background(), spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	wantContent(t, filepath.Join(target, "a.txt"), "from-a")
	wantContent(t, filepath.Join(target, "b.txt"), "from-b")
	// Leftmost source wins on lookup.
	wantContent(t, filepath.Join(target, "both.txt"), "from-a")

	// No upperdir means no writable layer, whatever the request said.
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("x"), 0o644); err == nil {
		t.Error("write to an all-RO merge succeeded, want a read-only filesystem")
	}
}

func TestMountWritableUpper(t *testing.T) {
	ws := newWorkspace(t)
	rw := makeSource(t, ws, "rw", nil)
	ro := makeSource(t, ws, "ro", map[string]string{"lower.txt": "from-lower"})
	target := makeTarget(t, ws)

	be := newBackend(t)
	spec := backend.MountSpec{
		VolumeID: "vol-rw",
		Target:   target,
		Sources:  []backend.Source{{Path: rw, Mode: modeRW}, {Path: ro, Mode: modeRO}},
		Options:  be.DefaultOptions(),
	}
	if err := be.Mount(context.Background(), spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	wantContent(t, filepath.Join(target, "lower.txt"), "from-lower")
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("written"), 0o644); err != nil {
		t.Fatalf("write to the merge: %v", err)
	}

	// Writes land in the driver-created workspace, not at the volume root, and the
	// lower is untouched.
	wantContent(t, filepath.Join(rw, workspaceDir, upperName, "new.txt"), "written")
	if _, err := os.Stat(filepath.Join(ro, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("stat lower/new.txt = %v, want not-exist", err)
	}

	// Copy-up: editing a lower file writes a copy to the upper.
	if err := os.WriteFile(filepath.Join(target, "lower.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatalf("edit a lower file: %v", err)
	}
	wantContent(t, filepath.Join(rw, workspaceDir, upperName, "lower.txt"), "edited")
	wantContent(t, filepath.Join(ro, "lower.txt"), "from-lower")
}

func TestMountReadOnlyRequestOverridesWritableSource(t *testing.T) {
	ws := newWorkspace(t)
	rw := makeSource(t, ws, "rw", nil)
	ro := makeSource(t, ws, "ro", map[string]string{"lower.txt": "from-lower"})
	target := makeTarget(t, ws)

	be := newBackend(t)
	spec := backend.MountSpec{
		VolumeID: "vol-forced-ro",
		Target:   target,
		Sources:  []backend.Source{{Path: rw, Mode: modeRW}, {Path: ro, Mode: modeRO}},
		Options:  be.DefaultOptions(),
		ReadOnly: true,
	}
	if err := be.Mount(context.Background(), spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	wantContent(t, filepath.Join(target, "lower.txt"), "from-lower")
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("x"), 0o644); err == nil {
		t.Error("write to a readOnly merge succeeded, want a read-only filesystem")
	}
}

func TestMountSingleSourceIsABind(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		// where the merged content is expected to live in the source volume
		contentDir string
	}{
		{name: "read-only source", mode: modeRO, contentDir: "."},
		{name: "writable source", mode: modeRW, contentDir: filepath.Join(workspaceDir, upperName)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := newWorkspace(t)
			src := makeSource(t, ws, "src", nil)
			target := makeTarget(t, ws)

			content := filepath.Join(src, tc.contentDir)
			if err := os.MkdirAll(content, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(content, "only.txt"), []byte("alone"), 0o644); err != nil {
				t.Fatal(err)
			}

			be := newBackend(t)
			spec := backend.MountSpec{
				VolumeID: "vol-single",
				Target:   target,
				Sources:  []backend.Source{{Path: src, Mode: tc.mode}},
				Options:  be.DefaultOptions(),
			}
			if err := be.Mount(context.Background(), spec); err != nil {
				t.Fatalf("Mount: %v", err)
			}
			wantContent(t, filepath.Join(target, "only.txt"), "alone")
		})
	}
}

// The classic mount API is the fallback on kernels without fsopen. CI kernels
// have fsopen, so mountUnion would never reach it: call it directly.
func TestMountClassicMatchesFsconfig(t *testing.T) {
	ws := newWorkspace(t)
	rw := makeSource(t, ws, "rw", nil)
	ro := makeSource(t, ws, "ro", map[string]string{"lower.txt": "from-lower"})
	target := makeTarget(t, ws)

	be := newBackend(t)
	spec := backend.MountSpec{
		VolumeID: "vol-classic",
		Target:   target,
		Sources:  []backend.Source{{Path: rw, Mode: modeRW}, {Path: ro, Mode: modeRO}},
		Options:  be.DefaultOptions(),
	}
	l, err := planLayout(spec)
	if err != nil {
		t.Fatalf("planLayout: %v", err)
	}
	if err := os.MkdirAll(l.work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.upper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mountClassic(l, target, be.Schema()); err != nil {
		t.Fatalf("mountClassic: %v", err)
	}

	wantContent(t, filepath.Join(target, "lower.txt"), "from-lower")
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("written"), 0o644); err != nil {
		t.Fatalf("write to the merge: %v", err)
	}
	wantContent(t, filepath.Join(rw, workspaceDir, upperName, "new.txt"), "written")
}

func TestUnmountIsIdempotent(t *testing.T) {
	ws := newWorkspace(t)
	src := makeSource(t, ws, "src", map[string]string{"f.txt": "x"})
	target := makeTarget(t, ws)

	be := newBackend(t)
	spec := backend.MountSpec{
		VolumeID: "vol-unmount",
		Target:   target,
		Sources:  []backend.Source{{Path: src, Mode: modeRO}},
		Options:  be.DefaultOptions(),
	}
	if err := be.Mount(context.Background(), spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := be.Unmount(context.Background(), spec.VolumeID, target); err != nil {
			t.Fatalf("Unmount %d: %v", i, err)
		}
	}
	// Unmounting a path that was never mounted is not an error either.
	if err := be.Unmount(context.Background(), "vol-never", filepath.Join(ws, "src")); err != nil {
		t.Fatalf("Unmount of an unmounted path: %v", err)
	}
}
