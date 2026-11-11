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

// The directories of a snapshot directory, and so of an image. The upper directory is made as stagedUpperName.
const (
	upperName       = "fs"
	workName        = "work"
	stagedUpperName = ".fs"
)

// Attach gives a container's rootfs a block writable layer of size bytes. It formats a new image, mounts it over
// dir, the snapshot directory that holds the rootfs's upper directory fs and work directory work, and recreates both
// in the image with the owner, mode and extended attributes of the ones it covers. The snapshot must be unused: fs
// empty, and no overlay mounted on it. A layer already attached at dir is left as it is, and a failed Attach takes
// back what it did.
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
		if _, err := os.Lstat(filepath.Join(dir, upperName)); err == nil {
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
	upper := filepath.Join(dir, upperName)
	if err := checkUnused(upper); err != nil {
		return err
	}
	var attrs [3]dirAttr
	for i, path := range []string{dir, upper, filepath.Join(dir, workName)} {
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
	// Past the request's deadline containerd creates the container without this layer.
	if err := ctx.Err(); err != nil {
		_ = os.Remove(img)
		return fmt.Errorf("attach image %s: %w", img, err)
	}
	if err := mountImage(img, dir); err != nil {
		_ = os.Remove(img)
		return err
	}
	if err := complete(ctx, dir, attrs); err != nil {
		if uerr := unmountAll([]string{dir}); uerr != nil {
			return errors.Join(err, uerr)
		}
		_ = os.Remove(img)
		return err
	}
	return nil
}

// Status reports what a container's image is to the container.
func (s *Store) Status(id string) (Status, error) {
	img, err := s.image(id)
	if err != nil {
		return Detached, err
	}
	mps, err := s.mountpoints(img)
	if err != nil {
		return Detached, err
	}
	if len(mps) == 0 {
		return Detached, nil
	}
	// Every mount point shows the same filesystem.
	if _, err := os.Lstat(filepath.Join(mps[0], upperName)); errors.Is(err, fs.ErrNotExist) {
		return Incomplete, nil
	} else if err != nil {
		return Detached, err
	}
	return Attached, nil
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

// Remove deletes a container's image, and reports whether there was one. It refuses while the image is mounted.
func (s *Store) Remove(id string) (removed bool, err error) {
	img, err := s.image(id)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(img); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	mps, err := s.mountpoints(img)
	if err != nil {
		return false, err
	}
	if len(mps) > 0 {
		return false, fmt.Errorf("image %s is still mounted at %s", img, strings.Join(mps, ", "))
	}
	if err := os.Remove(img); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// Preflight checks what block writable layers need, so that a node that cannot create them fails when the plugin
// starts rather than on its first container. snapshotterRoot is the directory the layers are mounted under.
func Preflight(snapshotterRoot string) error {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return fmt.Errorf("block writable layers need mkfs.ext4 (install e2fsprogs): %w", err)
	}
	if err := checkLoopControl(); err != nil {
		return err
	}
	return checkPropagation(snapshotterRoot)
}

// checkPropagation makes sure that what the plugin mounts under dir reaches the host's mount namespace, where
// containerd's shims mount container rootfs. The plugin's container has a mount namespace of its own, and a mount
// made in it stays there unless dir is on a shared mount, which is what a volume mounted with bidirectional mount
// propagation is. On the host itself / is shared unless the plugin's mounts are sandboxed.
func checkPropagation(dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("find the snapshotter's directory: %w", err)
	}
	mounts, err := mountinfo.GetMounts(mountinfo.ParentsFilter(resolved))
	if err != nil {
		return fmt.Errorf("read mount table: %w", err)
	}
	// Of the mounts whose mount points lead to the directory, it is on the deepest, and of those stacked on one
	// mount point on the last, which covers the others.
	var holder *mountinfo.Info
	for _, m := range mounts {
		if holder == nil || len(m.Mountpoint) >= len(holder.Mountpoint) {
			holder = m
		}
	}
	if holder == nil {
		return fmt.Errorf("no mount holds %s", resolved)
	}
	// A shared mount names its peer group among its optional fields, as shared:<n>.
	shared := slices.ContainsFunc(strings.Fields(holder.Optional), func(field string) bool {
		return strings.HasPrefix(field, "shared:")
	})
	if !shared {
		return fmt.Errorf("mounts under %s would not reach the host: %s is not a shared mount; mount the snapshotter's directory into the plugin's container with mountPropagation: Bidirectional", resolved, holder.Mountpoint)
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

// mountImage loop-mounts a formatted image over a snapshot directory.
func mountImage(img, dir string) error {
	m := mount.Mount{Type: "ext4", Source: img, Options: slices.Clone(mountOptions)}
	if err := m.Mount(dir); err != nil {
		return fmt.Errorf("mount %s on %s: %w", img, dir, err)
	}
	return nil
}

// complete turns the image freshly mounted over a snapshot directory into the snapshot's writable layer. Before it
// commits, it checks again that the request is alive and that no overlay uses the snapshot: if containerd stopped
// waiting while the image was being formatted, the container may already write to the directories the image hides.
func complete(ctx context.Context, dir string, attrs [3]dirAttr) error {
	if err := stage(dir, attrs); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("attach a block writable layer over %s: %w", dir, err)
	}
	if err := checkNoOverlay(filepath.Join(dir, upperName)); err != nil {
		return err
	}
	return commit(dir)
}

// stage gives a freshly mounted image the attributes of the snapshot directory it covers, and recreates the work and
// upper directories with those of the ones they replace. The upper directory, the container's /, gets its name only
// in commit: a container fails to start on an image without one, rather than start with the wrong owner on /.
func stage(dir string, attrs [3]dirAttr) error {
	if err := attrs[0].apply(dir); err != nil {
		return err
	}
	work := filepath.Join(dir, workName)
	if err := os.Mkdir(work, 0o700); err != nil {
		return fmt.Errorf("create overlay work directory: %w", err)
	}
	if err := attrs[2].apply(work); err != nil {
		return err
	}
	staged := filepath.Join(dir, stagedUpperName)
	if err := os.Mkdir(staged, 0o700); err != nil {
		return fmt.Errorf("create overlay upper directory: %w", err)
	}
	return attrs[1].apply(staged)
}

// commit makes a staged image the snapshot's writable layer, by giving the upper directory its name.
func commit(dir string) error {
	if err := os.Rename(filepath.Join(dir, stagedUpperName), filepath.Join(dir, upperName)); err != nil {
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
	return checkNoOverlay(upper)
}

// checkNoOverlay makes sure no mounted overlay names upper as its upper directory. A mounted overlay keeps writing
// to the directory that had that name when it was mounted, whatever is mounted over the name afterwards.
func checkNoOverlay(upper string) error {
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

// mountpoints lists where an image is mounted in this mount namespace: the mount table's ext4 filesystems on loop
// devices whose backing file is the image. The backing file is told by its device and inode. The path the kernel
// reports for it is its path in the mount namespace that set the loop device up, which is gone once the plugin's
// container has been replaced.
func (s *Store) mountpoints(img string) ([]string, error) {
	var st unix.Stat_t
	if err := unix.Stat(img, &st); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat image %s: %w", img, err)
	}
	infos, err := mountinfo.GetMounts(func(info *mountinfo.Info) (skip, stop bool) {
		return info.FSType != "ext4" || !strings.HasPrefix(info.Source, "/dev/loop"), false
	})
	if err != nil {
		return nil, fmt.Errorf("read mount table: %w", err)
	}
	var mps []string
	for _, info := range infos {
		// The loop device names its backing file by device and inode.
		var backing *unix.LoopInfo64
		loop, err := os.Open(info.Source)
		if err == nil {
			backing, err = unix.IoctlLoopGetStatus64(int(loop.Fd()))
			_ = loop.Close()
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ENXIO) {
			// Detached meanwhile.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the backing file of %s: %w", info.Source, err)
		}
		//nolint:unconvert // st.Dev is not a uint64 on every architecture.
		if backing.Device == uint64(st.Dev) && backing.Inode == st.Ino {
			mps = append(mps, info.Mountpoint)
		}
	}
	return mps, nil
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
