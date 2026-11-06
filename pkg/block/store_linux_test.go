//go:build linux

package block

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/testutil"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
)

const testSize = int64(64) << 20

// ownerDir stands in for the inner overlay's directory upper, whose owner and mode a new upper takes.
func ownerDir(t *testing.T, uid, gid int, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(dir, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func createLayer(t *testing.T, s *Store, id string, size int64, owner string) Layer {
	t.Helper()
	ctx := context.Background()
	if err := s.Create(ctx, id, State{Key: "k8s.io/" + id + "/ctr", Size: size}, owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Detach(ctx, id)
		s.Reap(ctx)
	})
	return s.Layer(id)
}

func requireMounted(t *testing.T, path string, want bool) {
	t.Helper()
	got, err := mountinfo.Mounted(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s mounted = %v, want %v", path, got, want)
	}
}

// loopDevicesUnder counts the loop devices whose backing file lies under dir, deleted or not.
func loopDevicesUnder(t *testing.T, dir string) int {
	t.Helper()
	files, err := filepath.Glob("/sys/block/loop*/loop/backing_file")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err == nil && strings.HasPrefix(string(data), dir+"/") {
			n++
		}
	}
	return n
}

func waitNoLoopDevices(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for loopDevicesUnder(t, dir) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("loop devices backed by files under %s are still attached", dir)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCreateMountsSizedLayer(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t, t.TempDir())
	l := createLayer(t, s, "1", testSize, ownerDir(t, 1000, 1001, 0o750))

	requireMounted(t, l.Mountpoint(), true)
	var fsst unix.Statfs_t
	if err := unix.Statfs(l.Mountpoint(), &fsst); err != nil {
		t.Fatal(err)
	}
	if fsst.Type != unix.EXT4_SUPER_MAGIC {
		t.Fatalf("filesystem type = %#x, want ext4", fsst.Type)
	}
	if total := toInt64(fsst.Blocks) * fsst.Bsize; total > testSize || total < testSize*3/4 {
		t.Fatalf("filesystem size = %d, want close to %d", total, testSize)
	}

	fi, err := os.Stat(l.Upper())
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if st.Uid != 1000 || st.Gid != 1001 || fi.Mode().Perm() != 0o750 {
		t.Fatalf("upper is %d:%d %v, want 1000:1001 0750", st.Uid, st.Gid, fi.Mode().Perm())
	}
	if fi, err := os.Stat(l.Work()); err != nil || fi.Mode().Perm() != 0o711 {
		t.Fatalf("work = %v, %v; want mode 0711", fi, err)
	}

	var ist unix.Stat_t
	if err := unix.Stat(l.Image(), &ist); err != nil {
		t.Fatal(err)
	}
	if allocated := ist.Blocks * 512; allocated > testSize/4 {
		t.Fatalf("image allocates %d of %d bytes, want it sparse", allocated, testSize)
	}
	if got, ok, err := s.Load("1"); err != nil || !ok || got.Size != testSize {
		t.Fatalf("Load() = %+v, %v, %v", got, ok, err)
	}
	if err := s.Create(context.Background(), "1", State{Key: "k", Size: testSize}, l.Upper()); err == nil {
		t.Fatal("creating an existing layer succeeded")
	}
}

func TestWritesStopAtTheLayerSize(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t, t.TempDir())
	l := createLayer(t, s, "1", 32<<20, ownerDir(t, 0, 0, 0o755))

	before, err := s.Usage("1")
	if err != nil {
		t.Fatal(err)
	}
	fill := filepath.Join(l.Upper(), "fill")
	f, err := os.Create(fill)
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte{1}, 1<<20)
	var werr error
	for i := 0; i < 64 && werr == nil; i++ {
		_, werr = f.Write(chunk)
	}
	if werr == nil {
		werr = f.Sync()
	}
	_ = f.Close()
	if !errors.Is(werr, syscall.ENOSPC) {
		t.Fatalf("writing 64Mi into a 32Mi layer gave %v, want ENOSPC", werr)
	}
	full, err := s.Usage("1")
	if err != nil {
		t.Fatal(err)
	}
	if full.Size <= before.Size+16<<20 || full.Size > 32<<20 || full.Inodes <= before.Inodes {
		t.Fatalf("usage went from %+v to %+v, want it to grow to at most 32Mi", before, full)
	}

	if err := os.Remove(fill); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Upper(), "ok"), chunk, 0o600); err != nil {
		t.Fatalf("writing after freeing space: %v", err)
	}
}

// A node restart unmounts every layer; a restarted daemon finds the layer on disk and mounts it again.
func TestEnsureMountedAfterRestart(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	s := openStore(t, root)
	owner := ownerDir(t, 1000, 1000, 0o755)
	l := createLayer(t, s, "1", testSize, owner)
	if err := os.WriteFile(filepath.Join(l.Upper(), "keep"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Unmount(l.Mountpoint(), 0); err != nil {
		t.Fatal(err)
	}
	requireMounted(t, l.Mountpoint(), false)
	if u, err := s.Usage("1"); err != nil || u.Size <= 0 {
		t.Fatalf("Usage() of an unmounted layer = %+v, %v; want the image's allocated bytes", u, err)
	}

	restarted := openStore(t, root)
	if err := restarted.EnsureMounted(context.Background(), "1", owner); err != nil {
		t.Fatal(err)
	}
	requireMounted(t, l.Mountpoint(), true)
	if data, err := os.ReadFile(filepath.Join(l.Upper(), "keep")); err != nil || string(data) != "data" {
		t.Fatalf("file after remount = %q, %v", data, err)
	}
	if err := restarted.EnsureMounted(context.Background(), "1", owner); err != nil {
		t.Fatalf("EnsureMounted() on a mounted layer = %v", err)
	}
}

func TestDetachReleasesTheLoopDevice(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	s := openStore(t, root)
	l := createLayer(t, s, "1", testSize, ownerDir(t, 0, 0, 0o755))
	if n := loopDevicesUnder(t, root); n != 1 {
		t.Fatalf("%d loop devices back the layer, want 1", n)
	}
	if err := s.Detach(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	requireMounted(t, l.Mountpoint(), false)
	if _, err := os.Stat(l.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("layer directory after Detach: %v", err)
	}
	s.Reap(context.Background())
	if names := entries(t, root); len(names) != 0 {
		t.Fatalf("store holds %v after Reap", names)
	}
	waitNoLoopDevices(t, root)
}

func TestCreateFailuresLeaveNothing(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	s := openStore(t, root)

	// mkfs.ext4 refuses an image this small.
	if err := s.Create(context.Background(), "1", State{Key: "k", Size: 4096}, ownerDir(t, 0, 0, 0o755)); err == nil {
		t.Fatal("Create() of a 4KiB layer succeeded")
	}
	// The image mounts, then the upper cannot be made: the recorded layer is withdrawn again.
	if err := s.Create(context.Background(), "2", State{Key: "k", Size: testSize}, filepath.Join(root, "missing")); err == nil {
		t.Fatal("Create() with a missing owner directory succeeded")
	}
	if _, ok, err := s.Load("2"); ok || err != nil {
		t.Fatalf("Load() of a withdrawn layer = %v, %v", ok, err)
	}
	s.Reap(context.Background())
	if names := entries(t, root); len(names) != 0 {
		t.Fatalf("store holds %v after failed creations", names)
	}
	waitNoLoopDevices(t, root)
}

// A trash directory can still hold a mounted image after a crash; it must be unmounted, not deleted through.
func TestOpenUnmountsBeforeSweeping(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	s := openStore(t, root)
	l := createLayer(t, s, "1", testSize, ownerDir(t, 0, 0, 0o755))
	trash := filepath.Join(root, trashPrefix+"1-1")
	if err := os.Rename(l.Dir, trash); err != nil {
		t.Fatal(err)
	}
	requireMounted(t, filepath.Join(trash, "rw"), true)

	openStore(t, root)
	if names := entries(t, root); len(names) != 0 {
		t.Fatalf("store holds %v after Open", names)
	}
	waitNoLoopDevices(t, root)
}

func TestReplaceable(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t, t.TempDir())
	base := t.TempDir()
	for _, dir := range []string{"lower", "upper", "work", "merged", "written"} {
		if err := os.Mkdir(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	upper := filepath.Join(base, "upper")
	if ok, reason, err := s.Replaceable(upper); err != nil || !ok {
		t.Fatalf("Replaceable() of an unused empty upper = %v, %q, %v", ok, reason, err)
	}

	if err := os.WriteFile(filepath.Join(base, "written", "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, reason, err := s.Replaceable(filepath.Join(base, "written")); err != nil || ok || reason == "" {
		t.Fatalf("Replaceable() of an upper with content = %v, %q, %v", ok, reason, err)
	}

	m := mount.Mount{Type: "overlay", Source: "overlay", Options: []string{
		"lowerdir=" + filepath.Join(base, "lower"), "upperdir=" + upper, "workdir=" + filepath.Join(base, "work"),
	}}
	if err := m.Mount(filepath.Join(base, "merged")); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mount.UnmountAll(filepath.Join(base, "merged"), 0) }()
	if ok, reason, err := s.Replaceable(upper); err != nil || ok || !strings.Contains(reason, "merged") {
		t.Fatalf("Replaceable() of an upper in use = %v, %q, %v", ok, reason, err)
	}
}

func TestCopyUpperKeepsOverlayMetadata(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	s := openStore(t, root)
	l := createLayer(t, s, "1", testSize, ownerDir(t, 0, 0, 0o755))
	up := l.Upper()
	if err := os.WriteFile(filepath.Join(up, "added"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(up, "opaque"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(up, "opaque"), "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(filepath.Join(up, "deleted"), unix.S_IFCHR, 0); err != nil {
		t.Fatal(err)
	}

	copied, err := s.CopyUpper(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(copied)
	if data, err := os.ReadFile(filepath.Join(copied, "added")); err != nil || string(data) != "new" {
		t.Fatalf("added file = %q, %v", data, err)
	}
	buf := make([]byte, 8)
	if n, err := unix.Getxattr(filepath.Join(copied, "opaque"), "trusted.overlay.opaque", buf); err != nil || string(buf[:n]) != "y" {
		t.Fatalf("opaque xattr = %q, %v", buf[:n], err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(copied, "deleted"), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Rdev != 0 {
		t.Fatalf("whiteout = mode %o rdev %d, %v; want a 0:0 character device", st.Mode, st.Rdev, err)
	}
	if !slices.Contains(entries(t, root), filepath.Base(copied)) || !strings.HasPrefix(filepath.Base(copied), stagingPrefix) {
		t.Fatalf("copy %s is not a staging directory of the store", copied)
	}
}
