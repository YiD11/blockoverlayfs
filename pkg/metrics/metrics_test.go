package metrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/YiD11/blockoverlayfs/pkg/block"
)

func TestLayersCountsTheImagesInTheStore(t *testing.T) {
	store, err := block.Open(filepath.Join(t.TempDir(), "images"))
	if err != nil {
		t.Fatal(err)
	}
	RegisterLayers(store)
	collector := layers{store: store}
	if got := testutil.ToFloat64(collector); got != 0 {
		t.Errorf("layers = %v in an empty store, want 0", got)
	}

	// The last one is not an image.
	for _, name := range []string{"a.img", "b.img", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(store.Root(), name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := testutil.ToFloat64(collector); got != 2 {
		t.Errorf("layers = %v, want 2", got)
	}
	if got, err := testutil.GatherAndCount(registry, namespace+"_layers"); err != nil || got != 1 {
		t.Errorf("the registry has the metric %d times, %v; want once", got, err)
	}

	// A store that cannot be listed leaves the metric out.
	if err := os.RemoveAll(store.Root()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.CollectAndCount(collector); got != 0 {
		t.Errorf("collected %d metrics from a store that is gone, want none", got)
	}
}
