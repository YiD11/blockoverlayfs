package containermeta

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestFromLabels(t *testing.T) {
	got := FromLabels("k8s.io", "abc", map[string]string{
		LabelKind:          KindContainer,
		LabelPodUID:        "uid-1",
		LabelPodNamespace:  "default",
		LabelPodName:       "web",
		LabelContainerName: "app",
	})
	want := Info{
		Namespace:     "k8s.io",
		ID:            "abc",
		Kind:          KindContainer,
		PodUID:        "uid-1",
		PodNamespace:  "default",
		PodName:       "web",
		ContainerName: "app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FromLabels() = %+v, want %+v", got, want)
	}
}

// Registered as containerd's client.go registers it, so MarshalAny encodes the spec the way containerd stores it
// in a container record.
func init() {
	typeurl.Register(&specs.Spec{}, "types.containerd.io", "opencontainers/runtime-spec", "1", "Spec")
}

func TestSpecAnnotations(t *testing.T) {
	spec := &specs.Spec{Version: specs.Version, Annotations: map[string]string{"a": "1", "io.kubernetes.cri.container-type": "container"}}
	encoded, err := typeurl.MarshalAny(spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SpecAnnotations(&anypb.Any{TypeUrl: encoded.GetTypeUrl(), Value: encoded.GetValue()})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, spec.Annotations) {
		t.Fatalf("SpecAnnotations() = %v, want %v", got, spec.Annotations)
	}
	if !json.Valid(encoded.GetValue()) {
		t.Fatalf("containerd no longer stores the spec as JSON: %q", encoded.GetValue())
	}
}

func TestSpecAnnotationsWithoutASpec(t *testing.T) {
	for _, spec := range []*anypb.Any{nil, {}} {
		if got, err := SpecAnnotations(spec); err != nil || got != nil {
			t.Fatalf("SpecAnnotations(%v) = %v, %v; want nothing", spec, got, err)
		}
	}
	if _, err := SpecAnnotations(&anypb.Any{TypeUrl: "x", Value: []byte("not json")}); err == nil {
		t.Fatal("SpecAnnotations() of a corrupt spec succeeded")
	}
}
