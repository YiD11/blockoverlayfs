package snapshot

import (
	"fmt"
	"strings"

	"github.com/containerd/containerd/mount"

	"github.com/YiD11/blockoverlayfs/pkg/block"
)

// rewriteMounts points the inner overlay's mount of an active snapshot at its block layer. The inner overlay
// returns an overlay mount, or a bind mount of the directory upper for a snapshot without parents. Anything else
// is an error, never a silent fallback to the directory upper.
func rewriteMounts(mounts []mount.Mount, innerUpper string, l block.Layer) ([]mount.Mount, error) {
	if len(mounts) != 1 {
		return nil, fmt.Errorf("expected one mount from the inner overlay, got %d", len(mounts))
	}
	m := mounts[0]
	m.Options = append([]string(nil), m.Options...)
	switch m.Type {
	case "overlay":
		var upper, work bool
		for i, o := range m.Options {
			switch {
			case strings.HasPrefix(o, "upperdir="):
				if v := strings.TrimPrefix(o, "upperdir="); v != innerUpper {
					return nil, fmt.Errorf("inner overlay mounts upper directory %s, expected %s", v, innerUpper)
				}
				m.Options[i], upper = "upperdir="+l.Upper(), true
			case strings.HasPrefix(o, "workdir="):
				m.Options[i], work = "workdir="+l.Work(), true
			}
		}
		if !upper || !work {
			return nil, fmt.Errorf("inner overlay mount lacks upperdir or workdir: %v", mounts[0].Options)
		}
	case "bind":
		if m.Source != innerUpper {
			return nil, fmt.Errorf("inner overlay binds %s, expected %s", m.Source, innerUpper)
		}
		m.Source = l.Upper()
	default:
		return nil, fmt.Errorf("unexpected %q mount from the inner overlay", m.Type)
	}
	return []mount.Mount{m}, nil
}
