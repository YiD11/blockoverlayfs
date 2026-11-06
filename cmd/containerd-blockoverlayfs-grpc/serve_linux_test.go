//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/pkg/testutil"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/snapshot"
)

// containers gives every key starting with "ctr-" a block layer, standing in for CRI's containers.
type containers struct{}

func (containers) Decide(_ context.Context, key string) (snapshot.Decision, error) {
	if !strings.HasPrefix(key, "ctr-") {
		return snapshot.Decision{}, nil
	}
	return snapshot.Decision{Final: true, Size: 64 << 20, Reason: "test"}, nil
}

// TestServeOverGRPC drives the daemon's server the way containerd's proxy plugin does.
func TestServeOverGRPC(t *testing.T) {
	testutil.RequiresRoot(t)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inner, err := snapshot.NewOverlay(filepath.Join(root, "overlay"), snapshot.OverlayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	layers, err := block.Open(ctx, filepath.Join(root, "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	sn := snapshot.New(inner, layers, snapshot.Options{Decider: containers{}})
	defer sn.Close()
	address := filepath.Join(root, "run", "snapshotter.sock")
	served := make(chan error, 1)
	go func() { served <- serve(ctx, address, sn) }()

	conn, err := grpc.Dial("unix://"+address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := proxy.NewSnapshotter(snapshotsapi.NewSnapshotsClient(conn), "blockoverlayfs")
	// A served Stat of an unknown key answers not found; anything else means the server is not up yet.
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := client.Stat(ctx, "absent")
		if errdefs.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	mounts, err := client.Prepare(ctx, "base-active", "")
	if err != nil {
		t.Fatal(err)
	}
	mounted(t, mounts, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "base"), []byte("b"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err := client.Commit(ctx, "base", "base-active"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prepare(ctx, "ctr-1", "base"); err != nil {
		t.Fatal(err)
	}
	if mounts, err = client.Mounts(ctx, "ctr-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(mounts[0].Options, ","), "upperdir="+layers.Root()+"/") {
		t.Fatalf("mounts over gRPC = %+v, want the upper directory in a block layer", mounts)
	}
	mounted(t, mounts, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if u, err := client.Usage(ctx, "ctr-1"); err != nil || u.Size < 1<<20 {
		t.Fatalf("Usage() over gRPC = %+v, %v; want at least the 1Mi written", u, err)
	}
	info, err := client.Stat(ctx, "ctr-1")
	if err != nil || info.Labels[snapshot.WritableMountpointLabel] == "" {
		t.Fatalf("Stat() over gRPC = %+v, %v; want the block layer's label", info, err)
	}

	for _, key := range []string{"ctr-1", "base"} {
		if err := client.Remove(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	// containerd reaches Cleanup through the snapshots.Cleaner assertion in snapshotservice.
	if err := client.(snapshots.Cleaner).Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(layers.Root()); err != nil || len(entries) != 0 {
		t.Fatalf("block layers left after Cleanup: %v, %v", entries, err)
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func mounted(t *testing.T, mounts []mount.Mount, fn func(dir string)) {
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
