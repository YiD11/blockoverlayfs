//go:build linux

package block

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
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
		if _, err := s.Remove(id); err != nil {
			t.Error(err)
		}
	})
	if err := s.Attach(context.Background(), id, dir, testSize); err != nil {
		t.Fatal(err)
	}
}

func checkStatus(t *testing.T, s *Store, id string, want Status) {
	t.Helper()
	if got, err := s.Status(id); err != nil || got != want {
		t.Fatalf("Status(%s) = %v, %v; want %v", id, got, err, want)
	}
}

// stepwise sets up a test that attaches the layer of container ctr one step of Attach at a time: a store, a
// snapshot directory, the image's path and the attributes Attach reads before it covers the directory. Whatever the
// test leaves mounted is cleared away.
func stepwise(t *testing.T) (s *Store, dir, img string, attrs [3]dirAttr) {
	t.Helper()
	s, dir = openStore(t), snapshotDir(t)
	img, err := s.image("ctr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Unmount("ctr")
		_, _ = s.Remove("ctr")
	})
	for i, path := range []string{dir, filepath.Join(dir, upperName), filepath.Join(dir, workName)} {
		if attrs[i], err = readAttr(path); err != nil {
			t.Fatal(err)
		}
	}
	return s, dir, img, attrs
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
	checkStatus(t, s, "ctr", Attached)
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
	checkStatus(t, s, "ctr", Detached)
	if entries, err := os.ReadDir(upper); err != nil || len(entries) != 0 {
		t.Errorf("the snapshotter's upper directory holds %v, %v; want it empty", entries, err)
	}
	if _, err := s.Remove("ctr"); err != nil {
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
		_, _ = s.Remove("ctr")
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
		_, _ = s.Remove("ctr")
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

// TestStatusOfAnAttachThatStopped stops attaching a layer after each of its steps, as the plugin or the node stopping
// would, and checks what Status makes of the remains and that clearing them away uncovers the snapshot directory.
func TestStatusOfAnAttachThatStopped(t *testing.T) {
	testutil.RequiresRoot(t)
	// The steps of Attach after its checks, in order.
	steps := []struct {
		name string
		run  func(img, dir string, attrs [3]dirAttr) error
	}{
		{"formatting the image", func(img, _ string, _ [3]dirAttr) error {
			return createImage(context.Background(), img, testSize)
		}},
		{"mounting the image", func(img, dir string, _ [3]dirAttr) error { return mountImage(img, dir) }},
		{"staging the directories", func(_, dir string, attrs [3]dirAttr) error { return stage(dir, attrs) }},
		{"committing", func(_, dir string, _ [3]dirAttr) error { return commit(dir) }},
	}
	for done, want := range []Status{Detached, Detached, Incomplete, Incomplete, Attached} {
		name := "before formatting the image"
		if done > 0 {
			name = "after " + steps[done-1].name
		}
		t.Run(name, func(t *testing.T) {
			s, dir, img, attrs := stepwise(t)
			upper := filepath.Join(dir, upperName)
			for _, step := range steps[:done] {
				if err := step.run(img, dir, attrs); err != nil {
					t.Fatalf("%s: %v", step.name, err)
				}
			}

			checkStatus(t, s, "ctr", want)
			// No overlay can be mounted from the snapshot while the image covers it without an upper directory.
			if _, err := os.Lstat(upper); (err == nil) != (want != Incomplete) {
				t.Errorf("upper directory: %v; want it there unless the image is incomplete", err)
			}

			if err := s.Unmount("ctr"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Remove("ctr"); err != nil {
				t.Fatal(err)
			}
			checkStatus(t, s, "ctr", Detached)
			if ids, err := s.IDs(); err != nil || len(ids) != 0 {
				t.Errorf("IDs = %v, %v; want none", ids, err)
			}
			if mounted, err := mountinfo.Mounted(dir); err != nil || mounted {
				t.Errorf("%s mounted = %v, %v; want false", dir, mounted, err)
			}
			if entries, err := os.ReadDir(upper); err != nil || len(entries) != 0 {
				t.Errorf("the snapshotter's upper directory holds %v, %v; want it empty", entries, err)
			}
		})
	}
}

func TestAttachReplacesAnImageLeftUnmounted(t *testing.T) {
	testutil.RequiresRoot(t)
	s, dir, img, _ := stepwise(t)
	// As if the plugin had stopped between formatting the image and mounting it.
	if err := createImage(context.Background(), img, testSize); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(context.Background(), "ctr", dir, testSize); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, s, "ctr", Attached)
}

// TestCompleteGivesUpWhenTheSnapshotGotMounted is containerd no longer waiting for the layer: it created the
// container without it, and the container's rootfs was mounted from the snapshot before the image covered it.
func TestCompleteGivesUpWhenTheSnapshotGotMounted(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.Background()
	s, dir, img, attrs := stepwise(t)
	upper, work := filepath.Join(dir, upperName), filepath.Join(dir, workName)
	if err := createImage(ctx, img, testSize); err != nil {
		t.Fatal(err)
	}

	// The container starts on the snapshotter's directories while the image is being formatted.
	lower, rootfs := t.TempDir(), t.TempDir()
	overlay := mount.Mount{Type: "overlay", Source: "overlay", Options: []string{
		"lowerdir=" + lower, "upperdir=" + upper, "workdir=" + work,
	}}
	if err := overlay.Mount(rootfs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mount.UnmountAll(rootfs, 0) })

	if err := mountImage(img, dir); err != nil {
		t.Fatal(err)
	}
	if err := complete(ctx, dir, attrs); err == nil {
		t.Fatal("expected an error: the snapshot's upper directory is in use")
	}
	checkStatus(t, s, "ctr", Incomplete)
	if err := s.Unmount("ctr"); err != nil {
		t.Fatal(err)
	}
	// The container went on writing to the snapshotter's upper directory all along.
	if err := os.WriteFile(filepath.Join(rootfs, "written"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(upper, "written")); err != nil || string(got) != "kept" {
		t.Errorf("the snapshotter's upper directory holds %q, %v; want what the container wrote", got, err)
	}
}

func TestCompleteGivesUpPastTheDeadline(t *testing.T) {
	testutil.RequiresRoot(t)
	s, dir, img, attrs := stepwise(t)
	if err := createImage(context.Background(), img, testSize); err != nil {
		t.Fatal(err)
	}
	if err := mountImage(img, dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := complete(ctx, dir, attrs); !errors.Is(err, context.Canceled) {
		t.Fatalf("complete error = %v, want context.Canceled", err)
	}
	checkStatus(t, s, "ctr", Incomplete)
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
	checkStatus(t, s, "ctr", Detached)
	// The open file keeps the detached filesystem alive.
	if _, err := f.WriteString("still writable"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveRefusesAMountedImage(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	attach(t, s, "ctr", snapshotDir(t))
	if _, err := s.Remove("ctr"); err == nil {
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
	if _, err := s.Remove("ctr"); err != nil {
		t.Error(err)
	}
}

// TestMountpointsFindAnImageAttachedThroughAMountThatIsGone sets an image's loop device up through a bind mount of
// the store and takes the bind mount away. That is what the loop devices of the plugin's previous container are to
// the plugin: the kernel no longer reports the image's path for the backing file.
func TestMountpointsFindAnImageAttachedThroughAMountThatIsGone(t *testing.T) {
	testutil.RequiresRoot(t)
	s := openStore(t)
	img, err := s.image("ctr")
	if err != nil {
		t.Fatal(err)
	}
	if err := createImage(context.Background(), img, testSize); err != nil {
		t.Fatal(err)
	}
	alias := t.TempDir()
	if err := unix.Mount(s.Root(), alias, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	loop, err := mount.AttachLoopDevice(filepath.Join(alias, filepath.Base(img)))
	if uerr := unix.Unmount(alias, unix.MNT_DETACH); uerr != nil {
		t.Error(uerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mount.DetachLoopDevice(loop); err != nil {
			t.Error(err)
		}
	})
	dir := t.TempDir()
	if err := unix.Mount(loop, dir, "ext4", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(dir, 0); err != nil {
			t.Error(err)
		}
	})

	reported, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(loop), "loop", "backing_file"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the kernel reports the backing file of %s as %q", img, strings.TrimSpace(string(reported)))
	if mps, err := s.mountpoints(img); err != nil || !slices.Equal(mps, []string{dir}) {
		t.Errorf("mount points = %v, %v; want [%s]", mps, err, dir)
	}
}

// TestCheckPropagationWantsASharedMount mounts where the plugin's container gets the snapshotter's directory: on a
// mount of its own, private unless the volume asks for bidirectional mount propagation.
func TestCheckPropagationWantsASharedMount(t *testing.T) {
	testutil.RequiresRoot(t)
	dir := t.TempDir()
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(dir, 0); err != nil {
			t.Error(err)
		}
	})
	sub := filepath.Join(dir, "snapshots")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := unix.Mount("", dir, "", unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	if err := checkPropagation(sub); err == nil {
		t.Error("a directory on a private mount passed")
	}
	if err := unix.Mount("", dir, "", unix.MS_SHARED, ""); err != nil {
		t.Fatal(err)
	}
	if err := checkPropagation(sub); err != nil {
		t.Errorf("a directory on a shared mount: %v", err)
	}
	if err := checkPropagation(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing directory passed")
	}

	// A second mount on the same mount point covers the first, and the directory is on that one now.
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(dir, 0); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("", dir, "", unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	if err := checkPropagation(sub); err == nil {
		t.Error("a directory on a private mount that covers a shared one passed")
	}
}
