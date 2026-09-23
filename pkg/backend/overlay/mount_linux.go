//go:build linux

package overlay

import (
	"fmt"
	"path/filepath"
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

	if l.rwRoot != "" {
		ws, err := openWorkspace(l.rwRoot)
		if err != nil {
			return err
		}
		defer ws.close()
		l.upper, l.work = ws.upper, ws.work
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

// workspace holds the RW volume's upper and work directories open. The mount is
// given their /proc/self/fd paths, so it uses exactly the directories checked
// here even if the volume is changed in between.
type workspace struct {
	fds         []int
	upper, work string
}

func openWorkspace(root string) (*workspace, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("overlay: open %s: %w", root, err)
	}
	defer func() { _ = unix.Close(rootFD) }()
	if err := checkUpperFS(rootFD, root); err != nil {
		return nil, err
	}

	wsFD, err := openDirNoFollow(rootFD, root, workspaceDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(wsFD) }()

	ws := &workspace{}
	for _, name := range []string{upperName, workName} {
		fd, err := openDirNoFollow(wsFD, filepath.Join(root, workspaceDir), name)
		if err != nil {
			ws.close()
			return nil, err
		}
		ws.fds = append(ws.fds, fd)
	}
	ws.upper = fmt.Sprintf("/proc/self/fd/%d", ws.fds[0])
	ws.work = fmt.Sprintf("/proc/self/fd/%d", ws.fds[1])
	return ws, nil
}

// unsupportedUpperFS are filesystems the kernel refuses as upperdir with a bare
// EINVAL: network and FUSE filesystems revalidate dentries, and overlay cannot
// nest its own upper.
var unsupportedUpperFS = map[uint32]string{
	unix.NFS_SUPER_MAGIC:       "NFS",
	unix.FUSE_SUPER_MAGIC:      "FUSE",
	unix.CEPH_SUPER_MAGIC:      "CephFS",
	unix.CIFS_SUPER_MAGIC:      "CIFS",
	unix.SMB_SUPER_MAGIC:       "SMB",
	unix.SMB2_SUPER_MAGIC:      "SMB",
	unix.OVERLAYFS_SUPER_MAGIC: "overlay",
}

func checkUpperFS(fd int, path string) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return fmt.Errorf("overlay: statfs %s: %w", path, err)
	}
	if name, bad := unsupportedUpperFS[uint32(st.Type)]; bad {
		return fmt.Errorf("overlay: the RW source %s is on %s, which the kernel does not support as a writable layer; use a local filesystem volume, or the mergerfs backend", path, name)
	}
	return nil
}

func (ws *workspace) close() {
	for _, fd := range ws.fds {
		_ = unix.Close(fd)
	}
}

// openDirNoFollow creates and opens the directory name under parent, refusing a
// symlink. The RW volume is pod-writable, so a planted link would otherwise
// point the writable layer, and the kernel's workdir cleanup, anywhere on the
// node.
func openDirNoFollow(parent int, parentPath, name string) (int, error) {
	path := filepath.Join(parentPath, name)
	if err := unix.Mkdirat(parent, name, 0o755); err != nil && err != unix.EEXIST {
		return -1, fmt.Errorf("overlay: create %s: %w", path, err)
	}
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch err {
	case nil:
		return fd, nil
	case unix.ELOOP:
		return -1, fmt.Errorf("overlay: %s is a symlink, refusing to use it", path)
	default:
		return -1, fmt.Errorf("overlay: open %s: %w", path, err)
	}
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
