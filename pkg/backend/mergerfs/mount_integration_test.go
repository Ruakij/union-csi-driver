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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
	"golang.org/x/sys/unix"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func TestMain(m *testing.M) {
	sandbox.Init()
	os.Exit(m.Run())
}

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

// sharedDir returns a temporary directory on its own shared mount, as kubelet's
// pods directory is, so a sandboxed daemon's mount propagates out of it.
func sharedDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := unix.Mount(root, root, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(root, unix.MNT_DETACH) })
	if err := unix.Mount("", root, "", unix.MS_SHARED, ""); err != nil {
		t.Fatal(err)
	}
	return root
}

// newMount publishes a two-branch merge and returns its target and state dir.
func newMount(t *testing.T, volumeID string) (target, stateDir, rw, ro string) {
	t.Helper()
	root := sharedDir(t)
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

// Symlinks the daemon follows and branches added through the control file resolve
// inside the sandbox, where the rest of the host does not exist.
func TestSandboxHidesTheHost(t *testing.T) {
	root := sharedDir(t)
	secret := makeSource(t, root, "secret", map[string]string{"key": "secret"})
	src := makeSource(t, root, "src", nil)
	if err := os.Symlink(secret, filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	be := &mergerfsBackend{}
	if err := be.Init(filepath.Join(root, "state")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Unmount(context.Background(), "vol-sandbox", target) })
	defer func(v bool) { sealControl = v }(sealControl)
	sealControl = false

	// Options the schema refuses, set here to show they reach nothing.
	spec := backend.MountSpec{
		VolumeID: "vol-sandbox",
		Target:   target,
		Sources:  []backend.Source{{Path: src, Mode: modeRW}},
		Options:  map[string]string{"follow-symlinks": "all", "cache.entry": "0", "cache.attr": "0"},
	}
	if err := be.Mount(context.Background(), spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	// Unable to follow it, mergerfs returns the link itself, which the reader then
	// resolves in its own mount namespace.
	if fi, err := os.Lstat(filepath.Join(target, "escape")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("escape = %v, %v; want the unfollowed symlink", fi, err)
	}
	ctl := filepath.Join(target, controlFile)
	if err := unix.Setxattr(ctl, "user.mergerfs.branches", []byte("+>"+secret), 0); err == nil {
		if _, err := os.ReadFile(filepath.Join(target, "key")); err == nil {
			t.Error("read a host file through a branch added at runtime")
		}
	}
}

func TestMountWithoutSandbox(t *testing.T) {
	defer func(v bool) { useSandbox = v }(useSandbox)
	useSandbox = false
	target, stateDir, _, _ := newMount(t, "vol-nosandbox")

	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
	states, err := loadStates(stateDir)
	if err != nil || len(states) != 1 || states[0].Branches != nil {
		t.Fatalf("loadStates = %+v, %v; want one unsandboxed entry", states, err)
	}
	killDaemon(t, target)
	reconcileOnce(context.Background(), stateDir)
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
}

func TestMountSealsControlFile(t *testing.T) {
	target, _, _, _ := newMount(t, "vol-seal")
	ctl := filepath.Join(target, controlFile)

	if err := unix.Setxattr(ctl, "user.mergerfs.follow-symlinks", []byte("all"), 0); !errors.Is(err, unix.EROFS) {
		t.Fatalf("setxattr on %s = %v, want EROFS", ctl, err)
	}
	buf := make([]byte, 64)
	n, err := unix.Getxattr(ctl, "user.mergerfs.follow-symlinks", buf)
	if err != nil || string(buf[:n]) != "never" {
		t.Fatalf("follow-symlinks = %q, %v; want never", buf[:n], err)
	}
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
	var want unix.Stat_t
	if err := unix.Stat(target, &want); err != nil {
		t.Fatal(err)
	}
	cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	var killed []int
	for _, f := range cmdlines {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		if filepath.Base(args[0]) != mergerfsBinary {
			continue
		}
		// The mountpoint is the last argument, as the daemon sees it: inside its
		// sandbox or not, /proc/<pid>/root leads there.
		var st unix.Stat_t
		proc := filepath.Dir(f)
		if unix.Stat(filepath.Join(proc, "root", args[len(args)-1]), &st) != nil || st.Dev != want.Dev {
			continue
		}
		pid, _ := strconv.Atoi(filepath.Base(proc))
		if err := unix.Kill(pid, unix.SIGKILL); err != nil {
			t.Fatalf("kill %d: %v", pid, err)
		}
		killed = append(killed, pid)
	}
	if len(killed) == 0 {
		t.Fatalf("no mergerfs process serves %s", target)
	}
	// Waits for the exit rather than for ENOTCONN, which a running reconcile loop
	// may already have replaced with a new mount.
	deadline := time.Now().Add(10 * time.Second)
	for _, pid := range killed {
		for unix.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				t.Fatalf("mergerfs %d serving %s did not exit after SIGKILL", pid, target)
			}
			time.Sleep(20 * time.Millisecond)
		}
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

func sealed(target string) bool {
	var st unix.Statfs_t
	return unix.Statfs(filepath.Join(target, controlFile), &st) == nil && st.Flags&unix.ST_RDONLY != 0
}

func waitRemounted(t *testing.T, target string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !isFUSEMount(target) || !sealed(target) {
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
