//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

const pluginSelector = "app.kubernetes.io/name=blockoverlayfs"

// nodeExec runs a shell command on the node: in the node's mount namespace, from the suite's Pod on the node, which
// shares the node's process and network namespaces.
func nodeExec(command string) (string, error) {
	return kubectl("-n", suiteNamespace, "exec", nodePod, "--", "nsenter", "-t", "1", "-m", "--", "sh", "-c", command)
}

// pluginField reads a field of the plugin's Pod on the node by its JSONPath, relative to the Pod.
func pluginField(jsonpath string) (string, error) {
	return kubectl("-n", suiteNamespace, "get", "pods", "-l", pluginSelector, "--field-selector", "spec.nodeName="+node,
		"-o", "jsonpath={.items[*]"+jsonpath+"}")
}

// registered reports through g whether the plugin is registered with containerd, which is what its Pod's readiness
// probe looks at.
func registered(g gomega.Gomega) {
	ready, err := pluginField(".status.containerStatuses[0].ready")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(ready).To(gomega.Equal("true"), pluginReport)
}

// pluginReport describes the plugin's Pod and returns its log, for the output of a failure: the plugin is gone by
// the time the suite's report is read.
func pluginReport() string {
	described, _ := kubectl("-n", suiteNamespace, "describe", "pods", "-l", pluginSelector)
	log, _ := kubectl("-n", suiteNamespace, "logs", "-l", pluginSelector, "--tail=-1", "--prefix")
	return "the plugin's Pod:\n" + described + "\nits log:\n" + log
}

// pluginRestarts counts how often kubelet has restarted the plugin's container.
func pluginRestarts() int {
	ginkgo.GinkgoHelper()
	return must(strconv.Atoi(must(pluginField(".status.containerStatuses[0].restartCount"))))
}

// pluginLog returns the log of the plugin's running container.
func pluginLog() (string, error) {
	name, err := pluginField(".metadata.name")
	if err != nil {
		return "", err
	}
	return kubectl("-n", suiteNamespace, "logs", name)
}

// pluginPID returns the process ID of the plugin on the node.
func pluginPID() string {
	ginkgo.GinkgoHelper()
	return must(nodeExec("pidof blockoverlayfs"))
}

// metric reads a sample of the plugin's metrics: that of a gauge or counter whose labels have the given names and
// values, or the count of a histogram. A sample that is not there reads as 0, like a counter that has yet to count.
func metric(name string, labels ...string) (float64, error) {
	// The suite's Pod on the node is on the node's network, where the plugin serves its metrics.
	text, err := kubectl("-n", suiteNamespace, "exec", nodePod, "--", "wget", "-q", "-T", "5", "-O", "-", "http://127.0.0.1:"+metricsPort+"/metrics")
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

// layerScript prints what the image of a container is to the container: its arguments are the store's root, the
// container's ID and the snapshotter's snapshots directory.
const layerScript = `img=%[1]s/%[2]s.img
[ -e "$img" ] || { echo dir; exit 0; }
dev=$(losetup -n -O NAME -j "$img" | head -n 1)
[ -n "$dev" ] || { echo image-not-attached; exit 0; }
mp=$(findmnt -rn -o TARGET -S "$dev" | head -n 1)
case "$mp" in
%[3]s/*) if [ -d "$mp/fs" ]; then echo block; else echo mounted-without-fs; fi ;;
*) echo "mounted-at:${mp:-nowhere}" ;;
esac`

// layerIn says what the writable layer of a container is, for a store at root: "dir" when the container has no
// image, "block" when its image is mounted over a snapshot directory and holds the upper directory, and otherwise
// what is wrong with the image.
func layerIn(root, id string) (string, error) {
	if id == "" {
		return "no-container", nil
	}
	return nodeExec(fmt.Sprintf(layerScript, root, id, snapshotterRoot+"/snapshots"))
}

func layer(id string) (string, error) { return layerIn(storeRoot, id) }

// kib reads a column of the last line of df -k output.
func kib(df string, column int) int64 {
	ginkgo.GinkgoHelper()
	lines := strings.Split(df, "\n")
	fields := strings.Fields(lines[len(lines)-1])
	gomega.Expect(len(fields)).To(gomega.BeNumerically(">", column), "df printed %q", df)
	return must(strconv.ParseInt(fields[column], 10, 64))
}

// storeScript prints the images of a store, at its argument, and the loop devices and the mounts that are theirs.
// The node has loop devices and ext4 mounts of others.
const storeScript = `ls -la %[1]s
losetup -n -O NAME,BACK-FILE | awk -v root=%[1]s/ 'index($2, root) == 1' | while read -r dev image; do
echo "$dev $image $(findmnt -rn -o TARGET -S "$dev")"
done`

// expectBlockSize checks that a container's layer is an image of size bytes, and that rootKiB, the size of the
// container's root filesystem, is that of an ext4 filesystem in such an image.
func expectBlockSize(id string, size, rootKiB int64) {
	ginkgo.GinkgoHelper()
	gomega.Expect(layer(id)).To(gomega.Equal("block"))
	gomega.Expect(nodeExec(fmt.Sprintf("stat -c %%s %s/%s.img", storeRoot, id))).To(gomega.Equal(strconv.FormatInt(size, 10)))
	// ext4 keeps part of the image for itself.
	gomega.Expect(rootKiB).To(gomega.And(gomega.BeNumerically(">", size/2048), gomega.BeNumerically("<", size/1024)))
}

// expectBlock checks that a running container has a block layer of size bytes.
func expectBlock(namespace, name, container string, size int64) {
	ginkgo.GinkgoHelper()
	expectBlockSize(containerID(namespace, name, container), size, kib(must(execIn(namespace, name, container, "df -k /")), 1))
}

// expectDir checks that a running container keeps the snapshotter's directory: it has no image, and its root
// filesystem is the node's disk.
func expectDir(namespace, name, container string) {
	ginkgo.GinkgoHelper()
	gomega.Expect(layer(containerID(namespace, name, container))).To(gomega.Equal("dir"))
	gomega.Expect(kib(must(execIn(namespace, name, container, "df -k /")), 1)).To(gomega.Equal(kib(must(nodeExec("df -k /var/lib/containerd")), 1)))
}

// settled reports through g whether the plugin has taken every layer away: the store holds no image, no loop device
// has one of its images, which it has for as long as the image is mounted, and the plugin's own count agrees.
func settled(g gomega.Gomega) {
	counts, err := nodeExec(fmt.Sprintf(`ls %[1]s | grep -c '\.img$'
losetup -n -O BACK-FILE | grep -c '^%[1]s/'
true`, storeRoot))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(strings.Fields(counts)).To(gomega.Equal([]string{"0", "0"}), "images in the store, loop devices that have one")
	for _, name := range []string{"blockoverlayfs_layers", "blockoverlayfs_reconcile_unsettled_layers"} {
		value, err := metric(name)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		g.Expect(value).To(gomega.BeZero(), name)
	}
}
