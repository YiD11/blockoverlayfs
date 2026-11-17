//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"
)

var _ = ginkgo.Describe("the plugin", ginkgo.Label("load"), func() {
	// containerd creates the container without a layer if the plugin takes longer, and formatting an image takes
	// longer the larger it is.
	ginkgo.It("attaches a large layer within containerd's request timeout", func() {
		limit := os.Getenv("E2E_LARGE_SIZE")
		if limit == "" {
			limit = "100Gi"
		}
		timeout, ok := nriConfig("plugin_request_timeout").(string)
		gomega.Expect(ok).To(gomega.BeTrue(), "containerd's configuration states its NRI plugin_request_timeout")

		ns := namespace()
		create(pod{Namespace: ns, Name: "large", Storage: fmt.Sprintf(`{"c":{"limit":%q}}`, limit)})
		ready(ns, "large")
		quantity := resource.MustParse(limit)
		expectBlock(ns, "large", "c", quantity.Value()-256*mi)

		// The plugin logs how long it took over the container, from the request to the attached layer.
		line := regexp.MustCompile(`msg="created block writable layer".* duration="?([^" ]+).* id=` + containerID(ns, "large", "c"))
		took := line.FindStringSubmatch(must(pluginLog()))
		gomega.Expect(took).NotTo(gomega.BeNil(), "the plugin logged the layer")
		gomega.Expect(must(time.ParseDuration(took[1]))).To(gomega.BeNumerically("<", must(time.ParseDuration(timeout))))
	})

	ginkgo.It("gives every container a layer and leaves none behind when Pods come and go together", func() {
		const replicas, rounds = 10, 3
		failures := []string{"attach_failed", "abandoned", "error"}
		before := map[string]float64{"block": creations("block")}
		for _, result := range failures {
			before[result] = creations(result)
		}

		// The group's cleanup checks that the store is empty once the namespace is gone.
		ns := namespace()
		apply("churn.yaml", pod{Namespace: ns})
		for range rounds {
			must(kubectl("-n", ns, "scale", "deployment/churn", fmt.Sprintf("--replicas=%d", replicas)))
			must(kubectl("-n", ns, "rollout", "status", "deployment/churn", "--timeout=300s"))
			must(kubectl("-n", ns, "scale", "deployment/churn", "--replicas=0"))
			must(kubectl("-n", ns, "wait", "--for=delete", "pod", "-l", "app=churn", "--timeout=300s"))
		}

		gomega.Expect(creations("block") - before["block"]).To(gomega.BeEquivalentTo(replicas * rounds))
		for _, result := range failures {
			gomega.Expect(creations(result)).To(gomega.Equal(before[result]), result)
		}
	})
})
