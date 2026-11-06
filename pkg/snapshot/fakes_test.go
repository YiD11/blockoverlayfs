package snapshot

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"

	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/containermeta"
)

// events is shared by the fakes so a test can check the order of calls across them.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(format string, args ...any) {
	e.mu.Lock()
	e.list = append(e.list, fmt.Sprintf(format, args...))
	e.mu.Unlock()
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

type fakeSnapshot struct {
	id     string
	kind   snapshots.Kind
	parent string
	labels map[string]string
}

// fakeInner behaves like containerd's overlay snapshotter as far as the wrapper can tell: it labels every
// snapshot with <root>/snapshots/<id>/fs and keeps the ID when an active snapshot is committed.
type fakeInner struct {
	root      string
	ev        *events
	next      int
	snaps     map[string]*fakeSnapshot
	removeErr error
	walkErr   error
}

func newFakeInner(root string, ev *events) *fakeInner {
	return &fakeInner{root: root, ev: ev, snaps: map[string]*fakeSnapshot{}}
}

func (f *fakeInner) upper(id string) string { return filepath.Join(f.root, "snapshots", id, "fs") }

func (f *fakeInner) info(key string, s *fakeSnapshot) snapshots.Info {
	labels := maps.Clone(s.labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[upperdirLabel] = f.upper(s.id)
	return snapshots.Info{Name: key, Kind: s.kind, Parent: s.parent, Labels: labels}
}

func (f *fakeInner) Stat(_ context.Context, key string) (snapshots.Info, error) {
	s, ok := f.snaps[key]
	if !ok {
		return snapshots.Info{}, fmt.Errorf("snapshot %s: %w", key, errdefs.ErrNotFound)
	}
	return f.info(key, s), nil
}

func (f *fakeInner) Update(_ context.Context, info snapshots.Info, _ ...string) (snapshots.Info, error) {
	s, ok := f.snaps[info.Name]
	if !ok {
		return snapshots.Info{}, errdefs.ErrNotFound
	}
	s.labels = maps.Clone(info.Labels)
	f.ev.add("inner-update:%s:%v", info.Name, slices.Sorted(maps.Keys(info.Labels)))
	return f.info(info.Name, s), nil
}

func (f *fakeInner) Usage(_ context.Context, key string) (snapshots.Usage, error) {
	f.ev.add("inner-usage:%s", key)
	return snapshots.Usage{Size: 1}, nil
}

func (f *fakeInner) Mounts(_ context.Context, key string) ([]mount.Mount, error) {
	s, ok := f.snaps[key]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	if s.parent == "" {
		return []mount.Mount{{Type: "bind", Source: f.upper(s.id), Options: []string{"rw", "rbind"}}}, nil
	}
	return []mount.Mount{{Type: "overlay", Source: "overlay", Options: []string{
		"index=off",
		"workdir=" + filepath.Join(f.root, "snapshots", s.id, "work"),
		"upperdir=" + f.upper(s.id),
		"lowerdir=" + f.upper(f.snaps[s.parent].id),
	}}}, nil
}

func (f *fakeInner) create(kind snapshots.Kind, key, parent string) ([]mount.Mount, error) {
	if _, ok := f.snaps[key]; ok {
		return nil, errdefs.ErrAlreadyExists
	}
	f.next++
	s := &fakeSnapshot{id: strconv.Itoa(f.next), kind: kind, parent: parent}
	if err := os.MkdirAll(f.upper(s.id), 0o755); err != nil {
		return nil, err
	}
	f.snaps[key] = s
	return f.Mounts(context.Background(), key)
}

func (f *fakeInner) Prepare(_ context.Context, key, parent string, _ ...snapshots.Opt) ([]mount.Mount, error) {
	return f.create(snapshots.KindActive, key, parent)
}

func (f *fakeInner) View(_ context.Context, key, parent string, _ ...snapshots.Opt) ([]mount.Mount, error) {
	return f.create(snapshots.KindView, key, parent)
}

func (f *fakeInner) Commit(_ context.Context, name, key string, _ ...snapshots.Opt) error {
	s, ok := f.snaps[key]
	if !ok || s.kind != snapshots.KindActive {
		return errdefs.ErrNotFound
	}
	delete(f.snaps, key)
	f.snaps[name] = &fakeSnapshot{id: s.id, kind: snapshots.KindCommitted, parent: s.parent, labels: s.labels}
	f.ev.add("inner-commit:%s", key)
	return nil
}

func (f *fakeInner) Remove(_ context.Context, key string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	if _, ok := f.snaps[key]; !ok {
		return errdefs.ErrNotFound
	}
	delete(f.snaps, key)
	f.ev.add("inner-remove:%s", key)
	return nil
}

func (f *fakeInner) Walk(ctx context.Context, fn snapshots.WalkFunc, _ ...string) error {
	if f.walkErr != nil {
		return f.walkErr
	}
	for _, key := range slices.Sorted(maps.Keys(f.snaps)) {
		if err := fn(ctx, f.info(key, f.snaps[key])); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeInner) Cleanup(context.Context) error {
	f.ev.add("inner-cleanup")
	return nil
}

func (f *fakeInner) Close() error {
	f.ev.add("inner-close")
	return nil
}

// fakeLayers keeps states in memory; it records what the wrapper asked of the block store.
type fakeLayers struct {
	root        string
	ev          *events
	states      map[string]block.State
	loadErr     map[string]error
	usedUpper   bool
	createErr   error
	ownerFroms  []string
	closedCount int
}

func newFakeLayers(root string, ev *events) *fakeLayers {
	return &fakeLayers{root: root, ev: ev, states: map[string]block.State{}, loadErr: map[string]error{}}
}

func (f *fakeLayers) Layer(id string) block.Layer {
	return block.Layer{ID: id, Dir: filepath.Join(f.root, id)}
}

func (f *fakeLayers) Load(id string) (block.State, bool, error) {
	if err := f.loadErr[id]; err != nil {
		return block.State{}, false, err
	}
	st, ok := f.states[id]
	return st, ok, nil
}

func (f *fakeLayers) RecordDirectory(id string, st block.State) error {
	if _, ok := f.states[id]; ok {
		return errdefs.ErrAlreadyExists
	}
	f.states[id] = st
	f.ev.add("record-directory:%s", id)
	return nil
}

func (f *fakeLayers) Create(_ context.Context, id string, st block.State, ownerFrom string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.states[id] = st
	f.ownerFroms = append(f.ownerFroms, ownerFrom)
	f.ev.add("create:%s:%d", id, st.Size)
	return nil
}

func (f *fakeLayers) EnsureMounted(_ context.Context, id, ownerFrom string) error {
	f.ownerFroms = append(f.ownerFroms, ownerFrom)
	f.ev.add("ensure-mounted:%s", id)
	return nil
}

func (f *fakeLayers) Replaceable(string) (bool, string, error) {
	if f.usedUpper {
		return false, "the directory upper already has content", nil
	}
	return true, "", nil
}

func (f *fakeLayers) Usage(id string) (snapshots.Usage, error) {
	f.ev.add("block-usage:%s", id)
	return snapshots.Usage{Size: 42, Inodes: 7}, nil
}

func (f *fakeLayers) CopyUpper(_ context.Context, id string) (string, error) {
	dir := filepath.Join(f.root, "new-commit-"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "copied"), []byte(id), 0o600); err != nil {
		return "", err
	}
	f.ev.add("copy-upper:%s", id)
	return dir, nil
}

func (f *fakeLayers) Detach(_ context.Context, id string) error {
	delete(f.states, id)
	delete(f.loadErr, id)
	f.ev.add("detach:%s", id)
	return nil
}

func (f *fakeLayers) Decided() ([]string, error) {
	ids := slices.Collect(maps.Keys(f.states))
	for id := range f.loadErr {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

func (f *fakeLayers) Reap(context.Context) { f.ev.add("reap") }

func (f *fakeLayers) Close() error {
	f.closedCount++
	return nil
}

// fakeDecider answers from a table and counts the questions.
type fakeDecider struct {
	decisions map[string]Decision
	err       error
	asked     map[string]int
}

func (f *fakeDecider) Decide(_ context.Context, key string) (Decision, error) {
	if f.asked == nil {
		f.asked = map[string]int{}
	}
	f.asked[key]++
	return f.decisions[key], f.err
}

type fakeContainers map[string]containermeta.Info

func (f fakeContainers) Lookup(_ context.Context, namespace, id string) (containermeta.Info, error) {
	if info, ok := f[namespace+"/"+id]; ok {
		return info, nil
	}
	return containermeta.Info{}, fmt.Errorf("container %s: %w", id, errdefs.ErrNotFound)
}

var errInjected = errors.New("injected")
