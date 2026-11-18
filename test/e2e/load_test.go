//go:build e2e

package e2e

import (
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("the plugin", ginkgo.Label("load"), func() {
	// Formatting an image takes longer the larger it is, and containerd creates the container without a layer if
	// the plugin takes longer than containerd's NRI request timeout.
	ginkgo.It("attaches a large layer within containerd's request timeout", func() {
		ns := namespace()
		create(pod{Namespace: ns, Name: "large", Storage: `{"c":{"limit":"100Gi"}}`})
		ready(ns, "large")
		expectBlock(ns, "large", "c", layerSize(0, 100<<30))
	})

	ginkgo.It("gives every container a layer and leaves none behind when Pods come and go together", func() {
		const replicas, rounds = 10, 3
		failures := []string{"attach_failed", "abandoned", "error"}
		before := map[string]float64{"block": creations("block")}
		for _, result := range failures {
			before[result] = creations(result)
		}

		// The group's cleanup checks that no layer is left once the namespace is gone.
		ns := namespace()
		apply("churn.yaml", pod{Namespace: ns})
		for range rounds {
			must(kubectl("-n", ns, "scale", "deployment/churn", fmt.Sprintf("--replicas=%d", replicas)))
			must(kubectl("-n", ns, "rollout", "status", "deployment/churn", "--timeout=300s"))
			must(kubectl("-n", ns, "scale", "deployment/churn", "--replicas=0"))
			must(kubectl("-n", ns, "wait", "--for=delete", "pod", "-l", "app=churn", "--timeout=300s"))
		}

		// At least: the plugin counts the node's other containers too.
		gomega.Expect(creations("block") - before["block"]).To(gomega.BeNumerically(">=", replicas*rounds))
		for _, result := range failures {
			gomega.Expect(creations(result)).To(gomega.Equal(before[result]), result)
		}
	})
})
