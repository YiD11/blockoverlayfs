//go:build e2e

package e2e

import (
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/pelletier/go-toml/v2"
)

// containerdConfig returns the configuration containerd runs with on the node.
func containerdConfig() map[string]any {
	ginkgo.GinkgoHelper()
	var config map[string]any
	gomega.Expect(toml.Unmarshal([]byte(must(nodeExec("containerd config dump"))), &config)).To(gomega.Succeed())
	return config
}

// nriConfig returns a setting of containerd's NRI plugin, nil when it is not set.
func nriConfig(path ...string) any {
	ginkgo.GinkgoHelper()
	var value any = containerdConfig()
	for _, key := range append([]string{"plugins", "io.containerd.nri.v1.nri"}, path...) {
		table, _ := value.(map[string]any)
		value = table[key]
	}
	return value
}

// movedRoot is the store of the spec that moves it.
const movedRoot = storeRoot + "-moved"

// Each spec deploys the chart with values of its own, and with the base values again once it is over. The plugin's
// counters start at zero then: changed values replace the plugin's Pod.
var _ = ginkgo.Describe("the chart's values", ginkgo.Label("config"), func() {
	ginkgo.It("config.enabled=false keeps new containers on directories and still removes layers", func() {
		ns := namespace()
		create(pod{Namespace: ns, Name: "before", Storage: storage600})
		ready(ns, "before")
		expectBlock(ns, "before", "c", layer600)
		before := containerID(ns, "before", "c")

		ginkgo.DeferCleanup(func() { upgrade() })
		upgrade("--set", "config.enabled=false")
		gomega.Expect(metric("blockoverlayfs_enabled")).To(gomega.BeZero())
		gomega.Expect(layer(before)).To(gomega.Equal("block"), "a layer attached before stays")

		create(pod{Namespace: ns, Name: "after", Storage: storage600})
		ready(ns, "after")
		expectDir(ns, "after", "c")
		gomega.Expect(creations("disabled")).To(gomega.BeNumerically(">=", 1))

		must(kubectl("-n", ns, "delete", "pod", "before"))
		gomega.Eventually(layer).WithArguments(before).WithTimeout(5 * time.Minute).Should(gomega.Equal("dir"))
	})

	ginkgo.It("config.includedNamespaces, excludedNamespaces and podSelector choose the Pods", func() {
		a, b, c := namespace(), namespace(), namespace()
		ginkgo.DeferCleanup(func() { upgrade() })
		upgrade(
			"--set-json", fmt.Sprintf("config.includedNamespaces=[%q,%q]", a, b),
			"--set-json", fmt.Sprintf("config.excludedNamespaces=[%q]", b),
			"--set-json", `config.podSelector.matchExpressions=[{"key":"blockoverlayfs.io/skip","operator":"NotIn","values":["true"]}]`,
			"--set", "config.size.defaultSize=",
		)
		create(pod{Namespace: a, Name: "selected", Storage: storage600})
		create(pod{Namespace: a, Name: "skipped", Storage: storage600, Labels: map[string]string{"blockoverlayfs.io/skip": "true"}})
		create(pod{Namespace: a, Name: "undeclared"})
		create(pod{Namespace: b, Name: "excluded", Storage: storage600})
		create(pod{Namespace: c, Name: "left-out", Storage: storage600})
		ready(a, "selected", "skipped", "undeclared")
		ready(b, "excluded")
		ready(c, "left-out")

		expectBlock(a, "selected", "c", layer600)
		expectDir(a, "skipped", "c")
		expectDir(a, "undeclared", "c")
		expectDir(b, "excluded", "c")
		expectDir(c, "left-out", "c")
		// Only Pods of the first namespace reach the label selector and the size policy.
		gomega.Expect(creations("block")).To(gomega.BeEquivalentTo(1))
		gomega.Expect(creations("pod_selector")).To(gomega.BeEquivalentTo(1))
		gomega.Expect(creations("no_size")).To(gomega.BeEquivalentTo(1))
		gomega.Expect(creations("namespace")).To(gomega.BeNumerically(">=", 2))
	})

	ginkgo.It("an empty config.podSelector selects every Pod", func() {
		ns := namespace()
		ginkgo.DeferCleanup(func() { upgrade() })
		// Without the base values' selector every new container of the node would get a layer.
		upgrade("--set", "config.podSelector.matchLabels=null", "--set-json", fmt.Sprintf("config.includedNamespaces=[%q]", ns))
		create(pod{Namespace: ns, Name: "unlabelled", Unselected: true})
		ready(ns, "unlabelled")
		expectBlock(ns, "unlabelled", "c", defaultSize)
	})

	ginkgo.It("config.size sets the size policy", func() {
		ns := namespace()
		ginkgo.DeferCleanup(func() { upgrade() })
		upgrade("--set", "config.size.limitOffset=0", "--set", "config.size.requestPercent=150", "--set", "config.size.defaultSize=1Gi")
		create(pod{Namespace: ns, Name: "limit", Storage: storage600})
		create(pod{Namespace: ns, Name: "request", Storage: `{"c":{"request":"400Mi"}}`})
		create(pod{Namespace: ns, Name: "undeclared"})
		ready(ns, "limit", "request", "undeclared")
		expectBlock(ns, "limit", "c", 600*mi)
		expectBlock(ns, "request", "c", 600*mi)
		expectBlock(ns, "undeclared", "c", 1024*mi)
	})

	ginkgo.It("config.root moves the store", func() {
		ns := namespace()
		ginkgo.DeferCleanup(func() { upgrade() })
		upgrade("--set", "config.root="+movedRoot)
		create(pod{Namespace: ns, Name: "moved", Storage: storage600})
		ready(ns, "moved")
		id := containerID(ns, "moved", "c")
		gomega.Expect(layerIn(movedRoot, id)).To(gomega.Equal("block"))
		gomega.Expect(layer(id)).To(gomega.Equal("dir"), "nothing in the base values' store")

		must(kubectl("-n", ns, "delete", "pod", "moved"))
		gomega.Eventually(layerIn).WithArguments(movedRoot, id).WithTimeout(5 * time.Minute).Should(gomega.Equal("dir"))
	})

	ginkgo.It("config.containerd.snapshotter leaves the containers of other snapshotters alone", func() {
		const native = "/var/lib/containerd/io.containerd.snapshotter.v1.native"
		if _, err := nodeExec("test -d " + native); err != nil {
			ginkgo.Skip("containerd on the node has no native snapshotter to configure the plugin for")
		}
		ns := namespace()
		ginkgo.DeferCleanup(func() { upgrade() })
		upgrade("--set", "config.containerd.snapshotter=native", "--set", "config.containerd.snapshotterRoot="+native)
		// Its rootfs is on the overlayfs snapshotter, as that of every container of the node.
		create(pod{Namespace: ns, Name: "overlay", Storage: storage600})
		ready(ns, "overlay")
		expectDir(ns, "overlay", "c")
		gomega.Expect(creations("other_snapshotter")).To(gomega.BeEquivalentTo(1))
	})

	ginkgo.It("a Pod that requires the plugin waits for it", func() {
		if nriConfig("default_validator", "enable") != true {
			ginkgo.Skip("containerd on the node runs without NRI's validator")
		}
		ns := namespace()
		ginkgo.DeferCleanup(func() { upgrade() })
		withoutPlugin()

		create(pod{Namespace: ns, Name: "requires", Storage: storage600, Annotations: map[string]string{"required-plugins.noderesource.dev": `["blockoverlayfs"]`}})
		create(pod{Namespace: ns, Name: "free", Storage: storage600})
		ready(ns, "free")
		expectDir(ns, "free", "c")
		// containerd refuses its container for as long as the plugin is away; kubelet goes on trying.
		gomega.Consistently(field).WithArguments(ns, "requires", `{.status.containerStatuses[*].containerID}`).
			WithTimeout(20 * time.Second).Should(gomega.BeEmpty())

		upgrade()
		ready(ns, "requires")
		expectBlock(ns, "requires", "c", layer600)
	})
})
