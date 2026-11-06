//go:build linux

package snapshot

import (
	"context"
	"testing"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/testutil"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/testsuite"
)

// The suite takes overlay-specific expectations from this name. Without redirect_dir overlayfs cannot rename a
// directory that lives in a lower layer, and the suite only skips that step for "overlayfs".
const suiteName = "overlayfs"

// Runs containerd's snapshotter conformance suite against the wrapper; needs root and `-test.root`.
func TestOverlaySuite(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.SnapshotterSuite(t, suiteName, func(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
		sn, layers, err := newRealSnapshotter(ctx, root, nil)
		if err != nil {
			return nil, nil, err
		}
		return sn, func() error { detachAll(ctx, layers); return sn.Close() }, nil
	})
}

// Runs the suite again with a block layer under every active snapshot, so Commit, View and Remove are exercised
// on block layers as well.
func TestOverlaySuiteWithBlockLayers(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.SnapshotterSuite(t, suiteName, func(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
		sn, layers, err := newRealSnapshotter(ctx, root, fixedDecider{size: 64 << 20})
		if err != nil {
			return nil, nil, err
		}
		return blockOnPrepare{sn}, func() error { detachAll(ctx, layers); return sn.Close() }, nil
	})
}

// blockOnPrepare hands out what Mounts returns from Prepare, so the suite writes into block layers. It hides the
// label marking a block layer, because the suite expects a snapshot's labels to be exactly the ones it set.
type blockOnPrepare struct{ *Snapshotter }

func (b blockOnPrepare) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	if _, err := b.Snapshotter.Prepare(ctx, key, parent, opts...); err != nil {
		return nil, err
	}
	return b.Mounts(ctx, key)
}

func (b blockOnPrepare) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	info, err := b.Snapshotter.Stat(ctx, key)
	return hideBlockLabel(info), err
}

func (b blockOnPrepare) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	updated, err := b.Snapshotter.Update(ctx, info, fieldpaths...)
	return hideBlockLabel(updated), err
}

func (b blockOnPrepare) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return b.Snapshotter.Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
		return fn(ctx, hideBlockLabel(info))
	}, filters...)
}

func hideBlockLabel(info snapshots.Info) snapshots.Info {
	delete(info.Labels, WritableMountpointLabel)
	if len(info.Labels) == 0 {
		info.Labels = nil
	}
	return info
}
