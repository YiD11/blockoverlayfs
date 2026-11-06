//go:build linux

package block

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	cfs "github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
)

// mkfsArgs keep the image sparse: with lazy initialisation mke2fs leaves the inode tables and the journal as
// holes instead of writing them out, and the kernel fills them in as they are first used. -m 0 drops the root
// reserve, so the container hits ENOSPC where df inside it says the filesystem ends.
var mkfsArgs = []string{"-q", "-F", "-m", "0", "-E", "lazy_itable_init=1,lazy_journal_init=1"}

// discard punches the blocks of deleted files out of the image, so the image shrinks on disk as the container
// deletes. The loop device is set up to clear itself once the filesystem is no longer mounted anywhere.
var mountOptions = []string{"loop", "rw", "discard"}

// Create formats an image of st.Size bytes, records st and mounts the image, ready for use as the upper and
// work directories of an overlay. The upper directory takes the owner and mode of ownerFrom, the directory
// upper the layer replaces.
func (s *Store) Create(ctx context.Context, id string, st State, ownerFrom string) error {
	if st.Size <= 0 {
		return fmt.Errorf("invalid writable layer size %d", st.Size)
	}
	staging, err := s.newStaging(id)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := createImage(ctx, filepath.Join(staging, ImageName), st.Size); err != nil {
		return err
	}
	if err := writeState(staging, st); err != nil {
		return err
	}
	if err := s.publish(id, staging, st); err != nil {
		return err
	}
	published = true

	if err := s.EnsureMounted(ctx, id, ownerFrom); err != nil {
		// The layer is recorded but cannot be used. Withdraw it, so that a retry decides afresh.
		if derr := s.Detach(context.WithoutCancel(ctx), id); derr != nil {
			log.G(ctx).WithError(derr).WithField("layer", id).Error("failed to withdraw a writable layer that could not be mounted")
		}
		return err
	}
	return nil
}

// createImage runs outside every lock: mkfs is the slowest step of creating a layer.
func createImage(ctx context.Context, path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create block image %s: %w", path, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return fmt.Errorf("resize block image %s to %d bytes: %w", path, size, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close block image %s: %w", path, err)
	}
	//nolint:gosec // The arguments are constants and the path of an image this store just created.
	out, err := exec.CommandContext(ctx, "mkfs.ext4", append(mkfsArgs, path)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureMounted mounts a recorded block layer unless it already is, as after the daemon or the node restarted.
func (s *Store) EnsureMounted(ctx context.Context, id, ownerFrom string) error {
	l := s.Layer(id)
	// Mkdir rather than MkdirAll: a layer detached meanwhile must not be brought back as an empty directory.
	if err := os.Mkdir(l.Mountpoint(), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create mount point for writable layer %s: %w", l.Dir, err)
	}
	mounted, err := mountinfo.Mounted(l.Mountpoint())
	if err != nil {
		return fmt.Errorf("check whether %s is mounted: %w", l.Mountpoint(), err)
	}
	if !mounted {
		m := mount.Mount{Type: "ext4", Source: l.Image(), Options: append([]string(nil), mountOptions...)}
		if err := m.Mount(l.Mountpoint()); err != nil {
			return fmt.Errorf("mount writable layer %s: %w", l.Image(), err)
		}
		log.G(ctx).WithField("layer", id).Debug("mounted writable layer")
	}
	if err := createUpper(l.Upper(), ownerFrom); err != nil {
		return err
	}
	if err := os.Mkdir(l.Work(), 0o711); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create overlay work directory %s: %w", l.Work(), err)
	}
	return nil
}

// Usage reports what a block layer holds. A mounted layer reports what its filesystem has in use, the bytes
// the container wrote plus the directories and extent blocks that index them, which kubelet compares against
// the container's ephemeral-storage. A layer that is not mounted, as after a restart for a stopped container
// awaiting garbage collection, reports the blocks its image occupies on the host.
func (s *Store) Usage(id string) (snapshots.Usage, error) {
	l := s.Layer(id)
	mounted, err := mountinfo.Mounted(l.Mountpoint())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return snapshots.Usage{}, fmt.Errorf("check whether %s is mounted: %w", l.Mountpoint(), err)
	}
	if mounted {
		var st unix.Statfs_t
		if err := unix.Statfs(l.Mountpoint(), &st); err != nil {
			return snapshots.Usage{}, fmt.Errorf("statfs %s: %w", l.Mountpoint(), err)
		}
		return snapshots.Usage{
			Size:   toInt64(st.Blocks-st.Bfree) * st.Bsize,
			Inodes: toInt64(st.Files - st.Ffree),
		}, nil
	}
	var st unix.Stat_t
	if err := unix.Stat(l.Image(), &st); err != nil {
		return snapshots.Usage{}, fmt.Errorf("stat %s: %w", l.Image(), err)
	}
	return snapshots.Usage{Size: st.Blocks * 512}, nil
}

// CopyUpper copies a mounted layer's upper directory to a new directory under the store root, for a commit to
// rename onto the inner overlay's directory upper. The caller removes the copy if it does not rename it.
func (s *Store) CopyUpper(ctx context.Context, id string) (string, error) {
	l := s.Layer(id)
	copyDir := filepath.Join(s.root, fmt.Sprintf("%scommit-%s-%d", stagingPrefix, id, time.Now().UnixNano()))
	// A single unreadable security.* xattr must not fail a commit that is otherwise complete, the trade-off
	// containerd's own snapshotters make.
	xattrErrors := func(dst, src, xattrKey string, copyErr error) error {
		log.G(ctx).WithError(copyErr).WithField("xattr", xattrKey).Debug("failed to copy xattr on commit")
		return nil
	}
	if err := cfs.CopyDir(copyDir, l.Upper(), cfs.WithXAttrErrorHandler(xattrErrors)); err != nil {
		_ = os.RemoveAll(copyDir)
		return "", fmt.Errorf("copy writable layer %s: %w", l.Upper(), err)
	}
	return copyDir, nil
}

// Replaceable reports whether a snapshot can still trade the inner overlay's directory upper for a block
// layer: nothing may have been written to it, and no mounted overlay may be using it. A container started by
// an earlier version of this daemon, or while block layers were off, keeps its directory upper.
func (s *Store) Replaceable(upper string) (bool, string, error) {
	f, err := os.Open(upper)
	if err != nil {
		return false, "", err
	}
	names, err := f.Readdirnames(1)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return false, "", fmt.Errorf("read %s: %w", upper, err)
	}
	if len(names) > 0 {
		return false, "the directory upper already has content", nil
	}
	mounts, err := mountinfo.GetMounts(func(info *mountinfo.Info) (skip, stop bool) {
		return info.FSType != "overlay" || !hasOption(info.VFSOptions, "upperdir", upper), false
	})
	if err != nil {
		return false, "", fmt.Errorf("read mount table: %w", err)
	}
	if len(mounts) > 0 {
		return false, "the overlay mounted at " + mounts[0].Mountpoint + " uses the directory upper", nil
	}
	return true, "", nil
}

// Preflight checks what creating block layers needs, so a node that cannot create them fails at startup rather
// than on its first container.
func Preflight(ctx context.Context, blocksRoot, innerRoot string) error {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return fmt.Errorf("block writable layers need mkfs.ext4 (install e2fsprogs): %w", err)
	}
	if err := checkLoopControl(); err != nil {
		return err
	}
	// Commit renames a copy made under blocksRoot onto a directory under innerRoot.
	if err := sameDevice(blocksRoot, innerRoot); err != nil {
		return err
	}
	self, err1 := os.Readlink("/proc/self/ns/mnt")
	host, err2 := os.Readlink("/proc/1/ns/mnt")
	if err1 == nil && err2 == nil && self != host {
		log.G(ctx).Warn("running outside the mount namespace of PID 1: block layers are mounted where containerd's shims cannot see them unless mount propagation carries them there")
	}
	return nil
}

// checkLoopControl asks the kernel for a free loop device, which is what `losetup -f` does.
// /sys/module/loop/parameters/max_loop is deliberately not consulted: it only sets how many devices the loop
// module creates up front, and the kernel creates more on demand through /dev/loop-control.
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

func sameDevice(a, b string) error {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return fmt.Errorf("stat %s: %w", a, err)
	}
	if err := unix.Stat(b, &sb); err != nil {
		return fmt.Errorf("stat %s: %w", b, err)
	}
	if sa.Dev != sb.Dev {
		return fmt.Errorf("%s and %s must be on the same filesystem", a, b)
	}
	return nil
}

// createUpper gives a new upper directory the owner and mode of the directory upper it replaces. The inner
// overlay gave that directory the owner of the parent layer's root, which under user namespace remapping is the
// remapped root, so the container can write to its own root directory. The directory is prepared under a
// temporary name, so a half-made upper is never taken for a finished one.
func createUpper(upper, ownerFrom string) error {
	if _, err := os.Lstat(upper); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	fi, err := os.Stat(ownerFrom)
	if err != nil {
		return fmt.Errorf("stat %s: %w", ownerFrom, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: no ownership information", ownerFrom)
	}
	tmp := upper + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("create overlay upper directory: %w", err)
	}
	// chown first: it clears the set-user-ID and set-group-ID bits that chmod sets.
	if err := os.Lchown(tmp, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("chown overlay upper directory: %w", err)
	}
	if err := os.Chmod(tmp, fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return fmt.Errorf("chmod overlay upper directory: %w", err)
	}
	return os.Rename(tmp, upper)
}

// toInt64 fits a kernel counter into the signed fields of snapshots.Usage.
func toInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func hasOption(options, key, value string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == key+"="+value {
			return true
		}
	}
	return false
}

// unmount takes every mount off target. A mount still in use is detached lazily: the kernel keeps the
// filesystem, its loop device and its image until the last user lets go.
func unmount(target string) error {
	if err := mount.UnmountAll(target, 0); err != nil {
		if derr := mount.UnmountAll(target, unix.MNT_DETACH); derr != nil {
			return fmt.Errorf("%v; lazy detach: %w", err, derr)
		}
	}
	return nil
}
