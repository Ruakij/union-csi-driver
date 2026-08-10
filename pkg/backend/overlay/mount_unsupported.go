//go:build !linux

package overlay

import (
	"errors"
	"runtime"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

var errUnsupported = errors.New("overlay: mounting is only supported on linux, not " + runtime.GOOS)

func mountUnion(spec backend.MountSpec, schema backend.OptionSchema) error {
	return errUnsupported
}

func unmountUnion(target string) error {
	return errUnsupported
}
