package overlay

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

const (
	// workspaceDir holds the writable branch's upper and work directories. They
	// must live on the same filesystem as each other and be separate subtrees, so
	// the RW source volume itself cannot be the upperdir: the kernel refuses a
	// workdir nested inside upperdir, and a sibling of the volume root is on the
	// kubelet filesystem rather than the volume's. Only <volume>/.union-csi/upper
	// takes part in the merge; anything else in the RW volume stays invisible.
	workspaceDir = ".union-csi"
	upperName    = "upper"
	workName     = "work"
)

// layout is the driver-computed overlayfs geometry for one mount.
type layout struct {
	lowers []string // topmost first
	upper  string   // empty when every source is RO
	work   string
	// readOnly is set when the CSI request asked for it or there is no writable
	// branch to absorb changes.
	readOnly bool
	options  map[string]string
}

// planLayout maps resolved sources onto overlayfs's asymmetric shape: many
// lowerdirs plus at most one upperdir, which the kernel always stacks topmost.
func planLayout(spec backend.MountSpec) (*layout, error) {
	if len(spec.Sources) == 0 {
		return nil, fmt.Errorf("overlay: no source volumes")
	}

	l := &layout{options: spec.Options}
	for i, s := range spec.Sources {
		switch s.Mode {
		case modeRW:
			if l.upper != "" {
				return nil, fmt.Errorf("overlay: more than one RW source volume, overlay has a single upperdir")
			}
			if i != 0 {
				return nil, fmt.Errorf("overlay: the RW entry must come first in sourceVolumes, overlay always stacks its writable layer topmost")
			}
			l.upper = filepath.Join(s.Path, workspaceDir, upperName)
			l.work = filepath.Join(s.Path, workspaceDir, workName)
		case modeRO:
			l.lowers = append(l.lowers, s.Path)
		default:
			return nil, fmt.Errorf("overlay: unknown source mode %q", s.Mode)
		}
	}

	l.readOnly = spec.ReadOnly || l.upper == ""
	return l, nil
}

// single reports the lone directory to bind-mount when there is exactly one
// branch, or "" when a real overlay mount is needed. Kernels before 6.7 reject a
// single lowerdir with no upperdir, and an upperdir with no lowerdir is never
// valid; a bind mount is semantically identical and works everywhere.
func (l *layout) single() string {
	if len(l.lowers) == 0 {
		return l.upper
	}
	if len(l.lowers) == 1 && l.upper == "" {
		return l.lowers[0]
	}
	return ""
}

// sortedOptions returns option keys in a stable order so mounts are reproducible.
func (l *layout) sortedOptions() []string {
	keys := make([]string, 0, len(l.options))
	for k := range l.options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
