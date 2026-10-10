//go:build linux

package mergerfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/daemon"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/supervisor"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

const (
	mergerfsBinary = "mergerfs"
	controlFile    = ".mergerfs"

	mountWaitTimeout = 30 * time.Second

	// daemonOutputTail is how much of a daemon's output goes to the log.
	daemonOutputTail = 4096
)

// checkSandbox fails the driver's startup on a kernel the sandbox cannot run on,
// rather than every mount after it.
func checkSandbox() error {
	if err := unix.MountSetattr(-1, "", 0, &unix.MountAttr{}); errors.Is(err, unix.ENOSYS) {
		return errors.New("mergerfs: the sandbox needs Linux 5.12 or newer; disable it with --mergerfs-sandbox=false")
	}
	return nil
}

func (b *mergerfsBackend) mountUnion(ctx context.Context, spec backend.MountSpec) error {
	st := volumeState{VolumeID: spec.VolumeID, Target: spec.Target}
	if useSandbox {
		spec, st.Branches = sandboxed(spec)
	}
	var err error
	if st.Argv, err = buildArgv(spec); err != nil {
		return err
	}

	// Written before the daemon starts: a crash between the two leaves a state
	// file for a mount that never came up, which the reconcile loop repairs. The
	// reverse order would leave a live mount nothing knows how to repair.
	if err := b.states.Save(spec.VolumeID, st); err != nil {
		return fmt.Errorf("mergerfs: %w", err)
	}

	if err := b.startDaemon(ctx, st); err != nil {
		_ = b.removeState(spec.VolumeID)
		return err
	}
	return nil
}

// shareUnion records the view so reconcile can bind it again: a bind of a dead
// FUSE mount stays dead after its source is remounted.
func (b *mergerfsBackend) shareUnion(ctx context.Context, volumeID, source, target string, readOnly bool) error {
	// The source is a FUSE mount a moment before its control file is sealed, and
	// a bind taken in between would not carry the seal.
	if err := fusemount.WaitMounted(ctx, source, nil, mountWaitTimeout, b.serving); err != nil {
		return fmt.Errorf("mergerfs: %s is not a live, sealed mount: %w", source, err)
	}
	st := volumeState{VolumeID: volumeID, Target: target, SharedFrom: source, ReadOnly: readOnly}
	if err := b.states.Save(volumeID, st); err != nil {
		return fmt.Errorf("mergerfs: %w", err)
	}
	if err := fusemount.Bind(source, target, readOnly); err != nil {
		_ = b.removeState(volumeID)
		return fmt.Errorf("mergerfs: %w", err)
	}
	return nil
}

// startDaemon launches mergerfs and returns once the target is a live FUSE mount.
func (b *mergerfsBackend) startDaemon(ctx context.Context, st volumeState) error {
	cmd, err := daemonCommand(st)
	if err != nil {
		return err
	}
	// Neither a pipe to the driver nor its stderr: a pipe breaks when the driver
	// restarts, and the container runtime reports the driver exited only once every
	// holder of its stdio has closed it, while the daemon outlives the driver.
	out, err := os.OpenFile(b.daemonLog(st.VolumeID), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("mergerfs: open daemon log: %w", err)
	}
	defer out.Close()
	cmd.Stdout, cmd.Stderr = out, out

	d, err := b.sup.Start(ctx, st.VolumeID, cmd, st.Target, supervisor.StartOptions{Timeout: mountWaitTimeout})
	if err != nil {
		if tail := b.daemonOutput(st.VolumeID); tail != "" {
			klog.Errorf("mergerfs: output of the daemon for %s: %s", st.Target, tail)
		}
		return fmt.Errorf("mergerfs: %w", err)
	}
	if sealControl {
		if err := sealControlFile(st.Target); err != nil {
			_ = fusemount.Unmount(st.Target)
			killDaemon(d)
			return err
		}
	}
	klog.V(4).Infof("mergerfs: mounted %s (pid %d)", st.Target, d.Pid())
	return nil
}

// killDaemon kills a daemon with its process group, which it leads.
func killDaemon(d *daemon.Daemon) {
	select {
	case <-d.Exited():
	default:
		_ = syscall.Kill(-d.Pid(), syscall.SIGKILL)
	}
}

// daemonLog is where the output of the daemon of a volume goes, next to its state.
func (b *mergerfsBackend) daemonLog(volumeID string) string {
	return filepath.Join(b.stateDir, sanitizeUnitName(volumeID)+".log")
}

// daemonOutput returns the end of the output of the last daemon of a volume.
func (b *mergerfsBackend) daemonOutput(volumeID string) string {
	f, err := os.Open(b.daemonLog(volumeID))
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	buf := make([]byte, min(fi.Size(), daemonOutputTail))
	n, _ := f.ReadAt(buf, fi.Size()-int64(len(buf)))
	return strings.TrimSpace(string(buf[:n]))
}

// removeState drops the record of a volume and the output of its daemon.
func (b *mergerfsBackend) removeState(volumeID string) error {
	_ = os.Remove(b.daemonLog(volumeID))
	if err := b.states.Remove(volumeID); err != nil {
		return fmt.Errorf("mergerfs: %w", err)
	}
	return nil
}

func daemonCommand(st volumeState) (*exec.Cmd, error) {
	bin, err := exec.LookPath(mergerfsBinary)
	if err != nil {
		return nil, fmt.Errorf("mergerfs: %w", err)
	}
	if st.Branches == nil {
		return exec.Command(bin, st.Argv...), nil
	}
	// The sandbox refuses symlinked paths, and binds the binary at its own path.
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return nil, fmt.Errorf("mergerfs: %w", err)
	}
	cmd, err := sandbox.Command(&sandbox.Config{
		Target:  sandbox.Bind{Host: st.Target, Sandbox: sandboxTarget},
		Binds:   st.Branches,
		Devices: []string{"/dev/fuse"},
		Command: append([]string{bin}, st.Argv...),
	})
	if err != nil {
		return nil, fmt.Errorf("mergerfs: %w", err)
	}
	return cmd, nil
}

// sealControlFile bind-mounts the .mergerfs control file read-only over itself.
// mergerfs applies setxattr on it (branches, follow-symlinks, ...) for anyone
// who may write the file, which includes root in every consumer container.
func sealControlFile(target string) error {
	ctl := filepath.Join(target, controlFile)
	if err := unix.Mount(ctl, ctl, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("mergerfs: bind %s: %w", ctl, err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("", ctl, "", flags, ""); err != nil {
		return fmt.Errorf("mergerfs: remount %s read-only: %w", ctl, err)
	}
	return nil
}

func (b *mergerfsBackend) unmountUnion(ctx context.Context, volumeID, target string) error {
	if err := fusemount.Unmount(target); err != nil {
		return fmt.Errorf("mergerfs: %w", err)
	}
	// A probe stuck on the old mount is not one of the next at this path.
	b.probe.Forget(target)
	// The daemon is usually gone already: unmounting makes mergerfs exit, and the
	// empty scope goes with it. Stopping the scope is the belt to that braces.
	if err := b.scopes.Stop(ctx, volumeID); err != nil {
		klog.Warningf("mergerfs: %v", err)
	}
	return b.removeState(volumeID)
}

// serving reports whether target is a live mount with its control file sealed.
func (b *mergerfsBackend) serving(target string) bool {
	state, err := b.probe.Probe(target)
	return state == fusemount.Live && err == nil && (!sealControl || sealed(target))
}

func sealed(target string) bool {
	var st unix.Statfs_t
	return unix.Statfs(filepath.Join(target, controlFile), &st) == nil && st.Flags&unix.ST_RDONLY != 0
}
