// Package mergerfs implements the mergerfs (FUSE) backend.
//
// TODO: process invocation and argv construction, daemon lifetime (host systemd
// transient scope, in-container fallback + reconcile loop).
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

// Schema is the set of mergerfs options a pod or admin may set. The branch
// argument and every option that shapes it or the process (branches,
// category.create, moveonenospc, allow_other) is structurally absent: those are
// driver-computed.
func (b *mergerfsBackend) Schema() backend.OptionSchema {
	return backend.OptionSchema{
		"cache.entry":          {Kind: backend.ValueDuration},
		"cache.attr":           {Kind: backend.ValueDuration},
		"cache.negative_entry": {Kind: backend.ValueDuration},
		"cache.readdir":        {Kind: backend.ValueBool},
		"cache.files":          {Kind: backend.ValueEnum, Enum: []string{"off", "partial", "full", "auto-full", "per-process", "libfuse"}},
		"func.getattr":         {Kind: backend.ValueEnum, Enum: []string{"ff", "newest"}},
		"category.search":      {Kind: backend.ValueEnum, Enum: []string{"ff", "all", "newest"}},
		"dropcacheonclose":     {Kind: backend.ValueBool},
		"inodecalc":            {Kind: backend.ValueEnum, Enum: []string{"passthrough", "path-hash", "devino-hash", "hybrid-hash", "path-hash32", "devino-hash32", "hybrid-hash32"}},
		"threads":              {Kind: backend.ValueInt, MinInt: -16, MaxInt: 1024},
		"minfreespace":         {Kind: backend.ValueSize},
	}
}

// DefaultOptions keeps lookups cheap while still reflecting live edits to the
// branches, which is the reason to pick mergerfs over overlay in the first place.
func (b *mergerfsBackend) DefaultOptions() map[string]string {
	return map[string]string{
		"cache.entry":          "1",
		"cache.attr":           "1",
		"cache.negative_entry": "0",
		"func.getattr":         "newest",
	}
}

// DefaultDenylist is empty: no mergerfs option in the schema is a privilege
// boundary the way overlay's metacopy/userxattr pair is.
func (b *mergerfsBackend) DefaultDenylist() []string {
	return nil
}

// SourceModes: mergerfs's own branch mode tags, bare entry defaults to RW
// (mergerfs's own union-mode default).
func (b *mergerfsBackend) SourceModes() ([]string, string) {
	return []string{"RW", "RO", "NC"}, "RW"
}

// MaxWritable is 0 (unlimited): any number of branches may be RW, category.create
// arbitrates which one a new file lands on.
func (b *mergerfsBackend) MaxWritable() int {
	return 0
}

func (b *mergerfsBackend) Mount(ctx context.Context, spec backend.MountSpec) error {
	return errors.New("mergerfs backend not implemented yet")
}

func (b *mergerfsBackend) Unmount(ctx context.Context, volumeID, target string) error {
	return errors.New("mergerfs backend not implemented yet")
}
