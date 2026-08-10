//go:build linux

package mergerfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

const (
	mergerfsBinary   = "mergerfs"
	fusermountBinary = "fusermount"

	mountWaitTimeout  = 30 * time.Second
	mountPollInterval = 100 * time.Millisecond
	reconcileInterval = 30 * time.Second

	// fuseSuperMagic identifies a FUSE mount in statfs, which is what confirms
	// mergerfs actually took over the target rather than merely starting.
	fuseSuperMagic = 0x65735546
)

func mountUnion(ctx context.Context, spec backend.MountSpec, stateDir string) error {
	argv, err := buildArgv(spec)
	if err != nil {
		return err
	}

	// Written before the daemon starts: a crash between the two leaves a state
	// file for a mount that never came up, which the reconcile loop repairs. The
	// reverse order would leave a live mount nothing knows how to repair.
	if err := saveState(stateDir, volumeState{VolumeID: spec.VolumeID, Target: spec.Target, Argv: argv}); err != nil {
		return err
	}

	if err := startDaemon(ctx, spec.VolumeID, spec.Target, argv); err != nil {
		_ = removeState(stateDir, spec.VolumeID)
		return err
	}
	return nil
}

// startDaemon launches mergerfs and returns once the target is a live FUSE mount.
func startDaemon(ctx context.Context, volumeID, target string, argv []string) error {
	cmd := exec.Command(mergerfsBinary, argv...)
	// A new session detaches the daemon from the driver's controlling terminal and
	// signal group, so a driver shutdown does not take the mount with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mergerfs: start: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	if systemdAvailable() {
		if err := adoptIntoScope(ctx, scopeUnitName(volumeID), cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			return err
		}
	}

	if err := waitMounted(ctx, target, exited); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	klog.V(4).Infof("mergerfs: mounted %s (pid %d)", target, cmd.Process.Pid)
	return nil
}

func unmountUnion(ctx context.Context, volumeID, target, stateDir string) error {
	if err := fuseUnmount(target); err != nil {
		return err
	}
	if systemdAvailable() {
		stopScope(ctx, scopeUnitName(volumeID))
	}
	return removeState(stateDir, volumeID)
}

// systemdAvailable probes the host service manager once. Without it the daemon
// stays in the driver's own cgroup and dies with the driver pod, which the
// reconcile loop then has to repair.
var systemdAvailable = sync.OnceValue(func() bool {
	conn, err := systemd.NewSystemdConnectionContext(context.Background())
	if err != nil {
		klog.Warningf("mergerfs: host systemd is unreachable (%v); mounts will not survive a restart of this driver pod, "+
			"and consumers holding open file descriptors across one will see ENOTCONN", err)
		return false
	}
	conn.Close()
	return true
})

// adoptIntoScope moves an already-running process into a transient host systemd
// scope, the same trick systemd-run --scope and kubelet's own mount helpers use.
// Once moved, the process is owned by host systemd rather than the container
// cgroup, so it survives driver restart, upgrade and eviction.
func adoptIntoScope(ctx context.Context, unit string, pid int) error {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("mergerfs: connect to host systemd: %w", err)
	}
	defer conn.Close()

	props := []systemd.Property{
		systemd.PropDescription("union-csi-driver mergerfs mount"),
		{Name: "PIDs", Value: godbus.MakeVariant([]uint32{uint32(pid)})},
		{Name: "Delegate", Value: godbus.MakeVariant(true)},
		// Let systemd garbage-collect the unit once the daemon exits, so a
		// re-published volume can reuse the same deterministic name.
		{Name: "CollectMode", Value: godbus.MakeVariant("inactive-or-failed")},
	}

	done := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, unit, "replace", props, done); err != nil {
		return fmt.Errorf("mergerfs: start transient scope %s: %w", unit, err)
	}
	select {
	case result := <-done:
		if result != "done" {
			return fmt.Errorf("mergerfs: transient scope %s: %s", unit, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func stopScope(ctx context.Context, unit string) {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		klog.Warningf("mergerfs: connect to host systemd to stop %s: %v", unit, err)
		return
	}
	defer conn.Close()

	// The unit is usually gone already: unmounting makes mergerfs exit, and
	// CollectMode reaps the empty scope. Stopping it is the belt to that braces.
	if _, err := conn.StopUnitContext(ctx, unit, "replace", nil); err != nil {
		klog.V(4).Infof("mergerfs: stop scope %s: %v", unit, err)
	}
}

// waitMounted blocks until the target is a FUSE mount, the daemon exits, or the
// timeout expires.
func waitMounted(ctx context.Context, target string, exited <-chan error) error {
	deadline := time.NewTimer(mountWaitTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(mountPollInterval)
	defer tick.Stop()

	for {
		if isFUSEMount(target) {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("mergerfs: exited before mounting %s: %w", target, err)
		case <-tick.C:
		case <-deadline.C:
			return fmt.Errorf("mergerfs: %s did not become a mountpoint within %s", target, mountWaitTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func isFUSEMount(target string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(target, &st); err != nil {
		return false
	}
	return st.Type == fuseSuperMagic
}

// fuseUnmount prefers fusermount, which drops the mount without needing the
// daemon to cooperate, and falls back to a lazy umount where it is unavailable.
func fuseUnmount(target string) error {
	out, err := exec.Command(fusermountBinary, "-u", "-z", target).CombinedOutput()
	if err == nil {
		return nil
	}
	klog.V(4).Infof("mergerfs: fusermount -uz %s: %v (%s), falling back to umount", target, err, out)

	switch err := unix.Unmount(target, unix.MNT_DETACH); err {
	case nil, unix.EINVAL, unix.ENOENT:
		// EINVAL: not a mountpoint. Unmount must tolerate being called twice.
		return nil
	default:
		return fmt.Errorf("mergerfs: unmount %s: %w", target, err)
	}
}
