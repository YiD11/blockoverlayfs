package snapshot

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"

	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/containermeta"
)

const (
	gi      = int64(1) << 30
	baseKey = "k8s.io/2/base"
	// appKey is an app container's rootfs on top of baseKey. The inner overlay gives it ID 2.
	appKey = "k8s.io/12/app"
	appID  = "2"
)

var ctx = context.Background()

type harness struct {
	sn      *Snapshotter
	inner   *fakeInner
	layers  *fakeLayers
	decider *fakeDecider
	ev      *events
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	dir := t.TempDir()
	ev := &events{}
	h := &harness{
		inner:   newFakeInner(filepath.Join(dir, "overlay"), ev),
		layers:  newFakeLayers(filepath.Join(dir, "blocks"), ev),
		decider: &fakeDecider{decisions: map[string]Decision{}},
		ev:      ev,
	}
	if opts.Decider == nil {
		opts.Decider = h.decider
	}
	h.sn = New(h.inner, h.layers, opts)
	for _, step := range []error{
		func() error { _, err := h.inner.Prepare(ctx, "k8s.io/1/base-tmp", ""); return err }(),
		h.inner.Commit(ctx, baseKey, "k8s.io/1/base-tmp"),
		func() error { _, err := h.inner.Prepare(ctx, appKey, baseKey); return err }(),
	} {
		if step != nil {
			t.Fatal(step)
		}
	}
	ev.list = nil
	return h
}

func (h *harness) requireEvents(t *testing.T, want ...string) {
	t.Helper()
	if got := h.ev.all(); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

func (h *harness) innerMounts(t *testing.T, key string) []mount.Mount {
	t.Helper()
	m, err := h.inner.Mounts(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMountsCreatesABlockLayerOnce(t *testing.T) {
	h := newHarness(t, Options{})
	h.decider.decisions[appKey] = Decision{Final: true, Size: 3 * gi, Reason: "limit"}
	l := h.layers.Layer(appID)
	for i := 0; i < 2; i++ {
		mounts, err := h.sn.Mounts(ctx, appKey)
		if err != nil {
			t.Fatal(err)
		}
		opts := mounts[0].Options
		if !slices.Contains(opts, "upperdir="+l.Upper()) || !slices.Contains(opts, "workdir="+l.Work()) || !slices.Contains(opts, "lowerdir="+h.inner.upper("1")) {
			t.Fatalf("mount options = %q, want the block layer over the base layer", opts)
		}
	}
	if n := h.decider.asked[appKey]; n != 1 {
		t.Fatalf("decided %d times, want once", n)
	}
	h.requireEvents(t, "create:2:3221225472", "ensure-mounted:2")
	if want := []string{h.inner.upper(appID), h.inner.upper(appID)}; !slices.Equal(h.layers.ownerFroms, want) {
		t.Fatalf("layer took its owner from %q, want the directory upper %q", h.layers.ownerFroms, want)
	}
	if st := h.layers.states[appID]; st.Key != appKey || st.Size != 3*gi {
		t.Fatalf("recorded %+v", st)
	}
}

func TestMountsPassesUndecidedSnapshotsThrough(t *testing.T) {
	h := newHarness(t, Options{})
	want := h.innerMounts(t, appKey)
	for i := 0; i < 2; i++ {
		got, err := h.sn.Mounts(ctx, appKey)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Mounts() = %+v, want the inner mounts %+v", got, want)
		}
	}
	// Nothing is recorded, so a later Mounts asks again: CRI mounts the rootfs before the container exists.
	if n := h.decider.asked[appKey]; n != 2 {
		t.Fatalf("decided %d times, want twice", n)
	}
	h.requireEvents(t)
}

func TestMountsWithoutDeciderStillServesRecordedLayers(t *testing.T) {
	h := newHarness(t, Options{})
	sn := New(h.inner, h.layers, Options{})
	got, err := sn.Mounts(ctx, appKey)
	if err != nil || !reflect.DeepEqual(got, h.innerMounts(t, appKey)) {
		t.Fatalf("Mounts() = %+v, %v; want the inner mounts", got, err)
	}
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}
	got, err = sn.Mounts(ctx, appKey)
	if err != nil || !slices.Contains(got[0].Options, "upperdir="+h.layers.Layer(appID).Upper()) {
		t.Fatalf("Mounts() = %+v, %v; want the recorded block layer", got, err)
	}
}

func TestMountsRecordsADirectoryDecision(t *testing.T) {
	h := newHarness(t, Options{})
	h.decider.decisions[appKey] = Decision{Final: true, Reason: "nothing declared"}
	want := h.innerMounts(t, appKey)
	for i := 0; i < 2; i++ {
		got, err := h.sn.Mounts(ctx, appKey)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Mounts() = %+v, %v; want the inner mounts", got, err)
		}
	}
	if n := h.decider.asked[appKey]; n != 1 {
		t.Fatalf("decided %d times, want once", n)
	}
	h.requireEvents(t, "record-directory:2")
}

func TestMountsKeepsADirectoryUpperAlreadyInUse(t *testing.T) {
	h := newHarness(t, Options{})
	h.layers.usedUpper = true
	h.decider.decisions[appKey] = Decision{Final: true, Size: gi, Reason: "limit"}
	got, err := h.sn.Mounts(ctx, appKey)
	if err != nil || !reflect.DeepEqual(got, h.innerMounts(t, appKey)) {
		t.Fatalf("Mounts() = %+v, %v; want the inner mounts", got, err)
	}
	if st := h.layers.states[appID]; st.Key != appKey || st.Size != 0 {
		t.Fatalf("recorded %+v, want a directory decision", st)
	}
	h.requireEvents(t, "record-directory:2")
}

func TestMountsSkipsSnapshotsThatAreNotActive(t *testing.T) {
	h := newHarness(t, Options{})
	const viewKey = "k8s.io/13/view"
	if _, err := h.inner.View(ctx, viewKey, baseKey); err != nil {
		t.Fatal(err)
	}
	h.decider.decisions[viewKey] = Decision{Final: true, Size: gi}
	got, err := h.sn.Mounts(ctx, viewKey)
	if err != nil || !reflect.DeepEqual(got, h.innerMounts(t, viewKey)) {
		t.Fatalf("Mounts() = %+v, %v; want the inner mounts", got, err)
	}
	if n := h.decider.asked[viewKey]; n != 0 {
		t.Fatalf("decided %d times for a view, want never", n)
	}
}

func TestMountsDiscardsALayerRecordedForAnotherSnapshot(t *testing.T) {
	h := newHarness(t, Options{})
	h.layers.states[appID] = block.State{Key: "k8s.io/99/old", Size: gi}
	h.decider.decisions[appKey] = Decision{Final: true, Size: 2 * gi}
	if _, err := h.sn.Mounts(ctx, appKey); err != nil {
		t.Fatal(err)
	}
	h.requireEvents(t, "detach:2", "create:2:2147483648")
}

func TestMountsFails(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*harness)
	}{
		{"unreadable state", func(h *harness) { h.layers.loadErr[appID] = block.ErrCorrupt }},
		{"layer cannot be created", func(h *harness) {
			h.layers.createErr = errInjected
			h.decider.decisions[appKey] = Decision{Final: true, Size: gi}
		}},
		{"decider fails", func(h *harness) { h.decider.err = errInjected }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, Options{})
			tt.setup(h)
			if mounts, err := h.sn.Mounts(ctx, appKey); err == nil {
				t.Fatalf("Mounts() = %+v, want an error rather than the directory upper", mounts)
			}
		})
	}
}

func TestUsage(t *testing.T) {
	h := newHarness(t, Options{})
	usage := func(key string) snapshots.Usage {
		t.Helper()
		u, err := h.sn.Usage(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if u := usage(appKey); u.Size != 1 {
		t.Fatalf("undecided snapshot usage = %+v, want the inner overlay's", u)
	}
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}
	if u := usage(appKey); u.Size != 42 || u.Inodes != 7 {
		t.Fatalf("block layer usage = %+v, want the block store's", u)
	}
	h.layers.states[appID] = block.State{Key: appKey}
	if u := usage(appKey); u.Size != 1 {
		t.Fatalf("directory layer usage = %+v, want the inner overlay's", u)
	}
	if u := usage(baseKey); u.Size != 1 {
		t.Fatalf("committed layer usage = %+v, want the inner overlay's", u)
	}
}

func TestStatAndWalkLabels(t *testing.T) {
	for _, show := range []bool{false, true} {
		h := newHarness(t, Options{UpperdirLabel: show})
		h.layers.states[appID] = block.State{Key: appKey, Size: gi}
		l := h.layers.Layer(appID)

		app, err := h.sn.Stat(ctx, appKey)
		if err != nil {
			t.Fatal(err)
		}
		base, err := h.sn.Stat(ctx, baseKey)
		if err != nil {
			t.Fatal(err)
		}
		wantApp := map[string]string{WritableMountpointLabel: l.Mountpoint()}
		var wantBase map[string]string
		if show {
			wantApp[upperdirLabel] = l.Upper()
			wantBase = map[string]string{upperdirLabel: h.inner.upper("1")}
		}
		if !maps.Equal(app.Labels, wantApp) || !reflect.DeepEqual(base.Labels, wantBase) {
			t.Fatalf("upperdir_label=%v: labels %v and %v, want %v and %v", show, app.Labels, base.Labels, wantApp, wantBase)
		}

		walked := map[string]snapshots.Info{}
		if err := h.sn.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
			walked[info.Name] = info
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(walked[appKey], app) || !reflect.DeepEqual(walked[baseKey], base) {
			t.Fatalf("upperdir_label=%v: Walk() = %+v, want what Stat returns", show, walked)
		}
	}
}

func TestUpdateDoesNotStoreComputedLabels(t *testing.T) {
	h := newHarness(t, Options{UpperdirLabel: true})
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}
	info, err := h.sn.Stat(ctx, appKey)
	if err != nil {
		t.Fatal(err)
	}
	info.Labels["l1"] = "v1"
	updated, err := h.sn.Update(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	if stored := slices.Sorted(maps.Keys(h.inner.snaps[appKey].labels)); !slices.Equal(stored, []string{"l1"}) {
		t.Fatalf("inner overlay stored labels %v, want only l1", stored)
	}
	if updated.Labels["l1"] != "v1" || updated.Labels[WritableMountpointLabel] == "" {
		t.Fatalf("Update() = %v, want l1 and the computed labels", updated.Labels)
	}
}

func TestRemoveDetachesAfterTheInnerSnapshotIsGone(t *testing.T) {
	h := newHarness(t, Options{})
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}
	if err := h.sn.Remove(ctx, appKey); err != nil {
		t.Fatal(err)
	}
	h.requireEvents(t, "inner-remove:"+appKey, "detach:2")
}

func TestRemoveKeepsTheLayerWhenTheInnerRemoveFails(t *testing.T) {
	h := newHarness(t, Options{})
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}
	h.inner.removeErr = errInjected
	if err := h.sn.Remove(ctx, appKey); !errors.Is(err, errInjected) {
		t.Fatalf("Remove() = %v, want the inner error", err)
	}
	h.requireEvents(t)
	if _, ok := h.layers.states[appID]; !ok {
		t.Fatal("layer was dropped although the snapshot still exists")
	}
	if err := h.sn.Remove(ctx, "k8s.io/77/absent"); err == nil {
		t.Fatal("Remove() of an absent snapshot succeeded")
	}
}

func TestCommitPublishesTheBlockLayer(t *testing.T) {
	h := newHarness(t, Options{Containers: fakeContainers{}})
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}
	if err := os.WriteFile(filepath.Join(h.inner.upper(appID), "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const committed = "k8s.io/20/committed"
	if err := h.sn.Commit(ctx, committed, appKey); err != nil {
		t.Fatal(err)
	}
	h.requireEvents(t, "ensure-mounted:2", "copy-upper:2", "inner-commit:"+appKey, "detach:2")
	names, err := os.ReadDir(h.inner.upper(appID))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].Name() != "copied" {
		t.Fatalf("committed layer holds %v, want exactly the copy of the block layer", names)
	}
	if h.inner.snaps[committed].kind != snapshots.KindCommitted {
		t.Fatal("snapshot was not committed")
	}
}

func TestCommitRefusesALayerItsContainerStillUses(t *testing.T) {
	h := newHarness(t, Options{Containers: fakeContainers{"k8s.io/app": containermeta.Info{SnapshotKey: "app"}}})
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}
	if err := h.sn.Commit(ctx, "k8s.io/20/committed", appKey); !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("Commit() = %v, want a failed precondition", err)
	}
	h.requireEvents(t)
	if h.inner.snaps[appKey].kind != snapshots.KindActive {
		t.Fatal("snapshot is no longer active")
	}
}

func TestCommitOfOtherLayers(t *testing.T) {
	t.Run("directory layer", func(t *testing.T) {
		h := newHarness(t, Options{})
		h.layers.states[appID] = block.State{Key: appKey}
		if err := h.sn.Commit(ctx, "k8s.io/20/committed", appKey); err != nil {
			t.Fatal(err)
		}
		h.requireEvents(t, "inner-commit:"+appKey, "detach:2")
	})
	t.Run("nothing recorded", func(t *testing.T) {
		h := newHarness(t, Options{})
		if err := h.sn.Commit(ctx, "k8s.io/20/committed", appKey); err != nil {
			t.Fatal(err)
		}
		h.requireEvents(t, "inner-commit:"+appKey)
	})
}

func TestCleanupReclaimsOrphanedLayers(t *testing.T) {
	h := newHarness(t, Options{})
	h.layers.states[appID] = block.State{Key: appKey, Size: gi}            // live: kept
	h.layers.states["1"] = block.State{Key: "k8s.io/1/base-tmp", Size: gi} // the ID now belongs to the committed base
	h.layers.states["9"] = block.State{Key: "k8s.io/9/gone", Size: gi}     // no snapshot at all
	h.layers.loadErr["8"] = block.ErrCorrupt                               // unreadable, no snapshot
	if err := h.sn.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	h.requireEvents(t, "inner-cleanup", "detach:1", "detach:8", "detach:9", "reap")
	if ids := slices.Sorted(maps.Keys(h.layers.states)); !slices.Equal(ids, []string{appID}) {
		t.Fatalf("layers left: %v", ids)
	}
}

// containerd's overlay reports not found from Walk while its metadata has never held a snapshot.
func TestCleanupWhenTheInnerOverlayHasNoSnapshots(t *testing.T) {
	h := newHarness(t, Options{})
	h.inner.walkErr = errdefs.ErrNotFound
	h.layers.states["9"] = block.State{Key: "k8s.io/9/gone", Size: gi}
	if err := h.sn.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	h.requireEvents(t, "inner-cleanup", "detach:9", "reap")

	h.inner.walkErr = errInjected
	h.layers.states["9"] = block.State{Key: "k8s.io/9/gone", Size: gi}
	if err := h.sn.Cleanup(ctx); !errors.Is(err, errInjected) {
		t.Fatalf("Cleanup() = %v, want the walk error", err)
	}
}

func TestCleanupKeepsAnUnreadableLayerOfALiveSnapshot(t *testing.T) {
	h := newHarness(t, Options{})
	h.layers.loadErr[appID] = block.ErrCorrupt
	if err := h.sn.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	h.requireEvents(t, "inner-cleanup", "reap")
}

func TestCloseIsIdempotent(t *testing.T) {
	h := newHarness(t, Options{})
	for i := 0; i < 2; i++ {
		if err := h.sn.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if h.layers.closedCount != 1 {
		t.Fatalf("block store closed %d times", h.layers.closedCount)
	}
	h.requireEvents(t, "inner-close")
}
