// Package overlay implements the kernel overlayfs backend.
//
// TODO: mount_linux.go (Fsopen/Fsmount union mount, classic mount fallback, N==1
// bind-mount special case), option schema, upper/workdir handling. See
// .docs/plan.md section 4.
package overlay

import (
	"context"
	"errors"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func init() {
	backend.Register("overlay", New)
}

type overlayBackend struct{}

// New constructs the overlay backend.
func New() backend.Backend {
	return &overlayBackend{}
}

func (b *overlayBackend) Name() string { return "overlay" }

func (b *overlayBackend) Schema() backend.OptionSchema {
	// TODO: default_permissions, redirect_dir, index, xino, uuid, verity, metacopy,
	// userxattr, nfs_export - see .docs/plan.md section 1.
	return backend.OptionSchema{}
}

// SourceModes: bare entry defaults to RW (matching mergerfs, per .docs/changelog.md).
func (b *overlayBackend) SourceModes() ([]string, string) {
	return []string{"RW", "RO"}, "RW"
}

// MaxWritable is 1: overlay has exactly one upperdir, a real kernel limit, not an
// arbitrary v1 cap.
func (b *overlayBackend) MaxWritable() int {
	return 1
}

func (b *overlayBackend) Mount(ctx context.Context, spec backend.MountSpec) error {
	return errors.New("overlay backend not implemented yet")
}

func (b *overlayBackend) Unmount(ctx context.Context, target string) error {
	return errors.New("overlay backend not implemented yet")
}
