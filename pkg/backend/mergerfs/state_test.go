package mergerfs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	if states, err := loadStates(dir); err != nil || len(states) != 0 {
		t.Fatalf("loadStates on a missing dir = %v, %v; want empty, nil", states, err)
	}

	want := volumeState{
		VolumeID: "csi-4f2a9b",
		Target:   "/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/merged/mount",
		Argv:     []string{"-f", "-o", "allow_other", "/a=RW:/b=RO", "/target"},
	}
	if err := saveState(dir, want); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	states, err := loadStates(dir)
	if err != nil {
		t.Fatalf("loadStates: %v", err)
	}
	if len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Fatalf("loadStates = %+v, want [%+v]", states, want)
	}

	if err := removeState(dir, want.VolumeID); err != nil {
		t.Fatalf("removeState: %v", err)
	}
	if states, err := loadStates(dir); err != nil || len(states) != 0 {
		t.Fatalf("loadStates after remove = %v, %v; want empty, nil", states, err)
	}
	// Removing twice is not an error: NodeUnpublishVolume is retried.
	if err := removeState(dir, want.VolumeID); err != nil {
		t.Fatalf("removeState twice: %v", err)
	}
}

func TestLoadStatesIgnoresNonState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	states, err := loadStates(dir)
	if err != nil || len(states) != 0 {
		t.Fatalf("loadStates = %v, %v; want empty, nil", states, err)
	}
}

// One unreadable state must not keep the others from being repaired.
func TestLoadStatesSkipsCorrupt(t *testing.T) {
	dir := t.TempDir()
	want := volumeState{VolumeID: "csi-good", Target: "/target", Argv: []string{"-f"}}
	if err := saveState(dir, want); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"empty.json": "", "junk.json": "{"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	states, err := loadStates(dir)
	if err != nil || len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Fatalf("loadStates = %+v, %v; want [%+v], nil", states, err, want)
	}
}
