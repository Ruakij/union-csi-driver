//go:build linux

package mergerfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"k8s.io/klog/v2"
)

// repairBranches replaces, in the mount namespace of the running daemon, every
// branch that is dead there while it serves on the host. A branch is a
// point-in-time copy of the host mount, so a source replaced on the host leaves the
// daemon holding the old, disconnected superblock, which mergerfs skips without an
// error: the union keeps answering, with the files of that branch absent. The
// daemon resolves its branch paths per operation, so a fresh clone at the same
// sandbox path serves the branch again at once, while the union, and the copies of
// it that containers hold, stay as they are. The daemon is never restarted: that
// would break every open file descriptor of the consumers.
//
// A branch is stale too when it is no mount in the daemon at all. The sandbox
// binds every branch as a mount of its own, so that is what is left of a
// replacement that detached the old mount and failed to attach the clone: an empty
// directory, which answers and so looks healthy to a probe.
//
// A branch must be stale for staleThreshold passes in a row, so one that is
// replaced right now is not repaired from under the source.
//
// A branch that is broken on the host too is left alone: cloning it would produce
// the same broken branch, and a plain directory on the host would be cloned over a
// mount that comes back later.
func (b *mergerfsBackend) repairBranches(st volumeState) {
	// An unsandboxed daemon shares the mount namespace of the driver, so it sees
	// exactly the host paths and no clone of them can go stale.
	if len(st.Branches) == 0 {
		return
	}
	id := st.VolumeID
	pid := b.sup.PID(id)
	if pid == 0 {
		klog.V(4).Infof("mergerfs: no daemon pid for %s, leaving its branches unprobed", st.Target)
		return
	}

	mounts := daemonMountPoints(pid, st.Target)

	// /proc/<pid>/root resolves into the mount namespace of the daemon from this
	// one, so an ordinary statfs reaches the branch as the daemon has it.
	root := filepath.Join("/proc", strconv.Itoa(pid), "root")
	for _, br := range st.Branches {
		inner := filepath.Join(root, br.Sandbox)
		var stale string
		state, err := b.probe.Probe(inner)
		switch {
		case deadInDaemon(state, err):
			stale = fmt.Sprintf("does not answer the daemon (%s, %v)", state, err)
		case mounts != nil && !mounts[br.Sandbox]:
			// The sandbox binds every branch as a mount of its own, so a branch that is
			// none is what is left of a replacement that failed after the old mount was
			// detached: an empty directory that answers, and that mergerfs serves as an
			// empty branch.
			stale = "is not a mount in the daemon"
		default:
			b.stale.clear(id, br.Sandbox)
			continue
		}

		if hostState, hostErr := b.probe.Probe(br.Host); hostState != fusemount.Live || hostErr != nil {
			b.stale.clear(id, br.Sandbox)
			klog.Warningf("mergerfs: branch %s of %s %s and is not live on the host either (%s, %v); cloning it again cannot help",
				br.Host, st.Target, stale, hostState, hostErr)
			continue
		}

		passes := b.stale.hit(id, br.Sandbox, pid)
		if passes < staleThreshold {
			klog.Warningf("mergerfs: branch %s of %s %s while it is healthy on the host (pass %d of %d)",
				br.Host, st.Target, stale, passes, staleThreshold)
			continue
		}

		klog.Warningf("mergerfs: replacing branch %s of %s in the running daemon, it %s for %d passes while healthy on the host",
			br.Host, st.Target, stale, passes)
		if err := b.sup.ReplaceIn(id, br.Host, br.Sandbox, br.ReadOnly); err != nil {
			klog.Errorf("mergerfs: reconcile: replace branch %s of %s: %v", br.Host, st.Target, err)
			continue
		}
		// A probe still stuck on the old mount is not one of the new.
		b.probe.Forget(inner)
		b.stale.clear(id, br.Sandbox)
	}
}

// daemonMountPoints returns the mount points in the namespace of the daemon, as it
// sees them, or nil when they cannot be read. Unlike a probe of a path in it, reading
// the mountinfo of a process never waits for a file system, so it cannot hang.
func daemonMountPoints(pid int, target string) map[string]bool {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "mountinfo"))
	if err != nil {
		klog.V(4).Infof("mergerfs: cannot read the mounts of the daemon of %s: %v", target, err)
		return nil
	}
	defer f.Close()
	points, err := mountPoints(f)
	if err != nil {
		klog.V(4).Infof("mergerfs: cannot read the mounts of the daemon of %s: %v", target, err)
		return nil
	}
	return points
}
