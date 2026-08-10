package backend

import (
	"testing"
)

func testSchema() OptionSchema {
	return OptionSchema{
		"cache.readdir":    {Kind: ValueBool},
		"func.getattr":     {Kind: ValueEnum, Enum: []string{"ff", "newest"}},
		"dropcacheonclose": {Kind: ValueFlag},
	}
}

func TestPolicyResolve(t *testing.T) {
	tests := []struct {
		name    string
		config  PolicyConfig
		pod     map[string]string
		want    map[string]string
		wantErr bool
	}{
		{
			name:   "defaults only, no pod options",
			config: PolicyConfig{Defaults: map[string]string{"func.getattr": "newest"}},
			pod:    map[string]string{},
			want:   map[string]string{"func.getattr": "newest"},
		},
		{
			name:   "pod option overrides default",
			config: PolicyConfig{Defaults: map[string]string{"func.getattr": "newest"}},
			pod:    map[string]string{"func.getattr": "ff"},
			want:   map[string]string{"func.getattr": "ff"},
		},
		{
			name:   "forced overrides pod",
			config: PolicyConfig{Forced: map[string]string{"func.getattr": "newest"}},
			pod:    map[string]string{"func.getattr": "ff"},
			want:   map[string]string{"func.getattr": "newest"},
		},
		{
			name:    "unknown key rejected",
			config:  PolicyConfig{},
			pod:     map[string]string{"nonsense": "1"},
			wantErr: true,
		},
		{
			name:    "bad enum value rejected",
			config:  PolicyConfig{},
			pod:     map[string]string{"func.getattr": "bogus"},
			wantErr: true,
		},
		{
			name:    "denylist refuse mode fails the request",
			config:  PolicyConfig{Denylist: []string{"dropcacheonclose"}, DenylistMode: DenylistRefuse},
			pod:     map[string]string{"dropcacheonclose": "true"},
			wantErr: true,
		},
		{
			name:   "denylist strip mode drops the option",
			config: PolicyConfig{Denylist: []string{"dropcacheonclose"}, DenylistMode: DenylistStrip},
			pod:    map[string]string{"dropcacheonclose": "true"},
			want:   map[string]string{},
		},
		{
			name:    "allowlist set: option not on it is rejected",
			config:  PolicyConfig{Allowlist: []string{"func.getattr"}},
			pod:     map[string]string{"cache.readdir": "true"},
			wantErr: true,
		},
		{
			name:   "allowlist set: allowed option passes",
			config: PolicyConfig{Allowlist: []string{"func.getattr"}},
			pod:    map[string]string{"func.getattr": "ff"},
			want:   map[string]string{"func.getattr": "ff"},
		},
		{
			name:   "allowlist unset: any schema-known option not denied passes",
			config: PolicyConfig{},
			pod:    map[string]string{"cache.readdir": "true"},
			want:   map[string]string{"cache.readdir": "true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPolicy(testSchema(), tt.config)
			got, err := p.Resolve(tt.pod)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Resolve() = %v, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Resolve() = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("Resolve()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestPolicyValidate(t *testing.T) {
	t.Run("schema-known defaults and forced pass", func(t *testing.T) {
		p := NewPolicy(testSchema(), PolicyConfig{
			Defaults: map[string]string{"func.getattr": "newest"},
			Forced:   map[string]string{"cache.readdir": "true"},
		})
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})

	t.Run("unknown key in forced options fails startup", func(t *testing.T) {
		p := NewPolicy(testSchema(), PolicyConfig{
			Forced: map[string]string{"category.create": "mfs"},
		})
		if err := p.Validate(); err == nil {
			t.Fatal("Validate() = nil, want error for unknown forced option")
		}
	})

	t.Run("bad value in defaults fails startup", func(t *testing.T) {
		p := NewPolicy(testSchema(), PolicyConfig{
			Defaults: map[string]string{"func.getattr": "bogus"},
		})
		if err := p.Validate(); err == nil {
			t.Fatal("Validate() = nil, want error for bad default value")
		}
	})

	t.Run("admin defaults and forced bypass allow/deny", func(t *testing.T) {
		p := NewPolicy(testSchema(), PolicyConfig{
			Denylist:     []string{"func.getattr"},
			DenylistMode: DenylistRefuse,
			Forced:       map[string]string{"func.getattr": "newest"},
		})
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
		got, err := p.Resolve(nil)
		if err != nil {
			t.Fatalf("Resolve() unexpected error: %v", err)
		}
		if got["func.getattr"] != "newest" {
			t.Fatalf("Resolve() = %v, want forced func.getattr=newest despite denylist", got)
		}
	})
}
