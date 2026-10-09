package mergerfs

import (
	"strings"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
)

const (
	scopePrefix = "union-csi-"
	scopeSuffix = ".scope"
)

// scopeConfig fixes the unit name of a volume to "union-csi-<sanitized id>.scope".
// A daemon an earlier driver pod started lives on in its scope and is found again
// by that name, so it must not depend on the version of the driver or the library.
func scopeConfig() scope.Config {
	return scope.Config{
		Prefix:      scopePrefix,
		Description: "union-csi-driver mergerfs mount",
		Unit:        func(id string) string { return scopePrefix + sanitizeUnitName(id) + scopeSuffix },
	}
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
