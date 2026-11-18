//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("a container's writable layer", ginkgo.Ordered, ginkgo.Label("basic"), func() {
	var (
		ns string
		// What the plugin had counted before the Pods of this group were created.
		blocks, attaches float64
	)

	ginkgo.BeforeAll(func() {
		blocks = creations("block")
		attaches = must(metric("blockoverlayfs_layer_attach_duration_seconds"))
		ns = namespace()
		apply("sizes.yaml", pod{Namespace: ns, Storage: fmt.Sprintf(`{"tiny":{"limit":"%d"},"zero":{"limit":"0"}}`, policy.LimitOffset+mi)})
		create(pod{Namespace: ns, Name: "noanno"})
		// An unknown field makes the whole annotation unreadable.
		create(pod{Namespace: ns, Name: "badjson", Storage: `{"c":{"limits":"1Gi"}}`})
		ready(ns, "matrix", "edge", "withinit", "noanno", "badjson")
	})

	// request and limit are what the plugin sizes the container by: neither for one it sizes by its default, which
	// leaves the container on a directory when the plugin has none.
	ginkgo.DescribeTable("is sized from what the annotation declares for the container",
		func(name, container string, request, limit int64) {
			if size := layerSize(request, limit); size > 0 {
				expectBlock(ns, name, container, size)
			} else {
				expectDir(ns, name, container)
			}
		},
		ginkgo.Entry("a request", "matrix", "request-only", 4096*mi, int64(0)),
		ginkgo.Entry("a limit", "matrix", "limit-only", int64(0), 1024*mi),
		ginkgo.Entry("a limit, which wins over a request", "matrix", "both", 512*mi, 2048*mi),
		ginkgo.Entry("an empty entry", "matrix", "none", int64(0), int64(0)),
		ginkgo.Entry("no entry", "matrix", "unlisted", int64(0), int64(0)),
		ginkgo.Entry("no annotation", "noanno", "c", int64(0), int64(0)),
		ginkgo.Entry("an unreadable annotation", "badjson", "c", int64(0), int64(0)),
		ginkgo.Entry("a limit that leaves less than the smallest layer", "edge", "tiny", int64(0), int64(0)),
		ginkgo.Entry("a limit of 0", "edge", "zero", int64(0), int64(0)),
		ginkgo.Entry("a container that follows an init container", "withinit", "main", 500*mi, int64(0)),
	)

	ginkgo.It("is sized for an init container too", func() {
		id := strings.TrimPrefix(must(field(ns, "withinit", `{.status.initContainerStatuses[0].containerID}`)), "containerd://")
		// A container that has exited keeps its layer until kubelet removes it.
		expectBlockSize(id, layerSize(0, 300*mi), kib(must(kubectl("-n", ns, "logs", "withinit", "-c", "init")), 1))
	})

	ginkgo.It("is counted by the plugin's metrics", func() {
		// The containers of the five Pods above that declare a size and, if the plugin has a default size, the others.
		containers := 5
		if policy.DefaultSize > 0 {
			containers = 11
		}
		// At least: the plugin counts the node's other containers too.
		gomega.Expect(creations("block") - blocks).To(gomega.BeNumerically(">=", containers))
		gomega.Expect(must(metric("blockoverlayfs_layer_attach_duration_seconds")) - attaches).To(gomega.BeNumerically(">=", containers))
		gomega.Expect(must(metric("blockoverlayfs_layers"))).To(gomega.BeNumerically(">=", containers))
	})

	ginkgo.It("fails writes with ENOSPC once it is full, and holds what was written in its image", func() {
		size := layerSize(0, 1024*mi)
		out := must(execIn(ns, "matrix", "limit-only", "dd if=/dev/zero of=/big bs=1M count=2000 2>&1; stat -c %s /big"))
		gomega.Expect(out).To(gomega.ContainSubstring("No space left on device"))
		written := must(strconv.ParseInt(out[strings.LastIndex(out, "\n")+1:], 10, 64))
		// ext4 keeps part of the image for itself.
		gomega.Expect(written).To(gomega.And(gomega.BeNumerically(">", size*3/4), gomega.BeNumerically("<=", size)))

		gomega.Expect(execIn(ns, "matrix", "both", "dd if=/dev/zero of=/ok bs=1M count=50 2>/dev/null && echo written; rm -f /ok")).
			To(gomega.Equal("written"), "a container of the same Pod writes on")
		gomega.Expect(execIn(ns, "matrix", "limit-only", "rm /big && echo x > /again && cat /again")).
			To(gomega.Equal("x"), "the container writes again once the file is deleted")
	})

	ginkgo.It("holds what the container writes, apart from every other container", func() {
		must(execIn(ns, "matrix", "request-only", "echo from-request-only > /marker"))
		gomega.Expect(execIn(ns, "matrix", "request-only", "cat /marker")).To(gomega.Equal("from-request-only"))
		absent := "test -e /marker && echo present || echo absent"
		gomega.Expect(execIn(ns, "matrix", "both", absent)).To(gomega.Equal("absent"), "in a container of the same Pod")
		gomega.Expect(execIn(ns, "noanno", "c", absent)).To(gomega.Equal("absent"), "in another Pod")
	})

	ginkgo.It("is a new one for the container kubelet creates after one exited", func() {
		apply("crash.yaml", pod{Namespace: ns})
		var first, second string
		gomega.Eventually(func(g gomega.Gomega) {
			var err error
			first, err = field(ns, "crash", `{.status.containerStatuses[0].lastState.terminated.containerID}`)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(first).NotTo(gomega.BeEmpty(), "the first container exited")
			second, err = field(ns, "crash", `{.status.containerStatuses[0].containerID}`)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(second).NotTo(gomega.Or(gomega.BeEmpty(), gomega.Equal(first)), "kubelet created another")
			g.Expect(field(ns, "crash", `{.status.containerStatuses[0].state.running.startedAt}`)).NotTo(gomega.BeEmpty())
		}).Should(gomega.Succeed())

		// Each wrote a file to its root filesystem after saying whether one was there.
		gomega.Expect(kubectl("-n", ns, "logs", "crash", "--previous")).To(gomega.Equal("FRESH"))
		gomega.Expect(kubectl("-n", ns, "logs", "crash")).To(gomega.Equal("FRESH"))
		// A container that has exited keeps its layer until kubelet removes it.
		for _, id := range []string{first, second} {
			gomega.Expect(blockLayer(strings.TrimPrefix(id, "containerd://"))).To(gomega.Equal(layerSize(0, 400*mi) &^ 511))
		}
		// A Pod that goes on crashing would keep creating containers under the specs that follow.
		must(kubectl("-n", ns, "delete", "pod", "crash", "--wait=false"))
	})

	ginkgo.It("is counted by kubelet against the Pod's ephemeral-storage limit", func() {
		apply("evict.yaml", pod{Namespace: ns})
		ready(ns, "evict")
		expectBlock(ns, "evict", "c", layerSize(0, 512*mi))
		// 580MiB against a limit of 512Mi, of which the 400MiB outside the layer alone stay under it.
		must(execIn(ns, "evict", "c", "dd if=/dev/zero of=/in-layer bs=1M count=180 2>/dev/null && dd if=/dev/zero of=/scratch/f bs=1M count=400 2>/dev/null"))
		gomega.Eventually(func() (string, error) {
			return field(ns, "evict", `{.status.phase}/{.status.reason}`)
		}).WithTimeout(5 * time.Minute).Should(gomega.Equal("Failed/Evicted"))
	})
})
