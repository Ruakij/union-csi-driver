//go:build linux

package mergerfs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"k8s.io/klog/v2"
)

// staleBranch returns the branch of st that the daemon cannot reach while the host
// path is healthy, or "" when there is none. A branch is a private clone of the
// host mount, so a source replaced on the host leaves the daemon holding the old,
// disconnected superblock for as long as it runs, and a remount is the only way
// back: a daemon resolves its branch paths per operation, so a fresh clone serves
// the branch again at once.
//
// A branch that is broken on the host too is left alone: re-cloning it would
// produce the same broken branch, and every attempt disrupts consumers.
func (b *mergerfsBackend) staleBranch(st volumeState) string {
	sandboxPaths, hostPaths := st.sandboxBranchPaths(), st.branchPaths()
	// An unsandboxed daemon shares the driver's mount namespace, so it sees exactly
	// the host paths and no clone of them can go stale.
	if len(sandboxPaths) == 0 || len(sandboxPaths) != len(hostPaths) {
		return ""
	}
	pid := b.sup.PID(st.VolumeID)
	if pid == 0 {
		klog.V(4).Infof("mergerfs: no daemon pid for %s, leaving its branches unprobed", st.Target)
		return ""
	}

	// /proc/<pid>/root resolves into the daemon's mount namespace from this one, so
	// an ordinary statfs reaches the branch as the daemon has it.
	root := filepath.Join("/proc", strconv.Itoa(pid), "root")
	for i, path := range sandboxPaths {
		_, err := b.probe.Probe(filepath.Join(root, path))
		switch {
		case err == nil:
			continue
		case errors.Is(err, os.ErrNotExist):
			// A branch still being set up, or a daemon that is already gone, which the
			// target's own check handles.
			continue
		}
		if _, hostErr := b.probe.Probe(hostPaths[i]); hostErr != nil {
			klog.Warningf("mergerfs: branch %s of %s does not answer the daemon (%v) nor this driver (%v); a remount cannot repair that",
				hostPaths[i], st.Target, err, hostErr)
			continue
		}
		klog.Warningf("mergerfs: branch %s of %s does not answer the daemon (%v) while it is healthy on the host", hostPaths[i], st.Target, err)
		return hostPaths[i]
	}
	return ""
}
