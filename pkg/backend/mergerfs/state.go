package mergerfs

import (
	"path/filepath"
	"strings"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/state"
)

// volumeState is what a restarted driver needs to rebuild a mount it no longer
// remembers. mountinfo names neither the branch list nor the options, so without
// this file a dead FUSE mount cannot be repaired, only removed.
type volumeState struct {
	VolumeID string `json:"volumeID"`
	Target   string `json:"target"`
	// Argv is the command line as the daemon sees it: inside the sandbox when
	// Branches is set, on the host otherwise.
	Argv []string `json:"argv"`
	// Branches are the host path and the sandbox path of every branch, and the
	// reconcile pass compares the two views to find a branch the daemon lost.
	Branches []sandbox.Bind `json:"branches,omitempty"`
	// SharedFrom marks Target as a bind of another volume's union, with no daemon of its own.
	SharedFrom string `json:"sharedFrom,omitempty"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
}

// newStateStore keeps one file per volume at <dir>/<sanitized id>.json. The layout
// is fixed: a driver pod that starts after an upgrade reads the files the earlier
// one wrote.
func newStateStore(dir string) *state.Store[volumeState] {
	return &state.Store[volumeState]{
		Path: func(id string) string { return filepath.Join(dir, sanitizeUnitName(id)+".json") },
		Glob: filepath.Join(globEscaper.Replace(dir), "*.json"),
	}
}

// globEscaper keeps a state directory with a pattern character in its name from
// matching nothing, which would leave every volume unrepaired without an error.
var globEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`)
