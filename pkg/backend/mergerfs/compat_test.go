package mergerfs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
)

// The scope name and the state file of a volume are how a driver pod finds what an
// earlier one left running. These are the functions of the drivers before the
// library, frozen, so a change to the naming of the driver or the library fails here.
func legacyScopeUnitName(volumeID string) string {
	return "union-csi-" + sanitizeUnitName(volumeID) + ".scope"
}

func legacyStatePath(dir, volumeID string) string {
	return filepath.Join(dir, sanitizeUnitName(volumeID)+".json")
}

// legacyStateFiles are the files the driver before the library loaded.
func legacyStateFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files
}

var compatIDs = []string{
	"csi-4f2a9b",
	"vol.with_chars-1",
	"vol/../../etc",
	"vol id@host",
	"pvc-ü€\x00\n\t*?[",
	"",
	strings.Repeat("a", 300),
	strings.Repeat("v/ö", 120),
}

func TestScopeUnitCompat(t *testing.T) {
	cfg := scopeConfig()
	cfg.Lifetime = scope.InContainer
	m, err := scope.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range compatIDs {
		got, err := m.Unit(id)
		if err != nil || got != legacyScopeUnitName(id) {
			t.Errorf("Unit(%q) = %q, %v; want %q", id, got, err, legacyScopeUnitName(id))
		}
	}
	if got, _ := m.Unit("csi-4f2a9b"); got != "union-csi-csi-4f2a9b.scope" {
		t.Errorf("Unit = %q", got)
	}
}

func TestStatePathCompat(t *testing.T) {
	// A state directory with glob characters in its name must still match its files.
	for _, base := range []string{"state", "st[a]te*?\\dir"} {
		dir := filepath.Join(t.TempDir(), base)
		store := newStateStore(dir)
		for _, id := range compatIDs {
			if got := store.Path(id); got != legacyStatePath(dir, id) {
				t.Errorf("Path(%q) = %q, want %q", id, got, legacyStatePath(dir, id))
			}
		}

		for _, id := range compatIDs[:5] {
			if err := store.Save(id, volumeState{VolumeID: id, Target: "/t"}); err != nil {
				t.Fatalf("Save(%q): %v", id, err)
			}
		}
		// What the driver before the library wrote: a pod of it must be read by this one.
		for name, data := range map[string]string{
			"legacy-1.json": `{"volumeID":"legacy-1","target":"/t1","argv":["-f","/branch/0=RW","/union"],` +
				`"branches":[{"host":"/h/a","sandbox":"/branch/0"}],"branchPaths":["/h/a"],"sandboxBranches":["/branch/0"]}`,
			"legacy-2.json": `{"volumeID":"legacy-2","target":"/t2","argv":["-f"],"sharedFrom":"/t1","readOnly":true}`,
			"other.txt":     "x",
			"legacy-1.log":  "x",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		states, errs := store.LoadAll()
		if len(errs) != 0 {
			t.Fatalf("LoadAll: %v", errs)
		}
		if want := len(legacyStateFiles(t, dir)); len(states) != want {
			t.Errorf("LoadAll in %q found %d states, the files of the earlier driver are %d", base, len(states), want)
		}

		legacy, err := store.Load("legacy-1")
		if err != nil {
			t.Fatal(err)
		}
		var want volumeState
		if err := json.Unmarshal([]byte(`{"volumeID":"legacy-1","target":"/t1","argv":["-f","/branch/0=RW","/union"],`+
			`"branches":[{"host":"/h/a","sandbox":"/branch/0"}]}`), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(legacy.Branches, want.Branches) || legacy.Target != "/t1" || !reflect.DeepEqual(legacy.Argv, want.Argv) {
			t.Errorf("legacy state = %+v", legacy)
		}
		if shared, err := store.Load("legacy-2"); err != nil || shared.SharedFrom != "/t1" || !shared.ReadOnly {
			t.Errorf("legacy shared state = %+v, %v", shared, err)
		}
	}
}
