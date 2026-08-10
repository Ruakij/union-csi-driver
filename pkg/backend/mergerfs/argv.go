package mergerfs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

const (
	modeRW = "RW"
	modeRO = "RO"
	modeNC = "NC"
)

// buildArgv renders the mergerfs command line:
//
//	mergerfs -f -o <opts> <branch1>=RW:<branch2>=RO:... <target>
//
// The branch list is one argv element, passed straight to execve, never a shell.
// Branch paths are driver-computed and contain only pod UIDs and PV names, so the
// colon separator cannot be injected, but the check costs nothing.
func buildArgv(spec backend.MountSpec) ([]string, error) {
	branches, err := buildBranches(spec.Sources)
	if err != nil {
		return nil, err
	}

	// -f keeps mergerfs in the foreground so the PID we hold is the daemon itself,
	// which is what gets moved into a systemd scope.
	argv := []string{"-f", "-o", buildOptions(spec)}
	return append(argv, branches, spec.Target), nil
}

func buildBranches(sources []backend.Source) (string, error) {
	if len(sources) == 0 {
		return "", fmt.Errorf("mergerfs: no source volumes")
	}

	branches := make([]string, 0, len(sources))
	for _, s := range sources {
		switch s.Mode {
		case modeRW, modeRO, modeNC:
		default:
			return "", fmt.Errorf("mergerfs: unknown source mode %q", s.Mode)
		}
		if strings.ContainsAny(s.Path, ":,") {
			return "", fmt.Errorf("mergerfs: branch path %q contains a separator", s.Path)
		}
		branches = append(branches, s.Path+"="+s.Mode)
	}
	return strings.Join(branches, ":"), nil
}

// buildOptions renders the -o value. allow_other is always set: the consumer's uid
// is not root, and without it the kernel refuses everyone but the mounting user.
func buildOptions(spec backend.MountSpec) string {
	opts := []string{"allow_other"}
	if spec.ReadOnly {
		opts = append(opts, "ro")
	}

	keys := make([]string, 0, len(spec.Options))
	for k := range spec.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		opts = append(opts, k+"="+spec.Options[k])
	}
	return strings.Join(opts, ",")
}
