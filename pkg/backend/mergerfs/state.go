package mergerfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
	"k8s.io/klog/v2"
)

// volumeState is what a restarted driver needs to rebuild a mount it no longer
// remembers. mountinfo names neither the branch list nor the options, so without
// this file a dead FUSE mount cannot be repaired, only removed.
type volumeState struct {
	VolumeID string `json:"volumeID"`
	Target   string `json:"target"`
	// Argv is the command line as the daemon sees it: inside the sandbox when
	// Branches is set, on the host otherwise.
	Argv     []string       `json:"argv"`
	Branches []sandbox.Bind `json:"branches,omitempty"`
	// SharedFrom marks Target as a bind of another volume's union, with no daemon of its own.
	SharedFrom string `json:"sharedFrom,omitempty"`
	ReadOnly   bool   `json:"readOnly,omitempty"`
}

func statePath(dir, volumeID string) string {
	return filepath.Join(dir, sanitizeUnitName(volumeID)+".json")
}

func saveState(dir string, st volumeState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mergerfs: create state dir: %w", err)
	}
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("mergerfs: marshal state: %w", err)
	}

	// Synced and renamed, so neither a reconcile pass nor a crash leaves a
	// half-written file.
	final := statePath(dir, st.VolumeID)
	tmp := final + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return fmt.Errorf("mergerfs: write state: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("mergerfs: write state: %w", err)
	}
	return nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func removeState(dir, volumeID string) error {
	if err := os.Remove(statePath(dir, volumeID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("mergerfs: remove state: %w", err)
	}
	return nil
}

func loadStates(dir string) ([]volumeState, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mergerfs: read state dir: %w", err)
	}

	states := make([]volumeState, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// One bad file must not keep every other volume from being repaired.
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			klog.Errorf("mergerfs: skipping state %s: %v", e.Name(), err)
			continue
		}
		var st volumeState
		if err := json.Unmarshal(data, &st); err != nil {
			klog.Errorf("mergerfs: skipping state %s: %v", e.Name(), err)
			continue
		}
		states = append(states, st)
	}
	return states, nil
}
