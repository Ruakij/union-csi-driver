package volsource

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	mount "k8s.io/mount-utils"
)

const pollInterval = 250 * time.Millisecond

// WaitReady blocks until every path in paths is ready, or timeout elapses.
// CSI-backed sources must be real mountpoints (checked via mountinfo, since
// bind-mounted publish paths do not change st_dev and IsLikelyNotMountPoint would
// misreport them); plain-directory sources only need to exist. Never blocks
// indefinitely: on timeout the error names the volumes still pending.
func WaitReady(ctx context.Context, mounter mount.Interface, paths []SourcePath, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		pending := notReady(mounter, paths)
		if len(pending) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for source volumes to become ready: %s", strings.Join(pending, ", "))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func notReady(mounter mount.Interface, paths []SourcePath) []string {
	var pending []string
	for _, p := range paths {
		ok, err := isReady(mounter, p)
		if err != nil || !ok {
			pending = append(pending, p.Name)
		}
	}
	return pending
}

func isReady(mounter mount.Interface, p SourcePath) (bool, error) {
	if !p.CSIBased {
		_, err := os.Stat(p.Path)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}

	isMountPoint, err := mounter.IsMountPoint(p.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		if mount.IsCorruptedMnt(err) {
			return false, nil
		}
		return false, err
	}
	return isMountPoint, nil
}
