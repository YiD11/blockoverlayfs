package snapshot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/YiD11/blockoverlayfs/pkg/annotation"
	"github.com/YiD11/blockoverlayfs/pkg/containermeta"
	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

type failingLookup struct{}

func (failingLookup) Lookup(context.Context, string, string) (containermeta.Info, error) {
	return containermeta.Info{}, errInjected
}

func TestCRIDecider(t *testing.T) {
	declared := map[string]string{annotation.Key: `{"app":{"request":"1Gi","limit":"4Gi"},"plain":{}}`}
	app := containermeta.Info{
		Namespace: "k8s.io", ID: "app", Kind: containermeta.KindContainer, SnapshotKey: "app",
		PodUID: "uid-1", PodNamespace: "default", PodName: "web", ContainerName: "app", Annotations: declared,
	}
	with := func(id string, change func(*containermeta.Info)) containermeta.Info {
		c := app
		c.ID, c.SnapshotKey, c.ContainerName = id, id, id
		change(&c)
		return c
	}
	containers := fakeContainers{
		"k8s.io/app":      app,
		"k8s.io/plain":    with("plain", func(*containermeta.Info) {}),
		"k8s.io/debugger": with("debugger", func(*containermeta.Info) {}),
		"k8s.io/static":   with("static", func(c *containermeta.Info) { c.Annotations = map[string]string{"other": "x"} }),
		"k8s.io/corrupt":  with("corrupt", func(c *containermeta.Info) { c.Annotations = map[string]string{annotation.Key: "{"} }),
		"k8s.io/sandbox":  {Kind: containermeta.KindSandbox, SnapshotKey: "sandbox", Annotations: declared},
		"k8s.io/other":    {Kind: containermeta.KindContainer, SnapshotKey: "different", Annotations: declared},
		"default/ctr":     {SnapshotKey: "ctr"},
	}
	policy := sizing.Policy{Enabled: true, LimitOffset: gi, RequestPercent: 105, DefaultSize: 10 * gi}

	tests := []struct {
		name       string
		key        string
		policy     sizing.Policy
		wantFinal  bool
		wantSize   int64
		wantReason string
	}{
		{"app container with a limit", "k8s.io/12/app", policy, true, 3 * gi, "limit"},
		{"undeclared container gets the default size", "k8s.io/12/plain", policy, true, 10 * gi, "default_size"},
		{"container missing from the annotation", "k8s.io/12/debugger", policy, true, 10 * gi, "not listed"},
		{"pod without the annotation", "k8s.io/12/static", policy, true, 10 * gi, "no " + annotation.Key},
		{"unreadable annotation", "k8s.io/12/corrupt", policy, true, 10 * gi, "unreadable"},
		{"undeclared without a default size keeps a directory", "k8s.io/12/plain", sizing.Policy{Enabled: true}, true, 0, ""},
		{"disabled policy decides nothing", "k8s.io/12/app", sizing.Policy{}, false, 0, ""},
		{"sandbox container", "k8s.io/12/sandbox", policy, false, 0, ""},
		{"container record not created yet", "k8s.io/12/unknown", policy, false, 0, ""},
		{"container record of another snapshot", "k8s.io/12/other", policy, false, 0, ""},
		{"container created outside CRI", "default/3/ctr", policy, false, 0, ""},
		{"image unpack", "k8s.io/12/extract-1-2 sha256:abc", policy, false, 0, ""},
		{"key outside the metadata layer", "app", policy, false, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &CRIDecider{Lookup: containers, Policy: tt.policy}
			got, err := d.Decide(context.Background(), tt.key)
			if err != nil {
				t.Fatal(err)
			}
			if got.Final != tt.wantFinal || got.Size != tt.wantSize {
				t.Fatalf("Decide() = %+v, want final=%v size=%d", got, tt.wantFinal, tt.wantSize)
			}
			if got.Final && (got.Reason == "" || got.Container == nil || got.Container.ID == "") {
				t.Fatalf("Decide() = %+v, want a reason and the container", got)
			}
			if !strings.Contains(got.Reason, tt.wantReason) {
				t.Fatalf("Decide().Reason = %q, want it to mention %q", got.Reason, tt.wantReason)
			}
		})
	}

	got, err := (&CRIDecider{Lookup: containers, Policy: policy}).Decide(context.Background(), "k8s.io/12/app")
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Container; c.Namespace != "k8s.io" || c.PodNamespace != "default" || c.PodName != "web" || c.PodUID != "uid-1" || c.Name != "app" {
		t.Fatalf("recorded container = %+v", c)
	}
}

func TestCRIDeciderFailsWhenContainerdFails(t *testing.T) {
	d := &CRIDecider{Lookup: failingLookup{}, Policy: sizing.Policy{Enabled: true}}
	if _, err := d.Decide(context.Background(), "k8s.io/12/app"); !errors.Is(err, errInjected) {
		t.Fatalf("Decide() = %v, want the lookup error", err)
	}
}
