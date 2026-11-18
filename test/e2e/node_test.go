//go:build e2e

package e2e

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

const pluginSelector = "app.kubernetes.io/name=blockoverlayfs"

// probeExec runs a shell command in the suite's Pod on the node. The Pod has no privilege there: it sees the node's
// loop devices in sysfs, and reaches the plugin's metrics at the node's address, its HOST_IP.
func probeExec(command string) (string, error) {
	return execIn(suiteNamespace, probePod, "probe", command)
}

// pluginField reads a field of the plugin's Pod on the node by its JSONPath, relative to the Pod.
func pluginField(jsonpath string) (string, error) {
	return kubectl("-n", pluginNamespace, "get", "pods", "-l", pluginSelector, "--field-selector", "spec.nodeName="+node,
		"-o", "jsonpath={.items[*]"+jsonpath+"}")
}

// registered reports through g whether the plugin is registered with containerd, which is what its Pod's readiness
// probe looks at.
func registered(g gomega.Gomega) {
	ready, err := pluginField(".status.containerStatuses[0].ready")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.Equal("true"), pluginReport)
}

// pluginReport describes the plugin's Pod on the node and returns the end of its log, for the output of a failure.
func pluginReport() string {
	name, _ := pluginField(".metadata.name")
	described, _ := kubectl("-n", pluginNamespace, "describe", "pod", name)
	log, _ := kubectl("-n", pluginNamespace, "logs", name, "--tail=500")
	return "the plugin's Pod:\n" + described + "\nthe end of its log:\n" + log
}

// pluginLog returns the log of the plugin's running container.
func pluginLog() (string, error) {
	name, err := pluginField(".metadata.name")
	if err != nil {
		return "", err
	}
	return kubectl("-n", pluginNamespace, "logs", name)
}

// metric reads a sample of the plugin's metrics: that of a gauge or counter whose labels have the given names and
// values, or the count of a histogram. A sample that is not there reads as 0, like a counter that has yet to count.
func metric(name string, labels ...string) (float64, error) {
	text, err := probeExec("wget -q -T 5 -O - http://$HOST_IP:" + metricsPort + "/metrics")
	if err != nil {
		return 0, err
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	// The text format ends every line with a newline, the last one included, which kubectl's output has lost.
	families, err := parser.TextToMetricFamilies(strings.NewReader(text + "\n"))
	if err != nil {
		return 0, err
	}
samples:
	for _, m := range families[name].GetMetric() {
		got := map[string]string{}
		for _, l := range m.GetLabel() {
			got[l.GetName()] = l.GetValue()
		}
		for i := 0; i+1 < len(labels); i += 2 {
			if got[labels[i]] != labels[i+1] {
				continue samples
			}
		}
		switch families[name].GetType() {
		case dto.MetricType_COUNTER:
			return m.GetCounter().GetValue(), nil
		case dto.MetricType_HISTOGRAM:
			return float64(m.GetHistogram().GetSampleCount()), nil
		default:
			return m.GetGauge().GetValue(), nil
		}
	}
	return 0, nil
}

// creations reads how many containers the plugin has counted as created with a result.
func creations(result string) float64 {
	ginkgo.GinkgoHelper()
	return must(metric("blockoverlayfs_container_creations_total", "result", result))
}

// loopsScript prints the node's loop devices that have a backing file: the size of each in bytes, and its file. The
// kernel lists them in sysfs to every container. The plugin names an image after its container's ID, and attaches a
// loop device to it for as long as it is the container's layer.
const loopsScript = `for d in /sys/block/loop*; do
f=$(cat $d/loop/backing_file 2>/dev/null) || continue
echo "$(($(cat $d/size) * 512)) $f"
done`

// blockLayer returns the size in bytes of a container's block layer, 0 when the container has none and keeps the
// snapshotter's directory.
func blockLayer(id string) (int64, error) {
	if id == "" {
		return 0, errors.New("the Pod has no such container yet")
	}
	loops, err := probeExec(loopsScript)
	if err != nil {
		return 0, err
	}
	for _, loop := range strings.Split(loops, "\n") {
		if size, file, _ := strings.Cut(loop, " "); strings.Contains(file, id+".img") {
			return strconv.ParseInt(size, 10, 64)
		}
	}
	return 0, nil
}

// kib reads a column of the last line of df -k output.
func kib(df string, column int) int64 {
	ginkgo.GinkgoHelper()
	lines := strings.Split(df, "\n")
	fields := strings.Fields(lines[len(lines)-1])
	gomega.Expect(len(fields)).To(gomega.BeNumerically(">", column), "df printed %q", df)
	return must(strconv.ParseInt(fields[column], 10, 64))
}

// expectBlockSize checks that a container's layer is an image of size bytes, and that rootKiB, the size of the
// container's root filesystem, is that of an ext4 filesystem in such an image.
func expectBlockSize(id string, size, rootKiB int64) {
	ginkgo.GinkgoHelper()
	// A loop device has whole sectors of 512 bytes.
	gomega.Expect(blockLayer(id)).To(gomega.Equal(size &^ 511))
	// ext4 keeps part of the image for itself.
	gomega.Expect(rootKiB).To(gomega.And(gomega.BeNumerically(">", size/2048), gomega.BeNumerically("<", size/1024)))
}

// expectBlock checks that a running container has a block layer of size bytes.
func expectBlock(namespace, name, container string, size int64) {
	ginkgo.GinkgoHelper()
	expectBlockSize(containerID(namespace, name, container), size, kib(must(execIn(namespace, name, container, "df -k /")), 1))
}

// expectDir checks that a running container keeps the snapshotter's directory.
func expectDir(namespace, name, container string) {
	ginkgo.GinkgoHelper()
	gomega.Expect(blockLayer(containerID(namespace, name, container))).To(gomega.BeZero())
}

// layersOfSuite finds, in the plugin's log, the IDs of the containers of the suite's Pods that the plugin gave a
// layer. The log names a container's Pod with its namespace, and the namespaces of the specs are named after the
// suite's.
var layersOfSuite = regexp.MustCompile(`msg="created block writable layer".* id=([0-9a-f]+) pod=` + regexp.QuoteMeta(suiteNamespace) + `-`)

// settled reports through g whether the plugin has taken away the layer of every container of the suite's Pods:
// none has its loop device any more, and the plugin has left no layer unsettled.
func settled(g gomega.Gomega) {
	log, err := pluginLog()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	loops, err := probeExec(loopsScript)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	for _, layer := range layersOfSuite.FindAllStringSubmatch(log, -1) {
		g.Expect(loops).NotTo(gomega.ContainSubstring(layer[1]+".img"), "the layer of a container of the suite is still attached")
	}
	unsettled, err := metric("blockoverlayfs_reconcile_unsettled_layers")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(unsettled).To(gomega.BeZero())
}
