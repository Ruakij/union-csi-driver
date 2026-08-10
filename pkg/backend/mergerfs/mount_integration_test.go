//go:build linux && mounttest

// Real mergerfs mounts against real directories. Needs a Linux kernel, /dev/fuse
// and the mergerfs binary, so it is behind the mounttest build tag: see
// "make test-mount".
package mergerfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

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

// newMount publishes a two-branch merge and returns its target and state dir.
func newMount(t *testing.T, volumeID string) (target, stateDir, rw, ro string) {
	t.Helper()
	root := t.TempDir()
	rw = makeSource(t, root, "rw", map[string]string{"rw.txt": "from-rw"})
	ro = makeSource(t, root, "ro", map[string]string{"ro.txt": "from-ro", "both.txt": "from-ro"})
	target = filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(root, "state")

	be := &mergerfsBackend{}
	if err := be.Init(stateDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Unmount(context.Background(), volumeID, target) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec := backend.MountSpec{
		VolumeID: volumeID,
		Target:   target,
		Sources:  []backend.Source{{Path: rw, Mode: modeRW}, {Path: ro, Mode: modeRO}},
		Options:  be.DefaultOptions(),
	}
	if err := be.Mount(ctx, spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if !isFUSEMount(target) {
		t.Fatalf("%s is not a FUSE mount after Mount", target)
	}
	return target, stateDir, rw, ro
}

func TestMountMergesBranches(t *testing.T) {
	target, _, rw, ro := newMount(t, "vol-merge")

	wantContent(t, filepath.Join(target, "rw.txt"), "from-rw")
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")

	// Writes land in a branch tagged RW, never in an RO one.
	if err := os.WriteFile(filepath.Join(target, "new.txt"), []byte("written"), 0o644); err != nil {
		t.Fatalf("write to the merge: %v", err)
	}
	wantContent(t, filepath.Join(rw, "new.txt"), "written")
	if _, err := os.Stat(filepath.Join(ro, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("stat ro/new.txt = %v, want not-exist", err)
	}

	// Branches may be edited out of band while mounted; that is the reason to pick
	// this backend at all.
	if err := os.WriteFile(filepath.Join(ro, "late.txt"), []byte("late"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantContent(t, filepath.Join(target, "late.txt"), "late")
}

func TestMountWritesState(t *testing.T) {
	target, stateDir, _, _ := newMount(t, "vol-state")

	states, err := loadStates(stateDir)
	if err != nil {
		t.Fatalf("loadStates: %v", err)
	}
	if len(states) != 1 || states[0].Target != target {
		t.Fatalf("loadStates = %+v, want one entry for %s", states, target)
	}

	be := &mergerfsBackend{}
	if err := be.Init(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := be.Unmount(context.Background(), "vol-state", target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if states, err := loadStates(stateDir); err != nil || len(states) != 0 {
		t.Fatalf("loadStates after Unmount = %v, %v; want empty, nil", states, err)
	}
}

// The reconcile loop is what keeps mounts alive on nodes with no host systemd,
// where the daemon dies with the driver pod.
func TestReconcileRemountsADeadMount(t *testing.T) {
	target, stateDir, _, _ := newMount(t, "vol-reconcile")

	// Standing in for the daemon dying with its cgroup.
	if err := fuseUnmount(target); err != nil {
		t.Fatalf("fuseUnmount: %v", err)
	}
	if isFUSEMount(target) {
		t.Fatal("target is still a FUSE mount after unmounting it")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reconcileOnce(ctx, stateDir)

	if !isFUSEMount(target) {
		t.Fatal("target was not remounted by reconcileOnce")
	}
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
}

func TestReconcileDropsStateForARemovedTarget(t *testing.T) {
	target, stateDir, _, _ := newMount(t, "vol-gone")

	if err := fuseUnmount(target); err != nil {
		t.Fatalf("fuseUnmount: %v", err)
	}
	// Kubelet removes the target directory on NodeUnpublishVolume; a state file
	// that outlived it must not resurrect the mount.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	reconcileOnce(context.Background(), stateDir)

	if states, err := loadStates(stateDir); err != nil || len(states) != 0 {
		t.Fatalf("loadStates = %v, %v; want empty, nil", states, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("stat target = %v, want not-exist", err)
	}
}
