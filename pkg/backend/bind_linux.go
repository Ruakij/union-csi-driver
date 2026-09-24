//go:build linux

package backend

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// BindMount recursively bind-mounts source at target, so submounts such as
// mergerfs's sealed control file come along, and makes it read-only if asked.
func BindMount(source, target string, readOnly bool) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s to %s: %w", source, target, err)
	}
	if !readOnly {
		return nil
	}
	// A bind mount cannot be made read-only in one step; MS_RDONLY is only honoured
	// by a follow-up remount.
	if err := unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
		_ = unix.Unmount(target, unix.MNT_DETACH)
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}
