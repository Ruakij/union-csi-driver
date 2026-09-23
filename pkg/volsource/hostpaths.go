package volsource

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrHostPathNotAllowed marks a hostPath source the host path policy refuses.
var ErrHostPathNotAllowed = errors.New("not allowed by the driver's host path policy")

// HostPaths limits which host directories hostPath sources may come from. Only
// the Allowed directories are bind-mounted into the container, under Root.
type HostPaths struct {
	Root    string
	Allowed []string
	Denied  []string
}

// Validate requires absolute, clean paths, since they are compared lexically.
func (h HostPaths) Validate() error {
	for _, p := range append(append([]string{h.Root}, h.Allowed...), h.Denied...) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("host path %q must be absolute and clean", p)
		}
	}
	return nil
}

func (h HostPaths) check(host string) error {
	switch {
	case len(h.Allowed) == 0:
		return fmt.Errorf("hostPath sources are disabled: %w", ErrHostPathNotAllowed)
	case !underAny(host, h.Allowed):
		return fmt.Errorf("host path %s is outside the allowed %v: %w", host, h.Allowed, ErrHostPathNotAllowed)
	case underAny(host, h.Denied):
		return fmt.Errorf("host path %s is denied: %w", host, ErrHostPathNotAllowed)
	}
	return nil
}

// reachable reports whether host is allowed, or an ancestor a symlink may pass
// through on its way into an allowed directory.
func (h HostPaths) reachable(host string) bool {
	for _, a := range h.Allowed {
		if under(host, a) || under(a, host) {
			return true
		}
	}
	return false
}

func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if under(p, d) {
			return true
		}
	}
	return false
}

func under(p, dir string) bool {
	return dir == "/" || p == dir || strings.HasPrefix(p, dir+"/")
}
