package plugin

import (
	"context"
	"time"

	"github.com/containerd/log"
)

// Kick asks the reconciler to run soon. Kicks while it runs are merged into one more run.
func (p *Plugin) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// RunReconciler reconciles whenever Kick is called and at every interval, until ctx is done.
func (p *Plugin) RunReconciler(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.kick:
		case <-t.C:
		}
		p.Reconcile(ctx)
	}
}

// Reconcile cleans up after what the plugin missed: a container removed while the plugin was not connected, a
// container whose creation failed while it was not, an image whose setup or removal was interrupted. A layer stays
// while its container or the container's snapshot still exists; a container being created has its snapshot but no
// container record yet. Every other layer is unmounted, and every image that is not mounted is deleted.
func (p *Plugin) Reconcile(ctx context.Context) {
	ids, err := p.layers.IDs()
	if err != nil {
		log.G(ctx).WithError(err).Warn("failed to list block writable layers")
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		p.reconcile(ctx, id)
	}
}

func (p *Plugin) reconcile(ctx context.Context, id string) {
	unlock := p.locks.lock(id)
	defer unlock()
	logger := log.G(ctx).WithField("id", id)

	mounted, err := p.layers.Mounted(id)
	if err != nil {
		logger.WithError(err).Warn("failed to check whether a block writable layer is mounted")
		return
	}
	if mounted {
		if keep, err := p.inUse(ctx, id); err != nil {
			logger.WithError(err).Warn("failed to check whether a block writable layer is still in use")
			return
		} else if keep {
			return
		}
		if err := p.layers.Unmount(id); err != nil {
			logger.WithError(err).Warn("failed to unmount the block writable layer of a removed container")
			return
		}
	}
	if err := p.layers.Remove(id); err != nil {
		logger.WithError(err).Warn("failed to delete a block writable layer")
		return
	}
	if mounted {
		logger.Info("deleted the block writable layer of a removed container")
	} else {
		logger.Info("deleted a block writable layer that was not mounted")
	}
}

// inUse reports whether a container or its rootfs snapshot still exists.
func (p *Plugin) inUse(ctx context.Context, id string) (bool, error) {
	if ok, err := p.runtime.ContainerExists(ctx, id); err != nil || ok {
		return ok, err
	}
	return p.runtime.SnapshotExists(ctx, id)
}
