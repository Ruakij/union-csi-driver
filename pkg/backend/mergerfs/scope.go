package mergerfs

import "strings"

// scopeUnitName derives the systemd scope name for a volume. It is deterministic
// so cleanup after a driver restart needs no bookkeeping: the volume ID from
// NodeUnpublishVolume is enough to name the unit again.
func scopeUnitName(volumeID string) string {
	return "union-csi-" + sanitizeUnitName(volumeID) + ".scope"
}

// sanitizeUnitName keeps only characters valid in a systemd unit name. CSI volume
// IDs are driver-generated and already tame, but the unit name reaches a host
// service manager, so it is not a place to assume.
func sanitizeUnitName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
