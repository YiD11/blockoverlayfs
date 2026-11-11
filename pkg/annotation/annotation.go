// Package annotation is the format of the Pod annotation that declares each container's ephemeral-storage. CRI
// carries no ephemeral-storage, so the platform that creates a Pod copies the request and limit of each of its
// containers into this annotation. NRI hands the plugin the Pod's annotations with every container it creates.
package annotation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

// Key names the annotation. Whoever can set it sizes the writable layers of the Pod's containers, so only the
// platform, or an admission webhook acting for it, should write it.
const Key = "blockoverlayfs.io/ephemeral-storage"

// Value is the annotation's content: a JSON object keyed by container name, for example
//
//	{"app":{"request":"10Gi","limit":"20Gi"},"init":{"limit":"1Gi"}}
//
// Containers and init containers are listed by name; an empty entry declares nothing.
type Value map[string]Container

// Container holds a container's ephemeral-storage quantities, in the notation of the Pod spec. An omitted field is
// undeclared.
type Container struct {
	Request *resource.Quantity `json:"request,omitempty"`
	Limit   *resource.Quantity `json:"limit,omitempty"`
}

// Decode rejects unknown fields, so a misspelt "limits" fails loudly instead of leaving the container undeclared.
func Decode(s string) (Value, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var v Value
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode %s: %w", Key, err)
	}
	if v == nil {
		return nil, fmt.Errorf("decode %s: %w", Key, errors.New("not a JSON object"))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode %s: %w", Key, errors.New("data after the JSON object"))
	}
	return v, nil
}

// Resources returns what the named container declares, and false if the annotation does not list it, as for an
// ephemeral container added after the Pod was created.
func (v Value) Resources(name string) (sizing.Resources, bool) {
	c, ok := v[name]
	if !ok {
		return sizing.Resources{}, false
	}
	return sizing.Resources{Request: positive(c.Request), Limit: positive(c.Limit)}, true
}

// Only a positive quantity counts as declared, as in kubelet. Value rounds a fractional byte count up.
func positive(q *resource.Quantity) *int64 {
	if q == nil || q.Sign() <= 0 {
		return nil
	}
	v := q.Value()
	return &v
}
