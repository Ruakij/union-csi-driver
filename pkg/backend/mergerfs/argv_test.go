package mergerfs

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func TestBuildArgv(t *testing.T) {
	spec := backend.MountSpec{
		Target: "/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/merged/mount",
		Sources: []backend.Source{
			{Path: "/vol/a", Mode: modeRW},
			{Path: "/vol/b", Mode: modeRO},
			{Path: "/vol/c", Mode: modeNC},
		},
		Options: map[string]string{"func.getattr": "newest", "cache.entry": "1"},
	}

	got, err := buildArgv(spec)
	if err != nil {
		t.Fatalf("buildArgv() unexpected error: %v", err)
	}
	want := []string{
		"-f",
		"-o", "allow_other,cache.entry=1,func.getattr=newest",
		"/vol/a=RW:/vol/b=RO:/vol/c=NC",
		spec.Target,
	}
	if len(got) != len(want) {
		t.Fatalf("buildArgv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("buildArgv()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildArgvReadOnly(t *testing.T) {
	spec := backend.MountSpec{
		Target:   "/target",
		ReadOnly: true,
		Sources:  []backend.Source{{Path: "/vol/a", Mode: modeRO}},
	}
	got, err := buildArgv(spec)
	if err != nil {
		t.Fatalf("buildArgv() unexpected error: %v", err)
	}
	if !strings.Contains(got[2], ",ro") {
		t.Fatalf("buildArgv() options = %q, want a ro option", got[2])
	}
}

func TestBuildBranchesRejects(t *testing.T) {
	tests := []struct {
		name    string
		sources []backend.Source
	}{
		{name: "no sources"},
		{name: "unknown mode", sources: []backend.Source{{Path: "/vol/a", Mode: "WAT"}}},
		{name: "colon in path", sources: []backend.Source{{Path: "/vol/a:b", Mode: modeRW}}},
		{name: "comma in path", sources: []backend.Source{{Path: "/vol/a,b", Mode: modeRW}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := buildBranches(tt.sources); err == nil {
				t.Fatalf("buildBranches() = %q, want error", got)
			}
		})
	}
}

func TestSandboxed(t *testing.T) {
	spec := backend.MountSpec{
		Target: "/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/merged/mount",
		Sources: []backend.Source{
			{Path: "/vol/a", Mode: modeRW},
			{Path: "/vol/b", Mode: modeRO},
			{Path: "/vol/c", Mode: modeNC},
		},
	}

	got, binds := sandboxed(spec)
	if got.Target != sandboxTarget {
		t.Errorf("target = %q, want %q", got.Target, sandboxTarget)
	}
	wantSources := []backend.Source{{Path: "/branch/0", Mode: modeRW}, {Path: "/branch/1", Mode: modeRO}, {Path: "/branch/2", Mode: modeNC}}
	if !reflect.DeepEqual(got.Sources, wantSources) {
		t.Errorf("sources = %v, want %v", got.Sources, wantSources)
	}
	wantBinds := []sandbox.Bind{
		{Host: "/vol/a", Sandbox: "/branch/0"},
		{Host: "/vol/b", Sandbox: "/branch/1", ReadOnly: true},
		{Host: "/vol/c", Sandbox: "/branch/2"},
	}
	if !reflect.DeepEqual(binds, wantBinds) {
		t.Errorf("binds = %v, want %v", binds, wantBinds)
	}
	if spec.Sources[0].Path != "/vol/a" {
		t.Error("sandboxed modified the caller's sources")
	}

	spec.ReadOnly = true
	if _, binds := sandboxed(spec); !binds[0].ReadOnly || !binds[2].ReadOnly {
		t.Errorf("read-only volume has writable binds: %v", binds)
	}
}
