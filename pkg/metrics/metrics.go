// Package metrics holds the plugin's Prometheus metrics.
package metrics

import (
	"net/http"

	"github.com/containerd/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/YiD11/blockoverlayfs/pkg/block"
)

const namespace = "blockoverlayfs"

// The values of ContainerCreations' result label.
const (
	// ResultBlock is a container that got a block writable layer.
	ResultBlock = "block"
	// ResultDisabled is a container created while the plugin is disabled.
	ResultDisabled = "disabled"
	// ResultNamespace is a container whose Pod's namespace is excluded or not included.
	ResultNamespace = "namespace"
	// ResultPodSelector is a container whose Pod the selector does not select.
	ResultPodSelector = "pod_selector"
	// ResultNoSize is a container that declares no size where there is no default.
	ResultNoSize = "no_size"
	// ResultOtherSnapshotter is a container whose rootfs is not on the configured snapshotter.
	ResultOtherSnapshotter = "other_snapshotter"
	// ResultAttachFailed is a container whose layer could not be attached.
	ResultAttachFailed = "attach_failed"
	// ResultAbandoned is a container whose layer was attached after containerd had given the request up.
	ResultAbandoned = "abandoned"
	// ResultError is a request the plugin answered with an error.
	ResultError = "error"
)

// registry is the plugin's own: the library's default one also carries the Go runtime's and the process's metrics.
var registry = prometheus.NewRegistry()

var (
	BuildInfo = promauto.With(registry).NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Always 1, labeled with the plugin's version and revision.",
	}, []string{"version", "revision"})

	Enabled = promauto.With(registry).NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "enabled",
		Help:      "1 if the plugin gives new containers block writable layers, 0 if it is disabled.",
	})

	ContainerCreations = promauto.With(registry).NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "container_creations_total",
		Help:      "Containers created while the plugin was registered, by what became of their writable layer.",
	}, []string{"result"})

	// LayerAttachDuration is measured from the arrival of the request, which is what containerd's
	// plugin_request_timeout, 2 seconds by default, bounds.
	LayerAttachDuration = promauto.With(registry).NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "layer_attach_duration_seconds",
		Help:      "Time from a container creation request to its block writable layer being attached, or failing to be.",
		Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	})

	ReconcileUnsettledLayers = promauto.With(registry).NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "reconcile_unsettled_layers",
		Help:      "Layers the last reconciliation left for another run: they failed, or their container is yet to be recorded.",
	})

	ReconcileDeletedLayers = promauto.With(registry).NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "reconcile_deleted_layers_total",
		Help:      "Images deleted by reconciliation, being no container's writable layer.",
	})

	ReconcileDuration = promauto.With(registry).NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "reconcile_duration_seconds",
		Help:      "Time taken by a reconciliation.",
	})

	NRIRegistrations = promauto.With(registry).NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "nri_registrations_total",
		Help:      "Times the plugin registered with NRI: once when it starts, and again whenever containerd restarted or dropped it.",
	})
)

func init() {
	// A result that has not occurred yet reads 0 rather than being absent.
	for _, result := range []string{
		ResultBlock, ResultDisabled, ResultNamespace, ResultPodSelector, ResultNoSize, ResultOtherSnapshotter,
		ResultAttachFailed, ResultAbandoned, ResultError,
	} {
		ContainerCreations.WithLabelValues(result)
	}
}

var layersDesc = prometheus.NewDesc(namespace+"_layers", "Images in the store, each a container's block writable layer.", nil, nil)

// layers counts a store's images whenever it is scraped: the images are the plugin's only state.
type layers struct {
	store *block.Store
}

func (l layers) Describe(ch chan<- *prometheus.Desc) { ch <- layersDesc }

// Collect leaves the metric out of a scrape in which the store cannot be listed.
func (l layers) Collect(ch chan<- prometheus.Metric) {
	ids, err := l.store.IDs()
	if err != nil {
		log.L.WithError(err).Warn("failed to list block writable layers")
		return
	}
	ch <- prometheus.MustNewConstMetric(layersDesc, prometheus.GaugeValue, float64(len(ids)))
}

// RegisterLayers adds the number of images in store to the metrics. It is called once.
func RegisterLayers(store *block.Store) {
	registry.MustRegister(layers{store: store})
}

// Handler serves the metrics in Prometheus's exposition format.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
