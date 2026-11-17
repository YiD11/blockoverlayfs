//go:build e2e

package e2e

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// removed reports through g whether containerd has removed a container.
func removed(g gomega.Gomega, id string) {
	out, err := nodeExec("crictl ps -a -q --no-trunc")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(out).NotTo(gomega.ContainSubstring(id))
}

// registrations reads how often the plugin has registered with containerd.
func registrations() (float64, error) { return metric("blockoverlayfs_nri_registrations_total") }

var _ = ginkgo.Describe("a block layer", ginkgo.Ordered, ginkgo.Label("restart"), func() {
	var (
		ns string
		// keep is a container created before every spec of the group, with a file in its layer.
		keep string
	)

	ginkgo.BeforeAll(func() {
		ns = namespace()
		create(pod{Namespace: ns, Name: "keep", Storage: storage600})
		ready(ns, "keep")
		expectBlock(ns, "keep", "c", layer600)
		must(execIn(ns, "keep", "c", "echo kept > /marker"))
		keep = containerID(ns, "keep", "c")
	})

	// intact checks that the container created before a spec still has its layer and its file, and that a container
	// created after the spec gets a layer. The checks are tried again while kubelet finds containerd back.
	intact := func(after string) {
		ginkgo.GinkgoHelper()
		gomega.Eventually(layer).WithArguments(keep).Should(gomega.Equal("block"))
		gomega.Eventually(execIn).WithArguments(ns, "keep", "c", "cat /marker").Should(gomega.Equal("kept"))
		create(pod{Namespace: ns, Name: after, Storage: storage600})
		ready(ns, after)
		expectBlock(ns, after, "c", layer600)
	}

	ginkgo.It("outlasts the plugin's Pod being deleted", func() {
		must(kubectl("-n", suiteNamespace, "delete", "pod", must(pluginField(".metadata.name"))))
		gomega.Eventually(registered).Should(gomega.Succeed())
		intact("after-delete")
	})

	ginkgo.It("outlasts the plugin being killed", func() {
		restarts := pluginRestarts()
		must(nodeExec("kill -KILL " + pluginPID()))
		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(pluginField(".status.containerStatuses[0].restartCount")).To(gomega.Equal(strconv.Itoa(restarts + 1)))
			registered(g)
		}).Should(gomega.Succeed())
		intact("after-kill")
	})

	ginkgo.It("outlasts containerd restarting and being killed", func() {
		// A container that has exited by the time containerd restarts, and is removed afterwards.
		create(pod{Namespace: ns, Name: "exited", Storage: storage600, Command: []string{"true"}, RestartPolicy: "Never"})
		gomega.Eventually(field).WithArguments(ns, "exited", "{.status.phase}").Should(gomega.Equal("Succeeded"))
		exited := containerID(ns, "exited", "c")
		gomega.Expect(layer(exited)).To(gomega.Equal("block"), "a container that has exited keeps its layer")

		pid, restarts := pluginPID(), pluginRestarts()
		// restarted checks that the plugin registers again in the process it has once command has run, and
		// what intact checks.
		restarted := func(command, after string) {
			before := must(registrations())
			// As a unit of its own: the restart ends the command that asks for it.
			must(nodeExec("systemd-run --no-block " + command))
			gomega.Eventually(registrations).Should(gomega.BeEquivalentTo(before+1), "the plugin registers again")
			gomega.Eventually(nodeExec).WithArguments("pidof blockoverlayfs").Should(gomega.Equal(pid), "in the process it had")
			gomega.Expect(pluginRestarts()).To(gomega.Equal(restarts))
			intact(after)
		}

		restarted("systemctl restart containerd", "after-restart")
		// containerd 2.3 tells no NRI plugin when it removes a container that had exited before containerd
		// restarted. The snapshotter empties the layer it cannot delete, and the plugin clears it away when it
		// next reconciles, which it does whenever it registers.
		must(kubectl("-n", ns, "delete", "pod", "exited"))
		gomega.Eventually(removed).WithArguments(exited).WithTimeout(5 * time.Minute).Should(gomega.Succeed())

		// Its main process alone: its shims, and with them the containers, are in its control group too.
		restarted("systemctl kill --kill-whom=main -s KILL containerd", "after-containerd-kill")
		gomega.Eventually(layer).WithArguments(exited).Should(gomega.Equal("dir"))
	})

	ginkgo.It("outlasts the plugin's Pod being off the node, and the plugin clears up after it", func() {
		create(pod{Namespace: ns, Name: "victim", Storage: storage600})
		ready(ns, "victim")
		victim := containerID(ns, "victim", "c")

		ginkgo.DeferCleanup(func() { upgrade() })
		withoutPlugin()
		gomega.Expect(layer(keep)).To(gomega.Equal("block"))
		gomega.Expect(execIn(ns, "keep", "c", "cat /marker && dd if=/dev/zero of=/more bs=1M count=20 2>/dev/null && echo written")).
			To(gomega.Equal("kept\nwritten"), "its container reads what it wrote, and writes")
		create(pod{Namespace: ns, Name: "meanwhile", Storage: storage600})
		ready(ns, "meanwhile")
		expectDir(ns, "meanwhile", "c")
		// containerd removes the container without the plugin, which leaves its image behind.
		must(kubectl("-n", ns, "delete", "pod", "victim"))
		gomega.Eventually(removed).WithArguments(victim).WithTimeout(5 * time.Minute).Should(gomega.Succeed())

		upgrade()
		gomega.Eventually(layer).WithArguments(victim).Should(gomega.Equal("dir"))
		expectDir(ns, "meanwhile", "c")
		intact("after-absence")
	})

	ginkgo.It("outlasts the plugin not answering, and the plugin clears up after it", func() {
		create(pod{Namespace: ns, Name: "leaving", Storage: storage600})
		ready(ns, "leaving")
		leaving := containerID(ns, "leaving", "c")
		pid := pluginPID()
		ginkgo.DeferCleanup(func() { _, _ = nodeExec("kill -CONT " + pid) })

		// containerd gives a request up after its plugin_request_timeout, drops the plugin, and goes on without it.
		before := must(registrations())
		must(nodeExec("kill -STOP " + pid))
		create(pod{Namespace: ns, Name: "unanswered", Storage: storage600})
		ready(ns, "unanswered")
		expectDir(ns, "unanswered", "c")
		must(nodeExec("kill -CONT " + pid))
		gomega.Eventually(registrations).Should(gomega.BeEquivalentTo(before + 1))

		must(nodeExec("kill -STOP " + pid))
		must(kubectl("-n", ns, "delete", "pod", "leaving"))
		gomega.Eventually(removed).WithArguments(leaving).WithTimeout(5 * time.Minute).Should(gomega.Succeed())
		must(nodeExec("kill -CONT " + pid))
		gomega.Eventually(registrations).Should(gomega.BeEquivalentTo(before + 2))
		gomega.Eventually(layer).WithArguments(leaving).Should(gomega.Equal("dir"))
		intact("after-silence")
	})

	ginkgo.It("is taken away again when another NRI plugin refuses the container", func() {
		path := os.Getenv("E2E_REJECTOR")
		if path == "" {
			ginkgo.Skip("E2E_REJECTOR does not name the helper NRI plugin, which make e2e-build builds")
		}
		// The helper runs in the suite's Pod on the node, which mounts containerd's NRI socket.
		inNodePod := []string{"-n", suiteNamespace, "exec", "-i", nodePod, "--", "sh", "-c"}
		must(run(string(must(os.ReadFile(path))), "kubectl", append(inNodePod, "cat > /rejector && chmod +x /rejector")...))
		pid := must(kubectl(append(inNodePod, "/rejector > /rejector.log 2>&1 & echo $!")...))
		stop := func() { _, _ = kubectl(append(inNodePod, "kill "+pid)...) }
		ginkgo.DeferCleanup(stop)
		gomega.Eventually(func() (string, error) { return kubectl(append(inNodePod, "cat /rejector.log")...) }).
			Should(gomega.ContainSubstring("Started plugin"))

		// containerd asks blockoverlayfs first, which attaches a layer. When the helper then refuses the container,
		// containerd gives the creation up and tells blockoverlayfs to remove the container it never recorded.
		create(pod{Namespace: ns, Name: "refused", Storage: storage600, Labels: map[string]string{"blockoverlayfs.io/e2e-reject": "true"}})
		of := regexp.MustCompile(`pod=` + regexp.QuoteMeta(ns+"/refused") + `( |$)`)
		id := regexp.MustCompile(`id=([0-9a-f]+)`)
		var created []string
		gomega.Eventually(func(g gomega.Gomega) {
			log, err := pluginLog()
			g.Expect(err).NotTo(gomega.HaveOccurred())
			created = created[:0]
			taken := 0
			for _, line := range strings.Split(log, "\n") {
				switch {
				case !of.MatchString(line):
				case strings.Contains(line, `msg="created block writable layer"`):
					created = append(created, id.FindStringSubmatch(line)[1])
				case strings.Contains(line, `msg="removed block writable layer"`):
					taken++
				}
			}
			g.Expect(created).NotTo(gomega.BeEmpty(), "layers attached for creations that were then refused")
			g.Expect(taken).To(gomega.BeNumerically(">=", 1), "layers taken away again")
		}).Should(gomega.Succeed())

		stop()
		ready(ns, "refused")
		expectBlock(ns, "refused", "c", layer600)
		running := containerID(ns, "refused", "c")
		for _, refused := range created {
			if refused != running {
				gomega.Expect(layer(refused)).To(gomega.Equal("dir"), "the image of a refused creation")
			}
		}
	})

	// The last spec of the group: the container the others look at does not outlast the node.
	ginkgo.It("does not outlast the node restarting: the plugin deletes the images that are left", func() {
		command := os.Getenv("E2E_NODE_RESTART")
		if command == "" {
			ginkgo.Skip("E2E_NODE_RESTART is not set to a command that restarts the node")
		}
		images := strings.Fields(must(nodeExec("ls " + storeRoot)))
		gomega.Expect(images).NotTo(gomega.BeEmpty())
		restarts := pluginRestarts()
		must(run("", "sh", "-c", command))

		gomega.Eventually(func(g gomega.Gomega) {
			g.Expect(pluginField(".status.containerStatuses[0].restartCount")).NotTo(gomega.Equal(strconv.Itoa(restarts)), "kubelet started the plugin again")
			registered(g)
			// The restart unmounted every layer, which leaves images that are no container's any more.
			left, err := nodeExec("ls " + storeRoot)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(strings.Fields(left)).NotTo(gomega.ContainElement(gomega.BeElementOf(images)))
		}).WithTimeout(10 * time.Minute).Should(gomega.Succeed())

		gomega.Eventually(execIn).WithArguments(ns, "keep", "c", "test -e /marker && echo present || echo absent").
			Should(gomega.Equal("absent"), "kubelet created the container anew, on an empty root filesystem")
		create(pod{Namespace: ns, Name: "after-node-restart", Storage: storage600})
		ready(ns, "after-node-restart")
		expectBlock(ns, "after-node-restart", "c", layer600)
	})
})
