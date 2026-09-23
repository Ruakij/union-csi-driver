//go:build linux

package mergerfs

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// wakeReconcile cuts the wait for the next reconcile pass short. It holds at most
// one pending wake, so bursts of daemon exits collapse into a single pass.
var wakeReconcile = make(chan struct{}, 1)

func requestReconcile() {
	select {
	case wakeReconcile <- struct{}{}:
	default:
	}
}

// reconcile repairs mounts whose daemon died: with the driver pod where there is
// no host systemd, or by crashing or being OOM-killed anywhere. A pass runs as
// soon as a daemon exits, with the ticker as a backstop for missed events.
func reconcile(ctx context.Context, stateDir string) {
	if !systemdAvailable() {
		klog.Warning("mergerfs: running without host systemd; mergerfs daemons die with this pod and are remounted when it restarts. " +
			"Consumers holding open file descriptors across a restart keep seeing ENOTCONN until they reopen the file")
	} else if err := watchScopes(ctx, requestReconcile); err != nil {
		klog.Warningf("mergerfs: cannot watch host systemd for daemon exits (%v); "+
			"daemons started by an earlier driver pod are remounted within %s of dying", err, reconcileInterval)
	}

	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()
	var last time.Time
	for {
		// A daemon that dies right after mounting must not turn the wake into a
		// tight remount loop.
		if wait := minReconcileGap - time.Since(last); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
		last = time.Now()
		reconcileOnce(ctx, stateDir)
		select {
		case <-tick.C:
		case <-wakeReconcile:
		case <-ctx.Done():
			return
		}
	}
}

// watchScopes calls wake whenever a daemon's scope stops. Daemons started by this
// driver pod are also its children and report their own exit, but those started
// by an earlier pod are only visible to host systemd.
func watchScopes(ctx context.Context, wake func()) error {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return err
	}
	updates := make(chan *systemd.PropertiesUpdate, 64)
	// Receives an error when updates overflowed and events were dropped.
	dropped := make(chan error, 1)
	conn.SetPropertiesSubscriber(updates, dropped)
	if err := conn.Subscribe(); err != nil {
		conn.Close()
		return err
	}

	go func() {
		defer conn.Close()
		for {
			select {
			case u := <-updates:
				state, ok := u.Changed["ActiveState"]
				if ok && isScopeUnit(u.UnitName) && state.Value() != "active" {
					wake()
				}
			case <-dropped:
				wake()
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func reconcileOnce(ctx context.Context, stateDir string) {
	states, err := loadStates(stateDir)
	if err != nil {
		klog.Errorf("mergerfs: reconcile: %v", err)
		return
	}

	for _, st := range states {
		reconcileVolume(ctx, stateDir, st)
	}
}

func reconcileVolume(ctx context.Context, stateDir string, st volumeState) {
	lockVolume(st.VolumeID)
	defer unlockVolume(st.VolumeID)

	// Unmounted while this pass was waiting for the lock.
	if _, err := os.Stat(statePath(stateDir, st.VolumeID)); err != nil {
		return
	}
	if isFUSEMount(st.Target) {
		return
	}

	// Only a dead FUSE mount answers with ENOTCONN. A plain directory is either a
	// mount still being set up, or a node that rebooted, where kubelet republishes.
	// statfs, unlike stat, is never answered from the kernel's attribute cache.
	err := unix.Statfs(st.Target, &unix.Statfs_t{})
	switch {
	case errors.Is(err, unix.ENOTCONN):
	case err == nil:
		return
	case errors.Is(err, os.ErrNotExist):
		// Kubelet removed the target: the volume is gone, and so is the reason to
		// keep its state.
		if err := removeState(stateDir, st.VolumeID); err != nil {
			klog.Errorf("mergerfs: reconcile: %v", err)
		}
		return
	default:
		klog.Errorf("mergerfs: reconcile: stat %s: %v", st.Target, err)
		return
	}

	klog.Warningf("mergerfs: %s is no longer a live mount, remounting", st.Target)
	if err := fuseUnmount(st.Target); err != nil {
		klog.Errorf("mergerfs: reconcile: %v", err)
		return
	}
	if systemdAvailable() {
		stopScope(ctx, scopeUnitName(st.VolumeID))
	}
	if err := startDaemon(ctx, st); err != nil {
		klog.Errorf("mergerfs: reconcile: remount %s: %v", st.Target, err)
	}
}

func isScopeUnit(name string) bool {
	return strings.HasPrefix(name, scopePrefix) && strings.HasSuffix(name, scopeSuffix)
}
