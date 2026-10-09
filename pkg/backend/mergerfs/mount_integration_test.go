//go:build linux && mounttest

// Real mergerfs mounts against real directories. Needs a Linux kernel, /dev/fuse
// and the mergerfs binary, so it is behind the mounttest build tag: see
// "make test-mount".
package mergerfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"golang.org/x/sys/unix"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func TestMain(m *testing.M) {
	sandbox.Init()
	os.Exit(m.Run())
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

// newMount publishes a two-branch merge and returns its backend and target.
func newMount(t *testing.T, volumeID string) (be *mergerfsBackend, target, rw, ro string) {
	t.Helper()
	root := sharedDir(t)
	rw = makeSource(t, root, "rw", map[string]string{"rw.txt": "from-rw"})
	ro = makeSource(t, root, "ro", map[string]string{"ro.txt": "from-ro", "both.txt": "from-ro"})
	target = filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "state")

	be = &mergerfsBackend{}
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
	if !fusemount.IsFUSE(target) {
		t.Fatalf("%s is not a FUSE mount after Mount", target)
	}
	return be, target, rw, ro
}

func TestMountMergesBranches(t *testing.T) {
	_, target, rw, ro := newMount(t, "vol-merge")

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
	src := makeSource(t, root, "src", map[string]string{"file": "inside"})
	links := map[string]string{"escape": secret, "environ": "/proc/1/environ", "mem": "/proc/self/mem"}
	for name, dst := range links {
		if err := os.Symlink(dst, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("file", filepath.Join(src, "inner")); err != nil {
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

	spec := backend.MountSpec{
		VolumeID: "vol-sandbox",
		Target:   target,
		Sources:  []backend.Source{{Path: src, Mode: modeRW}},
		Options:  map[string]string{"follow-symlinks": "all", "cache.entry": "0", "cache.attr": "0"},
	}
	if err := be.Mount(context.Background(), spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	if fi, err := os.Lstat(filepath.Join(target, "inner")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("inner = %v, %v; want the followed regular file", fi, err)
	}
	// Unable to follow them, mergerfs returns the links themselves, which the
	// reader then resolves in its own mount namespace.
	for name := range links {
		if fi, err := os.Lstat(filepath.Join(target, name)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s = %v, %v; want the unfollowed symlink", name, fi, err)
		}
	}
	ctl := filepath.Join(target, controlFile)
	if err := unix.Setxattr(ctl, "user.mergerfs.branches", []byte("+>"+secret), 0); err == nil {
		if _, err := os.ReadFile(filepath.Join(target, "key")); err == nil {
			t.Error("read a host file through a branch added at runtime")
		}
	}
	if err := unix.Setxattr(ctl, "user.mergerfs.branches", []byte("+>/proc"), 0); err == nil {
		ents, err := os.ReadDir(filepath.Join(target, "self"))
		if err != nil || len(ents) != 1 || ents[0].Name() != "fd" {
			t.Errorf("/proc/self through a branch added at runtime = %v, %v; want only fd", ents, err)
		}
	}
}

// A file the driver inherits must not reach the daemon, where a symlink it follows
// through /proc/self/fd would open it on the host.
func TestSandboxKeepsInheritedFilesOut(t *testing.T) {
	root := sharedDir(t)
	secret := makeSource(t, root, "secret", map[string]string{"key": "secret"})
	fd, err := unix.Open(secret, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	src := makeSource(t, root, "src", nil)
	if err := os.Symlink(fmt.Sprintf("/proc/self/fd/%d/key", fd), filepath.Join(src, "leak")); err != nil {
		t.Fatal(err)
	}
	_, target := mountSources(t, root, "vol-inherit", map[string]string{"follow-symlinks": "all"}, src)

	// Not followed by the daemon, the link comes back as is, and the test's own fd
	// would resolve it.
	leak := filepath.Join(target, "leak")
	if fi, err := os.Lstat(leak); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return
	}
	if b, err := os.ReadFile(leak); err == nil && string(b) == "secret" {
		t.Error("read a host file through a file descriptor the daemon inherited")
	}
}

// The daemon answers the lookup of the control file, from branches a pod writes,
// so what it answers must not decide what gets sealed.
func TestSealIgnoresPlantedControlFile(t *testing.T) {
	for name, plant := range map[string]func(path, decoy string) error{
		"symlink":   func(path, decoy string) error { return os.Symlink(decoy, path) },
		"directory": func(path, _ string) error { return os.Mkdir(path, 0o755) },
		"file":      func(path, _ string) error { return os.WriteFile(path, nil, 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			root := sharedDir(t)
			decoy := makeSource(t, root, "decoy", nil)
			src := makeSource(t, root, "src", nil)
			if err := plant(filepath.Join(src, controlFile), decoy); err != nil {
				t.Fatal(err)
			}
			_, target := mountSources(t, root, "vol-plant-"+name, nil, src)

			if sealed(decoy) {
				t.Error("sealed the planted link's destination")
			}
			ctl := filepath.Join(target, controlFile)
			if err := unix.Setxattr(ctl, "user.mergerfs.follow-symlinks", []byte("all"), 0); !errors.Is(err, unix.EROFS) {
				t.Errorf("setxattr on %s = %v, want EROFS", ctl, err)
			}
		})
	}
}

// mountSources publishes a merge of RW sources under root and returns its backend and target.
func mountSources(t *testing.T, root, volumeID string, options map[string]string, sources ...string) (*mergerfsBackend, string) {
	t.Helper()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	be := &mergerfsBackend{}
	if err := be.Init(filepath.Join(root, "state")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Unmount(context.Background(), volumeID, target) })
	spec := backend.MountSpec{VolumeID: volumeID, Target: target, Options: options}
	for _, s := range sources {
		spec.Sources = append(spec.Sources, backend.Source{Path: s, Mode: modeRW})
	}
	if err := be.Mount(context.Background(), spec); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	return be, target
}

// mergerfs reopens an already open file through /proc/self/fd, the only part of
// proc in the sandbox.
func TestMountReopensAnOpenFile(t *testing.T) {
	_, target, _, _ := newMount(t, "vol-reopen")
	path := filepath.Join(target, "rw.txt")
	first, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := os.WriteFile(path, []byte("rewritten"), 0o644); err != nil {
		t.Fatalf("write while open: %v", err)
	}
	wantContent(t, path, "rewritten")
}

func TestMountWithoutSandbox(t *testing.T) {
	defer func(v bool) { useSandbox = v }(useSandbox)
	useSandbox = false
	be, target, _, _ := newMount(t, "vol-nosandbox")

	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
	states, errs := be.states.LoadAll()
	if len(errs) != 0 || len(states) != 1 || states[0].Branches != nil {
		t.Fatalf("LoadAll = %+v, %v; want one unsandboxed entry", states, errs)
	}
	killServing(t, target)
	reconcileAll(context.Background(), be)
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
}

func TestMountSealsControlFile(t *testing.T) {
	_, target, _, _ := newMount(t, "vol-seal")
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
	be, target, _, _ := newMount(t, "vol-state")

	states, errs := be.states.LoadAll()
	if len(errs) != 0 {
		t.Fatalf("LoadAll: %v", errs)
	}
	if len(states) != 1 || states[0].Target != target {
		t.Fatalf("LoadAll = %+v, want one entry for %s", states, target)
	}

	if err := be.Unmount(context.Background(), "vol-state", target); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if states, errs := be.states.LoadAll(); len(errs) != 0 || len(states) != 0 {
		t.Fatalf("LoadAll after Unmount = %v, %v; want empty, nil", states, errs)
	}
}

// The daemon of a driver pod that was replaced lives on in its scope, named after the
// volume as it always was, and the next pod finds it there.
func TestRestartedDriverFindsTheDaemonInItsScope(t *testing.T) {
	be, target, _, _ := newMount(t, "vol-restart")
	if !be.scopes.Available() {
		if os.Getenv("MOUNTTEST_SYSTEMD") != "" {
			t.Fatal("MOUNTTEST_SYSTEMD is set but host systemd is unreachable")
		}
		t.Skip("no host systemd; see make test-mount-systemd")
	}
	pid := be.sup.PID("vol-restart")
	cgroup, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cgroup), "union-csi-vol-restart.scope") {
		t.Fatalf("cgroup of the daemon = %q, want it in union-csi-vol-restart.scope", cgroup)
	}

	next := &mergerfsBackend{}
	if err := next.Init(be.stateDir); err != nil {
		t.Fatal(err)
	}
	if got := next.sup.PID("vol-restart"); got == 0 {
		t.Fatal("the next driver does not find the daemon through its scope")
	}
	before := daemonPIDs(t, target)
	reconcileAll(context.Background(), next)
	if got := daemonPIDs(t, target); !reflect.DeepEqual(got, before) {
		t.Errorf("daemons = %v, want the unchanged %v", got, before)
	}
	if err := next.Unmount(context.Background(), "vol-restart", target); err != nil {
		t.Fatal(err)
	}
	if states, errs := next.states.LoadAll(); len(errs) != 0 || len(states) != 0 {
		t.Errorf("LoadAll after Unmount = %v, %v; want empty, nil", states, errs)
	}
}

// A daemon holding the driver's stderr keeps the driver's container from
// stopping.
func TestDaemonDoesNotHoldDriverStderr(t *testing.T) {
	_, target, _, _ := newMount(t, "vol-stdio")
	var stderr unix.Stat_t
	if err := unix.Fstat(int(os.Stderr.Fd()), &stderr); err != nil {
		t.Fatal(err)
	}
	for _, pid := range daemonPIDs(t, target) {
		for _, fd := range []string{"1", "2"} {
			var st unix.Stat_t
			if err := unix.Stat(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd), &st); err != nil {
				t.Fatal(err)
			}
			if st.Dev == stderr.Dev && st.Ino == stderr.Ino {
				t.Errorf("mergerfs %d has the driver's stderr as fd %s", pid, fd)
			}
		}
	}
}

// daemonPIDs returns the mergerfs processes serving target.
func daemonPIDs(t *testing.T, target string) []int {
	t.Helper()
	var want unix.Stat_t
	if err := unix.Stat(target, &want); err != nil {
		t.Fatal(err)
	}
	cmdlines, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	var pids []int
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
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		t.Fatalf("no mergerfs process serves %s", target)
	}
	return pids
}

// killServing SIGKILLs the mergerfs serving target, leaving the mount behind dead
// the way a daemon dying with its cgroup does.
func killServing(t *testing.T, target string) {
	t.Helper()
	killed := daemonPIDs(t, target)
	for _, pid := range killed {
		if err := unix.Kill(pid, unix.SIGKILL); err != nil {
			t.Fatalf("kill %d: %v", pid, err)
		}
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
	be, target, _, _ := newMount(t, "vol-reconcile")
	killServing(t, target)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reconcileAll(ctx, be)

	if !fusemount.IsFUSE(target) {
		t.Fatal("target was not remounted by reconcileOnce")
	}
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
}

// The incident this probe exists for: the source's FUSE daemon died and was
// replaced at the same host path, so the host is healthy and only the daemon's own
// clone of the branch is a dead superblock, which mergerfs skips without a word.
func TestReconcileRemountsForAStaleBranch(t *testing.T) {
	root := sharedDir(t)
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	killFirst := fuseSource(t, src, makeSource(t, root, "first", map[string]string{"first.txt": "from-first"}))
	be, target := mountSources(t, root, "vol-stale-branch", nil, src)
	before := daemonPIDs(t, target)
	wantContent(t, filepath.Join(target, "first.txt"), "from-first")

	// What csi-mount-healer does: the dead mount goes, a fresh daemon takes the path.
	killFirst()
	if err := unix.Unmount(src, unix.MNT_DETACH); err != nil {
		t.Fatal(err)
	}
	fuseSource(t, src, makeSource(t, root, "second", map[string]string{"second.txt": "from-second"}))
	if err := unix.Statfs(src, &unix.Statfs_t{}); err != nil {
		t.Fatalf("statfs %s = %v, want the healthy replacement", src, err)
	}
	// Neither the union nor the host says anything is wrong, which is what makes the
	// daemon's own view the only place to look.
	if !fusemount.IsFUSE(target) {
		t.Fatal("the union died with its branch")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reconcileAll(ctx, be)

	if got := daemonPIDs(t, target); reflect.DeepEqual(got, before) {
		t.Fatalf("daemons = %v, unchanged: the stale branch did not get the union remounted", got)
	}
	wantContent(t, filepath.Join(target, "second.txt"), "from-second")
}

// A branch that is dead on the host too cannot be repaired by a remount, so the
// union is left serving what it still has rather than being disrupted every pass.
func TestReconcileLeavesAHostDeadBranchAlone(t *testing.T) {
	root := sharedDir(t)
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	kill := fuseSource(t, src, makeSource(t, root, "first", map[string]string{"first.txt": "from-first"}))
	be, target := mountSources(t, root, "vol-host-dead-branch", nil, src)
	before := daemonPIDs(t, target)

	kill()
	if err := unix.Statfs(src, &unix.Statfs_t{}); !errors.Is(err, unix.ENOTCONN) {
		t.Fatalf("statfs %s = %v, want ENOTCONN", src, err)
	}

	reconcileAll(context.Background(), be)

	if got := daemonPIDs(t, target); !reflect.DeepEqual(got, before) {
		t.Errorf("daemons = %v, want the unchanged %v: a remount cannot repair a branch that is dead on the host", got, before)
	}
}

// A healthy branch must not get the union remounted every pass.
func TestReconcileLeavesALiveBranchAlone(t *testing.T) {
	be, target, _, _ := newMount(t, "vol-live-branch")
	before := daemonPIDs(t, target)

	reconcileAll(context.Background(), be)

	if got := daemonPIDs(t, target); !reflect.DeepEqual(got, before) {
		t.Errorf("daemons = %v, want the unchanged %v", got, before)
	}
}

// fuseSource mounts a mergerfs of its own at dir, standing in for a source volume
// served by a FUSE daemon, and returns a function that kills that daemon and leaves
// the mount behind dead.
func fuseSource(t *testing.T, dir, backing string) func() {
	t.Helper()
	bin, err := exec.LookPath(mergerfsBinary)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-f", "-o", "allow_other", backing+"=RW", dir)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Reaped, not merely signalled: a zombie still holds the /dev/fuse connection.
	var once sync.Once
	kill := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(func() {
		_ = unix.Unmount(dir, unix.MNT_DETACH)
		kill()
	})
	deadline := time.Now().Add(30 * time.Second)
	for !fusemount.IsFUSE(dir) {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become a FUSE mount", dir)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return kill
}

func waitRemounted(t *testing.T, target string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !fusemount.IsFUSE(target) || !sealed(target) {
		if time.Now().After(deadline) {
			t.Fatalf("%s was not remounted within 10s", target)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Both remounts land well inside reconcileInterval, so the ticker cannot be what
// drives them.
func TestReconcileRemountsOnDaemonExit(t *testing.T) {
	be, target, _, _ := newMount(t, "vol-event")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		be.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The first remount may come from the loop's initial pass; the second only
	// from the exit of the daemon that the first remount started.
	for range 2 {
		killServing(t, target)
		waitRemounted(t, target)
	}
	wantContent(t, filepath.Join(target, "ro.txt"), "from-ro")
}

// A plain directory is a publish in progress or a rebooted node, and remounting
// it would race kubelet.
func TestReconcileLeavesAnUnmountedTargetAlone(t *testing.T) {
	be, target, _, _ := newMount(t, "vol-plain")
	if err := fusemount.Unmount(target); err != nil {
		t.Fatalf("fuseUnmount: %v", err)
	}

	reconcileAll(context.Background(), be)

	if fusemount.IsFUSE(target) {
		t.Fatal("reconcileOnce remounted a plain directory")
	}
	if _, err := os.Stat(be.states.Path("vol-plain")); err != nil {
		t.Fatalf("state was dropped: %v", err)
	}
}

func TestReconcileDropsStateForARemovedTarget(t *testing.T) {
	be, target, _, _ := newMount(t, "vol-gone")

	if err := fusemount.Unmount(target); err != nil {
		t.Fatalf("fuseUnmount: %v", err)
	}
	// Kubelet removes the target directory on NodeUnpublishVolume; a state file
	// that outlived it must not resurrect the mount.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}

	reconcileAll(context.Background(), be)

	if states, errs := be.states.LoadAll(); len(errs) != 0 || len(states) != 0 {
		t.Fatalf("LoadAll = %v, %v; want empty, nil", states, errs)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("stat target = %v, want not-exist", err)
	}
}

func newShare(t *testing.T, be *mergerfsBackend, target, volumeID string) string {
	t.Helper()
	view := filepath.Join(filepath.Dir(target), "view-"+volumeID)
	if err := os.MkdirAll(view, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = be.Unmount(context.Background(), volumeID, view) })
	if err := be.Share(context.Background(), volumeID, target, view, false); err != nil {
		t.Fatalf("Share: %v", err)
	}
	return view
}

func TestShareBindsTheUnion(t *testing.T) {
	be, target, rw, _ := newMount(t, "vol-shared")
	view := newShare(t, be, target, "vol-shared-2")

	if !fusemount.IsFUSE(view) {
		t.Fatal("view is not a FUSE mount")
	}
	// The bind is recursive, so the seal on the control file comes along.
	if !sealed(view) {
		t.Error("control file is not sealed in the view")
	}
	wantContent(t, filepath.Join(view, "ro.txt"), "from-ro")
	if err := os.WriteFile(filepath.Join(view, "new.txt"), []byte("via-view"), 0o644); err != nil {
		t.Fatalf("write through the view: %v", err)
	}
	wantContent(t, filepath.Join(rw, "new.txt"), "via-view")
	if got := len(daemonPIDs(t, target)); got != 1 {
		t.Errorf("%d daemons serve the union, want 1", got)
	}
}

// A bind of a dead FUSE mount stays dead after its source is remounted, so the
// same pass must bind it again.
func TestReconcileRebindsASharedView(t *testing.T) {
	be, target, _, _ := newMount(t, "vol-rebind")
	view := newShare(t, be, target, "vol-rebind-2")
	killServing(t, target)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reconcileAll(ctx, be)

	for _, p := range []string{target, view} {
		if !fusemount.IsFUSE(p) || !sealed(p) {
			t.Fatalf("%s was not brought back by reconcileOnce", p)
		}
		wantContent(t, filepath.Join(p, "ro.txt"), "from-ro")
	}
}

func TestReconcileDropsStateForARemovedShare(t *testing.T) {
	be, target, _, _ := newMount(t, "vol-share-gone")
	view := newShare(t, be, target, "vol-share-gone-2")
	if err := fusemount.Unmount(view); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(view); err != nil {
		t.Fatal(err)
	}

	reconcileAll(context.Background(), be)

	if _, err := os.Stat(be.states.Path("vol-share-gone-2")); !os.IsNotExist(err) {
		t.Fatalf("stat shared state = %v, want not-exist", err)
	}
}

// reconcileAll runs one repair pass over every volume, as the supervisor does.
func reconcileAll(ctx context.Context, be *mergerfsBackend) {
	ids, _ := be.ids()
	for _, id := range ids {
		be.sup.Lock(id)
		be.reconcile(ctx, id, false)
		be.sup.Unlock(id)
	}
}
