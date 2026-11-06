//go:build !linux

package snapshot

import (
	"fmt"
	"runtime"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/snapshots"
)

func NewOverlay(string, OverlayOptions) (snapshots.Snapshotter, error) {
	return nil, fmt.Errorf("overlay snapshotter on %s: %w", runtime.GOOS, errdefs.ErrNotImplemented)
}
