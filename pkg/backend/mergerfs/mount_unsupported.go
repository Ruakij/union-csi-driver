//go:build !linux

package mergerfs

import (
	"context"
	"errors"
	"runtime"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

var errUnsupported = errors.New("mergerfs: mounting is only supported on linux, not " + runtime.GOOS)

func mountUnion(context.Context, backend.MountSpec, string) error {
	return errUnsupported
}

func unmountUnion(context.Context, string, string, string) error {
	return errUnsupported
}

func reconcile(context.Context, string) {}
