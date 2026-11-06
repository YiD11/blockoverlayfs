//go:build linux

package snapshot

import (
	"os"

	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay"
	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
)

// NewOverlay builds the inner overlay snapshotter the way containerd's built-in overlayfs plugin does, removing
// asynchronously unless SyncRemove is set. It always labels snapshots with their upper directory, which is how
// the wrapper learns snapshot IDs; the wrapper decides whether clients see that label.
func NewOverlay(root string, opts OverlayOptions) (snapshots.Snapshotter, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := overlayutils.Supported(root); err != nil {
		return nil, err
	}
	oOpts := []overlay.Opt{overlay.WithUpperdirLabel}
	if !opts.SyncRemove {
		oOpts = append(oOpts, overlay.AsynchronousRemove)
	}
	if len(opts.MountOptions) > 0 {
		oOpts = append(oOpts, overlay.WithMountOptions(opts.MountOptions))
	}
	return overlay.NewSnapshotter(root, oOpts...)
}
