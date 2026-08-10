//go:build linux

package overlay

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func mountUnion(spec backend.MountSpec, schema backend.OptionSchema) error {
	l, err := planLayout(spec)
	if err != nil {
		return err
	}

	if l.upper != "" {
		if err := os.MkdirAll(l.upper, 0o755); err != nil {
			return fmt.Errorf("overlay: create upperdir: %w", err)
		}
		if err := os.MkdirAll(l.work, 0o755); err != nil {
			return fmt.Errorf("overlay: create workdir: %w", err)
		}
	}

	if dir := l.single(); dir != "" {
		return bindMount(dir, spec.Target, l.readOnly)
	}

	if fsopenSupported() {
		if err := mountFsconfig(l, spec.Target, schema); err != nil {
			return fmt.Errorf("overlay: mount %s: %w", spec.Target, err)
		}
		return nil
	}
	if err := mountClassic(l, spec.Target, schema); err != nil {
		return fmt.Errorf("overlay: mount %s: %w", spec.Target, err)
	}
	return nil
}

func unmountUnion(target string) error {
	err := unix.Unmount(target, unix.MNT_DETACH)
	switch err {
	case nil, unix.EINVAL, unix.ENOENT:
		// EINVAL: not a mountpoint. Unmount must tolerate being called twice.
		return nil
	default:
		return fmt.Errorf("overlay: unmount %s: %w", target, err)
	}
}

// fsopenSupported probes the new mount API once. The result is cached: the kernel
// does not change under a running driver, and parsing uname is not a reliable
// substitute for asking.
var fsopenSupported = sync.OnceValue(func() bool {
	fd, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	if err != nil {
		klog.V(2).Infof("overlay: fsopen unavailable (%v), using the classic mount API", err)
		return false
	}
	_ = unix.Close(fd)
	return true
})

// mountFsconfig uses the fsopen/fsconfig/fsmount API. Each lowerdir is passed as
// its own argument via lowerdir+, which removes both the colon/comma escaping
// problem and the classic API's ~4096-byte option-string cap on layer count.
func mountFsconfig(l *layout, target string, schema backend.OptionSchema) error {
	fd, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("fsopen: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	for _, dir := range l.lowers {
		if err := unix.FsconfigSetString(fd, "lowerdir+", dir); err != nil {
			return fmt.Errorf("fsconfig lowerdir+=%s: %w", dir, err)
		}
	}
	if l.upper != "" {
		if err := unix.FsconfigSetString(fd, "upperdir", l.upper); err != nil {
			return fmt.Errorf("fsconfig upperdir=%s: %w", l.upper, err)
		}
		if err := unix.FsconfigSetString(fd, "workdir", l.work); err != nil {
			return fmt.Errorf("fsconfig workdir=%s: %w", l.work, err)
		}
	}
	for _, k := range l.sortedOptions() {
		if schema[k].Kind == backend.ValueFlag {
			err = unix.FsconfigSetFlag(fd, k)
		} else {
			err = unix.FsconfigSetString(fd, k, l.options[k])
		}
		if err != nil {
			return fmt.Errorf("fsconfig %s: %w", k, err)
		}
	}

	if err := unix.FsconfigCreate(fd); err != nil {
		return fmt.Errorf("fsconfig create: %w", err)
	}

	attr := 0
	if l.readOnly {
		attr = unix.MOUNT_ATTR_RDONLY
	}
	mfd, err := unix.Fsmount(fd, unix.FSMOUNT_CLOEXEC, attr)
	if err != nil {
		return fmt.Errorf("fsmount: %w", err)
	}
	defer func() { _ = unix.Close(mfd) }()

	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount: %w", err)
	}
	return nil
}

// mountClassic is the pre-5.2 fallback: everything in one comma-separated option
// string, with the layer paths escaped.
func mountClassic(l *layout, target string, schema backend.OptionSchema) error {
	escaped := make([]string, 0, len(l.lowers))
	for _, dir := range l.lowers {
		escaped = append(escaped, escapeOptionValue(dir))
	}

	opts := []string{"lowerdir=" + strings.Join(escaped, ":")}
	if l.upper != "" {
		opts = append(opts, "upperdir="+escapeOptionValue(l.upper), "workdir="+escapeOptionValue(l.work))
	}
	for _, k := range l.sortedOptions() {
		if schema[k].Kind == backend.ValueFlag {
			opts = append(opts, k)
		} else {
			opts = append(opts, k+"="+l.options[k])
		}
	}

	var flags uintptr
	if l.readOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount("overlay", target, "overlay", flags, strings.Join(opts, ",")); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	return nil
}

func bindMount(source, target string, readOnly bool) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("overlay: bind %s to %s: %w", source, target, err)
	}
	if !readOnly {
		return nil
	}
	// A bind mount cannot be made read-only in one step; MS_RDONLY is only honoured
	// by a follow-up remount.
	if err := unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("overlay: remount %s read-only: %w", target, err)
	}
	return nil
}

// escapeOptionValue escapes the characters that terminate a field in the classic
// option string. Driver-computed paths never contain them, but the invariant is
// one line.
func escapeOptionValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `:`, `\:`, `,`, `\,`)
	return r.Replace(s)
}
