//go:build !linux

package mergerfs

import (
	"context"
	"errors"
	"runtime"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

var errUnsupported = errors.New("mergerfs: mounting is only supported on linux, not " + runtime.GOOS)

func mountUnion(ctx context.Context, spec backend.MountSpec, stateDir string) error {
	return errUnsupported
}

func unmountUnion(ctx context.Context, volumeID, target, stateDir string) error {
	return errUnsupported
}

func reconcile(ctx context.Context, stateDir string) {}
