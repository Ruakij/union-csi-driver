//go:build !linux

package backend

import (
	"errors"
	"runtime"
)

func BindMount(string, string, bool) error {
	return errors.New("bind mounts are only supported on linux, not " + runtime.GOOS)
}
