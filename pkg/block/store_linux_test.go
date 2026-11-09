//go:build linux

package block

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/pkg/testutil"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
)

const testSize = int64(64) << 20

// testXattr marks the directories a layer covers, to check that the layer takes their extended attributes over.
// Given root, trusted.* works on every filesystem a test directory may be on.
const testXattr = "trusted.blockoverlayfs-test"

// snapshotDir lays a directory out as containerd's overlayfs snapshotter prepares a container's rootfs.
func snapshotDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "snapshots", "1")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		path string
		mode fs.FileMode
	}{{dir, 0o700}, {filepath.Join(dir, "fs"), 0o755}, {filepath.Join(dir, "work"), 0o711}} {
		if err := os.Mkdir(d.path, d.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "layers"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// attach attaches a layer and makes sure the test leaves nothing mounted.
func attach(t *testing.T, s *Store, id, dir string) {
	t.Helper()
	t.Cleanup(func() {
		if err := s.Unmount(id); err != nil {
			t.Error(err)
		}
		if err := s.Remove(id); err != nil {
			t.Error(err)
		}
	})
	if err := s.Attach(context.Background(), id, dir, testSize); err != nil {
		t.Fatal(err)
	}
}

func checkAttr(t *testing.T, path string, uid, gid int, mode fs.FileMode, xattr string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Errorf("%s is owned by %d:%d, want %d:%d", path, st.Uid, st.Gid, uid, gid)
	}
	if got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != mode {
		t.Errorf("%s has mode %v, want %v", path, got, mode)
	}
	value, err := getXattr(path, testXattr)
	if xattr == "" {
		if !errors.Is(err, unix.ENODATA) {
			t.Errorf("%s has %s = %q, %v; want none", path, testXattr, value, err)
		}
		return
	}
	if err != nil || string(value) != xattr {
		t.Errorf("%s has %s = %q, %v; want %q", path, testXattr, value, err, xattr)
	}
}

func TestAttachCoversTheSnapshotDirectoryWithAnImage(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	dir := snapshotDir(t)
	upper := filepath.Join(dir, "fs")
	// The upper directory as the snapshotter leaves it for a user namespace: owned by the remapped root.
	if err := os.Lchown(upper, 100000, 100000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(upper, 0o750|fs.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lsetxattr(upper, testXattr, []byte("upper"), 0); err != nil {
		t.Fatal(err)
	}

	attach(t, s, "ctr", dir)

	if mounted, err := mountinfo.Mounted(dir); err != nil || !mounted {
		t.Fatalf("%s mounted = %v, %v; want true", dir, mounted, err)
	}
	if mounted, err := s.Mounted("ctr"); err != nil || !mounted {
		t.Fatalf("Mounted = %v, %v; want true", mounted, err)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	total := int64(st.Blocks) * st.Bsize //nolint:gosec // The block count of a 64Mi filesystem fits.
	if total > testSize || total < testSize*8/10 {
		t.Errorf("filesystem size = %d, want close to %d", total, testSize)
	}
	checkAttr(t, dir, 0, 0, 0o700, "")
	checkAttr(t, upper, 100000, 100000, 0o750|fs.ModeSetgid, "upper")
	checkAttr(t, filepath.Join(dir, "work"), 0, 0, 0o711, "")
	if _, err := os.Lstat(filepath.Join(dir, ".fs")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("temporary upper directory left behind: %v", err)
	}

	// Attaching again changes nothing.
	if err := s.Attach(context.Background(), "ctr", dir, testSize); err != nil {
		t.Fatal(err)
	}

	// What is written goes to the image; unmounting it uncovers the snapshotter's directories, untouched.
	if err := os.WriteFile(filepath.Join(upper, "written"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Unmount("ctr"); err != nil {
		t.Fatal(err)
	}
	if mounted, err := s.Mounted("ctr"); err != nil || mounted {
		t.Fatalf("Mounted = %v, %v after Unmount; want false", mounted, err)
	}
	if entries, err := os.ReadDir(upper); err != nil || len(entries) != 0 {
		t.Errorf("the snapshotter's upper directory holds %v, %v; want it empty", entries, err)
	}
	if err := s.Remove("ctr"); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.IDs(); err != nil || len(ids) != 0 {
		t.Errorf("IDs = %v, %v after Remove; want none", ids, err)
	}
}

func TestAttachRefusesAnUpperThatHoldsFiles(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	dir := snapshotDir(t)
	if err := os.WriteFile(filepath.Join(dir, "fs", "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(context.Background(), "ctr", dir, testSize); err == nil {
		_ = s.Unmount("ctr")
		_ = s.Remove("ctr")
		t.Fatal("expected an error")
	}
	if mounted, _ := mountinfo.Mounted(dir); mounted {
		t.Error("the snapshot directory got mounted over")
	}
	if ids, _ := s.IDs(); len(ids) != 0 {
		t.Errorf("images left behind: %v", ids)
	}
}

func TestAttachGivesUpPastTheDeadline(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	dir := snapshotDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Attach(ctx, "ctr", dir, testSize); !errors.Is(err, context.Canceled) {
		_ = s.Unmount("ctr")
		_ = s.Remove("ctr")
		t.Fatalf("Attach error = %v, want context.Canceled", err)
	}
	if mounted, _ := mountinfo.Mounted(dir); mounted {
		t.Error("the snapshot directory got mounted over")
	}
	if ids, _ := s.IDs(); len(ids) != 0 {
		t.Errorf("images left behind: %v", ids)
	}
}

func TestAttachStartsOverWhenTheUpperIsMissing(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	dir := snapshotDir(t)
	attach(t, s, "ctr", dir)
	// As if the plugin had stopped between mounting the image and renaming the upper directory into place.
	if err := os.Remove(filepath.Join(dir, "fs")); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(context.Background(), "ctr", dir, testSize); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "fs")); err != nil {
		t.Fatal(err)
	}
	img, _ := s.image("ctr")
	if mps, err := s.mountpoints(img); err != nil || len(mps) != 1 {
		t.Errorf("mount points = %v, %v; want one", mps, err)
	}
}

func TestAttachRefusesAnImageMountedElsewhere(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	attach(t, s, "ctr", snapshotDir(t))
	if err := s.Attach(context.Background(), "ctr", snapshotDir(t), testSize); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUnmountDetachesALayerStillInUse(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	dir := snapshotDir(t)
	attach(t, s, "ctr", dir)
	f, err := os.Create(filepath.Join(dir, "fs", "open"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := s.Unmount("ctr"); err != nil {
		t.Fatal(err)
	}
	if mounted, err := s.Mounted("ctr"); err != nil || mounted {
		t.Fatalf("Mounted = %v, %v after Unmount; want false", mounted, err)
	}
	// The open file keeps the detached filesystem alive.
	if _, err := f.WriteString("still writable"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRefusesAMountedImage(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	attach(t, s, "ctr", snapshotDir(t))
	if err := s.Remove("ctr"); err == nil {
		t.Fatal("expected an error")
	}
	if ids, err := s.IDs(); err != nil || len(ids) != 1 {
		t.Errorf("IDs = %v, %v; want the image kept", ids, err)
	}
}

func TestUnmountAndRemoveIgnoreAContainerWithoutALayer(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	if err := s.Unmount("ctr"); err != nil {
		t.Error(err)
	}
	if err := s.Remove("ctr"); err != nil {
		t.Error(err)
	}
}
