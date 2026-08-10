package backend

import "fmt"

// DenylistMode controls how a denied pod-supplied option is handled.
type DenylistMode string

const (
	// DenylistRefuse fails NodePublishVolume for a denied option.
	DenylistRefuse DenylistMode = "refuse"
	// DenylistStrip drops a denied option and logs it, rather than failing.
	DenylistStrip DenylistMode = "strip"
)

// PolicyConfig is the admin-facing configuration for one backend process (driver
// flags / chart values). See .docs/plan.md, "Admin knobs".
type PolicyConfig struct {
	Allowlist    []string // empty = any schema-known option not denied
	Denylist     []string
	DenylistMode DenylistMode
	Defaults     map[string]string // pod-overridable
	Forced       map[string]string // applied last, not pod-overridable
}

// Policy resolves pod-supplied options against a backend's OptionSchema and the
// admin's PolicyConfig: start from Defaults, merge pod options (schema-checked,
// denylist, allowlist), apply Forced last.
type Policy struct {
	schema OptionSchema
	config PolicyConfig
}

// NewPolicy builds a Policy for the given schema and admin configuration.
func NewPolicy(schema OptionSchema, config PolicyConfig) *Policy {
	return &Policy{schema: schema, config: config}
}

// Validate checks that Defaults and Forced are schema-known with permitted values.
// Admin defaults and forced options bypass allow/deny (the admin is trusted) but
// are still schema-checked. Call once at startup, so a typo fails the DaemonSet
// immediately rather than every mount.
func (p *Policy) Validate() error {
	for k, v := range p.config.Defaults {
		if err := p.checkSchema(k, v); err != nil {
			return fmt.Errorf("default-options: %w", err)
		}
	}
	for k, v := range p.config.Forced {
		if err := p.checkSchema(k, v); err != nil {
			return fmt.Errorf("forced-options: %w", err)
		}
	}
	return nil
}

// Resolve computes the final option set for one pod's requested options.
func (p *Policy) Resolve(podOptions map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(p.config.Defaults)+len(podOptions))
	for k, v := range p.config.Defaults {
		result[k] = v
	}

	for k, v := range podOptions {
		if err := p.checkSchema(k, v); err != nil {
			return nil, err
		}
		if p.denied(k) {
			if p.config.DenylistMode == DenylistStrip {
				continue
			}
			return nil, fmt.Errorf("option %q is denied by admin policy", k)
		}
		if !p.allowed(k) {
			return nil, fmt.Errorf("option %q is not allowlisted", k)
		}
		result[k] = v
	}

	for k, v := range p.config.Forced {
		result[k] = v
	}

	return result, nil
}

func (p *Policy) checkSchema(key, value string) error {
	spec, ok := p.schema[key]
	if !ok {
		return fmt.Errorf("unknown option %q", key)
	}
	switch spec.Kind {
	case ValueFlag:
		if value != "" && value != "true" {
			return fmt.Errorf("option %q takes no value", key)
		}
	case ValueEnum:
		for _, allowed := range spec.Enum {
			if value == allowed {
				return nil
			}
		}
		return fmt.Errorf("option %q: value %q not in %v", key, value, spec.Enum)
	case ValueBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("option %q: value %q is not a bool", key, value)
		}
	}
	// ValueDuration, ValueInt, ValueSize: TODO parse/range-check once a backend
	// schema actually uses them.
	return nil
}

func (p *Policy) denied(key string) bool {
	for _, d := range p.config.Denylist {
		if d == key {
			return true
		}
	}
	return false
}

func (p *Policy) allowed(key string) bool {
	if len(p.config.Allowlist) == 0 {
		return true
	}
	for _, a := range p.config.Allowlist {
		if a == key {
			return true
		}
	}
	return false
}
