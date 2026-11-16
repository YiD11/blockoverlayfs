package plugin

import (
	"context"
	"time"

	"github.com/containerd/log"

	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/metrics"
)

// outcome is what reconciling did with one layer.
type outcome int

const (
	// kept is a layer left to its container.
	kept outcome = iota
	// unsettled is a layer left for another run: it failed, or its container is yet to be recorded.
	unsettled
	// deleted is an image that was deleted.
	deleted
	// gone is an image that went with its container after the layers were listed.
	gone
)

// Kick asks the reconciler to run soon. Kicks while it runs are merged into one more run.
func (p *Plugin) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// RunReconciler reconciles whenever Kick is called and at every interval, until ctx is done. A run that could not
// settle every layer is followed by another after retry, and then after twice as long each time, up to interval.
func (p *Plugin) RunReconciler(ctx context.Context, interval, retry time.Duration) {
	next := retry
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
			next = retry
		case <-t.C:
		}
		wait := interval
		if p.Reconcile(ctx) {
			next = retry
		} else {
			wait = min(next, interval)
			next = min(2*next, interval)
		}
		t.Reset(wait)
	}
}

// Reconcile deletes the images that are no container's writable layer: those mounted nowhere, those mounted without
// an upper directory, and attached ones whose container containerd does not have and is not creating. It reports
// whether every layer is settled: none failed, and none is attached for a container that is yet to be recorded. A
// run that got through every layer logs how many it left and deleted.
func (p *Plugin) Reconcile(ctx context.Context) (settled bool) {
	start := time.Now()
	ids, err := p.layers.IDs()
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to list block writable layers")
		return false
	}
	var count [gone + 1]int
	// Once containerd fails to answer, the run stops asking it: only attached layers need its answer.
	answers := true
	for _, id := range ids {
		if ctx.Err() != nil {
			return false
		}
		count[p.reconcile(ctx, id, &answers)]++
	}
	duration := time.Since(start)
	metrics.ReconcileUnsettledLayers.Set(float64(count[unsettled]))
	metrics.ReconcileDeletedLayers.Add(float64(count[deleted]))
	metrics.ReconcileDuration.Observe(duration.Seconds())
	log.G(ctx).WithFields(log.Fields{
		"layers":    count[kept] + count[unsettled],
		"unsettled": count[unsettled],
		"deleted":   count[deleted],
		"duration":  duration.String(),
	}).Info("reconciled block writable layers")
	return count[unsettled] == 0
}

func (p *Plugin) reconcile(ctx context.Context, id string, answers *bool) outcome {
	unlock := p.locks.lock(id)
	defer unlock()
	logger := log.G(ctx).WithField("id", id)

	status, err := p.layers.Status(id)
	if err != nil {
		logger.WithError(err).Warn("failed to check a block writable layer")
		return unsettled
	}
	var reason string
	switch status {
	case block.Detached:
		// Safe even if the container still exists: kubelet never starts a container again, so one that lost its
		// layer's mount, as all do when the node restarts, is only waiting to be removed.
		reason = "it is mounted nowhere"
	case block.Incomplete:
		// Taking the image away uncovers the snapshotter's own directories for the container to run on.
		reason = "it holds no upper directory"
	default:
		if !*answers {
			return unsettled
		}
		container, err := p.runtime.ContainerExists(ctx, id)
		if err != nil {
			*answers = false
			logger.WithError(err).Warn("failed to check whether a container still exists; leaving the attached block writable layers as they are")
			return unsettled
		}
		if container {
			p.created(id)
			return kept
		}
		if p.awaited(id) {
			return unsettled
		}
		reason = "it has no container"
	}
	if err := p.layers.Unmount(id); err != nil {
		logger.WithError(err).Warn("failed to unmount a block writable layer")
		return unsettled
	}
	removed, err := p.layers.Remove(id)
	if err != nil {
		logger.WithError(err).Warn("failed to delete a block writable layer")
		return unsettled
	}
	if !removed {
		return gone
	}
	logger.WithField("reason", reason).Info("deleted a block writable layer")
	return deleted
}
