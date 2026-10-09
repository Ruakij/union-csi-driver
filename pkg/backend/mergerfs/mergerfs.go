// Package mergerfs implements the mergerfs (FUSE) backend.
package mergerfs

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/state"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/supervisor"
	"k8s.io/klog/v2"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func init() {
	backend.Register("mergerfs", New)
}

// probeTimeout bounds one probe of a mount. A FUSE mount whose daemon is gone
// answers with ENOTCONN at once, but one whose daemon is alive and stuck never
// answers at all.
const probeTimeout = 5 * time.Second

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
	states   *state.Store[volumeState]
	scopes   *scope.Manager
	// sup starts the daemons and runs the reconcile passes. Mount, Share and
	// Unmount hold its lock of the volume, so a remount cannot resurrect a volume
	// that is being unpublished.
	sup   *supervisor.Supervisor
	probe *fusemount.Prober
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
// followed symlink reaches nothing but the volume's own branches. link-exdev's
// absolute forms and symlinkify create links to paths only the daemon sees, as
// does rename-exdev, whose relative form mergerfs fails at.
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
		"link-exdev":       {Kind: backend.ValueEnum, Enum: []string{"passthrough", "rel-symlink"}},
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
	scopes, err := scope.New(context.Background(), scopeConfig())
	if err != nil {
		return fmt.Errorf("mergerfs: %w", err)
	}
	sup, err := supervisor.New(supervisor.Config{
		Scope:     scopes,
		IDs:       b.ids,
		Reconcile: b.reconcile,
		OnError:   func(err error) { klog.Errorf("mergerfs: reconcile: %v", err) },
	})
	if err != nil {
		return fmt.Errorf("mergerfs: %w", err)
	}
	b.stateDir, b.states, b.scopes, b.sup = stateDir, newStateStore(stateDir), scopes, sup
	b.probe = fusemount.NewProber(probeTimeout)
	return nil
}

// Run repairs volumes whose daemon died, until ctx is done.
func (b *mergerfsBackend) Run(ctx context.Context) {
	if err := b.scopes.FellBack(); err != nil {
		klog.Warningf("mergerfs: host systemd is unreachable (%v); mergerfs daemons die with this pod and are remounted when it restarts. "+
			"Consumers holding open file descriptors across a restart keep seeing ENOTCONN until they reopen the file", err)
	}
	b.sup.Run(ctx)
}

func (b *mergerfsBackend) Mount(ctx context.Context, spec backend.MountSpec) error {
	b.sup.Lock(spec.VolumeID)
	defer b.sup.Unlock(spec.VolumeID)
	return b.mountUnion(ctx, spec)
}

func (b *mergerfsBackend) Share(ctx context.Context, volumeID, source, target string, readOnly bool) error {
	b.sup.Lock(volumeID)
	defer b.sup.Unlock(volumeID)
	return b.shareUnion(ctx, volumeID, source, target, readOnly)
}

func (b *mergerfsBackend) Unmount(ctx context.Context, volumeID, target string) error {
	b.sup.Lock(volumeID)
	defer b.sup.Unlock(volumeID)
	return b.unmountUnion(ctx, volumeID, target)
}
