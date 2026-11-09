//go:build linux

package block

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
)

// mkfsArgs keep the image sparse: with lazy initialisation mke2fs leaves the inode tables and the journal as holes,
// and the kernel fills them in as they are first used. -m 0 drops the root reserve, so the container gets ENOSPC
// where df inside it says the filesystem ends.
var mkfsArgs = []string{"-q", "-F", "-m", "0", "-E", "lazy_itable_init=1,lazy_journal_init=1"}

// discard punches the blocks of deleted files out of the image, so the image shrinks on the host as the container
// deletes. containerd sets the loop device up to detach itself once nothing mounts the filesystem any more.
var mountOptions = []string{"loop", "rw", "discard"}

// Attach gives a container's rootfs a block writable layer of size bytes. dir is the snapshot directory in which
// containerd's overlayfs snapshotter keeps the rootfs's upper directory fs and work directory work. Attach formats a
// new image, mounts it over dir and recreates fs and work in it with the owner, mode and extended attributes of the
// directories it covers, so that the overlay mounted later from the snapshot's mounts writes into the image.
//
// Nothing may use the snapshot yet: fs must be empty and no overlay may be mounted on it. Attaching a layer that is
// already attached at dir does nothing.
func (s *Store) Attach(ctx context.Context, id, dir string, size int64) error {
	img, err := s.image(id)
	if err != nil {
		return err
	}
	if size <= 0 {
		return fmt.Errorf("invalid writable layer size %d", size)
	}
	mps, err := s.mountpoints(img)
	if err != nil {
		return err
	}
	switch {
	case slices.Contains(mps, dir):
		if _, err := os.Lstat(filepath.Join(dir, "fs")); err == nil {
			return nil
		}
		// Setting the image up was interrupted. Unmounting it uncovers the directories it hid: start over.
		if err := unmountAll(mps); err != nil {
			return err
		}
	case len(mps) > 0:
		return fmt.Errorf("image %s is mounted at %s, not at %s", img, strings.Join(mps, ", "), dir)
	}

	if mounted, err := mountinfo.Mounted(dir); err != nil {
		return fmt.Errorf("check whether %s is a mount point: %w", dir, err)
	} else if mounted {
		return fmt.Errorf("%s is already a mount point", dir)
	}
	upper := filepath.Join(dir, "fs")
	if err := checkUnused(upper); err != nil {
		return err
	}
	var attrs [3]dirAttr
	for i, path := range []string{dir, upper, filepath.Join(dir, "work")} {
		if attrs[i], err = readAttr(path); err != nil {
			return err
		}
	}

	// An image left by an interrupted attempt is mounted nowhere: replace it.
	if err := os.Remove(img); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale image %s: %w", img, err)
	}
	if err := createImage(ctx, img, size); err != nil {
		_ = os.Remove(img)
		return err
	}
	// Past the request's deadline containerd has stopped waiting and creates the container without this layer.
	// Covering the directories it is about to use would only hide them.
	if err := ctx.Err(); err != nil {
		_ = os.Remove(img)
		return fmt.Errorf("attach image %s: %w", img, err)
	}
	m := mount.Mount{Type: "ext4", Source: img, Options: slices.Clone(mountOptions)}
	if err := m.Mount(dir); err != nil {
		_ = os.Remove(img)
		return fmt.Errorf("mount %s on %s: %w", img, dir, err)
	}
	if err := populate(dir, attrs); err != nil {
		if uerr := unmountAll([]string{dir}); uerr != nil {
			return errors.Join(err, uerr)
		}
		_ = os.Remove(img)
		return err
	}
	return nil
}

func (s *Store) image(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid container ID %q", id)
	}
	return filepath.Join(s.root, id+imageSuffix), nil
}

// Unmount takes a container's image off every mount point in this mount namespace. A mount still in use is detached
// lazily: the kernel keeps the filesystem, its loop device and the image until the last user lets go.
func (s *Store) Unmount(id string) error {
	img, err := s.image(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(img); errors.Is(err, fs.ErrNotExist) {
		// The container never had a block layer.
		return nil
	}
	mps, err := s.mountpoints(img)
	if err != nil {
		return err
	}
	return unmountAll(mps)
}

// Mounted reports whether a container's image is mounted anywhere in this mount namespace.
func (s *Store) Mounted(id string) (bool, error) {
	img, err := s.image(id)
	if err != nil {
		return false, err
	}
	mps, err := s.mountpoints(img)
	return len(mps) > 0, err
}

// Remove deletes a container's image. It refuses while the image is mounted.
func (s *Store) Remove(id string) error {
	img, err := s.image(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(img); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	mps, err := s.mountpoints(img)
	if err != nil {
		return err
	}
	if len(mps) > 0 {
		return fmt.Errorf("image %s is still mounted at %s", img, strings.Join(mps, ", "))
	}
	if err := os.Remove(img); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Preflight checks what block writable layers need, so that a node that cannot create them fails when the plugin
// starts rather than on its first container.
func Preflight() error {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return fmt.Errorf("block writable layers need mkfs.ext4 (install e2fsprogs): %w", err)
	}
	if err := checkLoopControl(); err != nil {
		return err
	}
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return fmt.Errorf("read the plugin's mount namespace: %w", err)
	}
	host, err := os.Readlink("/proc/1/ns/mnt")
	if err != nil {
		return fmt.Errorf("read the mount namespace of PID 1: %w", err)
	}
	if self != host {
		return errors.New("the plugin must run in the mount namespace of PID 1, where containerd's shims mount container rootfs; do not sandbox its mounts")
	}
	return nil
}

// checkLoopControl asks the kernel for a free loop device, as `losetup -f` does. The kernel creates loop devices on
// demand through /dev/loop-control, however many the loop module created up front.
func checkLoopControl() error {
	f, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("block writable layers need loop devices (modprobe loop): %w", err)
	}
	defer f.Close()
	if _, err := unix.IoctlRetInt(int(f.Fd()), unix.LOOP_CTL_GET_FREE); err != nil {
		return fmt.Errorf("no free loop device: %w", err)
	}
	return nil
}

// createImage creates a sparse image of size bytes and formats it. mkfs.ext4 is the slow part of attaching a layer,
// so it runs under the request's deadline.
func createImage(ctx context.Context, path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create image %s: %w", path, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return fmt.Errorf("size image %s to %d bytes: %w", path, size, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close image %s: %w", path, err)
	}
	//nolint:gosec // The arguments are constants and the path of an image this store just created.
	out, err := exec.CommandContext(ctx, "mkfs.ext4", append(slices.Clone(mkfsArgs), path)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// populate fills a freshly mounted image in: its root takes the attributes of the snapshot directory it covers, and
// the work and upper directories are recreated with those of the directories they replace. The upper directory is
// the container's root directory, so it is made under a temporary name and renamed into place last: an interrupted
// populate leaves no upper directory, and the container fails to start rather than start with the wrong owner on /.
func populate(dir string, attrs [3]dirAttr) error {
	if err := attrs[0].apply(dir); err != nil {
		return err
	}
	work := filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		return fmt.Errorf("create overlay work directory: %w", err)
	}
	if err := attrs[2].apply(work); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".fs")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("create overlay upper directory: %w", err)
	}
	if err := attrs[1].apply(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "fs")); err != nil {
		return fmt.Errorf("create overlay upper directory: %w", err)
	}
	return nil
}

// checkUnused makes sure nothing uses an upper directory yet: it must be empty, and no mounted overlay may use it.
func checkUnused(upper string) error {
	f, err := os.Open(upper)
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(1)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read %s: %w", upper, err)
	}
	if len(names) > 0 {
		return fmt.Errorf("%s is not empty", upper)
	}
	mounts, err := mountinfo.GetMounts(func(info *mountinfo.Info) (skip, stop bool) {
		return info.FSType != "overlay" || !slices.Contains(strings.Split(info.VFSOptions, ","), "upperdir="+upper), false
	})
	if err != nil {
		return fmt.Errorf("read mount table: %w", err)
	}
	if len(mounts) > 0 {
		return fmt.Errorf("the overlay mounted at %s uses %s", mounts[0].Mountpoint, upper)
	}
	return nil
}

// dirAttr is what a recreated directory takes over from the one it replaces.
type dirAttr struct {
	mode     fs.FileMode
	uid, gid int
	xattrs   map[string][]byte
}

func readAttr(path string) (dirAttr, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return dirAttr{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok {
		return dirAttr{}, fmt.Errorf("%s is not a directory", path)
	}
	xattrs, err := readXattrs(path)
	if err != nil {
		return dirAttr{}, err
	}
	return dirAttr{
		mode:   fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky),
		uid:    int(st.Uid),
		gid:    int(st.Gid),
		xattrs: xattrs,
	}, nil
}

// apply gives path the attributes. chown goes first, because it clears the set-user-ID and set-group-ID bits that
// chmod sets.
func (a dirAttr) apply(path string) error {
	if err := os.Lchown(path, a.uid, a.gid); err != nil {
		return err
	}
	if err := os.Chmod(path, a.mode); err != nil {
		return err
	}
	for name, value := range a.xattrs {
		if err := unix.Lsetxattr(path, name, value, 0); err != nil {
			return fmt.Errorf("set xattr %s on %s: %w", name, path, err)
		}
	}
	return nil
}

func readXattrs(path string) (map[string][]byte, error) {
	names, err := listXattrs(path)
	if err != nil {
		return nil, err
	}
	xattrs := make(map[string][]byte, len(names))
	for _, name := range names {
		value, err := getXattr(path, name)
		if errors.Is(err, unix.ENODATA) {
			// Removed since it was listed.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read xattr %s of %s: %w", name, path, err)
		}
		xattrs[name] = value
	}
	return xattrs, nil
}

func listXattrs(path string) ([]string, error) {
	for {
		size, err := unix.Llistxattr(path, nil)
		if errors.Is(err, unix.ENOTSUP) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list xattrs of %s: %w", path, err)
		}
		if size == 0 {
			return nil, nil
		}
		buf := make([]byte, size)
		n, err := unix.Llistxattr(path, buf)
		if errors.Is(err, unix.ERANGE) {
			// One was added since the size was read.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list xattrs of %s: %w", path, err)
		}
		var names []string
		for _, name := range strings.Split(string(buf[:n]), "\x00") {
			if name != "" {
				names = append(names, name)
			}
		}
		return names, nil
	}
}

func getXattr(path, name string) ([]byte, error) {
	for {
		size, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size)
		n, err := unix.Lgetxattr(path, name, buf)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return buf[:n], nil
	}
}

// mountpoints lists where an image is mounted in this mount namespace.
func (s *Store) mountpoints(img string) ([]string, error) {
	mounts, err := s.mounts()
	if err != nil {
		return nil, err
	}
	return mounts[img], nil
}

// mounts maps each mounted image of the store to its mount points: the mount table's ext4 filesystems on loop
// devices whose backing file is one of the store's images.
func (s *Store) mounts() (map[string][]string, error) {
	infos, err := mountinfo.GetMounts(func(info *mountinfo.Info) (skip, stop bool) {
		return info.FSType != "ext4" || !strings.HasPrefix(info.Source, "/dev/loop"), false
	})
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	mounts := map[string][]string{}
	for _, info := range infos {
		backing, err := os.ReadFile(filepath.Join("/sys/block", filepath.Base(info.Source), "loop", "backing_file"))
		if errors.Is(err, fs.ErrNotExist) {
			// Detached meanwhile, or a partition of a loop device.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the backing file of %s: %w", info.Source, err)
		}
		img := strings.TrimSuffix(string(backing), "\n")
		if filepath.Dir(img) == s.root && strings.HasSuffix(img, imageSuffix) {
			mounts[img] = append(mounts[img], info.Mountpoint)
		}
	}
	return mounts, nil
}

// unmountAll unmounts everything at each mount point. A mount still in use is detached lazily at once rather than
// waited for: RemoveContainer has to finish within containerd's NRI request timeout.
func unmountAll(mountpoints []string) error {
	for _, mp := range mountpoints {
		for {
			err := unix.Unmount(mp, 0)
			if errors.Is(err, unix.EBUSY) {
				err = unix.Unmount(mp, unix.MNT_DETACH)
			}
			if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
				// Nothing is mounted there any more.
				break
			}
			if err != nil {
				return fmt.Errorf("unmount %s: %w", mp, err)
			}
		}
	}
	return nil
}
