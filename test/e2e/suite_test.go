//go:build e2e

// Package e2e tests the plugin as its chart deploys it, on one node of a cluster: kubelet, containerd, NRI, the
// plugin and the shims together. It drives the cluster with kubectl and helm, and reads and acts on the node through
// a privileged Pod.
//
// The suite deploys the plugin under test on that node alone, in a namespace of its own, and takes it away when it
// is done. The node must run no other blockoverlayfs. The specs kill the plugin and restart containerd on the node,
// and restart the node if E2E_NODE_RESTART tells them how. Suites that run on nodes of one cluster at the same time
// each need their own namespace, which E2E_SUITE_NAMESPACE names.
package e2e

import (
	"cmp"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	releaseName = "blockoverlayfs"
	chart       = "../../charts/blockoverlayfs"
	baseValues  = "testdata/values.yaml"
	// nodePod is the Pod through which the suite reads and acts on the node.
	nodePod = "node"

	// What the base values set, and the chart by default.
	storeRoot       = "/var/lib/containerd/blockoverlayfs-e2e"
	snapshotterRoot = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs"
	metricsPort     = "9466"

	// Sizes under the base values, which take 256Mi off a limit and give 20Gi to a container that declares
	// nothing.
	mi          = int64(1) << 20
	defaultSize = 20 << 30
	// layer600 is the layer of a container with a limit of 600Mi, the declaration of most Pods of the suite.
	layer600   = 600*mi - 256*mi
	storage600 = `{"c":{"limit":"600Mi"}}`
)

var (
	// node is the node under test, image the plugin image deployed on it, and testImage the image of the suite's
	// own Pods, which needs a shell, dd, df, wget and nsenter.
	node, image, testImage string

	// suiteNamespace holds the release and nodePod. The namespaces of the specs are named after it.
	suiteNamespace = cmp.Or(os.Getenv("E2E_SUITE_NAMESPACE"), "bofs-e2e")
	// suiteLabel is on every namespace the suite creates, and on no other suite's.
	suiteLabel = "blockoverlayfs.io/e2e=" + suiteNamespace
)

func TestE2E(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	gomega.SetDefaultEventuallyTimeout(3 * time.Minute)
	gomega.SetDefaultEventuallyPollingInterval(2 * time.Second)
	ginkgo.RunSpecs(t, "blockoverlayfs e2e")
}

var _ = ginkgo.BeforeSuite(func() {
	node, image, testImage = os.Getenv("E2E_NODE"), os.Getenv("E2E_IMAGE"), os.Getenv("E2E_TEST_IMAGE")
	gomega.Expect(node).NotTo(gomega.BeEmpty(), "E2E_NODE names the node under test")
	gomega.Expect(image).NotTo(gomega.BeEmpty(), "E2E_IMAGE is the plugin image to deploy")
	gomega.Expect(testImage).NotTo(gomega.BeEmpty(), "E2E_TEST_IMAGE is the image of the suite's Pods")

	// What a run that was cut short left behind.
	must(kubectl("delete", "namespaces", "-l", suiteLabel, "--timeout=300s"))
	// Two plugins on a node would both act on its containers.
	gomega.Expect(kubectl("get", "pods", "--all-namespaces", "-l", pluginSelector, "--field-selector", "spec.nodeName="+node,
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name} {end}")).
		To(gomega.BeEmpty(), "the node already runs blockoverlayfs, which has to leave it before the suite deploys its own")

	createNamespace(suiteNamespace)
	apply("node.yaml", pod{Namespace: suiteNamespace})
	ready(suiteNamespace, nodePod)
	gomega.Expect(nodeExec("pidof blockoverlayfs || true")).To(gomega.BeEmpty(), "a blockoverlayfs process already runs on the node")
	upgrade()

	// The report names what ran.
	ginkgo.AddReportEntry("plugin image", must(pluginField(".status.containerStatuses[0].imageID")))
	ginkgo.AddReportEntry("plugin version", must(kubectl("-n", suiteNamespace, "exec", "daemonset/"+releaseName, "--",
		"blockoverlayfs", "-version")))
	ginkgo.AddReportEntry("node", must(kubectl("get", "node", node, "-o", "jsonpath={.metadata.name}: {.status.nodeInfo.containerRuntimeVersion}"+
		", kernel {.status.nodeInfo.kernelVersion}, kubelet {.status.nodeInfo.kubeletVersion}")))
})

// A failed spec's output ends with what the plugin and the node have to say.
var _ = ginkgo.JustAfterEach(func() {
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	state, _ := nodeExec(fmt.Sprintf(storeScript, storeRoot))
	pods, _ := kubectl("get", "pods", "--all-namespaces", "-o", "wide", "--field-selector", "spec.nodeName="+node)
	var events strings.Builder
	names, _ := kubectl("get", "namespaces", "-l", suiteLabel, "-o", "jsonpath={.items[*].metadata.name}")
	for _, name := range strings.Fields(names) {
		out, _ := kubectl("-n", name, "get", "events", "--field-selector", "type!=Normal", "--sort-by=.lastTimestamp")
		events.WriteString(out + "\n")
	}
	fmt.Fprintf(ginkgo.GinkgoWriter, "%s\nthe store, with the loop devices and mounts of its images:\n%s\nthe node's Pods:\n%s\nevents:\n%s\n",
		pluginReport(), state, pods, events.String())
})

var _ = ginkgo.AfterSuite(func() {
	// The plugin leaves with its release. The directories it kept images in are empty by then.
	_, _ = run("", "helm", "uninstall", releaseName, "--namespace", suiteNamespace, "--wait")
	_, _ = nodeExec("rmdir " + storeRoot + " " + movedRoot)
	_, _ = kubectl("delete", "namespaces", "-l", suiteLabel, "--timeout=300s")
})
