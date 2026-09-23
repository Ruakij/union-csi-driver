//go:build linux && mounttest

// Real mergerfs mounts against real directories. Needs a Linux kernel, /dev/fuse
// and the mergerfs binary, so it is behind the mounttest build tag: see
// "make test-mount".
package mergerfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

// Runs before any test here mounts, so no earlier scope's exit can pose as the wake.
func TestWatchScopesWakesOnScopeStop(t *testing.T) {
	if !systemdAvailable() {
		if os.Getenv("MOUNTTEST_SYSTEMD") != "" {
			t.Fatal("MOUNTTEST_SYSTEMD is set but host systemd is unreachable")
		}
		t.Skip("no host systemd; see make test-mount-systemd")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := adoptIntoScope(ctx, scopeUnitName("vol-watch"), cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}

	woken := make(chan struct{}, 1)
	if err := watchScopes(ctx, func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	select {
	case <-woken:
	case <-time.After(10 * time.Second):
		t.Fatal("no wake after the scope's process died")
	}
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

// killDaemon SIGKILLs the mergerfs serving target, leaving the mount behind dead
// the way a daemon dying with its cgroup does.
func killDaemon(t *testing.T, target string) {
	t.Helper()
	cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	killed := false
	for _, f := range cmdlines {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if filepath.Base(args[0]) != mergerfsBinary || !slices.Contains(args, target) {
			continue
		}
		pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(f)))
		if err := unix.Kill(pid, unix.SIGKILL); err != nil {
			t.Fatalf("kill %d: %v", pid, err)
		}
		killed = true
	}
	if !killed {
		t.Fatalf("no mergerfs process serves %s", target)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(target); errors.Is(err, unix.ENOTCONN) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become ENOTCONN after killing its daemon", target)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestReconcileRemountsADeadMount(t *testing.T) {
	target, stateDir, _, _ := newMount(t, "vol-reconcile")
	killDaemon(t, target)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reconcileOnce(ctx, stateDir)

	if !isFUSEMount(target) {
		t.Fatal("target was not remounted by reconcileOnce")
	}
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
}

func waitRemounted(t *testing.T, target string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !isFUSEMount(target) {
		if time.Now().After(deadline) {
			t.Fatalf("%s was not remounted within 10s", target)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Both remounts land well inside reconcileInterval, so the ticker cannot be what
// drives them.
func TestReconcileRemountsOnDaemonExit(t *testing.T) {
	target, stateDir, _, _ := newMount(t, "vol-event")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go reconcile(ctx, stateDir)

	// The first remount may come from the loop's initial pass; the second only
	// from the exit of the daemon that the first remount started.
	for range 2 {
		killDaemon(t, target)
		waitRemounted(t, target)
	}
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
}

// A plain directory is a publish in progress or a rebooted node, and remounting
// it would race kubelet.
func TestReconcileLeavesAnUnmountedTargetAlone(t *testing.T) {
	target, stateDir, _, _ := newMount(t, "vol-plain")
	if err := fuseUnmount(target); err != nil {
		t.Fatalf("fuseUnmount: %v", err)
	}

	reconcileOnce(context.Background(), stateDir)

	if isFUSEMount(target) {
		t.Fatal("reconcileOnce remounted a plain directory")
	}
	if _, err := os.Stat(statePath(stateDir, "vol-plain")); err != nil {
		t.Fatalf("state was dropped: %v", err)
	}
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
