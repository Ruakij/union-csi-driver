package mergerfs

import "testing"

func TestScopeUnitName(t *testing.T) {
	tests := []struct {
		volumeID string
		want     string
	}{
		{"csi-4f2a9b", "union-csi-csi-4f2a9b.scope"},
		{"vol.with_chars-1", "union-csi-vol.with_chars-1.scope"},
		{"vol/../../etc", "union-csi-vol-..-..-etc.scope"},
		{"vol id@host", "union-csi-vol-id-host.scope"},
	}
	for _, tt := range tests {
		if got := scopeUnitName(tt.volumeID); got != tt.want {
			t.Errorf("scopeUnitName(%q) = %q, want %q", tt.volumeID, got, tt.want)
		}
	}
}
