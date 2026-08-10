package driver

import (
	"context"
	"testing"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

// fakeBackend is a minimal backend.Backend for exercising parseAttributes
// without depending on a real overlay/mergerfs implementation.
type fakeBackend struct {
	modes       []string
	defaultMode string
	maxWritable int
}

func (b *fakeBackend) Name() string                                   { return "fake" }
func (b *fakeBackend) Schema() backend.OptionSchema                   { return backend.OptionSchema{} }
func (b *fakeBackend) SourceModes() ([]string, string)                { return b.modes, b.defaultMode }
func (b *fakeBackend) MaxWritable() int                               { return b.maxWritable }
func (b *fakeBackend) DefaultOptions() map[string]string              { return nil }
func (b *fakeBackend) DefaultDenylist() []string                      { return nil }
func (b *fakeBackend) Mount(context.Context, backend.MountSpec) error { return nil }
func (b *fakeBackend) Unmount(context.Context, string) error          { return nil }

func newTestDriver(maxSourceVolumes int, be backend.Backend) *Driver {
	return &Driver{config: Config{MaxSourceVolumes: maxSourceVolumes, Backend: be}}
}

func overlayLikeBackend() backend.Backend {
	return &fakeBackend{modes: []string{"RW", "RO"}, defaultMode: "RW", maxWritable: 1}
}

func mergerfsLikeBackend() backend.Backend {
	return &fakeBackend{modes: []string{"RW", "RO", "NC"}, defaultMode: "RW", maxWritable: 0}
}

func TestParseSourceVolumes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		backend backend.Backend
		max     int
		want    []SourceVolume
		wantErr bool
	}{
		{
			name:    "bare name defaults to RW",
			raw:     "base-data",
			backend: mergerfsLikeBackend(),
			max:     32,
			want:    []SourceVolume{{Name: "base-data", Mode: "RW"}},
		},
		{
			name:    "explicit modes, leftmost first",
			raw:     "config-overrides=RO,base-data",
			backend: mergerfsLikeBackend(),
			max:     32,
			want: []SourceVolume{
				{Name: "config-overrides", Mode: "RO"},
				{Name: "base-data", Mode: "RW"},
			},
		},
		{
			name:    "mergerfs allows NC",
			raw:     "a=NC",
			backend: mergerfsLikeBackend(),
			max:     32,
			want:    []SourceVolume{{Name: "a", Mode: "NC"}},
		},
		{
			name:    "mergerfs allows multiple RW",
			raw:     "a=RW,b=RW",
			backend: mergerfsLikeBackend(),
			max:     32,
			want:    []SourceVolume{{Name: "a", Mode: "RW"}, {Name: "b", Mode: "RW"}},
		},
		{
			name:    "overlay rejects a second RW (single upperdir)",
			raw:     "a=RW,b=RW",
			backend: overlayLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "overlay rejects a second bare entry too",
			raw:     "a,b",
			backend: overlayLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "overlay rejects NC (not in its vocabulary)",
			raw:     "a=NC",
			backend: overlayLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "empty is rejected",
			raw:     "",
			backend: mergerfsLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "rejects colon injection in name",
			raw:     "a:b",
			backend: mergerfsLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "rejects comma injection in name",
			raw:     `a\,b`,
			backend: mergerfsLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "rejects backslash in name",
			raw:     `a\b`,
			backend: mergerfsLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "rejects path traversal in name",
			raw:     "../x",
			backend: mergerfsLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "rejects duplicate names",
			raw:     "a,a",
			backend: mergerfsLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "rejects unknown mode suffix",
			raw:     "a=WAT",
			backend: mergerfsLikeBackend(),
			max:     32,
			wantErr: true,
		},
		{
			name:    "rejects over-cap lists",
			raw:     "a,b,c",
			backend: mergerfsLikeBackend(),
			max:     2,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDriver(tt.max, tt.backend)
			got, err := d.parseSourceVolumes(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSourceVolumes(%q) = %v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSourceVolumes(%q) unexpected error: %v", tt.raw, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseSourceVolumes(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("parseSourceVolumes(%q)[%d] = %v, want %v", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestParseAttributesRejectsUnknownKey(t *testing.T) {
	d := newTestDriver(32, mergerfsLikeBackend())
	_, err := d.parseAttributes(map[string]string{
		"sourceVolumes": "base-data",
		"lowerdir":      "/etc",
	})
	if err == nil {
		t.Fatal("parseAttributes() = nil, want error for unknown key")
	}
}

func TestParseOptions(t *testing.T) {
	got, err := parseOptions("func.getattr=newest,category.search=ff")
	if err != nil {
		t.Fatalf("parseOptions() unexpected error: %v", err)
	}
	want := map[string]string{"func.getattr": "newest", "category.search": "ff"}
	if len(got) != len(want) {
		t.Fatalf("parseOptions() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseOptions()[%q] = %q, want %q", k, got[k], v)
		}
	}

	if _, err := parseOptions("nokeyvalue"); err == nil {
		t.Fatal("parseOptions(\"nokeyvalue\") = nil, want error")
	}
}
