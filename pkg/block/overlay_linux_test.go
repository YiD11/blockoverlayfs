//go:build linux

package block

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/testutil"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay"
)

// TestContainerWritesLandInTheBlockLayer runs a block layer under containerd's own overlayfs snapshotter the way the
// plugin does on a node: the snapshotter prepares a container's rootfs, the layer is attached over the snapshot's
// directory, and the rootfs, mounted afterwards from the same mounts, writes into the layer until it is full.
func TestContainerWritesLandInTheBlockLayer(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.Background()
	sn, err := overlay.NewSnapshotter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sn.Close() })

	// An image of one layer holding one file.
	mounts, err := sn.Prepare(ctx, "extract", "")
	if err != nil {
		t.Fatal(err)
	}
	withMounted(t, mounts, func(root string) {
		if err := os.WriteFile(filepath.Join(root, "from-image"), []byte("image"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err := sn.Commit(ctx, "layer", "extract"); err != nil {
		t.Fatal(err)
	}

	// The container's rootfs, as CRI prepares it.
	mounts, err = sn.Prepare(ctx, "ctr", "layer")
	if err != nil {
		t.Fatal(err)
	}
	dir := snapshotDirOf(t, mounts)
	s := openStore(t)
	attach(t, s, "ctr", dir)

	// The shim mounts the rootfs later, from the snapshot's mounts.
	mounts, err = sn.Mounts(ctx, "ctr")
	if err != nil {
		t.Fatal(err)
	}
	withMounted(t, mounts, func(root string) {
		if data, err := os.ReadFile(filepath.Join(root, "from-image")); err != nil || string(data) != "image" {
			t.Errorf("file from the image layer = %q, %v", data, err)
		}
		written, err := fill(filepath.Join(root, "fill"))
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("filling the rootfs stopped after %d bytes with %v, want ENOSPC", written, err)
		}
		if written < testSize/2 || written > testSize {
			t.Errorf("the rootfs took %d bytes before ENOSPC, want about %d", written, testSize)
		}
	})

	// Everything the container wrote went to the image: the snapshotter's own upper directory stayed empty.
	if err := s.Unmount("ctr"); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "fs")); err != nil || len(entries) != 0 {
		t.Errorf("the snapshotter's upper directory holds %v, %v; want it empty", entries, err)
	}
	if _, err := s.Remove("ctr"); err != nil {
		t.Fatal(err)
	}
	// With the layer gone, the snapshotter removes the snapshot as usual.
	if err := sn.Remove(ctx, "ctr"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("snapshot directory left behind: %v", err)
	}
}

func withMounted(t *testing.T, mounts []mount.Mount, f func(root string)) {
	t.Helper()
	root := t.TempDir()
	if err := mount.All(mounts, root); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mount.UnmountAll(root, 0); err != nil {
			t.Error(err)
		}
	}()
	f(root)
}

// snapshotDirOf returns the directory holding the upper directory of an active snapshot's overlay mount.
func snapshotDirOf(t *testing.T, mounts []mount.Mount) string {
	t.Helper()
	for _, m := range mounts {
		for _, o := range m.Options {
			if upper, ok := strings.CutPrefix(o, "upperdir="); ok {
				return filepath.Dir(upper)
			}
		}
	}
	t.Fatalf("no upperdir in %+v", mounts)
	return ""
}

// fill writes to path until a write fails, and returns how much it wrote.
func fill(path string) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var written int64
	for written <= 2*testSize {
		n, err := f.Write(buf)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, errors.New("no limit")
}
