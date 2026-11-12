package containerd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const root = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs"

func TestSnapshotDir(t *testing.T) {
	const dir = root + "/snapshots/42"
	overlay := func(options ...string) []*types.Mount {
		return []*types.Mount{{Type: "overlay", Source: "overlay", Options: options}}
	}
	cases := []struct {
		name   string
		mounts []*types.Mount
		want   string
	}{
		{
			name:   "an overlay",
			mounts: overlay("index=off", "workdir="+dir+"/work", "upperdir="+dir+"/fs", "lowerdir="+root+"/snapshots/1/fs"),
			want:   dir,
		},
		{
			name:   "an overlay with ID mappings",
			mounts: overlay("uidmap=0:100000:65536", "gidmap=0:100000:65536", "workdir="+dir+"/work", "upperdir="+dir+"/fs", "lowerdir=/l"),
			want:   dir,
		},
		{
			name:   "a writable bind mount, for an image without layers",
			mounts: []*types.Mount{{Type: "bind", Source: dir + "/fs", Options: []string{"rw", "rbind"}}},
			want:   dir,
		},
		{name: "a read-only bind mount", mounts: []*types.Mount{{Type: "bind", Source: dir + "/fs", Options: []string{"ro", "rbind"}}}},
		{name: "a view", mounts: overlay("lowerdir=" + root + "/snapshots/1/fs:" + root + "/snapshots/2/fs")},
		{name: "no work directory", mounts: overlay("upperdir=" + dir + "/fs")},
		{name: "work directory of another snapshot", mounts: overlay("workdir="+root+"/snapshots/43/work", "upperdir="+dir+"/fs")},
		{name: "upper directory not named fs", mounts: overlay("workdir="+dir+"/work", "upperdir="+dir+"/upper")},
		{name: "outside the root", mounts: overlay("workdir=/tmp/1/work", "upperdir=/tmp/1/fs")},
		{name: "a nested directory", mounts: overlay("workdir="+dir+"/x/work", "upperdir="+dir+"/x/fs")},
		{name: "a non-numeric ID", mounts: overlay("workdir="+root+"/snapshots/new-1/work", "upperdir="+root+"/snapshots/new-1/fs")},
		{name: "an unclean path", mounts: overlay("workdir="+root+"/snapshots/../snapshots/42/work", "upperdir="+root+"/snapshots/../snapshots/42/fs")},
		{name: "another mount type", mounts: []*types.Mount{{Type: "erofs", Source: dir + "/layer.erofs"}}},
		{name: "two mounts", mounts: append(overlay("workdir="+dir+"/work", "upperdir="+dir+"/fs"), overlay()...)},
		{name: "no mounts"},
	}
	// Each case is a snapshot named after it.
	f := &fakeContainerd{namespace: "k8s.io", snapshots: map[string][]*types.Mount{}}
	for _, tc := range cases {
		f.snapshots["overlayfs/"+tc.name] = tc.mounts
	}
	c := serve(t, f)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.SnapshotDir(context.Background(), tc.name)
			if tc.want == "" {
				// A snapshot that is refused must not read as one the snapshotter does not have.
				if err == nil || errdefs.IsNotFound(err) {
					t.Fatalf("SnapshotDir = %q, %v; want a refusal", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("SnapshotDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeContainerd holds the snapshots and containers of one namespace.
type fakeContainerd struct {
	namespace  string
	snapshots  map[string][]*types.Mount
	containers map[string]bool

	mu  sync.Mutex
	err error
}

func (f *fakeContainerd) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeContainerd) check(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if ns := md.Get(namespaceHeader); len(ns) != 1 || ns[0] != f.namespace {
		return errgrpc.ToGRPC(errdefs.ErrFailedPrecondition)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return errgrpc.ToGRPC(f.err)
}

type fakeSnapshots struct {
	snapshotsapi.UnimplementedSnapshotsServer
	*fakeContainerd
}

type fakeContainers struct {
	containersapi.UnimplementedContainersServer
	*fakeContainerd
}

func (f fakeSnapshots) Mounts(ctx context.Context, req *snapshotsapi.MountsRequest) (*snapshotsapi.MountsResponse, error) {
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	mounts, ok := f.snapshots[req.GetSnapshotter()+"/"+req.GetKey()]
	if !ok {
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	return &snapshotsapi.MountsResponse{Mounts: mounts}, nil
}

func (f fakeContainers) Get(ctx context.Context, req *containersapi.GetContainerRequest) (*containersapi.GetContainerResponse, error) {
	if err := f.check(ctx); err != nil {
		return nil, err
	}
	if !f.containers[req.GetID()] {
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	return &containersapi.GetContainerResponse{}, nil
}

func serve(t *testing.T, f *fakeContainerd) *Client {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes, which a test's temporary directory can exceed.
	dir, err := os.MkdirTemp("", "bofs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	address := filepath.Join(dir, "containerd.sock")
	l, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(srv, fakeSnapshots{fakeContainerd: f})
	containersapi.RegisterContainersServer(srv, fakeContainers{fakeContainerd: f})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	c, err := Dial(Options{Address: address, Namespace: "k8s.io", Snapshotter: "overlayfs", SnapshotterRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClient(t *testing.T) {
	ctx := context.Background()
	f := &fakeContainerd{
		namespace: "k8s.io",
		snapshots: map[string][]*types.Mount{
			"overlayfs/ctr":   {{Type: "overlay", Options: []string{"workdir=" + root + "/snapshots/7/work", "upperdir=" + root + "/snapshots/7/fs", "lowerdir=/l"}}},
			"overlayfs/bad":   {{Type: "overlay", Options: []string{"workdir=/tmp/work", "upperdir=/tmp/fs"}}},
			"other/elsewhere": {{Type: "overlay", Options: []string{"workdir=" + root + "/snapshots/8/work", "upperdir=" + root + "/snapshots/8/fs"}}},
		},
		containers: map[string]bool{"ctr": true},
	}
	c := serve(t, f)

	if dir, err := c.SnapshotDir(ctx, "ctr"); err != nil || dir != root+"/snapshots/7" {
		t.Errorf("SnapshotDir(ctr) = %q, %v", dir, err)
	}
	if _, err := c.SnapshotDir(ctx, "elsewhere"); !errdefs.IsNotFound(err) {
		t.Errorf("SnapshotDir of a snapshot in another snapshotter: error = %v, want not found", err)
	}
	if _, err := c.SnapshotDir(ctx, "bad"); err == nil || errdefs.IsNotFound(err) {
		t.Errorf("SnapshotDir of a snapshot outside the root: error = %v, want a refusal", err)
	}
	for _, tc := range []struct {
		id        string
		container bool
	}{{"ctr", true}, {"bad", false}, {"gone", false}} {
		if got, err := c.ContainerExists(ctx, tc.id); err != nil || got != tc.container {
			t.Errorf("ContainerExists(%s) = %v, %v; want %v", tc.id, got, err, tc.container)
		}
	}

	// Errors other than not found reach the caller.
	f.fail(errdefs.ErrUnavailable)
	if _, err := c.ContainerExists(ctx, "ctr"); !errors.Is(err, errdefs.ErrUnavailable) {
		t.Errorf("ContainerExists error = %v, want unavailable", err)
	}
	if _, err := c.SnapshotDir(ctx, "ctr"); !errors.Is(err, errdefs.ErrUnavailable) {
		t.Errorf("SnapshotDir error = %v, want unavailable", err)
	}
}
