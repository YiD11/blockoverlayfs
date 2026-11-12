// Package containerd reads from containerd what the plugin cannot learn from NRI: where the snapshotter keeps a
// container's rootfs, and whether a container still exists.
package containerd

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// namespaceHeader is the gRPC metadata key containerd reads a request's namespace from.
const namespaceHeader = "containerd-namespace"

// callTimeout bounds every call. Calls made for an NRI request are bounded by its shorter deadline anyway.
const callTimeout = 10 * time.Second

// Options say where containerd is and which snapshots to look at.
type Options struct {
	// Address is containerd's gRPC socket.
	Address string
	// Namespace is the containerd namespace of the containers, k8s.io for CRI.
	Namespace string
	// Snapshotter names the snapshotter whose snapshots the plugin covers, and SnapshotterRoot is its root
	// directory.
	Snapshotter     string
	SnapshotterRoot string
}

// Client talks to containerd's snapshots and containers services.
type Client struct {
	opts       Options
	conn       *grpc.ClientConn
	snapshots  snapshotsapi.SnapshotsClient
	containers containersapi.ContainersClient
}

// Dial sets a client up. It connects lazily, so containerd need not be running yet.
func Dial(opts Options) (*Client, error) {
	conn, err := grpc.NewClient("unix://"+opts.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", opts.Address, err)
	}
	return &Client{
		opts:       opts,
		conn:       conn,
		snapshots:  snapshotsapi.NewSnapshotsClient(conn),
		containers: containersapi.NewContainersClient(conn),
	}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// SnapshotDir returns the directory in which the snapshotter keeps the upper and work directories of a container's
// rootfs. CRI names that snapshot after the container. The error satisfies errdefs.IsNotFound when the snapshotter
// has no such snapshot, as for a container whose runtime handler uses another snapshotter.
//
// containerd's overlayfs snapshotter keeps every snapshot as <root>/snapshots/<numeric ID>, holding the upper
// directory fs and the work directory work. An active snapshot with parents mounts as an overlay naming both, one
// without parents as a writable bind mount of its upper directory. Anything else, or a directory anywhere else, is
// refused: the plugin mounts over the directory returned.
func (c *Client) SnapshotDir(ctx context.Context, id string) (string, error) {
	ctx, cancel := c.context(ctx)
	defer cancel()
	resp, err := c.snapshots.Mounts(ctx, &snapshotsapi.MountsRequest{Snapshotter: c.opts.Snapshotter, Key: id})
	if err != nil {
		return "", fmt.Errorf("get the mounts of snapshot %s from %s: %w", id, c.opts.Snapshotter, errgrpc.ToNative(err))
	}
	name := fmt.Sprintf("snapshot %s of %s", id, c.opts.Snapshotter)
	mounts := resp.GetMounts()
	if len(mounts) != 1 {
		return "", fmt.Errorf("%s: want one mount, got %d", name, len(mounts))
	}
	m := mounts[0]
	var upper, work string
	switch m.GetType() {
	case "overlay":
		for _, o := range m.GetOptions() {
			if v, ok := strings.CutPrefix(o, "upperdir="); ok {
				upper = v
			} else if v, ok := strings.CutPrefix(o, "workdir="); ok {
				work = v
			}
		}
		if upper == "" || work == "" {
			return "", fmt.Errorf("%s: an overlay mount without upperdir and workdir is not a container's rootfs", name)
		}
	case "bind":
		if !slices.Contains(m.GetOptions(), "rw") {
			return "", fmt.Errorf("%s: a read-only bind mount is not a container's rootfs", name)
		}
		upper = m.GetSource()
	default:
		return "", fmt.Errorf("%s: unsupported mount type %q", name, m.GetType())
	}

	dir := filepath.Dir(upper)
	if upper != filepath.Join(dir, "fs") || (work != "" && work != filepath.Join(dir, "work")) {
		return "", fmt.Errorf("%s: upper directory %q and work directory %q are not fs and work of one snapshot directory", name, upper, work)
	}
	snapshots := filepath.Join(c.opts.SnapshotterRoot, "snapshots")
	if _, err := strconv.ParseUint(filepath.Base(dir), 10, 64); filepath.Dir(dir) != snapshots || err != nil {
		return "", fmt.Errorf("%s: %s is not a snapshot directory of %s", name, dir, snapshots)
	}
	return dir, nil
}

// ContainerExists reports whether containerd still has a container.
func (c *Client) ContainerExists(ctx context.Context, id string) (bool, error) {
	ctx, cancel := c.context(ctx)
	defer cancel()
	_, err := c.containers.Get(ctx, &containersapi.GetContainerRequest{ID: id})
	if err == nil {
		return true, nil
	}
	if err = errgrpc.ToNative(err); errdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func (c *Client) context(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx = metadata.AppendToOutgoingContext(ctx, namespaceHeader, c.opts.Namespace)
	return context.WithTimeout(ctx, callTimeout)
}
