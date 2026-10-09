//go:build !linux

package mergerfs

import (
	"context"
	"errors"
	"runtime"

	"github.com/Ruakij/union-csi-driver/pkg/backend"
)

var errUnsupported = errors.New("mergerfs: mounting is only supported on linux, not " + runtime.GOOS)

func (b *mergerfsBackend) mountUnion(context.Context, backend.MountSpec) error {
	return errUnsupported
}

func (b *mergerfsBackend) shareUnion(context.Context, string, string, string, bool) error {
	return errUnsupported
}

func (b *mergerfsBackend) unmountUnion(context.Context, string, string) error {
	return errUnsupported
}

func (b *mergerfsBackend) ids() ([]string, error) { return nil, nil }

func (b *mergerfsBackend) reconcile(context.Context, string, bool) {}

func checkSandbox() error { return nil }
