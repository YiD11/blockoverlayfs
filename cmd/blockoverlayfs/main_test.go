package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReadinessKeepsAFileWhileReady(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "run", "ready")
	ready := readiness(path)

	// Nothing to remove yet.
	ready.set(ctx, false)
	ready.set(ctx, true)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("after becoming ready: %v", err)
	}
	// Registering again finds the file there.
	ready.set(ctx, true)
	ready.set(ctx, false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("after losing readiness: %v, want the file gone", err)
	}
}

func TestReadinessWithoutAPathKeepsNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	readiness("").set(context.Background(), true)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("entries = %v, %v; want none", entries, err)
	}
}
