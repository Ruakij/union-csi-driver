//go:build linux

package mergerfs

import (
	"context"
	"errors"
	"io/fs"
	"sort"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"k8s.io/klog/v2"
)

// ids returns the volumes with a state file. Shared views come last, so they bind
// to unions this pass has just remounted.
func (b *mergerfsBackend) ids() ([]string, error) {
	states, errs := b.states.LoadAll()
	sort.SliceStable(states, func(i, j int) bool { return states[i].SharedFrom == "" && states[j].SharedFrom != "" })
	ids := make([]string, len(states))
	for i, st := range states {
		ids[i] = st.VolumeID
	}
	return ids, errors.Join(errs...)
}

// reconcile checks one volume. logLive reports the live mounts, which on startup
// are the ones an earlier driver left running.
func (b *mergerfsBackend) reconcile(ctx context.Context, id string, logLive bool) {
	st, err := b.states.Load(id)
	if err != nil {
		// Unmounted while this pass was waiting for the lock.
		if !errors.Is(err, fs.ErrNotExist) {
			klog.Errorf("mergerfs: reconcile: %v", err)
		}
		return
	}

	switch fusemount.Status(st.Target) {
	case fusemount.Live:
		if logLive {
			klog.Infof("mergerfs: found live mount %s", st.Target)
		}
		// mergerfs skips a branch it cannot reach without an error anywhere: the union
		// keeps answering, with that branch's files simply absent. Only the branches as
		// the daemon has them say so.
		if stale := b.staleBranch(st); stale != "" {
			klog.Warningf("mergerfs: remounting %s to pick up branch %s again", st.Target, stale)
			b.remountUnion(ctx, st)
		}
	case fusemount.Gone:
		// Kubelet removed the target: the volume is gone, and so is the reason to
		// keep its state.
		if err := b.removeState(id); err != nil {
			klog.Errorf("mergerfs: reconcile: %v", err)
		}
	case fusemount.Dead:
		if st.SharedFrom != "" {
			b.rebindShared(st)
			return
		}
		klog.Warningf("mergerfs: %s is not a live mount, remounting", st.Target)
		b.remountUnion(ctx, st)
	default:
		// A plain directory is either a mount still being set up, or a node that
		// rebooted, where kubelet republishes.
	}
}

func (b *mergerfsBackend) remountUnion(ctx context.Context, st volumeState) {
	if err := fusemount.Unmount(st.Target); err != nil {
		klog.Errorf("mergerfs: reconcile: %v", err)
		return
	}
	if err := b.scopes.Stop(ctx, st.VolumeID); err != nil {
		klog.Errorf("mergerfs: reconcile: %v", err)
		return
	}
	if err := b.startDaemon(ctx, st); err != nil {
		klog.Errorf("mergerfs: reconcile: remount %s: %v", st.Target, err)
	}
}

func (b *mergerfsBackend) rebindShared(st volumeState) {
	if !b.serving(st.SharedFrom) {
		klog.V(4).Infof("mergerfs: %s is shared from %s, which is not live yet", st.Target, st.SharedFrom)
		return
	}
	klog.Warningf("mergerfs: %s is not a live mount, binding %s to it again", st.Target, st.SharedFrom)
	if err := fusemount.Unmount(st.Target); err != nil {
		klog.Errorf("mergerfs: reconcile: %v", err)
		return
	}
	if err := fusemount.Bind(st.SharedFrom, st.Target, st.ReadOnly); err != nil {
		klog.Errorf("mergerfs: reconcile: rebind %s: %v", st.Target, err)
	}
}
