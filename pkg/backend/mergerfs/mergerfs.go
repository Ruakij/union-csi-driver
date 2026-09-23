// Package mergerfs implements the mergerfs (FUSE) backend.
package mergerfs

import (
	"context"
	"errors"
	"flag"

	"k8s.io/utils/keymutex"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func init() {
	backend.Register("mergerfs", New)
}

// volumeLocks serializes Mount, Unmount and the reconcile loop per volume, so a
// remount cannot resurrect a volume that is being unpublished.
var volumeLocks = keymutex.NewHashed(0)

func lockVolume(id string)   { volumeLocks.LockKey(id) }
func unlockVolume(id string) { _ = volumeLocks.UnlockKey(id) }

var (
	useSandbox  = true
	sealControl = true
)

// RegisterFlags adds the backend's startup flags to fs.
func RegisterFlags(fs *flag.FlagSet) {
	fs.BoolVar(&useSandbox, "mergerfs-sandbox", useSandbox,
		"run each mergerfs daemon in an empty root holding only its branches and target (Linux 5.12+)")
	fs.BoolVar(&sealControl, "mergerfs-seal-control-file", sealControl,
		"bind-mount each union's .mergerfs control file read-only over itself, so consumers cannot reconfigure the union")
}

type mergerfsBackend struct {
	stateDir string
}

// New constructs the mergerfs backend.
func New() backend.Backend {
	return &mergerfsBackend{}
}

func (b *mergerfsBackend) Name() string { return "mergerfs" }

// Schema is the set of mergerfs options a pod or admin may set. The branch
// argument and every option that shapes it or the process (branches,
// moveonenospc, allow_other) is structurally absent: those are driver-computed.
// cache.files=per-process is absent too: it looks callers up by pid, which the
// sandbox's PID namespace hides. follow-symlinks needs the sandbox, where a
// followed symlink reaches nothing but the volume's own branches.
func (b *mergerfsBackend) Schema() backend.OptionSchema {
	s := backend.OptionSchema{
		"cache.entry":          {Kind: backend.ValueDuration},
		"cache.attr":           {Kind: backend.ValueDuration},
		"cache.negative_entry": {Kind: backend.ValueDuration},
		"cache.readdir":        {Kind: backend.ValueBool},
		"cache.files":          {Kind: backend.ValueEnum, Enum: []string{"off", "partial", "full", "auto-full", "libfuse"}},
		"func.getattr":         {Kind: backend.ValueEnum, Enum: []string{"ff", "newest"}},
		"category.search":      {Kind: backend.ValueEnum, Enum: []string{"ff", "all", "newest"}},
		"category.create": {Kind: backend.ValueEnum, Enum: []string{
			"all", "epall", "ff", "epff", "lfs", "eplfs", "lus", "eplus", "mfs", "epmfs",
			"msplfs", "msplus", "mspmfs", "msppfrd", "newest", "pfrd", "eppfrd", "rand", "eprand",
		}},
		"dropcacheonclose": {Kind: backend.ValueBool},
		"inodecalc":        {Kind: backend.ValueEnum, Enum: []string{"passthrough", "path-hash", "devino-hash", "hybrid-hash", "path-hash32", "devino-hash32", "hybrid-hash32"}},
		"threads":          {Kind: backend.ValueInt, MinInt: -16, MaxInt: 1024},
		"minfreespace":     {Kind: backend.ValueSize},
	}
	if useSandbox {
		s["follow-symlinks"] = backend.OptionSpec{Kind: backend.ValueEnum, Enum: []string{"never", "directory", "regular", "all"}}
	}
	return s
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

// Init records where per-volume state lives. Every mount writes its branch list
// and options there, because a restarted driver cannot recover them from
// mountinfo and would otherwise have no way to repair a dead FUSE mount.
func (b *mergerfsBackend) Init(stateDir string) error {
	if stateDir == "" {
		return errors.New("mergerfs: no state directory configured")
	}
	if useSandbox {
		if err := checkSandbox(); err != nil {
			return err
		}
	}
	b.stateDir = stateDir
	return nil
}

// Run remounts volumes whose daemon died, until ctx is done.
func (b *mergerfsBackend) Run(ctx context.Context) {
	reconcile(ctx, b.stateDir)
}

func (b *mergerfsBackend) Mount(ctx context.Context, spec backend.MountSpec) error {
	lockVolume(spec.VolumeID)
	defer unlockVolume(spec.VolumeID)
	return mountUnion(ctx, spec, b.stateDir)
}

func (b *mergerfsBackend) Unmount(ctx context.Context, volumeID, target string) error {
	lockVolume(volumeID)
	defer unlockVolume(volumeID)
	return unmountUnion(ctx, volumeID, target, b.stateDir)
}
