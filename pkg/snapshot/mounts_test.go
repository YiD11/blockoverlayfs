package snapshot

import (
	"reflect"
	"testing"

	"github.com/containerd/containerd/mount"

	"github.com/YiD11/blockoverlayfs/pkg/block"
)

func TestRewriteMounts(t *testing.T) {
	l := block.Layer{ID: "7", Dir: "/b/7"}
	const upper = "/o/snapshots/7/fs"

	overlay := []mount.Mount{{Type: "overlay", Source: "overlay", Options: []string{
		"index=off", "workdir=/o/snapshots/7/work", "upperdir=" + upper, "lowerdir=/o/snapshots/1/fs",
	}}}
	got, err := rewriteMounts(overlay, upper, l)
	if err != nil {
		t.Fatal(err)
	}
	want := []mount.Mount{{Type: "overlay", Source: "overlay", Options: []string{
		"index=off", "workdir=/b/7/rw/work", "upperdir=/b/7/rw/upper", "lowerdir=/o/snapshots/1/fs",
	}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteMounts() = %+v, want %+v", got, want)
	}
	if overlay[0].Options[2] != "upperdir="+upper {
		t.Fatal("rewriteMounts() changed its input")
	}

	bind := []mount.Mount{{Type: "bind", Source: upper, Options: []string{"rw", "rbind"}}}
	got, err = rewriteMounts(bind, upper, l)
	if err != nil {
		t.Fatal(err)
	}
	if want := []mount.Mount{{Type: "bind", Source: "/b/7/rw/upper", Options: []string{"rw", "rbind"}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteMounts() = %+v, want %+v", got, want)
	}
}

func TestRewriteMountsRejectsUnexpectedMounts(t *testing.T) {
	l := block.Layer{ID: "7", Dir: "/b/7"}
	const upper = "/o/snapshots/7/fs"
	tests := []struct {
		name   string
		mounts []mount.Mount
	}{
		{"no mounts", nil},
		{"two mounts", []mount.Mount{{Type: "bind", Source: upper}, {Type: "bind", Source: upper}}},
		{"another upper directory", []mount.Mount{{Type: "overlay", Options: []string{"workdir=/w", "upperdir=/o/snapshots/8/fs", "lowerdir=/l"}}}},
		{"no work directory", []mount.Mount{{Type: "overlay", Options: []string{"upperdir=" + upper, "lowerdir=/l"}}}},
		{"read-only overlay", []mount.Mount{{Type: "overlay", Options: []string{"lowerdir=/l1:/l2"}}}},
		{"bind of another directory", []mount.Mount{{Type: "bind", Source: "/o/snapshots/1/fs"}}},
		{"unknown type", []mount.Mount{{Type: "tmpfs", Source: "tmpfs"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := rewriteMounts(tt.mounts, upper, l); err == nil {
				t.Fatalf("rewriteMounts() = %+v, want an error", got)
			}
		})
	}
}
