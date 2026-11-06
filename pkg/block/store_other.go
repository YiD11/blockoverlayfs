//go:build !linux

package block

import (
	"context"
	"fmt"
	"runtime"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/snapshots"
)

var errUnsupported = fmt.Errorf("block writable layers on %s: %w", runtime.GOOS, errdefs.ErrNotImplemented)

func (s *Store) Create(context.Context, string, State, string) error { return errUnsupported }

func (s *Store) EnsureMounted(context.Context, string, string) error { return errUnsupported }

func (s *Store) Usage(string) (snapshots.Usage, error) { return snapshots.Usage{}, errUnsupported }

func (s *Store) CopyUpper(context.Context, string) (string, error) { return "", errUnsupported }

func (s *Store) Replaceable(string) (bool, string, error) { return false, "", errUnsupported }

func Preflight(context.Context, string, string) error { return errUnsupported }

// Nothing is ever mounted where block layers cannot be created.
func unmount(string) error { return nil }
