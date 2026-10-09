package mergerfs

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
)

func TestStaleCounterCountsConsecutivePasses(t *testing.T) {
	c := staleCounter{}
	for want := 1; want <= staleThreshold+1; want++ {
		if got := c.hit("vol", "/branch/0", 10); got != want {
			t.Fatalf("hit %d = %d", want, got)
		}
	}
	c.clear("vol", "/branch/0")
	if got := c.hit("vol", "/branch/0", 10); got != 1 {
		t.Errorf("hit after clear = %d, want 1", got)
	}
}

func TestStaleCounterRestartsForANewDaemon(t *testing.T) {
	c := staleCounter{}
	c.hit("vol", "/branch/0", 10)
	c.hit("vol", "/branch/0", 10)
	if got := c.hit("vol", "/branch/0", 11); got != 1 {
		t.Errorf("hit under a new pid = %d, want 1", got)
	}
}

func TestStaleCounterKeepsVolumesAndBranchesApart(t *testing.T) {
	c := staleCounter{}
	c.hit("a", "/branch/0", 10)
	c.hit("a", "/branch/0", 10)
	if got := c.hit("a", "/branch/1", 10); got != 1 {
		t.Errorf("other branch = %d, want 1", got)
	}
	if got := c.hit("b", "/branch/0", 10); got != 1 {
		t.Errorf("other volume = %d, want 1", got)
	}
}

func TestStaleCounterRetain(t *testing.T) {
	c := staleCounter{}
	c.hit("a", "/branch/0", 10)
	c.hit("b", "/branch/0", 10)
	c.retain([]string{"b"})
	if len(c) != 1 {
		t.Fatalf("counter = %v, want only the run of b", c)
	}
	if got := c.hit("b", "/branch/0", 10); got != 2 {
		t.Errorf("run of b = %d, want 2", got)
	}
}

func TestDeadInDaemon(t *testing.T) {
	tests := []struct {
		name  string
		state fusemount.State
		err   error
		want  bool
	}{
		{"live", fusemount.Live, nil, false},
		{"plain directory", fusemount.Other, nil, false},
		{"missing", fusemount.Gone, syscall.ENOENT, false},
		{"dead", fusemount.Dead, syscall.ENOTCONN, true},
		{"hung", fusemount.Other, fusemount.ErrHung, true},
		{"other error", fusemount.Other, errors.New("EIO"), false},
	}
	for _, tt := range tests {
		if got := deadInDaemon(tt.state, tt.err); got != tt.want {
			t.Errorf("%s: deadInDaemon = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestMountPoints(t *testing.T) {
	const mountinfo = `1032 1031 0:67 / / ro,nosuid,nodev,relatime - tmpfs tmpfs rw,mode=755
1033 1032 0:68 / /branch/0 ro,nosuid,nodev,relatime shared:5 - fuse.mergerfs src rw,user_id=0
1034 1032 259:1 /data/x /branch/1 rw,nosuid,nodev,relatime - ext4 /dev/vda1 rw
1035 1032 0:69 / /union rw,nosuid,nodev,relatime shared:6 - fuse.mergerfs mergerfs rw
`
	got, err := mountPoints(strings.NewReader(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/branch/0", "/branch/1", "/union"} {
		if !got[p] {
			t.Errorf("mount point %s not found in %v", p, got)
		}
	}
	if got["/branch/2"] || got["/data/x"] {
		t.Errorf("mount points = %v, want neither /branch/2 nor the root of a mount", got)
	}
}
