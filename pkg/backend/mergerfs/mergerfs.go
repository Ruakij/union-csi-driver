// Package mergerfs implements the mergerfs (FUSE) backend.
//
// TODO: process invocation and argv construction, daemon lifetime (host systemd
// transient scope, in-container fallback + reconcile loop), option schema. See
// .docs/plan.md section 5.
package mergerfs

import (
	"context"
	"errors"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func init() {
	backend.Register("mergerfs", New)
}

type mergerfsBackend struct{}

// New constructs the mergerfs backend.
func New() backend.Backend {
	return &mergerfsBackend{}
}

func (b *mergerfsBackend) Name() string { return "mergerfs" }

func (b *mergerfsBackend) Schema() backend.OptionSchema {
	// TODO: cache.entry, cache.attr, cache.negative_entry, cache.readdir,
	// cache.files, func.getattr, category.search, dropcacheonclose, inodecalc,
	// threads, minfreespace - see .docs/plan.md section 1.
	return backend.OptionSchema{}
}

func (b *mergerfsBackend) Mount(ctx context.Context, spec backend.MountSpec) error {
	return errors.New("mergerfs backend not implemented yet")
}

func (b *mergerfsBackend) Unmount(ctx context.Context, target string) error {
	return errors.New("mergerfs backend not implemented yet")
}
