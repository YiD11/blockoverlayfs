// Package block keeps size-limited container writable layers. Each container gets one sparse ext4 image,
// loop-mounted over the directory in which containerd's overlayfs snapshotter keeps the upper and work directories
// of the container's rootfs, so that everything the container writes lands in the image. The images are the only
// state on disk: what each is to its container is read back from the kernel's mount table, as a Status.
package block

import (
	"os"
	"regexp"
	"strings"
)

const imageSuffix = ".img"

// Status is what a container's image is to the container.
type Status int

const (
	// Detached is an image that is mounted nowhere, or no image at all.
	Detached Status = iota
	// Incomplete is an image mounted over a snapshot directory without an upper directory in it. The snapshot cannot
	// be mounted until the image is taken away.
	Incomplete
	// Attached is an image mounted over a snapshot directory, holding the snapshot's upper directory.
	Attached
)

func (s Status) String() string {
	switch s {
	case Detached:
		return "detached"
	case Incomplete:
		return "incomplete"
	case Attached:
		return "attached"
	}
	return "unknown"
}

// validID admits a container ID as a single path element. CRI's IDs are 64 hexadecimal digits.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// ValidID reports whether id can name an image.
func ValidID(id string) bool { return validID.MatchString(id) }

// Store keeps the images in one directory, as <root>/<container ID>.img. Callers serialize the operations on one
// container; the store keeps no state of its own.
type Store struct {
	root string
}

// Open opens the store at root, creating the directory if needed.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// Root returns the store's directory.
func (s *Store) Root() string { return s.root }

// IDs lists the containers that have an image, mounted or not.
func (s *Store) IDs() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), imageSuffix)
		if ok && e.Type().IsRegular() && ValidID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
