package overlay

import (
	"testing"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

func TestPlanLayout(t *testing.T) {
	tests := []struct {
		name       string
		spec       backend.MountSpec
		wantLowers []string
		wantUpper  string
		wantWork   string
		wantRO     bool
		wantSingle string
		wantErr    bool
	}{
		{
			name: "RW first, then lowers in declared order",
			spec: backend.MountSpec{Sources: []backend.Source{
				{Path: "/vol/a", Mode: modeRW},
				{Path: "/vol/b", Mode: modeRO},
				{Path: "/vol/c", Mode: modeRO},
			}},
			wantLowers: []string{"/vol/b", "/vol/c"},
			wantUpper:  "/vol/a/.union-csi/upper",
			wantWork:   "/vol/a/.union-csi/work",
		},
		{
			name: "all RO mounts read-only with no upper",
			spec: backend.MountSpec{Sources: []backend.Source{
				{Path: "/vol/a", Mode: modeRO},
				{Path: "/vol/b", Mode: modeRO},
			}},
			wantLowers: []string{"/vol/a", "/vol/b"},
			wantRO:     true,
		},
		{
			name: "readonly request keeps the upper but mounts read-only",
			spec: backend.MountSpec{ReadOnly: true, Sources: []backend.Source{
				{Path: "/vol/a", Mode: modeRW},
				{Path: "/vol/b", Mode: modeRO},
			}},
			wantLowers: []string{"/vol/b"},
			wantUpper:  "/vol/a/.union-csi/upper",
			wantWork:   "/vol/a/.union-csi/work",
			wantRO:     true,
		},
		{
			name:       "single RO source becomes a read-only bind",
			spec:       backend.MountSpec{Sources: []backend.Source{{Path: "/vol/a", Mode: modeRO}}},
			wantLowers: []string{"/vol/a"},
			wantRO:     true,
			wantSingle: "/vol/a",
		},
		{
			name:       "single RW source binds its upper directory",
			spec:       backend.MountSpec{Sources: []backend.Source{{Path: "/vol/a", Mode: modeRW}}},
			wantUpper:  "/vol/a/.union-csi/upper",
			wantWork:   "/vol/a/.union-csi/work",
			wantSingle: "/vol/a/.union-csi/upper",
		},
		{
			name: "RW after a lower is rejected: overlay always stacks it topmost",
			spec: backend.MountSpec{Sources: []backend.Source{
				{Path: "/vol/b", Mode: modeRO},
				{Path: "/vol/a", Mode: modeRW},
			}},
			wantErr: true,
		},
		{
			name: "two RW sources are rejected",
			spec: backend.MountSpec{Sources: []backend.Source{
				{Path: "/vol/a", Mode: modeRW},
				{Path: "/vol/b", Mode: modeRW},
			}},
			wantErr: true,
		},
		{
			name:    "unknown mode is rejected",
			spec:    backend.MountSpec{Sources: []backend.Source{{Path: "/vol/a", Mode: "NC"}}},
			wantErr: true,
		},
		{
			name:    "no sources is rejected",
			spec:    backend.MountSpec{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := planLayout(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("planLayout() = %+v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("planLayout() unexpected error: %v", err)
			}
			if len(got.lowers) != len(tt.wantLowers) {
				t.Fatalf("lowers = %v, want %v", got.lowers, tt.wantLowers)
			}
			for i := range tt.wantLowers {
				if got.lowers[i] != tt.wantLowers[i] {
					t.Errorf("lowers[%d] = %q, want %q", i, got.lowers[i], tt.wantLowers[i])
				}
			}
			if got.upper != tt.wantUpper {
				t.Errorf("upper = %q, want %q", got.upper, tt.wantUpper)
			}
			if got.work != tt.wantWork {
				t.Errorf("work = %q, want %q", got.work, tt.wantWork)
			}
			if got.readOnly != tt.wantRO {
				t.Errorf("readOnly = %v, want %v", got.readOnly, tt.wantRO)
			}
			if got.single() != tt.wantSingle {
				t.Errorf("single() = %q, want %q", got.single(), tt.wantSingle)
			}
		})
	}
}

func TestSortedOptionsIsStable(t *testing.T) {
	l := &layout{options: map[string]string{"xino": "off", "index": "off", "redirect_dir": "nofollow"}}
	want := []string{"index", "redirect_dir", "xino"}
	got := l.sortedOptions()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedOptions() = %v, want %v", got, want)
		}
	}
}
