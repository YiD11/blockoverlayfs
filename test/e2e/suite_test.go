//go:build e2e

// Package e2e tests the plugin a cluster runs, on one node of it: kubelet, containerd, NRI, the plugin and the shims
// together. It drives the cluster with kubectl, and needs no privilege on the node: it reads the node's loop devices
// from a Pod of its own, and the plugin's log and metrics.
//
// The suite deploys no plugin. The node under test has to run one that is ready, enabled, serves metrics and gives
// the suite's Pods block layers, and the specs expect what its configuration says. The specs leave the plugin,
// containerd and the node's other Pods alone: they act only on the Pods they create. Suites that run on nodes of one
// cluster at the same time each need their own namespace, which E2E_SUITE_NAMESPACE names.
package e2e

import (
	"cmp"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/YiD11/blockoverlayfs/pkg/config"
	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

const (
	// probePod is the Pod through which the suite reads the node.
	probePod = "probe"

	mi         = int64(1) << 20
	storage600 = `{"c":{"limit":"600Mi"}}`
)

var (
	// node is the node under test, and testImage the image of the suite's own Pods, which needs a shell, dd, df,
	// stat, tail and wget.
	node, testImage string

	// suiteNamespace holds probePod. The namespaces of the specs are named after it.
	suiteNamespace = cmp.Or(os.Getenv("E2E_SUITE_NAMESPACE"), "bofs-e2e")
	// suiteLabel is on every namespace the suite creates, and on no other suite's.
	suiteLabel = "blockoverlayfs.io/e2e=" + suiteNamespace

	// What the plugin on the node runs with: the namespace of its Pod, and of its configuration the port of its
	// metrics and the sizing policy.
	pluginNamespace, metricsPort string
	policy                       sizing.Policy
	// layer600 is the layer of a container with a limit of 600Mi, the declaration of most Pods of the suite.
	layer600 int64
)

// layerSize is the size the plugin's policy gives the layer of a container that declares request and limit, 0 for
// neither: 0 for a container the policy leaves on a directory.
func layerSize(request, limit int64) int64 {
	var size int64
	switch {
	case limit > 0:
		size = limit - policy.LimitOffset
	case request > 0:
		size = (request*policy.RequestPercent + 99) / 100
	}
	if size < sizing.MinSize {
		return policy.DefaultSize
	}
	return size
}

func TestE2E(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	gomega.SetDefaultEventuallyTimeout(3 * time.Minute)
	gomega.SetDefaultEventuallyPollingInterval(2 * time.Second)
	ginkgo.RunSpecs(t, "blockoverlayfs e2e")
}

var _ = ginkgo.BeforeSuite(func() {
	node, testImage = os.Getenv("E2E_NODE"), os.Getenv("E2E_TEST_IMAGE")
	gomega.Expect(node).NotTo(gomega.BeEmpty(), "E2E_NODE names the node under test")
	gomega.Expect(testImage).NotTo(gomega.BeEmpty(), "E2E_TEST_IMAGE is the image of the suite's Pods")

	// What a run that was cut short left behind.
	must(kubectl("delete", "namespaces", "-l", suiteLabel, "--timeout=300s"))

	plugins := strings.Fields(must(kubectl("get", "pods", "--all-namespaces", "-l", pluginSelector, "--field-selector", "spec.nodeName="+node,
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name} {end}")))
	gomega.Expect(plugins).To(gomega.HaveLen(1), "the cluster is to run one blockoverlayfs on the node")
	pluginNamespace, _, _ = strings.Cut(plugins[0], "/")
	gomega.Eventually(registered).Should(gomega.Succeed())

	// The chart mounts the plugin's configuration from a ConfigMap, as the volume config.
	text := must(kubectl("-n", pluginNamespace, "get", "configmap", must(pluginField(`.spec.volumes[?(@.name=="config")].configMap.name`)),
		"-o", `jsonpath={.data.config\.toml}`))
	fmt.Fprintf(ginkgo.GinkgoWriter, "the configuration of %s:\n%s\n", plugins[0], text)
	path := filepath.Join(ginkgo.GinkgoT().TempDir(), "config.toml")
	gomega.Expect(os.WriteFile(path, []byte(text), 0o600)).To(gomega.Succeed())
	cfg := must(config.Load(path))
	gomega.Expect(cfg.Enabled).To(gomega.BeTrue(), "the plugin on the node is disabled, and gives no container a block layer")
	_, port, err := net.SplitHostPort(cfg.Metrics.Address)
	gomega.Expect(err).NotTo(gomega.HaveOccurred(), "the plugin on the node serves no metrics, which the specs read")
	// The suite's Pods are in namespaces it creates, and some have no label.
	gomega.Expect(cfg.IncludedNamespaces).To(gomega.BeEmpty(), "the plugin's included_namespaces leaves the suite's namespaces out")
	gomega.Expect(must(metav1.LabelSelectorAsSelector(&cfg.PodSelector)).Matches(labels.Set{})).
		To(gomega.BeTrue(), "the plugin's pod_selector does not select the suite's Pods")
	metricsPort = port
	policy = must(cfg.Size.Policy())
	layer600 = layerSize(0, 600*mi)

	createNamespace(suiteNamespace)
	apply("probe.yaml", pod{Namespace: suiteNamespace})
	ready(suiteNamespace, probePod)

	// The report names what ran.
	ginkgo.AddReportEntry("plugin", plugins[0])
	ginkgo.AddReportEntry("plugin image", must(pluginField(".spec.containers[0].image")))
	ginkgo.AddReportEntry("plugin image id", must(pluginField(".status.containerStatuses[0].imageID")))
	ginkgo.AddReportEntry("node", must(kubectl("get", "node", node, "-o", "jsonpath={.metadata.name}: {.status.nodeInfo.containerRuntimeVersion}"+
		", kernel {.status.nodeInfo.kernelVersion}, kubelet {.status.nodeInfo.kubeletVersion}")))
})

// A failed spec's output ends with what the plugin and the node have to say.
var _ = ginkgo.JustAfterEach(func() {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	loops, _ := probeExec(loopsScript)
	pods, _ := kubectl("get", "pods", "--all-namespaces", "-o", "wide", "--field-selector", "spec.nodeName="+node)
	var events strings.Builder
	names, _ := kubectl("get", "namespaces", "-l", suiteLabel, "-o", "jsonpath={.items[*].metadata.name}")
	for _, name := range strings.Fields(names) {
		out, _ := kubectl("-n", name, "get", "events", "--field-selector", "type!=Normal", "--sort-by=.lastTimestamp")
		events.WriteString(out + "\n")
	}
	fmt.Fprintf(ginkgo.GinkgoWriter, "%s\nthe node's loop devices, with their sizes and files:\n%s\nthe node's Pods:\n%s\nevents:\n%s\n",
		pluginReport(), loops, pods, events.String())
})

var _ = ginkgo.AfterSuite(func() {
	_, _ = kubectl("delete", "namespaces", "-l", suiteLabel, "--timeout=300s")
})
