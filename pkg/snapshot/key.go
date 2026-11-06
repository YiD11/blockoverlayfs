package snapshot

import (
	"strings"

	"github.com/containerd/containerd/snapshots"
)

// Backend keys are built by containerd's metadata layer as "<namespace>/<internal id>/<client key>".
type Ref struct {
	Namespace string
	ID        string
	Name      string
}

func ParseKey(key string) (Ref, bool) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Ref{}, false
	}
	return Ref{Namespace: parts[0], ID: parts[1], Name: parts[2]}, true
}

func (r Ref) IsUnpack() bool {
	return strings.HasPrefix(r.Name, snapshots.UnpackKeyPrefix+"-")
}
