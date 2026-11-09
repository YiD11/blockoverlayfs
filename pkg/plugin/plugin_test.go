package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/nri/pkg/api"

	"github.com/YiD11/blockoverlayfs/pkg/annotation"
	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

const gi = int64(1) << 30

// fakeRuntime is containerd as the plugin sees it.
type fakeRuntime struct {
	mu         sync.Mutex
	snapshots  map[string]string // container ID to snapshot directory
	containers map[string]bool
	err        error
	lookups    int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{snapshots: map[string]string{}, containers: map[string]bool{}}
}

func (f *fakeRuntime) SnapshotDir(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.err != nil {
		return "", f.err
	}
	dir, ok := f.snapshots[id]
	if !ok {
		return "", fmt.Errorf("snapshot %s: %w", id, errdefs.ErrNotFound)
	}
	return dir, nil
}

func (f *fakeRuntime) SnapshotExists(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.snapshots[id]
	return ok, f.err
}

func (f *fakeRuntime) ContainerExists(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[id], f.err
}

func (f *fakeRuntime) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// fakeLayers keeps layers in memory.
type fakeLayers struct {
	mu        sync.Mutex
	layers    map[string]*fakeLayer
	attachErr error
}

type fakeLayer struct {
	dir     string
	size    int64
	mounted bool
}

func newFakeLayers() *fakeLayers { return &fakeLayers{layers: map[string]*fakeLayer{}} }

func (f *fakeLayers) Attach(_ context.Context, id, dir string, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return f.attachErr
	}
	if l, ok := f.layers[id]; ok && l.mounted && l.dir != dir {
		return fmt.Errorf("%s is mounted at %s", id, l.dir)
	}
	f.layers[id] = &fakeLayer{dir: dir, size: size, mounted: true}
	return nil
}

func (f *fakeLayers) Unmount(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.layers[id]; ok {
		l.mounted = false
	}
	return nil
}

func (f *fakeLayers) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.layers[id]; ok && l.mounted {
		return fmt.Errorf("%s is mounted", id)
	}
	delete(f.layers, id)
	return nil
}

func (f *fakeLayers) Mounted(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.layers[id]
	return ok && l.mounted, nil
}

func (f *fakeLayers) IDs() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.layers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// get returns a copy of a container's layer.
func (f *fakeLayers) get(id string) (fakeLayer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.layers[id]
	if !ok {
		return fakeLayer{}, false
	}
	return *l, true
}

func (f *fakeLayers) put(id string, l fakeLayer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.layers[id] = &l
}

func pod(value string) *api.PodSandbox {
	p := &api.PodSandbox{Id: "pod-id", Name: "web", Namespace: "default"}
	if value != "" {
		p.Annotations = map[string]string{annotation.Key: value}
	}
	return p
}

func container(id, name string) *api.Container {
	return &api.Container{Id: id, PodSandboxId: "pod-id", Name: name}
}

func TestCreateContainer(t *testing.T) {
	policy := sizing.Policy{LimitOffset: 256 << 20, RequestPercent: 105}
	for _, tc := range []struct {
		name       string
		policy     sizing.Policy
		annotation string
		snapshot   bool
		size       int64 // zero: no layer
		lookups    int
	}{
		{name: "a limit", policy: policy, annotation: `{"app":{"limit":"10Gi"}}`, snapshot: true, size: 10*gi - 256<<20, lookups: 1},
		{name: "a request", policy: policy, annotation: `{"app":{"request":"1Gi"}}`, snapshot: true, size: (gi*105 + 99) / 100, lookups: 1},
		{name: "no annotation and no default size", policy: policy},
		{name: "no annotation and a default size", policy: sizing.Policy{DefaultSize: 20 * gi}, snapshot: true, size: 20 * gi, lookups: 1},
		{name: "an unreadable annotation and a default size", policy: sizing.Policy{DefaultSize: 20 * gi}, annotation: `{"app":{"limits":"1Gi"}}`, snapshot: true, size: 20 * gi, lookups: 1},
		{name: "a container the annotation does not list", policy: policy, annotation: `{"init":{"limit":"1Gi"}}`},
		{name: "a rootfs on another snapshotter", policy: policy, annotation: `{"app":{"limit":"10Gi"}}`, lookups: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, layers := newFakeRuntime(), newFakeLayers()
			if tc.snapshot {
				rt.snapshots["ctr"] = "/snapshots/7"
			}
			p := New(rt, layers, tc.policy)

			adj, updates, err := p.CreateContainer(context.Background(), pod(tc.annotation), container("ctr", "app"))
			if err != nil {
				t.Fatal(err)
			}
			if adj != nil || updates != nil {
				t.Errorf("CreateContainer adjusted %v and updated %v, want nothing", adj, updates)
			}
			l, ok := layers.get("ctr")
			if tc.size == 0 {
				if ok {
					t.Errorf("got a layer %+v, want none", l)
				}
			} else if !ok || l.size != tc.size || l.dir != "/snapshots/7" || !l.mounted {
				t.Errorf("layer = %+v, %v; want %d bytes mounted at /snapshots/7", l, ok, tc.size)
			}
			if got := rt.lookupCount(); got != tc.lookups {
				t.Errorf("looked the snapshot up %d times, want %d", got, tc.lookups)
			}
		})
	}
}

func TestCreateContainerKeepsADirectoryLayerWhenTheLayerCannotBeAttached(t *testing.T) {
	ctx := context.Background()
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.snapshots["ctr"] = "/snapshots/7"
	p := New(rt, layers, sizing.Policy{DefaultSize: gi})

	layers.attachErr = errors.New("mkfs failed")
	adj, updates, err := p.CreateContainer(ctx, pod(""), container("ctr", "app"))
	if err != nil {
		t.Errorf("CreateContainer returned %v, want no error: a container that cannot get its layer runs on a directory", err)
	}
	if adj != nil || updates != nil {
		t.Errorf("CreateContainer adjusted %v and updated %v, want nothing", adj, updates)
	}
	if l, ok := layers.get("ctr"); ok {
		t.Errorf("got a layer %+v, want none", l)
	}

	layers.attachErr = nil
	rt.err = errors.New("containerd is gone")
	if _, _, err := p.CreateContainer(ctx, pod(""), container("ctr", "app")); err == nil {
		t.Error("expected an error when the snapshot cannot be looked up")
	}
}

func TestRemoveContainerUnmountsAndDeletesTheLayer(t *testing.T) {
	ctx := context.Background()
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.snapshots["ctr"] = "/snapshots/7"
	p := New(rt, layers, sizing.Policy{DefaultSize: gi})
	if _, _, err := p.CreateContainer(ctx, pod(""), container("ctr", "app")); err != nil {
		t.Fatal(err)
	}

	if err := p.RemoveContainer(ctx, pod(""), container("ctr", "app")); err != nil {
		t.Fatal(err)
	}
	p.Wait()
	if l, ok := layers.get("ctr"); ok {
		t.Errorf("layer %+v left behind", l)
	}

	// A container that never had a layer.
	if err := p.RemoveContainer(ctx, pod(""), container("other", "app")); err != nil {
		t.Fatal(err)
	}
	p.Wait()
}

func TestReconcile(t *testing.T) {
	rt, layers := newFakeRuntime(), newFakeLayers()
	// A running container.
	rt.containers["running"] = true
	rt.snapshots["running"] = "/snapshots/1"
	layers.put("running", fakeLayer{dir: "/snapshots/1", mounted: true})
	// A container being created: its snapshot exists, its container record not yet.
	rt.snapshots["creating"] = "/snapshots/2"
	layers.put("creating", fakeLayer{dir: "/snapshots/2", mounted: true})
	// A container removed while the plugin was away.
	layers.put("removed", fakeLayer{dir: "/snapshots/3", mounted: true})
	// An image whose setup was interrupted, or that a node reboot unmounted.
	rt.containers["rebooted"] = true
	layers.put("rebooted", fakeLayer{dir: "/snapshots/4"})

	p := New(rt, layers, sizing.Policy{})
	p.Reconcile(context.Background())

	ids, _ := layers.IDs()
	if want := []string{"creating", "running"}; !slices.Equal(ids, want) {
		t.Errorf("layers left = %v, want %v", ids, want)
	}
	for _, id := range ids {
		if l, _ := layers.get(id); !l.mounted {
			t.Errorf("layer %s got unmounted", id)
		}
	}
}

func TestReconcileKeepsLayersWhenContainerdCannotAnswer(t *testing.T) {
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.err = errors.New("containerd is gone")
	layers.put("ctr", fakeLayer{dir: "/snapshots/1", mounted: true})

	New(rt, layers, sizing.Policy{}).Reconcile(context.Background())

	if l, ok := layers.get("ctr"); !ok || !l.mounted {
		t.Errorf("layer = %+v, %v; want it kept mounted", l, ok)
	}
}

func TestSynchronizeReconcilesInTheBackground(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, layers := newFakeRuntime(), newFakeLayers()
	layers.put("removed", fakeLayer{dir: "/snapshots/3", mounted: true})
	p := New(rt, layers, sizing.Policy{})
	go p.RunReconciler(ctx, time.Hour)

	if _, err := p.Synchronize(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := layers.get("removed"); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Synchronize did not reconcile")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestKeyedLockerSerializesOneKey(t *testing.T) {
	var k keyedLocker
	unlockA := k.lock("a")
	// Another key is not held up.
	k.lock("b")()

	locked := make(chan struct{})
	go func() {
		unlock := k.lock("a")
		close(locked)
		unlock()
	}()
	select {
	case <-locked:
		t.Fatal("a second lock of the same key did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	<-locked

	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.locks) != 0 {
		t.Errorf("%d locks left behind", len(k.locks))
	}
}
