// Package block keeps size-limited writable layers: one sparse ext4 image per container snapshot, loop-mounted
// where that snapshot's overlay upper and work directories live.
package block

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/log"
)

const (
	// ImageName is the sparse ext4 image holding a writable layer.
	ImageName = "rwlayer.img"

	stateName     = "state.json"
	stagingPrefix = "new-"
	trashPrefix   = "trash-"
	stateVersion  = 1
)

// ErrCorrupt means a layer directory exists without a readable state. Such a layer is never guessed at: the
// snapshot it belongs to fails to mount until the directory is repaired or the snapshot is removed.
var ErrCorrupt = errors.New("corrupt writable layer state")

// Layer names the paths of one snapshot's writable layer. ID is the inner overlay snapshot's ID.
type Layer struct {
	ID  string
	Dir string
}

func (l Layer) Image() string      { return filepath.Join(l.Dir, ImageName) }
func (l Layer) Mountpoint() string { return filepath.Join(l.Dir, "rw") }
func (l Layer) Upper() string      { return filepath.Join(l.Mountpoint(), "upper") }
func (l Layer) Work() string       { return filepath.Join(l.Mountpoint(), "work") }

// State records how a snapshot's writable layer was decided. It is written once, before the layer is first
// used, so a later call can never decide differently for the same snapshot.
type State struct {
	Version int    `json:"version"`
	Key     string `json:"key"`
	// Size is the image size in bytes. Zero means the snapshot keeps the inner overlay's directory upper.
	Size int64 `json:"size"`
}

// Store keeps layers under one root:
//
//	<id>/state.json    the decision, written before the directory gets its name
//	<id>/rwlayer.img   the sparse ext4 image, for block layers only
//	<id>/rw/           where the image is mounted, holding upper/ and work/
//	new-*              a layer or a commit copy being written
//	trash-*            a detached layer waiting for the reaper
//
// Callers serialize operations on one snapshot; the store only guards its own cache and the reaper.
type Store struct {
	root string

	mu    sync.Mutex
	cache map[string]State
	// gen counts detaches, so a read that raced one does not put the detached state back into the cache.
	gen uint64

	reapMu    sync.Mutex
	wake      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// Open opens the store at root, creating it if needed. Directories left behind by a creation or removal that
// a crash interrupted are reclaimed first: nothing can be using them while the daemon starts.
func Open(ctx context.Context, root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		root:  root,
		cache: map[string]State{},
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	s.sweep(ctx)
	go s.reaper(context.WithoutCancel(ctx))
	return s, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) Layer(id string) Layer { return Layer{ID: id, Dir: filepath.Join(s.root, id)} }

// ValidID reports whether id can name a layer. The inner overlay numbers its snapshots.
func ValidID(id string) bool {
	_, err := strconv.ParseUint(id, 10, 64)
	return err == nil
}

// Load returns the recorded state of a snapshot's layer, or false if none was recorded.
func (s *Store) Load(id string) (State, bool, error) {
	s.mu.Lock()
	st, ok := s.cache[id]
	gen := s.gen
	s.mu.Unlock()
	if ok {
		return st, true, nil
	}
	st, ok, err := readState(s.Layer(id))
	if err != nil || !ok {
		return State{}, false, err
	}
	s.mu.Lock()
	if s.gen == gen {
		s.cache[id] = st
	}
	s.mu.Unlock()
	return st, true, nil
}

// RecordDirectory records that the snapshot keeps the inner overlay's directory upper.
func (s *Store) RecordDirectory(id string, st State) error {
	if st.Size != 0 {
		return fmt.Errorf("a directory upper has no size, got %d", st.Size)
	}
	staging, err := s.newStaging(id)
	if err != nil {
		return err
	}
	if err := writeState(staging, st); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := s.publish(id, staging, st); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	return nil
}

// Detach unmounts a layer and moves it out of the way for the reaper. A layer that cannot be unmounted keeps
// its name, so the next Cleanup finds it again and retries.
func (s *Store) Detach(ctx context.Context, id string) error {
	l := s.Layer(id)
	if err := unmount(l.Mountpoint()); err != nil {
		return fmt.Errorf("unmount writable layer %s: %w", l.Mountpoint(), err)
	}
	trash := filepath.Join(s.root, fmt.Sprintf("%s%s-%d", trashPrefix, id, time.Now().UnixNano()))
	err := os.Rename(l.Dir, trash)
	s.forget(id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("detach writable layer %s: %w", l.Dir, err)
	}
	log.G(ctx).WithField("layer", id).Debug("detached writable layer")
	s.kick()
	return nil
}

// Decided lists the IDs of the snapshots that have a recorded layer.
func (s *Store) Decided() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// Reap deletes detached layers. Deleting a large image can take a while, so it never runs under a snapshot lock.
func (s *Store) Reap(ctx context.Context) {
	s.reapMu.Lock()
	defer s.reapMu.Unlock()
	s.removeEntries(ctx, trashPrefix)
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
	})
	return nil
}

func (s *Store) sweep(ctx context.Context) {
	s.reapMu.Lock()
	defer s.reapMu.Unlock()
	s.removeEntries(ctx, stagingPrefix, trashPrefix)
}

func (s *Store) removeEntries(ctx context.Context, prefixes ...string) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		log.G(ctx).WithError(err).WithField("root", s.root).Warn("failed to list writable layers")
		return
	}
	for _, e := range entries {
		for _, prefix := range prefixes {
			if !strings.HasPrefix(e.Name(), prefix) {
				continue
			}
			dir := filepath.Join(s.root, e.Name())
			if err := removeLayerDir(dir); err != nil {
				log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove writable layer directory")
			}
			break
		}
	}
}

func (s *Store) reaper(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
			s.Reap(ctx)
		}
	}
}

func (s *Store) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Store) newStaging(id string) (string, error) {
	dir, err := os.MkdirTemp(s.root, stagingPrefix+id+"-")
	if err != nil {
		return "", fmt.Errorf("create staging directory for writable layer %s: %w", id, err)
	}
	return dir, nil
}

// publish gives a fully written staging directory its final name. The rename is what records the decision;
// it fails rather than replace a layer that already exists.
func (s *Store) publish(id, staging string, st State) error {
	l := s.Layer(id)
	if err := os.Rename(staging, l.Dir); err != nil {
		return fmt.Errorf("publish writable layer %s: %w", l.Dir, err)
	}
	if err := syncDir(s.root); err != nil {
		log.L.WithError(err).WithField("root", s.root).Warn("failed to sync writable layer directory")
	}
	s.mu.Lock()
	s.cache[id] = st
	s.mu.Unlock()
	return nil
}

func (s *Store) forget(id string) {
	s.mu.Lock()
	delete(s.cache, id)
	s.gen++
	s.mu.Unlock()
}

func readState(l Layer) (State, bool, error) {
	data, err := os.ReadFile(filepath.Join(l.Dir, stateName))
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(l.Dir); errors.Is(serr, fs.ErrNotExist) {
			return State{}, false, nil
		} else if serr != nil {
			return State{}, false, serr
		}
		return State{}, false, fmt.Errorf("%s has no %s: %w", l.Dir, stateName, ErrCorrupt)
	}
	if err != nil {
		return State{}, false, err
	}
	var st State
	// Earlier versions also wrote reason, created and container: unknown fields must keep decoding.
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, false, fmt.Errorf("%s: %v: %w", filepath.Join(l.Dir, stateName), err, ErrCorrupt)
	}
	if st.Version != stateVersion || st.Key == "" || st.Size < 0 {
		return State{}, false, fmt.Errorf("%s: unexpected version %d, key %q or size %d: %w", filepath.Join(l.Dir, stateName), st.Version, st.Key, st.Size, ErrCorrupt)
	}
	return st, true, nil
}

func writeState(dir string, st State) error {
	st.Version = stateVersion
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, stateName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// removeLayerDir deletes a layer directory, making sure first that no image is mounted in it: deleting
// through a mounted image would delete the contents of a container's writable layer.
func removeLayerDir(dir string) error {
	if err := unmount(filepath.Join(dir, "rw")); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
