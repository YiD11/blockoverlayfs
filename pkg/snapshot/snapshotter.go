package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/kmutex"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/log"

	"github.com/YiD11/blockoverlayfs/pkg/block"
)

const (
	// upperdirLabel is added by the inner overlay, which is always created with overlay.WithUpperdirLabel so
	// the wrapper can learn snapshot IDs. Clients see it only when Options.UpperdirLabel is set, as with the
	// built-in overlayfs plugin's upperdir_label.
	upperdirLabel = "containerd.io/snapshot/overlay.upperdir"
	// WritableMountpointLabel names where an active snapshot's block layer is mounted. It has the name the
	// containerd branch with CRI support for block layers reads, and it locates a layer for troubleshooting.
	WritableMountpointLabel = "containerd.io/snapshot/writable-mountpoint"
)

// Layers keeps the block writable layers; *block.Store implements it.
type Layers interface {
	Layer(id string) block.Layer
	Load(id string) (block.State, bool, error)
	RecordDirectory(id string, st block.State) error
	Create(ctx context.Context, id string, st block.State, ownerFrom string) error
	EnsureMounted(ctx context.Context, id, ownerFrom string) error
	Replaceable(upper string) (bool, string, error)
	Usage(id string) (snapshots.Usage, error)
	CopyUpper(ctx context.Context, id string) (string, error)
	Detach(ctx context.Context, id string) error
	Decided() ([]string, error)
	Reap(ctx context.Context)
	Close() error
}

type Options struct {
	// Decider decides the writable layer of an active snapshot the first time it is mounted. Nil keeps every
	// snapshot on a directory upper, while layers recorded earlier keep being served.
	Decider Decider
	// Containers lets Commit refuse a block layer whose container still exists. Nil skips the check.
	Containers ContainerLookup
	// UpperdirLabel shows the upper directory label to clients, pointing at the block layer's upper for block
	// snapshots.
	UpperdirLabel bool
}

// Snapshotter wraps containerd's overlay snapshotter. Operations that touch a snapshot's writable layer hold
// that snapshot's key lock, so creating, mounting, measuring, committing and removing a layer never overlap.
type Snapshotter struct {
	inner  snapshots.Snapshotter
	layers Layers
	opts   Options
	keys   kmutex.KeyedLocker

	closeOnce sync.Once
	closeErr  error
}

var (
	_ snapshots.Snapshotter = (*Snapshotter)(nil)
	_ snapshots.Cleaner     = (*Snapshotter)(nil)
)

func New(inner snapshots.Snapshotter, layers Layers, opts Options) *Snapshotter {
	return &Snapshotter{inner: inner, layers: layers, opts: opts, keys: kmutex.New()}
}

func (s *Snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	sn, err := s.stat(ctx, key)
	if err != nil {
		return snapshots.Info{}, err
	}
	return s.public(ctx, sn), nil
}

// Labels this wrapper computes are never stored, even when a client writes back what Stat returned.
func (s *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	info.Labels = withoutComputedLabels(info.Labels)
	updated, err := s.inner.Update(ctx, info, fieldpaths...)
	if err != nil {
		return snapshots.Info{}, err
	}
	sn, err := parse(updated)
	if err != nil {
		return snapshots.Info{}, err
	}
	return s.public(ctx, sn), nil
}

// Usage of a block layer is what its filesystem has in use; kubelet adds it to the container's ephemeral-storage.
// Any other snapshot is measured by the inner overlay without the key lock, since walking a large directory upper
// can take long and nothing about it needs guarding.
func (s *Snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	usage, measured, err := s.blockUsage(ctx, key)
	if err != nil || measured {
		return usage, err
	}
	return s.inner.Usage(ctx, key)
}

// blockUsage measures a block layer under the key lock, which keeps Remove from unmounting it between the mount
// check and statfs.
func (s *Snapshotter) blockUsage(ctx context.Context, key string) (snapshots.Usage, bool, error) {
	if err := s.keys.Lock(ctx, key); err != nil {
		return snapshots.Usage{}, false, err
	}
	defer s.keys.Unlock(key)
	sn, err := s.stat(ctx, key)
	if err != nil {
		return snapshots.Usage{}, false, err
	}
	_, ok, err := s.blockLayer(sn)
	if err != nil || !ok {
		return snapshots.Usage{}, false, err
	}
	usage, err := s.layers.Usage(sn.id)
	return usage, true, err
}

// Mounts is where an app container's writable layer is decided, not Prepare: the backend's Prepare runs inside
// containerd's metadata transaction and before the container record exists, while task create calls Mounts once
// the record, with its labels and OCI spec, is in place.
func (s *Snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	if err := s.keys.Lock(ctx, key); err != nil {
		return nil, err
	}
	defer s.keys.Unlock(key)
	mounts, err := s.inner.Mounts(ctx, key)
	if err != nil {
		return nil, err
	}
	sn, err := s.stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if sn.info.Kind != snapshots.KindActive {
		return mounts, nil
	}
	st, err := s.writableLayer(ctx, sn)
	if err != nil {
		return nil, err
	}
	if st.Size == 0 {
		return mounts, nil
	}
	return rewriteMounts(mounts, sn.upper, s.layers.Layer(sn.id))
}

func (s *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.inner.Prepare(ctx, key, parent, opts...)
}

func (s *Snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.inner.View(ctx, key, parent, opts...)
}

// Commit publishes a block layer's contents where the inner overlay reads a committed layer from before it
// commits the snapshot. containerd's metadata layer calls Commit inside its write transaction, so the copy
// holds up containerd's metadata writes for as long as it takes.
func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	if err := s.keys.Lock(ctx, key); err != nil {
		return err
	}
	defer s.keys.Unlock(key)
	sn, err := s.stat(ctx, key)
	if err != nil {
		return err
	}
	st, recorded, err := s.layers.Load(sn.id)
	if err != nil {
		return err
	}
	recorded = recorded && st.Key == key
	if recorded && st.Size > 0 && sn.info.Kind == snapshots.KindActive {
		if err := s.publishBlock(ctx, sn); err != nil {
			return err
		}
	}
	if err := s.inner.Commit(ctx, name, key, opts...); err != nil {
		return err
	}
	if recorded {
		// The committed snapshot keeps the ID, but the record belonged to the active snapshot that is gone.
		if err := s.layers.Detach(ctx, sn.id); err != nil {
			log.G(ctx).WithError(err).WithField("layer", sn.id).Warn("failed to detach the writable layer of a committed snapshot, Cleanup will retry")
		}
	}
	return nil
}

// Remove deletes the inner snapshot first: once its record is gone, nothing can use the layer, and a failure
// detaching it is left to Cleanup.
func (s *Snapshotter) Remove(ctx context.Context, key string) error {
	if err := s.keys.Lock(ctx, key); err != nil {
		return err
	}
	defer s.keys.Unlock(key)
	sn, statErr := s.stat(ctx, key)
	if err := s.inner.Remove(ctx, key); err != nil {
		return err
	}
	if statErr != nil {
		log.G(ctx).WithError(statErr).WithField("key", key).Warn("removed a snapshot without knowing its writable layer, Cleanup will reclaim it")
		return nil
	}
	if err := s.layers.Detach(ctx, sn.id); err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"key": key, "layer": sn.id}).Warn("failed to detach writable layer, Cleanup will retry")
	}
	return nil
}

func (s *Snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, filters ...string) error {
	return s.inner.Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
		sn, err := parse(info)
		if err != nil {
			return err
		}
		return fn(ctx, s.public(ctx, sn))
	}, filters...)
}

// Cleanup is implemented explicitly: snapshotservice type-asserts snapshots.Cleaner. containerd calls it after
// garbage collection has removed snapshots.
func (s *Snapshotter) Cleanup(ctx context.Context) error {
	if c, ok := s.inner.(snapshots.Cleaner); ok {
		if err := c.Cleanup(ctx); err != nil {
			return err
		}
	}
	if err := s.reclaimOrphans(ctx); err != nil {
		return err
	}
	s.layers.Reap(ctx)
	return nil
}

// Close leaves every layer mounted: running containers keep using them across a restart of the daemon.
func (s *Snapshotter) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = errors.Join(s.layers.Close(), s.inner.Close())
	})
	return s.closeErr
}

// writableLayer returns the recorded layer of an active snapshot, deciding and creating it first if needed.
// A zero size means the snapshot keeps the inner overlay's directory upper. The caller holds the key lock.
func (s *Snapshotter) writableLayer(ctx context.Context, sn snapshot) (block.State, error) {
	key := sn.info.Name
	st, ok, err := s.layers.Load(sn.id)
	if err != nil {
		return block.State{}, err
	}
	if ok && st.Key != key {
		// Left behind by an earlier snapshot with the same ID, as after the inner metadata was recreated.
		log.G(ctx).WithFields(log.Fields{"key": key, "layer": sn.id, "recorded": st.Key}).Warn("discarding a writable layer recorded for another snapshot")
		if err := s.layers.Detach(ctx, sn.id); err != nil {
			return block.State{}, err
		}
		ok = false
	}
	if ok {
		if st.Size > 0 {
			if err := s.layers.EnsureMounted(ctx, sn.id, sn.upper); err != nil {
				return block.State{}, err
			}
		}
		return st, nil
	}

	if s.opts.Decider == nil {
		return block.State{}, nil
	}
	d, err := s.opts.Decider.Decide(ctx, key)
	if err != nil {
		return block.State{}, err
	}
	if !d.Final {
		return block.State{}, nil
	}
	st = block.State{Key: key, Size: d.Size, Reason: d.Reason, Created: time.Now().UTC(), Container: d.Container}
	if st.Size > 0 {
		replaceable, why, err := s.layers.Replaceable(sn.upper)
		if err != nil {
			return block.State{}, err
		}
		if !replaceable {
			st.Size, st.Reason = 0, fmt.Sprintf("%s, so it is kept instead of a block layer (%s)", why, d.Reason)
		}
	}

	fields := log.Fields{"key": key, "layer": sn.id, "size": st.Size, "reason": st.Reason}
	if c := st.Container; c != nil {
		fields["pod"] = c.PodNamespace + "/" + c.PodName
		fields["container"] = c.Name
	}
	if st.Size == 0 {
		if err := s.layers.RecordDirectory(sn.id, st); err != nil {
			return block.State{}, err
		}
		log.G(ctx).WithFields(fields).Info("container keeps a directory writable layer")
		return st, nil
	}
	start := time.Now()
	if err := s.layers.Create(ctx, sn.id, st, sn.upper); err != nil {
		return block.State{}, fmt.Errorf("create writable layer of %s: %w", key, err)
	}
	fields["duration"] = time.Since(start).String()
	log.G(ctx).WithFields(fields).Info("created block writable layer")
	return st, nil
}

// blockLayer returns the recorded block layer of an active snapshot, if it has one.
func (s *Snapshotter) blockLayer(sn snapshot) (block.State, bool, error) {
	if sn.info.Kind != snapshots.KindActive {
		return block.State{}, false, nil
	}
	st, ok, err := s.layers.Load(sn.id)
	if err != nil || !ok || st.Key != sn.info.Name || st.Size == 0 {
		return block.State{}, false, err
	}
	return st, true, nil
}

// publishBlock puts a block layer's contents into the inner overlay's directory upper, which a block snapshot
// leaves empty and a committed snapshot is read from. A failed attempt leaves the snapshot active on its block
// layer; the next one replaces the directory again.
func (s *Snapshotter) publishBlock(ctx context.Context, sn snapshot) error {
	if err := s.checkNotInUse(ctx, sn.info.Name); err != nil {
		return err
	}
	if err := s.layers.EnsureMounted(ctx, sn.id, sn.upper); err != nil {
		return err
	}
	copied, err := s.layers.CopyUpper(ctx, sn.id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(sn.upper); err != nil {
		_ = os.RemoveAll(copied)
		return fmt.Errorf("clear %s: %w", sn.upper, err)
	}
	if err := os.Rename(copied, sn.upper); err != nil {
		_ = os.RemoveAll(copied)
		return fmt.Errorf("move the committed layer into %s: %w", sn.upper, err)
	}
	return nil
}

// checkNotInUse refuses to commit the block layer of a container that still exists: the copy is a full pass
// over the layer, and the container could keep writing to it meanwhile.
func (s *Snapshotter) checkNotInUse(ctx context.Context, key string) error {
	if s.opts.Containers == nil {
		return nil
	}
	ref, ok := ParseKey(key)
	if !ok {
		return nil
	}
	c, err := s.opts.Containers.Lookup(ctx, ref.Namespace, ref.Name)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check whether a container uses snapshot %s: %w", key, err)
	}
	if c.SnapshotKey == ref.Name {
		return fmt.Errorf("snapshot %s is the writable layer of container %s: %w", ref.Name, ref.Name, errdefs.ErrFailedPrecondition)
	}
	return nil
}

// reclaimOrphans detaches the layers whose snapshot no longer exists, as when the daemon stopped between
// removing a snapshot and detaching its layer. Layers are listed before the snapshots are walked: a layer is
// only ever created for a snapshot that already exists, so every listed layer of a live snapshot is in the walk.
func (s *Snapshotter) reclaimOrphans(ctx context.Context) error {
	ids, err := s.layers.Decided()
	if err != nil || len(ids) == 0 {
		return err
	}
	live := map[string]string{}
	// The overlay reports not found when its metadata has never held a snapshot; then nothing is live.
	if err := s.inner.Walk(ctx, func(ctx context.Context, info snapshots.Info) error {
		sn, err := parse(info)
		if err != nil {
			return err
		}
		live[sn.id] = info.Name
		return nil
	}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	for _, id := range ids {
		liveKey, isLive := live[id]
		st, ok, err := s.layers.Load(id)
		switch {
		case err != nil && isLive:
			log.G(ctx).WithError(err).WithFields(log.Fields{"layer": id, "key": liveKey}).Error("cannot read the writable layer of a live snapshot")
			continue
		case err == nil && ok && st.Key == liveKey:
			continue
		}
		s.reclaim(ctx, id, liveKey, st.Key)
	}
	return nil
}

// reclaim detaches an orphaned layer after checking again, under the locks of every snapshot that could be using
// it, that none is.
func (s *Snapshotter) reclaim(ctx context.Context, id string, keys ...string) {
	keys = slices.Compact(slices.Sorted(slices.Values(keys)))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := s.keys.Lock(ctx, key); err != nil {
			return
		}
		defer s.keys.Unlock(key)
	}
	if st, ok, err := s.layers.Load(id); err == nil && ok {
		if !slices.Contains(keys, st.Key) {
			// Recorded again meanwhile; the next Cleanup looks at it afresh.
			return
		}
		if sn, err := s.stat(ctx, st.Key); err == nil && sn.id == id {
			return
		}
	}
	if err := s.layers.Detach(ctx, id); err != nil {
		log.G(ctx).WithError(err).WithField("layer", id).Warn("failed to detach orphaned writable layer")
		return
	}
	log.G(ctx).WithField("layer", id).Info("reclaimed orphaned writable layer")
}

// public turns an inner snapshot into what clients see.
func (s *Snapshotter) public(ctx context.Context, sn snapshot) snapshots.Info {
	info := sn.info
	labels := make(map[string]string, len(info.Labels))
	for k, v := range info.Labels {
		labels[k] = v
	}
	delete(labels, upperdirLabel)
	upper := sn.upper
	if _, ok, err := s.blockLayer(sn); err != nil {
		// Stat and Walk keep working, so one bad layer does not stop containerd from listing snapshots.
		log.G(ctx).WithError(err).WithField("key", info.Name).Warn("cannot read the writable layer of a snapshot")
	} else if ok {
		l := s.layers.Layer(sn.id)
		labels[WritableMountpointLabel] = l.Mountpoint()
		upper = l.Upper()
	}
	if s.opts.UpperdirLabel {
		labels[upperdirLabel] = upper
	}
	if len(labels) == 0 {
		labels = nil
	}
	info.Labels = labels
	return info
}

func withoutComputedLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if k != upperdirLabel && k != WritableMountpointLabel {
			out[k] = v
		}
	}
	return out
}

type snapshot struct {
	info  snapshots.Info
	id    string
	upper string // the inner overlay's directory upper, <root>/snapshots/<id>/fs
}

func (s *Snapshotter) stat(ctx context.Context, key string) (snapshot, error) {
	info, err := s.inner.Stat(ctx, key)
	if err != nil {
		return snapshot{}, err
	}
	return parse(info)
}

func parse(info snapshots.Info) (snapshot, error) {
	upper := info.Labels[upperdirLabel]
	id := filepath.Base(filepath.Dir(upper))
	if upper == "" || filepath.Base(upper) != "fs" || !block.ValidID(id) {
		return snapshot{}, fmt.Errorf("snapshot %q: inner overlay reported upper directory %q; it must be created with overlay.WithUpperdirLabel", info.Name, upper)
	}
	return snapshot{info: info, id: id, upper: upper}, nil
}
