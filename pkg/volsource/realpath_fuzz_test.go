//go:build linux

package volsource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// FuzzRealPath resolves a pod-supplied hostPath through symlinks whose targets
// are fuzzed, one per line. Whatever RealPath accepts must be symlink-free, inside
// the root, allowed and not denied, and the same directory the kernel reaches when
// resolving the path with the root as "/".
func FuzzRealPath(f *testing.F) {
	f.Add("/allowed/l0", "/allowed/x")
	f.Add("/allowed/x/l1", "\n../../allowed/deny")
	f.Add("/allowed/l0/x", "../other/l2\n\n/allowed/x")
	f.Add("/l3/x", "\n\n\n/allowed/../allowed")
	f.Add("/allowed/l0", "l0")

	f.Fuzz(func(t *testing.T, host, targets string) {
		root := t.TempDir()
		for _, d := range []string{"allowed/x", "allowed/deny", "other"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		links := []string{"allowed/l0", "allowed/x/l1", "other/l2", "l3"}
		for i, target := range strings.SplitN(targets, "\n", len(links)) {
			if target == "" {
				continue
			}
			if err := os.Symlink(target, filepath.Join(root, links[i])); err != nil {
				return // e.g. a NUL in the target
			}
		}

		hp := HostPaths{Root: root, Allowed: []string{"/allowed"}, Denied: []string{"/allowed/deny"}}
		sp, err := (&Resolver{hostPaths: hp}).hostPath("v", host)
		if err != nil {
			return
		}
		got, err := sp.RealPath()
		if err != nil {
			return
		}

		rel, err := filepath.Rel(root, got)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			t.Fatalf("RealPath(%s) = %s, outside the root", host, got)
		}
		if err := hp.check(filepath.Join("/", rel)); err != nil {
			t.Fatalf("RealPath(%s) = %s: %v", host, got, err)
		}
		for p := got; p != root; p = filepath.Dir(p) {
			if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("RealPath(%s) = %s, which is no resolved path: %s is %v, %v", host, got, p, fi.Mode(), err)
			}
		}

		rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(rootFD)
		fd, err := unix.Openat2(rootFD, strings.TrimPrefix(filepath.Join("/", host), "/"), &unix.OpenHow{
			Flags:   unix.O_PATH,
			Resolve: unix.RESOLVE_IN_ROOT,
		})
		if errors.Is(err, unix.ENOSYS) {
			t.Skip("no openat2")
		}
		if err != nil {
			t.Fatalf("RealPath(%s) = %s, but the kernel cannot resolve it: %v", host, got, err)
		}
		defer unix.Close(fd)
		var want, have unix.Stat_t
		if err := unix.Fstat(fd, &want); err != nil {
			t.Fatal(err)
		}
		if err := unix.Stat(got, &have); err != nil {
			t.Fatal(err)
		}
		if want.Ino != have.Ino || want.Dev != have.Dev {
			t.Fatalf("RealPath(%s) = %s, but the kernel resolves it elsewhere", host, got)
		}
	})
}
