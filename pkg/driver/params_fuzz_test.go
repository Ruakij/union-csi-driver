package driver

import (
	"strings"
	"testing"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

// fuzzSchema has one option per value kind, so every checkSchema branch is reachable.
var fuzzSchema = backend.OptionSchema{
	"flag":     {Kind: backend.ValueFlag},
	"enum":     {Kind: backend.ValueEnum, Enum: []string{"a", "b"}},
	"bool":     {Kind: backend.ValueBool},
	"duration": {Kind: backend.ValueDuration},
	"int":      {Kind: backend.ValueInt, MinInt: -16, MaxInt: 1024},
	"size":     {Kind: backend.ValueSize},
}

// FuzzParseAttributes drives the pod-controlled volumeAttributes through parsing
// and option policy, and checks that nothing accepted can break out of the
// mount option string or mergerfs branch list it ends up in.
func FuzzParseAttributes(f *testing.F) {
	f.Add("data", "")
	f.Add("a=RW,b=RO,c=NC", "flag=true,enum=a,bool=false,duration=5,int=-3,size=4G")
	f.Add("a=,=RW,a", "int=1025,size=4G,x,=1,a=b=c")

	policy := backend.NewPolicy(fuzzSchema, backend.PolicyConfig{})
	backends := []backend.Backend{overlayLikeBackend(), mergerfsLikeBackend()}

	f.Fuzz(func(t *testing.T, sourceVolumes, options string) {
		for _, be := range backends {
			attrs, err := newTestDriver(8, be).parseAttributes(map[string]string{
				attrSourceVolumes: sourceVolumes,
				attrOptions:       options,
			})
			if err != nil {
				continue
			}
			checkSources(t, be, attrs.SourceVolumes)

			resolved, err := policy.Resolve(attrs.Options)
			if err != nil {
				continue
			}
			for k, v := range resolved {
				if _, ok := fuzzSchema[k]; !ok {
					t.Fatalf("accepted unknown option %q", k)
				}
				if strings.ContainsAny(v, ",:=\\ \t\n\x00") {
					t.Fatalf("accepted option %s=%q with a separator", k, v)
				}
			}
		}
	})
}

func checkSources(t *testing.T, be backend.Backend, sources []SourceVolume) {
	t.Helper()
	modes, _ := be.SourceModes()
	if len(sources) == 0 || len(sources) > 8 {
		t.Fatalf("accepted %d source volumes", len(sources))
	}
	seen := map[string]bool{}
	writable := 0
	for _, s := range sources {
		if !volumeNameRE.MatchString(s.Name) || seen[s.Name] {
			t.Fatalf("accepted invalid or duplicate volume name %q", s.Name)
		}
		seen[s.Name] = true
		if !containsString(modes, s.Mode) {
			t.Fatalf("accepted mode %q for %s", s.Mode, be.Name())
		}
		if s.Mode == "RW" {
			writable++
		}
	}
	if max := be.MaxWritable(); max > 0 && writable > max {
		t.Fatalf("accepted %d RW sources, %s allows %d", writable, be.Name(), max)
	}
}
