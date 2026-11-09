package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/adaptation"
	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"

	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

// TestPluginOverNRI runs the plugin through NRI's own runtime side, as containerd embeds it: the plugin registers,
// subscribes to the events it handles, and gets a container's creation and removal.
func TestPluginOverNRI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Unix socket paths are limited to about 100 bytes, which a test's temporary directory can exceed.
	dir, err := os.MkdirTemp("", "bofs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "nri.sock")

	synced := make(chan struct{}, 1)
	sync := func(ctx context.Context, cb adaptation.SyncCB) error {
		_, err := cb(ctx, nil, nil)
		synced <- struct{}{}
		return err
	}
	update := func(context.Context, []*adaptation.ContainerUpdate) ([]*adaptation.ContainerUpdate, error) {
		return nil, nil
	}
	runtime, err := adaptation.New("test", "v0", sync, update,
		adaptation.WithSocketPath(socket),
		adaptation.WithPluginPath(filepath.Join(dir, "plugins")),
		adaptation.WithPluginConfigPath(filepath.Join(dir, "conf")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)

	rt, layers := newFakeRuntime(), newFakeLayers()
	rt.snapshots["ctr"] = "/snapshots/7"
	p := New(rt, layers, sizing.Policy{})
	s, err := stub.New(p, stub.WithPluginName("blockoverlayfs"), stub.WithPluginIdx("10"), stub.WithSocketPath(socket))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	select {
	case <-synced:
	case <-ctx.Done():
		t.Fatal("the plugin was never synchronized")
	}

	pod := pod(`{"app":{"limit":"1Gi"}}`)
	ctr := container("ctr", "app")
	// The runtime adds the plugin to the ones it calls just after synchronizing it.
	for {
		if _, err := runtime.CreateContainer(ctx, &adaptation.CreateContainerRequest{Pod: pod, Container: ctr}); err != nil {
			t.Fatal(err)
		}
		if l, ok := layers.get("ctr"); ok {
			if l.size != 1<<30 || !l.mounted {
				t.Fatalf("layer = %+v, want 1Gi mounted", l)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the plugin never handled CreateContainer")
		case <-time.After(10 * time.Millisecond):
		}
	}

	evt := &adaptation.StateChangeEvent{Event: api.Event_REMOVE_CONTAINER, Pod: pod, Container: ctr}
	if err := runtime.RemoveContainer(ctx, evt); err != nil {
		t.Fatal(err)
	}
	p.Wait()
	if l, ok := layers.get("ctr"); ok {
		t.Errorf("layer %+v left behind after RemoveContainer", l)
	}
}
