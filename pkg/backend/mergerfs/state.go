package mergerfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// volumeState is what a restarted driver needs to rebuild a mount it no longer
// remembers. mountinfo names neither the branch list nor the options, so without
// this file a dead FUSE mount cannot be repaired, only removed.
type volumeState struct {
	VolumeID string   `json:"volumeID"`
	Target   string   `json:"target"`
	Argv     []string `json:"argv"`
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

	// Rename so a reconcile pass never reads a half-written file.
	final := statePath(dir, st.VolumeID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("mergerfs: write state: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("mergerfs: write state: %w", err)
	}
	return nil
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
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("mergerfs: read state %s: %w", e.Name(), err)
		}
		var st volumeState
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, fmt.Errorf("mergerfs: parse state %s: %w", e.Name(), err)
		}
		states = append(states, st)
	}
	return states, nil
}
