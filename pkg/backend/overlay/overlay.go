// Package overlay implements the kernel overlayfs backend.
package overlay

import (
	"context"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

const (
	modeRW = "RW"
	modeRO = "RO"
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

// Schema is the set of overlayfs options a pod or admin may set. Every
// path-bearing option (lowerdir, lowerdir+, upperdir, workdir, datadir+) is
// structurally absent: those are driver-computed.
func (b *overlayBackend) Schema() backend.OptionSchema {
	return backend.OptionSchema{
		"default_permissions": {Kind: backend.ValueFlag},
		"redirect_dir":        {Kind: backend.ValueEnum, Enum: []string{"nofollow", "off", "follow"}},
		"index":               {Kind: backend.ValueEnum, Enum: []string{"off"}},
		"xino":                {Kind: backend.ValueEnum, Enum: []string{"off", "auto", "on"}},
		"uuid":                {Kind: backend.ValueEnum, Enum: []string{"auto", "null", "off", "on"}},
		"verity":              {Kind: backend.ValueEnum, Enum: []string{"off", "on", "require"}},
		"metacopy":            {Kind: backend.ValueEnum, Enum: []string{"on", "off"}},
		"userxattr":           {Kind: backend.ValueFlag},
		"nfs_export":          {Kind: backend.ValueEnum, Enum: []string{"on", "off"}},
	}
}

// DefaultOptions is the only combination the kernel docs sanction when lower
// trees may be edited at all.
func (b *overlayBackend) DefaultOptions() map[string]string {
	return map[string]string{
		"index":        "off",
		"metacopy":     "off",
		"xino":         "off",
		"redirect_dir": "nofollow",
	}
}

// DefaultDenylist blocks metacopy and userxattr because together they let an
// unprivileged writer forge user.overlay.redirect xattrs on a lower layer.
func (b *overlayBackend) DefaultDenylist() []string {
	return []string{"metacopy", "userxattr", "nfs_export"}
}

// SourceModes: bare entry defaults to RW (matching mergerfs, per .docs/changelog.md).
func (b *overlayBackend) SourceModes() ([]string, string) {
	return []string{modeRW, modeRO}, modeRW
}

// MaxWritable is 1: overlay has exactly one upperdir, a real kernel limit, not an
// arbitrary v1 cap.
func (b *overlayBackend) MaxWritable() int {
	return 1
}

func (b *overlayBackend) Mount(ctx context.Context, spec backend.MountSpec) error {
	return mountUnion(spec, b.Schema())
}

func (b *overlayBackend) Unmount(ctx context.Context, target string) error {
	return unmountUnion(target)
}
