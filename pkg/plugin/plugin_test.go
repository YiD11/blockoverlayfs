package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/mockey"
	"github.com/containerd/errdefs"
	"github.com/containerd/nri/pkg/api"

	"github.com/YiD11/blockoverlayfs/pkg/annotation"
	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/containerd"
	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

const gi = int64(1) << 30

// fakeRuntime is containerd as the plugin sees it.
type fakeRuntime struct {
	mu         sync.Mutex
	snapshots  map[string]string // container ID to snapshot directory
	containers map[string]bool
	err        error
	asked      int // how often the plugin asked whether a container exists
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{snapshots: map[string]string{}, containers: map[string]bool{}}
}

func (f *fakeRuntime) SnapshotDir(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	dir, ok := f.snapshots[id]
	if !ok {
		return "", fmt.Errorf("snapshot %s: %w", id, errdefs.ErrNotFound)
	}
	return dir, nil
}

func (f *fakeRuntime) ContainerExists(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	return f.containers[id], f.err
}

func (f *fakeRuntime) askedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

func (f *fakeRuntime) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeRuntime) addContainer(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id] = true
}

// fakeLayers keeps layers in memory.
type fakeLayers struct {
	mu        sync.Mutex
	layers    map[string]*fakeLayer
	attachErr error
	// attaching, when set, receives the ID of each layer Attach starts on, and Attach then waits for release.
	attaching chan string
	release   chan struct{}
}

type fakeLayer struct {
	dir     string
	size    int64
	mounted bool
	// incomplete is a mounted image without an upper directory.
	incomplete bool
}

func newFakeLayers() *fakeLayers { return &fakeLayers{layers: map[string]*fakeLayer{}} }

func (f *fakeLayers) Attach(_ context.Context, id, dir string, size int64) error {
	if f.attaching != nil {
		f.attaching <- id
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attachErr != nil {
		return f.attachErr
	}
	f.layers[id] = &fakeLayer{dir: dir, size: size, mounted: true}
	return nil
}

func (f *fakeLayers) Status(id string) (block.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.layers[id]
	switch {
	case !ok || !l.mounted:
		return block.Detached, nil
	case l.incomplete:
		return block.Incomplete, nil
	}
	return block.Attached, nil
}

func (f *fakeLayers) Unmount(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.layers[id]; ok {
		l.mounted = false
	}
	return nil
}

func (f *fakeLayers) Remove(id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.layers[id]
	if ok && l.mounted {
		return false, fmt.Errorf("%s is mounted", id)
	}
	delete(f.layers, id)
	return ok, nil
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

// newPlugin returns a plugin whose calls to containerd and to the block store go to rt and layers until the test
// ends. The calls are replaced at run time, which needs -gcflags="all=-N -l".
func newPlugin(t *testing.T, rt *fakeRuntime, layers *fakeLayers, policy sizing.Policy) *Plugin {
	t.Helper()
	for _, m := range []struct{ target, hook any }{
		{(*containerd.Client).SnapshotDir, rt.SnapshotDir},
		{(*containerd.Client).ContainerExists, rt.ContainerExists},
		{(*block.Store).Attach, layers.Attach},
		{(*block.Store).Status, layers.Status},
		{(*block.Store).Unmount, layers.Unmount},
		{(*block.Store).Remove, layers.Remove},
		{(*block.Store).IDs, layers.IDs},
	} {
		mocker := mockey.Mock(m.target).To(m.hook).Build()
		t.Cleanup(func() { mocker.UnPatch() })
	}
	return New(&containerd.Client{}, &block.Store{}, policy)
}

// runReconciler runs the plugin's reconciler until the test ends, and waits for it to return: it has to be gone
// before newPlugin gives the calls back.
func runReconciler(t *testing.T, p *Plugin, interval, retry time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.RunReconciler(ctx, interval, retry)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
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
		elsewhere  bool  // the rootfs is not on the configured snapshotter
		size       int64 // zero: no layer
	}{
		{name: "a limit", policy: policy, annotation: `{"app":{"limit":"10Gi"}}`, size: 10*gi - 256<<20},
		{name: "a request", policy: policy, annotation: `{"app":{"request":"1Gi"}}`, size: (gi*105 + 99) / 100},
		{name: "no annotation and no default size", policy: policy},
		{name: "no annotation and a default size", policy: sizing.Policy{DefaultSize: 20 * gi}, size: 20 * gi},
		{name: "an unreadable annotation and a default size", policy: sizing.Policy{DefaultSize: 20 * gi}, annotation: `{"app":{"limits":"1Gi"}}`, size: 20 * gi},
		{name: "a container the annotation does not list", policy: policy, annotation: `{"init":{"limit":"1Gi"}}`},
		{name: "a rootfs on another snapshotter", policy: policy, annotation: `{"app":{"limit":"10Gi"}}`, elsewhere: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, layers := newFakeRuntime(), newFakeLayers()
			if !tc.elsewhere {
				rt.snapshots["ctr"] = "/snapshots/7"
			}
			p := newPlugin(t, rt, layers, tc.policy)

			if _, _, err := p.CreateContainer(context.Background(), pod(tc.annotation), container("ctr", "app")); err != nil {
				t.Fatal(err)
			}
			l, ok := layers.get("ctr")
			if tc.size == 0 {
				if ok {
					t.Errorf("got a layer %+v, want none", l)
				}
			} else if !ok || l.size != tc.size || l.dir != "/snapshots/7" || !l.mounted {
				t.Errorf("layer = %+v, %v; want %d bytes mounted at /snapshots/7", l, ok, tc.size)
			}
		})
	}
}

func TestCreateContainerKeepsADirectoryLayerWhenTheLayerCannotBeAttached(t *testing.T) {
	ctx := context.Background()
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.snapshots["ctr"] = "/snapshots/7"
	p := newPlugin(t, rt, layers, sizing.Policy{DefaultSize: gi})

	layers.attachErr = errors.New("mkfs failed")
	if _, _, err := p.CreateContainer(ctx, pod(""), container("ctr", "app")); err != nil {
		t.Errorf("CreateContainer returned %v, want no error: a container that cannot get its layer runs on a directory", err)
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
	p := newPlugin(t, rt, layers, sizing.Policy{DefaultSize: gi})
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

// synchronized returns a plugin that containerd has synchronized, as it does when the plugin registers.
func synchronized(t *testing.T, rt *fakeRuntime, layers *fakeLayers) *Plugin {
	t.Helper()
	p := newPlugin(t, rt, layers, sizing.Policy{DefaultSize: gi})
	if _, err := p.Synchronize(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReconcile(t *testing.T) {
	rt, layers := newFakeRuntime(), newFakeLayers()
	// A running container.
	rt.containers["running"] = true
	layers.put("running", fakeLayer{mounted: true})
	// A container removed while the plugin was away.
	layers.put("removed", fakeLayer{mounted: true})
	// A node restart unmounted this layer. Its container is still there, but kubelet will not start it again.
	rt.containers["rebooted"] = true
	layers.put("rebooted", fakeLayer{})
	// The plugin stopped while it attached this layer, after mounting the image.
	rt.containers["interrupted"] = true
	layers.put("interrupted", fakeLayer{mounted: true, incomplete: true})

	if !synchronized(t, rt, layers).Reconcile(context.Background()) {
		t.Error("Reconcile left something unsettled")
	}

	ids, _ := layers.IDs()
	if want := []string{"running"}; !slices.Equal(ids, want) {
		t.Errorf("layers left = %v, want %v", ids, want)
	}
	if l, _ := layers.get("running"); !l.mounted {
		t.Error("the layer of the running container got unmounted")
	}
}

func TestReconcileLeavesAttachedLayersUntilContainerdSynchronizesThePlugin(t *testing.T) {
	ctx := context.Background()
	rt, layers := newFakeRuntime(), newFakeLayers()
	// An earlier run attached this layer just before it stopped: containerd may not have recorded the container yet.
	layers.put("ctr", fakeLayer{mounted: true})
	p := newPlugin(t, rt, layers, sizing.Policy{})

	if p.Reconcile(ctx) {
		t.Error("Reconcile reported the layer as settled")
	}
	if l, ok := layers.get("ctr"); !ok || !l.mounted {
		t.Fatalf("layer = %+v, %v; want it kept mounted", l, ok)
	}

	// containerd synchronizes the plugin only while it creates no container: this one never came to be.
	if _, err := p.Synchronize(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !p.Reconcile(ctx) {
		t.Error("Reconcile left something unsettled")
	}
	if l, ok := layers.get("ctr"); ok {
		t.Errorf("layer %+v left behind", l)
	}
}

func TestReconcileWaitsForAContainerBeingCreated(t *testing.T) {
	ctx := context.Background()
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.snapshots["recorded"], rt.snapshots["given-up"] = "/snapshots/7", "/snapshots/8"
	p := synchronized(t, rt, layers)
	for _, id := range []string{"recorded", "given-up"} {
		if _, _, err := p.CreateContainer(ctx, pod(""), container(id, "app")); err != nil {
			t.Fatal(err)
		}
	}

	// containerd records a container only after the plugin has answered.
	if p.Reconcile(ctx) {
		t.Error("Reconcile reported the layers as settled")
	}
	if ids, _ := layers.IDs(); len(ids) != 2 {
		t.Fatalf("layers left = %v, want both", ids)
	}

	// containerd recorded one container, restarted while creating the other, and synchronized the plugin again.
	rt.addContainer("recorded")
	if _, err := p.Synchronize(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !p.Reconcile(ctx) {
		t.Error("Reconcile left something unsettled")
	}
	if ids, _ := layers.IDs(); !slices.Equal(ids, []string{"recorded"}) {
		t.Errorf("layers left = %v, want [recorded]", ids)
	}
}

func TestReconcileDoesNotWaitForAContainerOnceContainerdGaveUp(t *testing.T) {
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.snapshots["ctr"] = "/snapshots/7"
	p := synchronized(t, rt, layers)
	// containerd stopped waiting while the layer was being attached: the request is over when Attach returns.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := p.CreateContainer(ctx, pod(""), container("ctr", "app")); err != nil {
		t.Fatal(err)
	}
	if _, ok := layers.get("ctr"); !ok {
		t.Fatal("the layer was not attached")
	}

	if !p.Reconcile(context.Background()) {
		t.Error("Reconcile left something unsettled")
	}
	if l, ok := layers.get("ctr"); ok {
		t.Errorf("layer %+v left behind", l)
	}
}

func TestReconcileKeepsAttachedLayersWhenContainerdCannotAnswer(t *testing.T) {
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.err = errors.New("containerd is gone")
	layers.put("a", fakeLayer{mounted: true})
	layers.put("b", fakeLayer{mounted: true})
	// What is not a layer needs no answer from containerd.
	layers.put("rebooted", fakeLayer{})
	layers.put("interrupted", fakeLayer{mounted: true, incomplete: true})

	if synchronized(t, rt, layers).Reconcile(context.Background()) {
		t.Error("Reconcile reported everything as settled")
	}

	ids, _ := layers.IDs()
	if want := []string{"a", "b"}; !slices.Equal(ids, want) {
		t.Errorf("layers left = %v, want %v", ids, want)
	}
	for _, id := range ids {
		if l, _ := layers.get(id); !l.mounted {
			t.Errorf("layer %s got unmounted", id)
		}
	}
	if got := rt.askedCount(); got != 1 {
		t.Errorf("asked containerd %d times, want once: it did not answer", got)
	}
}

func TestSynchronizeReconcilesInTheBackground(t *testing.T) {
	rt, layers := newFakeRuntime(), newFakeLayers()
	layers.put("removed", fakeLayer{mounted: true})
	p := newPlugin(t, rt, layers, sizing.Policy{})
	runReconciler(t, p, time.Hour, time.Hour)

	if _, err := p.Synchronize(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Synchronize to reconcile", func() bool {
		_, ok := layers.get("removed")
		return !ok
	})
}

func TestRunReconcilerLooksAgainSoonAfterAnUnsettledRun(t *testing.T) {
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.err = errors.New("containerd is restarting")
	layers.put("removed", fakeLayer{mounted: true})
	p := synchronized(t, rt, layers)
	runReconciler(t, p, time.Hour, 10*time.Millisecond)

	waitFor(t, "the first run", func() bool { return rt.askedCount() > 0 })
	rt.fail(nil)
	// Nothing kicks the reconciler again: it comes back by itself, long before the hour is over.
	waitFor(t, "the reconciler to look again", func() bool {
		_, ok := layers.get("removed")
		return !ok
	})
}

func TestDrainWaitsForTheRequestBeingHandled(t *testing.T) {
	ctx := context.Background()
	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.snapshots["ctr"] = "/snapshots/7"
	rt.snapshots["late"] = "/snapshots/8"
	layers.attaching, layers.release = make(chan string), make(chan struct{})
	p := newPlugin(t, rt, layers, sizing.Policy{})

	created := make(chan error, 1)
	go func() {
		_, _, err := p.CreateContainer(ctx, pod(`{"app":{"limit":"1Gi"}}`), container("ctr", "app"))
		created <- err
	}()
	<-layers.attaching

	drained := make(chan struct{})
	go func() {
		p.Drain()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("Drain did not wait for the layer being attached")
	case <-time.After(50 * time.Millisecond):
	}

	close(layers.release)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	<-drained
	if l, ok := layers.get("ctr"); !ok || !l.mounted {
		t.Errorf("layer = %+v, %v; want the layer that was being attached", l, ok)
	}

	// Draining again changes nothing.
	p.Drain()

	// A container that would get a layer is not created without one.
	layers.attaching = nil
	if _, _, err := p.CreateContainer(ctx, pod(`{"app":{"limit":"1Gi"}}`), container("late", "app")); !errors.Is(err, errDraining) {
		t.Errorf("CreateContainer error = %v, want %v", err, errDraining)
	}
	if l, ok := layers.get("late"); ok {
		t.Errorf("got a layer %+v after draining", l)
	}
	// One that gets none is.
	if _, _, err := p.CreateContainer(ctx, pod(""), container("plain", "app")); err != nil {
		t.Errorf("CreateContainer of a container without a layer: %v", err)
	}
	// A layer the plugin can no longer unmount is left for the next run.
	if err := p.RemoveContainer(ctx, pod(""), container("ctr", "app")); !errors.Is(err, errDraining) {
		t.Errorf("RemoveContainer error = %v, want %v", err, errDraining)
	}
	if l, ok := layers.get("ctr"); !ok || !l.mounted {
		t.Errorf("layer = %+v, %v; want it left mounted", l, ok)
	}
}

// waitFor polls until done reports true.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestKeyedLockerSerializesOneKey(t *testing.T) {
	var k keyedLocker
	unlockA := k.lock("a")
	// Another key is not held up.
	k.lock("b")()

	// The key is locked a second time, and unlocked again.
	done := make(chan struct{})
	go func() {
		k.lock("a")()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a second lock of the same key did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	<-done

	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.locks) != 0 {
		t.Errorf("%d locks left behind", len(k.locks))
	}
}
