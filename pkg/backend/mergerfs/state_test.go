package mergerfs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store := newStateStore(dir)

	if states, errs := store.LoadAll(); len(errs) != 0 || len(states) != 0 {
		t.Fatalf("LoadAll on a missing dir = %v, %v; want empty, nil", states, errs)
	}

	want := volumeState{
		VolumeID: "csi-4f2a9b",
		Target:   "/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/merged/mount",
		Argv:     []string{"-f", "-o", "allow_other", "/a=RW:/b=RO", "/target"},
	}
	if err := store.Save(want.VolumeID, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	states, errs := store.LoadAll()
	if len(errs) != 0 || len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Fatalf("LoadAll = %+v, %v, want [%+v]", states, errs, want)
	}

	if err := store.Remove(want.VolumeID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if states, errs := store.LoadAll(); len(errs) != 0 || len(states) != 0 {
		t.Fatalf("LoadAll after remove = %v, %v; want empty, nil", states, errs)
	}
	// Removing twice is not an error: NodeUnpublishVolume is retried.
	if err := store.Remove(want.VolumeID); err != nil {
		t.Fatalf("Remove twice: %v", err)
	}
}

func TestLoadAllIgnoresNonState(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "csi-x.log", "csi-x.json.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	states, errs := newStateStore(dir).LoadAll()
	if len(errs) != 0 || len(states) != 0 {
		t.Fatalf("LoadAll = %v, %v; want empty, nil", states, errs)
	}
}

// One unreadable state must not keep the others from being repaired.
func TestLoadAllSkipsCorrupt(t *testing.T) {
	dir := t.TempDir()
	store := newStateStore(dir)
	want := volumeState{VolumeID: "csi-good", Target: "/target", Argv: []string{"-f"}}
	if err := store.Save(want.VolumeID, want); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"empty.json": "", "junk.json": "{"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	states, errs := store.LoadAll()
	if len(errs) != 2 || len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Fatalf("LoadAll = %+v, %v; want [%+v] and 2 errors", states, errs, want)
	}
}
