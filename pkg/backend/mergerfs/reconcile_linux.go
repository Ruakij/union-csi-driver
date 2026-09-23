//go:build linux

package mergerfs

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// reconcile repairs mounts whose daemon died with the driver pod. It is a no-op
// where host systemd took ownership of the daemons, since those outlive the
// driver and there is nothing to repair.
func reconcile(ctx context.Context, stateDir string) {
	if systemdAvailable() {
		return
	}
	klog.Warningf("mergerfs: running without host systemd; mergerfs daemons die with this pod and are remounted every %s. "+
		"Consumers holding open file descriptors across a restart keep seeing ENOTCONN until they reopen the file", reconcileInterval)

	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()
	for {
		reconcileOnce(ctx, stateDir)
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
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
	_, err := os.Stat(st.Target)
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
	if err := startDaemon(ctx, st.VolumeID, st.Target, st.Argv); err != nil {
		klog.Errorf("mergerfs: reconcile: remount %s: %v", st.Target, err)
	}
}
