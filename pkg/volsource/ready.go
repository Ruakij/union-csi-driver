package volsource

import (
	"context"
	"time"
)

// WaitReady blocks until every path in paths is ready, or timeout elapses.
// CSI-backed sources must be real mountpoints; plain-directory sources
// (emptyDir/configMap/secret/projected) only need to exist.
//
// TODO implement per .docs/plan.md section 3 step 2: poll on a tick (250ms),
// mount-utils mounter.IsMountPoint (mountinfo-based, not IsLikelyNotMountPoint,
// since bind-mounted publish paths do not change st_dev), treat
// mount.IsCorruptedMnt as not-ready rather than fatal. On timeout, name the
// volumes still pending.
func WaitReady(ctx context.Context, paths []SourcePath, timeout time.Duration) error {
	panic("not implemented")
}
