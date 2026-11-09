//go:build !linux

package block

import (
	"context"
	"fmt"
	"runtime"
)

var errUnsupported = fmt.Errorf("block writable layers are not supported on %s", runtime.GOOS)

func (s *Store) Attach(context.Context, string, string, int64) error { return errUnsupported }

func (s *Store) Unmount(string) error { return errUnsupported }

func (s *Store) Mounted(string) (bool, error) { return false, errUnsupported }

func (s *Store) Remove(string) error { return errUnsupported }

func Preflight() error { return errUnsupported }
