//go:build !linux

package overlay

import (
	"errors"
	"runtime"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

var errUnsupported = errors.New("overlay: mounting is only supported on linux, not " + runtime.GOOS)

func mountUnion(backend.MountSpec, backend.OptionSchema) error {
	return errUnsupported
}

func unmountUnion(string) error {
	return errUnsupported
}
