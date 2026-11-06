package block

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func entries(t *testing.T, root string) []string {
	t.Helper()
	des, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func TestRecordDirectoryPersists(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	st := State{Key: "k8s.io/12/abc", Reason: "no ephemeral-storage", Container: &Container{Namespace: "k8s.io", ID: "abc"}}
	if err := s.RecordDirectory("7", st); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDirectory("7", st); err == nil {
		t.Fatal("recording the same layer twice succeeded")
	}

	// A second store reads the record from disk rather than from the first store's cache.
	got, ok, err := openStore(t, root).Load("7")
	if err != nil || !ok {
		t.Fatalf("Load() = %+v, %v, %v", got, ok, err)
	}
	if got.Key != st.Key || got.Size != 0 || got.Version != stateVersion || got.Container.ID != "abc" {
		t.Fatalf("Load() = %+v, want %+v", got, st)
	}
	if names := entries(t, root); !slices.Equal(names, []string{"7"}) {
		t.Fatalf("store holds %v, want only the layer", names)
	}
}

func TestRecordDirectoryRejectsSize(t *testing.T) {
	if err := openStore(t, t.TempDir()).RecordDirectory("7", State{Key: "k", Size: 1}); err == nil {
		t.Fatal("RecordDirectory() accepted a size")
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	if _, ok, err := s.Load("8"); ok || err != nil {
		t.Fatalf("Load() of an absent layer = %v, %v", ok, err)
	}

	if err := os.Mkdir(filepath.Join(root, "9"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("9"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load() without a state = %v, want ErrCorrupt", err)
	}
	for _, content := range []string{"{not json", `{"version":99,"key":"k"}`, `{"version":1,"key":""}`, `{"version":1,"key":"k","size":-1}`} {
		if err := os.WriteFile(filepath.Join(root, "9", stateName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Load("9"); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Load() of %q = %v, want ErrCorrupt", content, err)
		}
	}
}

func TestDetachThenReap(t *testing.T) {
	root := t.TempDir()
	s := openStore(t, root)
	if err := s.RecordDirectory("7", State{Key: "k8s.io/12/abc"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Detach(context.Background(), "7"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Load("7"); ok || err != nil {
		t.Fatalf("Load() after Detach = %v, %v; want nothing recorded", ok, err)
	}
	if err := s.Detach(context.Background(), "7"); err != nil {
		t.Fatalf("Detach() of a detached layer = %v", err)
	}
	s.Reap(context.Background())
	if names := entries(t, root); len(names) != 0 {
		t.Fatalf("store holds %v after Reap, want nothing", names)
	}
}

func TestOpenSweepsLeftovers(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"new-3-123", "new-commit-4-5", "trash-5-6", "11"} {
		if err := os.MkdirAll(filepath.Join(root, dir, "rw"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	openStore(t, root)
	if names := entries(t, root); !slices.Equal(names, []string{"11"}) {
		t.Fatalf("store holds %v after Open, want only the layer", names)
	}
}

func TestDecided(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"1", "22", "new-3-1", "trash-4-1", "notanid", "-5", "+6"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "7"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Store{root: root}
	ids, err := s.Decided()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"1", "22"}) {
		t.Fatalf("Decided() = %v, want [1 22]", ids)
	}
}

func TestLayerPaths(t *testing.T) {
	l := (&Store{root: "/r"}).Layer("7")
	got := strings.Join([]string{l.Dir, l.Image(), l.Mountpoint(), l.Upper(), l.Work()}, " ")
	if want := "/r/7 /r/7/rwlayer.img /r/7/rw /r/7/rw/upper /r/7/rw/work"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
}
