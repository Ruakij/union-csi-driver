// Package backend defines the pluggable mount-backend interface implemented by
// pkg/backend/overlay and pkg/backend/mergerfs, plus the option-schema types the
// policy engine in policy.go validates against.
package backend

import "context"

// ValueKind describes what kind of value an option accepts.
type ValueKind int

const (
	// ValueFlag options take no value (presence-only).
	ValueFlag ValueKind = iota
	// ValueEnum options must be one of Spec.Enum.
	ValueEnum
	// ValueBool options take "true" or "false".
	ValueBool
	// ValueDuration options take a Go duration string.
	ValueDuration
	// ValueInt options take a bounded integer (Spec.MinInt..Spec.MaxInt).
	ValueInt
	// ValueSize options take a byte-size string (e.g. "10G").
	ValueSize
)

// OptionSpec describes one backend option known to the schema.
type OptionSpec struct {
	Kind   ValueKind
	Enum   []string // valid values, only for ValueEnum
	MinInt int64    // inclusive, only for ValueInt
	MaxInt int64    // inclusive, only for ValueInt
}

// OptionSchema maps an option name to its spec. This is the ground truth for what
// a backend accepts, independent of admin allow/deny policy. Path-bearing or
// process-shaping options (lowerdir, upperdir, branches, category.create,
// allow_other, ...) are never present here - they are driver-computed and so are
// structurally absent from anything a pod or admin can set.
type OptionSchema map[string]OptionSpec

// Source is one resolved branch: a node-local path plus its write mode, in the
// backend's own vocabulary (RW/RO/NC for mergerfs, RW/RO for overlay).
type Source struct {
	Path string
	Mode string
}

// MountSpec is everything a backend needs to perform one union mount. Sources are
// ordered leftmost-wins on lookup. All paths are driver-resolved; Options have
// already passed through the policy engine.
type MountSpec struct {
	Target   string
	Sources  []Source
	Options  map[string]string
	ReadOnly bool
}

// Backend is implemented once per merge strategy (overlay, mergerfs) and selected
// at process startup via --backend. It is never selectable from volumeAttributes.
type Backend interface {
	// Name is the backend identifier, e.g. "overlay" or "mergerfs".
	Name() string
	// Schema returns the backend's option schema.
	Schema() OptionSchema
	// Mount performs the union mount described by spec. Must be idempotent: callers
	// check mountinfo first, but Mount may be called again after a driver restart.
	Mount(ctx context.Context, spec MountSpec) error
	// Unmount tears down a mount previously created by Mount. Must tolerate the
	// target already being unmounted.
	Unmount(ctx context.Context, target string) error
}

// registry of compiled-in backend constructors, populated by each backend
// package's init().
var registry = map[string]func() Backend{}

// Register makes a backend constructor available under name.
func Register(name string, ctor func() Backend) {
	registry[name] = ctor
}

// Get constructs the named backend, or reports it unknown.
func Get(name string) (Backend, bool) {
	ctor, ok := registry[name]
	if !ok {
		return nil, false
	}
	return ctor(), true
}

// Names returns the compiled-in backend names.
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
