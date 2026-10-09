package mergerfs

import (
	"bufio"
	"errors"
	"io"
	"strings"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
)

// staleThreshold is the number of repair passes in a row a branch must be stale
// for before it is repaired. A branch is dead in the daemon's view for a moment
// whenever its source volume is being replaced, and the host side heals as soon as
// the replacement serves, so one sighting is not yet a reason to touch the
// daemon's mounts.
const staleThreshold = 3

// staleKey names a branch of a volume by its path in the sandbox, which is unique
// within the volume.
type staleKey struct{ id, branch string }

type staleRun struct{ pid, passes int }

// staleCounter counts the consecutive passes in which a branch was stale. It has
// no lock: only the repair pass, which runs one volume at a time, uses it.
type staleCounter map[staleKey]staleRun

// hit records one more pass in which branch was stale for the daemon pid, and
// returns how many in a row that makes. A new daemon starts from one: it has
// its own clone of the branch, taken when it started.
func (c staleCounter) hit(id, branch string, pid int) int {
	k := staleKey{id, branch}
	r := c[k]
	if r.pid != pid {
		r = staleRun{pid: pid}
	}
	r.passes++
	c[k] = r
	return r.passes
}

// clear ends the run of branch: it was healthy, or has been repaired.
func (c staleCounter) clear(id, branch string) {
	delete(c, staleKey{id, branch})
}

// retain drops the runs of every volume not in ids.
func (c staleCounter) retain(ids []string) {
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	for k := range c {
		if !keep[k.id] {
			delete(c, k)
		}
	}
}

// deadInDaemon reports whether a branch, probed in the view of the daemon, is a
// mount that will not serve again: its daemon is gone, or it does not answer. A
// branch that is missing, or a plain directory, is a branch still being set up,
// and is not this.
func deadInDaemon(state fusemount.State, err error) bool {
	return state == fusemount.Dead || errors.Is(err, fusemount.ErrHung)
}

// mountPoints returns the mount points listed in a mountinfo file, as seen from
// the root of the process it belongs to. The sandbox paths of the branches have no
// character that mountinfo escapes, so the field is compared as it is.
func mountPoints(mountinfo io.Reader) (map[string]bool, error) {
	points := map[string]bool{}
	sc := bufio.NewScanner(mountinfo)
	for sc.Scan() {
		// mount id, parent id, major:minor, root, mount point, ...
		if f := strings.Fields(sc.Text()); len(f) > 4 {
			points[f[4]] = true
		}
	}
	return points, sc.Err()
}
