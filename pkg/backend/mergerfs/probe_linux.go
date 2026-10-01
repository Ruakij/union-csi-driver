//go:build linux

package mergerfs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// branchProbeTimeout bounds one branch probe. A FUSE mount whose daemon is gone
// answers with ENOTCONN at once, but one whose daemon is alive and stuck never
// answers at all.
const branchProbeTimeout = 5 * time.Second

var errProbeHung = errors.New("statfs did not return in time")

var branchProbe = &prober{timeout: branchProbeTimeout, call: statfs, hung: map[string]bool{}}

// daemonPIDByVolume holds the pid of every daemon this driver process started. One
// started by an earlier driver pod lives on in its systemd scope, and is found
// through that.
var daemonPIDByVolume sync.Map

func statfs(path string) error { return unix.Statfs(path, &unix.Statfs_t{}) }

// prober runs statfs with a timeout. statfs, unlike stat, is never answered from
// the kernel's attribute cache, and one stuck in the kernel cannot be cancelled,
// so a path counts as hung, and is not probed again, until it returns.
type prober struct {
	timeout time.Duration
	call    func(string) error

	mu   sync.Mutex
	hung map[string]bool
}

// statfs reports whether path answers, with errProbeHung for a path that does not.
func (p *prober) statfs(path string) error {
	p.mu.Lock()
	hung := p.hung[path]
	p.mu.Unlock()
	if hung {
		return errProbeHung
	}

	done := make(chan error, 1)
	go func() { done <- p.call(path) }()
	select {
	case err := <-done:
		return err
	case <-time.After(p.timeout):
	}

	p.mu.Lock()
	p.hung[path] = true
	p.mu.Unlock()
	go func() {
		<-done
		p.mu.Lock()
		delete(p.hung, path)
		p.mu.Unlock()
	}()
	return errProbeHung
}

// staleBranch returns the branch of st that the daemon cannot reach while the host
// path is healthy, or "" when there is none. A branch is a private clone of the
// host mount, so a source replaced on the host leaves the daemon holding the old,
// disconnected superblock for as long as it runs, and a remount is the only way
// back: a daemon resolves its branch paths per operation, so a fresh clone serves
// the branch again at once.
//
// A branch that is broken on the host too is left alone: re-cloning it would
// produce the same broken branch, and every attempt disrupts consumers.
func staleBranch(st volumeState) string {
	sandboxPaths, hostPaths := st.sandboxBranchPaths(), st.branchPaths()
	// An unsandboxed daemon shares the driver's mount namespace, so it sees exactly
	// the host paths and no clone of them can go stale.
	if len(sandboxPaths) == 0 || len(sandboxPaths) != len(hostPaths) {
		return ""
	}
	pid := daemonPID(st.VolumeID)
	if pid == 0 {
		klog.V(4).Infof("mergerfs: no daemon pid for %s, leaving its branches unprobed", st.Target)
		return ""
	}

	// /proc/<pid>/root resolves into the daemon's mount namespace from this one, so
	// an ordinary statfs reaches the branch as the daemon has it.
	root := filepath.Join("/proc", strconv.Itoa(pid), "root")
	for i, path := range sandboxPaths {
		err := branchProbe.statfs(filepath.Join(root, path))
		switch {
		case err == nil:
			continue
		case errors.Is(err, os.ErrNotExist):
			// A branch still being set up, or a daemon that is already gone, which the
			// target's own check handles.
			continue
		}
		if hostErr := branchProbe.statfs(hostPaths[i]); hostErr != nil {
			klog.Warningf("mergerfs: branch %s of %s does not answer the daemon (%v) nor this driver (%v); a remount cannot repair that",
				hostPaths[i], st.Target, err, hostErr)
			continue
		}
		klog.Warningf("mergerfs: branch %s of %s does not answer the daemon (%v) while it is healthy on the host", hostPaths[i], st.Target, err)
		return hostPaths[i]
	}
	return ""
}

// daemonPID returns the pid of the daemon serving volumeID, or 0 when it cannot be
// determined. Daemons that outlive the driver pod are adopted into a systemd scope,
// and name it in their cgroup; hostPID is on wherever that happens, so they are in
// this process's /proc.
func daemonPID(volumeID string) int {
	if pid, ok := daemonPIDByVolume.Load(volumeID); ok {
		return pid.(int)
	}
	unit := scopeUnitName(volumeID)
	cgroups, _ := filepath.Glob("/proc/[0-9]*/cgroup")
	for _, f := range cgroups {
		// Reading a cgroup file cannot block on a wedged filesystem, unlike anything
		// that walks into the daemon's own mounts.
		b, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(b), unit) {
			continue
		}
		if pid, err := strconv.Atoi(filepath.Base(filepath.Dir(f))); err == nil {
			return pid
		}
	}
	return 0
}
