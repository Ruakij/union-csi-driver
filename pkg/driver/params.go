package driver

import (
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// volumeNameRE matches a DNS-1123 label, the same validation the API server
// applies to pod.spec.volumes[].name. This is what makes colon/comma/backslash
// injection into a mount option string or an argv element structurally
// impossible.
var volumeNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	attrSourceVolumes  = "sourceVolumes"
	attrOptions        = "options"
	attrPodName        = "csi.storage.k8s.io/pod.name"
	attrPodNamespace   = "csi.storage.k8s.io/pod.namespace"
	attrPodUID         = "csi.storage.k8s.io/pod.uid"
	attrServiceAccount = "csi.storage.k8s.io/serviceAccount.name"
)

var knownAttributes = map[string]bool{
	attrSourceVolumes:  true,
	attrOptions:        true,
	attrPodName:        true,
	attrPodNamespace:   true,
	attrPodUID:         true,
	attrServiceAccount: true,
	attrEphemeral:      true,
}

// SourceVolume is one parsed sourceVolumes entry: a pod volume name and its
// resolved write mode (defaulted per the backend when no suffix was given).
type SourceVolume struct {
	Name string
	Mode string
}

// ParsedAttributes is the validated, structured form of a NodePublishVolume
// request's VolumeContext.
type ParsedAttributes struct {
	SourceVolumes []SourceVolume
	Options       map[string]string
	PodNamespace  string
	PodName       string
	PodUID        string
}

// parseAttributes validates req.VolumeContext against the fixed grammar in
// .docs/plan.md section 1: reject unknown keys, validate sourceVolumes names and
// modes against the configured backend, cap the count via --max-source-volumes.
func (d *Driver) parseAttributes(attrs map[string]string) (*ParsedAttributes, error) {
	for k := range attrs {
		if !knownAttributes[k] {
			return nil, status.Errorf(codes.InvalidArgument, "unknown volumeAttributes key %q", k)
		}
	}

	sourceVolumes, err := d.parseSourceVolumes(attrs[attrSourceVolumes])
	if err != nil {
		return nil, err
	}

	options, err := parseOptions(attrs[attrOptions])
	if err != nil {
		return nil, err
	}

	return &ParsedAttributes{
		SourceVolumes: sourceVolumes,
		Options:       options,
		PodNamespace:  attrs[attrPodNamespace],
		PodName:       attrs[attrPodName],
		PodUID:        attrs[attrPodUID],
	}, nil
}

func (d *Driver) parseSourceVolumes(raw string) ([]SourceVolume, error) {
	if raw == "" {
		return nil, status.Error(codes.InvalidArgument, "sourceVolumes is required")
	}

	entries := strings.Split(raw, ",")
	if len(entries) > d.config.MaxSourceVolumes {
		return nil, status.Errorf(codes.InvalidArgument, "sourceVolumes has %d entries, exceeds --max-source-volumes=%d", len(entries), d.config.MaxSourceVolumes)
	}

	validModes, defaultMode := d.config.Backend.SourceModes()
	maxWritable := d.config.Backend.MaxWritable()

	seen := make(map[string]bool, len(entries))
	sources := make([]SourceVolume, 0, len(entries))
	writable := 0

	for _, entry := range entries {
		name, mode := entry, defaultMode
		if idx := strings.IndexByte(entry, '='); idx >= 0 {
			name, mode = entry[:idx], entry[idx+1:]
		}

		if !volumeNameRE.MatchString(name) {
			return nil, status.Errorf(codes.InvalidArgument, "sourceVolumes: invalid volume name %q", name)
		}
		if seen[name] {
			return nil, status.Errorf(codes.InvalidArgument, "sourceVolumes: duplicate volume name %q", name)
		}
		seen[name] = true

		if !containsString(validModes, mode) {
			return nil, status.Errorf(codes.InvalidArgument, "sourceVolumes: %q has unknown mode %q, must be one of %v", name, mode, validModes)
		}

		if mode == "RW" {
			writable++
			if maxWritable > 0 && writable > maxWritable {
				return nil, status.Errorf(codes.InvalidArgument, "sourceVolumes: backend %q allows at most %d RW entries", d.config.Backend.Name(), maxWritable)
			}
		}

		sources = append(sources, SourceVolume{Name: name, Mode: mode})
	}

	return sources, nil
}

func parseOptions(raw string) (map[string]string, error) {
	options := map[string]string{}
	if raw == "" {
		return options, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			return nil, status.Errorf(codes.InvalidArgument, "options: invalid entry %q, expected key=value", pair)
		}
		options[kv[0]] = kv[1]
	}
	return options, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
