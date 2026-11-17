//go:build e2e

package e2e

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

//go:embed testdata/*.yaml
var testdata embed.FS

var manifests = template.Must(template.New("").Funcs(template.FuncMap{
	// A JSON value is a YAML value.
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
}).ParseFS(testdata, "testdata/*.yaml"))

// run runs a command with stdin as its standard input and returns its standard output, less the trailing newlines.
// The error of a command that failed holds its standard error.
func run(stdin, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimRight(stdout.String(), "\n")
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func kubectl(args ...string) (string, error) { return run("", "kubectl", args...) }

// must fails the spec on err, with the error's own text: a matcher would add a dump of the error's value.
func must[T any](v T, err error) T {
	ginkgo.GinkgoHelper()
	if err != nil {
		ginkgo.Fail(err.Error())
	}
	return v
}

// pod describes a Pod of one container, c, on the node under test. Its zero value sleeps, declares nothing, and
// carries the label by which the base values select Pods.
type pod struct {
	Namespace, Name string
	// Storage is the value of the plugin's annotation.
	Storage             string
	Labels, Annotations map[string]string
	// Unselected leaves out the label by which the base values select Pods.
	Unselected    bool
	Command       []string
	RestartPolicy string
}

// apply creates the objects of a manifest of testdata. The manifest sees p's fields, the node under test as .Node
// and the image of the suite's Pods as .Image.
func apply(manifest string, p pod) {
	ginkgo.GinkgoHelper()
	var b bytes.Buffer
	gomega.Expect(manifests.ExecuteTemplate(&b, manifest, struct {
		pod
		Node, Image string
	}{p, node, testImage})).To(gomega.Succeed())
	must(run(b.String(), "kubectl", "apply", "-f", "-"))
}

// create creates a Pod of one container.
func create(p pod) {
	ginkgo.GinkgoHelper()
	apply("pod.yaml", p)
}

// ready waits for Pods to be ready.
func ready(namespace string, pods ...string) {
	ginkgo.GinkgoHelper()
	for _, name := range pods {
		must(kubectl("-n", namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=180s"))
	}
}

// field reads a field of a Pod by its JSONPath.
func field(namespace, name, jsonpath string) (string, error) {
	return kubectl("-n", namespace, "get", "pod", name, "-o", "jsonpath="+jsonpath)
}

// containerID returns the ID of a Pod's container as containerd knows it, empty while the Pod has none.
func containerID(namespace, name, container string) string {
	ginkgo.GinkgoHelper()
	id := must(field(namespace, name, fmt.Sprintf(`{.status.containerStatuses[?(@.name==%q)].containerID}`, container)))
	return strings.TrimPrefix(id, "containerd://")
}

// execIn runs a shell command in a Pod's container.
func execIn(namespace, name, container, command string) (string, error) {
	return kubectl("-n", namespace, "exec", name, "-c", container, "--", "sh", "-c", command)
}

// createNamespace creates a namespace of the suite's.
func createNamespace(name string) {
	ginkgo.GinkgoHelper()
	must(kubectl("create", "namespace", name))
	must(kubectl("label", "namespace", name, suiteLabel))
}

// namespaces counts the namespaces the suite has created for specs, and liveNamespaces those that are still there.
var namespaces, liveNamespaces int

// namespace creates a namespace for the specs of the container or spec that calls it, and deletes it once those are
// over. When the last namespace is gone it waits for the plugin to have taken every layer away.
func namespace() string {
	ginkgo.GinkgoHelper()
	namespaces++
	liveNamespaces++
	name := fmt.Sprintf("%s-%d", suiteNamespace, namespaces)
	createNamespace(name)
	ginkgo.DeferCleanup(func() {
		must(kubectl("delete", "namespace", name, "--timeout=300s"))
		if liveNamespaces--; liveNamespaces > 0 {
			return
		}
		// kubelet removes the containers of a deleted Pod about a minute later, and the layers go with them. A
		// layer whose removal the plugin was not told of goes when the plugin reconciles, every 10 minutes.
		gomega.Eventually(settled).WithTimeout(12 * time.Minute).Should(gomega.Succeed())
	})
	return name
}

// helmUpgrade deploys the chart on the node under test alone, with the base values and the plugin image under test,
// and what args add.
func helmUpgrade(args ...string) {
	ginkgo.GinkgoHelper()
	i := strings.LastIndex(image, ":")
	gomega.Expect(i).To(gomega.BeNumerically(">", strings.LastIndex(image, "/")), "E2E_IMAGE names a tag")
	name, tag := image[:i], image[i+1:]
	must(run("", "helm", append([]string{
		"upgrade", "--install", releaseName, chart, "--namespace", suiteNamespace, "--reset-values",
		"--values", baseValues, "--set-string", "image.name=" + name, "--set-string", "image.tag=" + tag,
		"--set-json", fmt.Sprintf("affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms="+
			`[{"matchFields":[{"key":"metadata.name","operator":"In","values":[%q]}]}]`, node),
	}, args...)...))
}

// upgrade deploys the chart and waits for the plugin to have registered with containerd.
func upgrade(args ...string) {
	ginkgo.GinkgoHelper()
	helmUpgrade(args...)
	if _, err := kubectl("-n", suiteNamespace, "rollout", "status", "daemonset/"+releaseName, "--timeout=180s"); err != nil {
		// BeforeSuite deploys the chart too, and nothing reports on the plugin after it.
		fmt.Fprintln(ginkgo.GinkgoWriter, pluginReport())
		ginkgo.Fail(err.Error())
	}
	gomega.Eventually(registered).Should(gomega.Succeed())
}

// withoutPlugin takes the plugin's Pod off the node, and waits for the plugin to have stopped.
func withoutPlugin() {
	ginkgo.GinkgoHelper()
	// The node has no such label.
	helmUpgrade("--set-string", `nodeSelector.blockoverlayfs\.io/e2e-absent=true`)
	gomega.Eventually(nodeExec).WithArguments("pidof blockoverlayfs || echo gone").Should(gomega.Equal("gone"))
}
