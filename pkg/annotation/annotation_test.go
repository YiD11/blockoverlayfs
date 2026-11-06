package annotation

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestDecode(t *testing.T) {
	v, err := Decode(`{"app":{"request":"10Gi","limit":"20Gi"},"init":{"limit":"1Gi"},"sidecar":{},"debug":null}`)
	if err != nil {
		t.Fatal(err)
	}
	res, ok := v.Resources("app")
	if !ok || res.Request == nil || *res.Request != 10<<30 || res.Limit == nil || *res.Limit != 20<<30 {
		t.Fatalf("Resources(app) = %+v, %v", res, ok)
	}
	if res, ok := v.Resources("init"); !ok || res.Request != nil || res.Limit == nil || *res.Limit != 1<<30 {
		t.Fatalf("Resources(init) = %+v, %v; want only a 1Gi limit", res, ok)
	}
	for _, name := range []string{"sidecar", "debug"} {
		if res, ok := v.Resources(name); !ok || res.Request != nil || res.Limit != nil {
			t.Fatalf("Resources(%s) = %+v, %v; want listed and undeclared", name, res, ok)
		}
	}
	if _, ok := v.Resources("debugger"); ok {
		t.Fatal("Resources(debugger) is listed, want unlisted")
	}
}

func TestDecodeEmptyObject(t *testing.T) {
	v, err := Decode(" {} \n")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.Resources("app"); ok {
		t.Fatal("Resources(app) is listed in an empty annotation")
	}
}

func TestResourcesIgnoresNonPositiveQuantities(t *testing.T) {
	v, err := Decode(`{"app":{"request":"0","limit":"-1"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if res, ok := v.Resources("app"); !ok || res.Request != nil || res.Limit != nil {
		t.Fatalf("Resources(app) = %+v, %v; want undeclared", res, ok)
	}
}

func TestResourcesRoundsUp(t *testing.T) {
	v, err := Decode(`{"app":{"limit":"1500m"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := v.Resources("app"); res.Limit == nil || *res.Limit != 2 {
		t.Fatalf("Resources(app).Limit = %v, want 2", res.Limit)
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, s := range []string{
		``,
		`{`,
		`null`,
		`[]`,
		`"20Gi"`,
		`{"app":"20Gi"}`,
		`{"app":{"limit":"lots"}}`,
		`{"app":{"limits":"20Gi"}}`,
		`{"containers":{"app":{"limit":"20Gi"}}}`,
		`{"app":{}} {}`,
	} {
		if _, err := Decode(s); err == nil || !strings.Contains(err.Error(), Key) {
			t.Errorf("Decode(%q) = %v, want an error naming the annotation", s, err)
		}
	}
}

// The containerd snippet must list Key in pod_annotations, or the annotation never reaches the snapshotter.
func TestContainerdSnippetPassesTheAnnotation(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/containerd/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Plugins map[string]struct {
			Containerd struct {
				Runtimes map[string]struct {
					PodAnnotations []string `toml:"pod_annotations"`
				} `toml:"runtimes"`
			} `toml:"containerd"`
		} `toml:"plugins"`
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	runtimes := cfg.Plugins["io.containerd.grpc.v1.cri"].Containerd.Runtimes
	if len(runtimes) == 0 {
		t.Fatal("the snippet configures no runtime handler")
	}
	for name, rt := range runtimes {
		if !slices.Contains(rt.PodAnnotations, Key) {
			t.Errorf("runtime handler %s: pod_annotations = %v, want it to list %s", name, rt.PodAnnotations, Key)
		}
	}
}
