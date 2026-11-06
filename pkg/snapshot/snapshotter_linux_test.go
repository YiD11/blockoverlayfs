//go:build linux

package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/testutil"
	"github.com/containerd/containerd/snapshots"
	"golang.org/x/sys/unix"

	"github.com/YiD11/blockoverlayfs/pkg/block"
)

const layerSize = int64(64) << 20

// fixedDecider gives a block layer of size to every key that match accepts, or to every key without match.
type fixedDecider struct {
	size  int64
	match func(key string) bool
}

func (d fixedDecider) Decide(_ context.Context, key string) (Decision, error) {
	if d.match != nil && !d.match(key) {
		return Decision{}, nil
	}
	return Decision{Final: true, Size: d.size, Reason: "test"}, nil
}

// refusingDecider fails the test if anything still needs deciding.
type refusingDecider struct{ t *testing.T }

func (d refusingDecider) Decide(_ context.Context, key string) (Decision, error) {
	d.t.Errorf("asked to decide %s again", key)
	return Decision{}, fmt.Errorf("unexpected decision for %s", key)
}

// newRealSnapshotter builds the wrapper as the daemon does, over a real overlay and a real block store.
func newRealSnapshotter(ctx context.Context, root string, d Decider) (*Snapshotter, *block.Store, error) {
	inner, err := NewOverlay(filepath.Join(root, "overlay"), OverlayOptions{})
	if err != nil {
		return nil, nil, err
	}
	layers, err := block.Open(ctx, filepath.Join(root, "blocks"))
	if err != nil {
		_ = inner.Close()
		return nil, nil, err
	}
	return New(inner, layers, Options{Decider: d}), layers, nil
}

func realSnapshotter(t *testing.T, root string, d Decider) (*Snapshotter, *block.Store) {
	t.Helper()
	sn, layers, err := newRealSnapshotter(ctx, root, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		detachAll(ctx, layers)
		_ = sn.Close()
	})
	return sn, layers
}

// detachAll unmounts every layer left, so deleting the test directory never deletes through a mounted image.
func detachAll(ctx context.Context, layers *block.Store) {
	ids, _ := layers.Decided()
	for _, id := range ids {
		_ = layers.Detach(ctx, id)
	}
	layers.Reap(ctx)
}

func containerKey(key string) bool { return strings.HasPrefix(key, "ctr-") }

// commitLayer creates a committed snapshot holding files, the way an image layer is unpacked.
func commitLayer(t *testing.T, sn snapshots.Snapshotter, name, parent string, files map[string]string) {
	t.Helper()
	mounts, err := sn.Prepare(ctx, name+"-active", parent)
	if err != nil {
		t.Fatal(err)
	}
	withMounted(t, mounts, func(dir string) {
		for path, content := range files {
			if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := sn.Commit(ctx, name, name+"-active"); err != nil {
		t.Fatal(err)
	}
}

func withMounted(t *testing.T, mounts []mount.Mount, fn func(dir string)) {
	t.Helper()
	dir := t.TempDir()
	if err := mount.All(mounts, dir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mount.UnmountAll(dir, 0); err != nil {
			t.Fatal(err)
		}
	}()
	fn(dir)
}

func mountsOf(t *testing.T, sn snapshots.Snapshotter, key string) []mount.Mount {
	t.Helper()
	mounts, err := sn.Mounts(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	return mounts
}

func loopDevicesUnder(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob("/sys/block/loop*/loop/backing_file")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if data, err := os.ReadFile(f); err == nil && strings.HasPrefix(string(data), dir+"/") {
			n++
		}
	}
	return n
}

func requireNothingLeft(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{filepath.Join(root, "blocks"), filepath.Join(root, "overlay", "snapshots")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("%s still holds %d entries", dir, len(entries))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for loopDevicesUnder(t, root) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("loop devices backed by the test's images are still attached")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestBlockLayerLifecycle(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	sn, layers := realSnapshotter(t, root, fixedDecider{size: layerSize, match: containerKey})
	commitLayer(t, sn, "base", "", map[string]string{"keep": "k", "gone": "g"})

	if _, err := sn.Prepare(ctx, "ctr-1", "base"); err != nil {
		t.Fatal(err)
	}
	info, err := sn.Stat(ctx, "ctr-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := info.Labels[WritableMountpointLabel]; ok {
		t.Fatal("a snapshot has a block layer before its first Mounts")
	}
	withMounted(t, mountsOf(t, sn, "ctr-1"), func(dir string) {
		var st unix.Statfs_t
		if err := unix.Statfs(dir, &st); err != nil {
			t.Fatal(err)
		}
		//nolint:gosec // The block count of a 64Mi filesystem fits in an int64.
		if total := int64(st.Blocks) * st.Bsize; total > layerSize {
			t.Fatalf("rootfs reports %d bytes, want at most the layer's %d", total, layerSize)
		}
		if err := os.WriteFile(filepath.Join(dir, "added"), []byte("a"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
			t.Fatal(err)
		}
	})
	info, err = sn.Stat(ctx, "ctr-1")
	if err != nil {
		t.Fatal(err)
	}
	mountpoint := info.Labels[WritableMountpointLabel]
	if !strings.HasPrefix(mountpoint, layers.Root()+"/") {
		t.Fatalf("writable mountpoint label = %q, want a layer under %s", mountpoint, layers.Root())
	}
	if data, err := os.ReadFile(filepath.Join(mountpoint, "upper", "added")); err != nil || string(data) != "a" {
		t.Fatalf("block layer holds %q, %v; want the container's write", data, err)
	}
	if u, err := sn.Usage(ctx, "ctr-1"); err != nil || u.Size <= 0 || u.Inodes <= 0 {
		t.Fatalf("Usage() = %+v, %v", u, err)
	}

	// Commit copies the layer into the committed snapshot, and a child sees the write and the deletion.
	if err := sn.Commit(ctx, "ctr-1-committed", "ctr-1"); err != nil {
		t.Fatal(err)
	}
	mounts, err := sn.View(ctx, "view", "ctr-1-committed")
	if err != nil {
		t.Fatal(err)
	}
	withMounted(t, mounts, func(dir string) {
		for path, want := range map[string]string{"keep": "k", "added": "a"} {
			if data, err := os.ReadFile(filepath.Join(dir, path)); err != nil || string(data) != want {
				t.Fatalf("%s = %q, %v; want %q", path, data, err, want)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "gone")); !os.IsNotExist(err) {
			t.Fatalf("deleted file is back: %v", err)
		}
	})
	if ids, err := layers.Decided(); err != nil || len(ids) != 0 {
		t.Fatalf("layers after commit = %v, %v; want the block layer dropped", ids, err)
	}

	for _, key := range []string{"view", "ctr-1-committed", "base"} {
		if err := sn.Remove(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := sn.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	requireNothingLeft(t, root)
}

// A node restart unmounts every layer; the restarted daemon mounts it again from what it recorded.
func TestBlockLayerSurvivesARestart(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	sn, _ := realSnapshotter(t, root, fixedDecider{size: layerSize, match: containerKey})
	commitLayer(t, sn, "base", "", map[string]string{"keep": "k"})
	if _, err := sn.Prepare(ctx, "ctr-1", "base"); err != nil {
		t.Fatal(err)
	}
	withMounted(t, mountsOf(t, sn, "ctr-1"), func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte("d"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	info, err := sn.Stat(ctx, "ctr-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Unmount(info.Labels[WritableMountpointLabel], 0); err != nil {
		t.Fatal(err)
	}
	if err := sn.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, _ := realSnapshotter(t, root, refusingDecider{t})
	withMounted(t, mountsOf(t, restarted, "ctr-1"), func(dir string) {
		if data, err := os.ReadFile(filepath.Join(dir, "data")); err != nil || string(data) != "d" {
			t.Fatalf("data after restart = %q, %v", data, err)
		}
	})
}

func TestRemoveReleasesTheBlockLayer(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	sn, _ := realSnapshotter(t, root, fixedDecider{size: layerSize, match: containerKey})
	commitLayer(t, sn, "base", "", map[string]string{"keep": "k"})
	if _, err := sn.Prepare(ctx, "ctr-1", "base"); err != nil {
		t.Fatal(err)
	}
	mountsOf(t, sn, "ctr-1")
	if n := loopDevicesUnder(t, root); n != 1 {
		t.Fatalf("%d loop devices back the layer, want 1", n)
	}
	for _, key := range []string{"ctr-1", "base"} {
		if err := sn.Remove(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := sn.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	requireNothingLeft(t, root)
}

// Run with -race: Mounts, Usage, Stat, Walk, Remove and Cleanup overlap on many containers at once.
func TestConcurrentContainers(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	sn, _ := realSnapshotter(t, root, fixedDecider{size: 32 << 20, match: containerKey})
	commitLayer(t, sn, "base", "", map[string]string{"keep": "k"})

	const containers = 8
	errs := make(chan error, containers*3)
	done := make(chan struct{})
	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if err := sn.Cleanup(ctx); err != nil {
				errs <- fmt.Errorf("cleanup: %w", err)
				return
			}
			if err := sn.Walk(ctx, func(context.Context, snapshots.Info) error { return nil }); err != nil {
				errs <- fmt.Errorf("walk: %w", err)
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < containers; i++ {
		key := fmt.Sprintf("ctr-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sn.Prepare(ctx, key, "base"); err != nil {
				errs <- err
				return
			}
			var inner sync.WaitGroup
			for j := 0; j < 3; j++ {
				inner.Add(1)
				go func() {
					defer inner.Done()
					if _, err := sn.Mounts(ctx, key); err != nil {
						errs <- fmt.Errorf("mounts %s: %w", key, err)
					}
					if _, err := sn.Usage(ctx, key); err != nil {
						errs <- fmt.Errorf("usage %s: %w", key, err)
					}
					if _, err := sn.Stat(ctx, key); err != nil {
						errs <- fmt.Errorf("stat %s: %w", key, err)
					}
				}()
			}
			inner.Wait()
			if err := sn.Remove(ctx, key); err != nil {
				errs <- fmt.Errorf("remove %s: %w", key, err)
			}
		}()
	}
	wg.Wait()
	close(done)
	background.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if err := sn.Remove(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	if err := sn.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	requireNothingLeft(t, root)
}
