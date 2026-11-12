// Package plugin is the NRI plugin. When CRI creates a container, after the container's rootfs snapshot is prepared
// and before anything mounts it, the plugin gives the snapshot's upper and work directories a size-limited block
// device. When CRI removes the container, the plugin takes the device away again, before containerd deletes the
// snapshot.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/nri/pkg/api"

	"github.com/YiD11/blockoverlayfs/pkg/annotation"
	"github.com/YiD11/blockoverlayfs/pkg/block"
	"github.com/YiD11/blockoverlayfs/pkg/containerd"
	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

// errDraining answers the requests that arrive once the plugin has begun to stop.
var errDraining = errors.New("the plugin is stopping")

// Plugin handles NRI's Synchronize, CreateContainer and RemoveContainer. NRI serializes these; the plugin also
// reconciles in the background, and a lock per container keeps the two apart.
type Plugin struct {
	runtime *containerd.Client
	layers  *block.Store
	policy  sizing.Policy
	// included and excluded are Kubernetes namespaces: see CreateContainer.
	included []string
	excluded []string

	locks keyedLocker
	kick  chan struct{}
	// serving is held shared while a request changes a layer, and exclusively, for good, once the plugin drains.
	serving sync.RWMutex
	drain   sync.Once
	// removals counts the layers being deleted in the background.
	removals sync.WaitGroup

	mu sync.Mutex
	// synchronized is set once containerd has synchronized the plugin.
	synchronized bool
	// creating holds the containers that got a layer since containerd last synchronized the plugin, until
	// containerd is seen to have them: it records a container only after the plugin has answered.
	creating map[string]struct{}
}

func New(runtime *containerd.Client, layers *block.Store, policy sizing.Policy, included, excluded []string) *Plugin {
	return &Plugin{
		runtime:  runtime,
		layers:   layers,
		policy:   policy,
		included: included,
		excluded: excluded,
		kick:     make(chan struct{}, 1),
		creating: map[string]struct{}{},
	}
}

// Synchronize is called whenever the plugin registers with containerd: when it starts, and again after containerd
// restarted or dropped the connection. containerd synchronizes a plugin only while it is creating no container, so
// a layer that has no container by now will never get one. Reconciling, in the background, clears those away along
// with whatever else the plugin missed.
func (p *Plugin) Synchronize(ctx context.Context, _ []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	log.G(ctx).WithField("containers", len(containers)).Info("synchronized with the runtime")
	p.mu.Lock()
	p.synchronized = true
	clear(p.creating)
	p.mu.Unlock()
	p.Kick()
	return nil, nil
}

// Drain waits for the request that is changing a layer to finish, and makes the plugin refuse the ones that follow.
// It is called before the connection to NRI is closed.
func (p *Plugin) Drain() {
	p.drain.Do(p.serving.Lock)
}

// CreateContainer gives a new container its block writable layer. The container keeps the snapshotter's directory
// if it gets no size, if its rootfs is not on the configured snapshotter, or if the layer cannot be attached: it then
// starts without a limit rather than not at all. Once the plugin drains, a container that would get a layer fails
// to be created instead, so that kubelet creates it again when the plugin is back.
func (p *Plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	start := time.Now()
	id := ctr.GetId()
	fields := containerFields(pod, ctr)

	// Exclusion wins over inclusion.
	namespace := pod.GetNamespace()
	if slices.Contains(p.excluded, namespace) {
		fields["reason"] = "namespace is excluded"
		log.G(ctx).WithFields(fields).Info("container keeps a directory writable layer")
		return nil, nil, nil
	}
	if len(p.included) > 0 && !slices.Contains(p.included, namespace) {
		fields["reason"] = "namespace is not included"
		log.G(ctx).WithFields(fields).Info("container keeps a directory writable layer")
		return nil, nil, nil
	}

	res, note := declaration(ctx, pod, ctr)
	size, err := p.policy.Size(res)
	if err != nil {
		return nil, nil, fmt.Errorf("size the writable layer of container %s: %w", id, err)
	}
	fields["reason"] = size.Reason
	if note != "" {
		fields["reason"] = note + ": " + size.Reason
	}
	if size.Size == 0 {
		log.G(ctx).WithFields(fields).Info("container keeps a directory writable layer")
		return nil, nil, nil
	}

	if !p.serving.TryRLock() {
		return nil, nil, fmt.Errorf("create the block writable layer of container %s: %w", id, errDraining)
	}
	defer p.serving.RUnlock()
	unlock := p.locks.lock(id)
	defer unlock()
	dir, err := p.runtime.SnapshotDir(ctx, id)
	if errdefs.IsNotFound(err) {
		log.G(ctx).WithFields(fields).WithError(err).Warn("container keeps a directory writable layer: its rootfs is not on the configured snapshotter")
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("find the rootfs of container %s: %w", id, err)
	}
	if err := p.layers.Attach(ctx, id, dir, size.Size); err != nil {
		log.G(ctx).WithFields(fields).WithError(err).Error("container keeps a directory writable layer: failed to attach its block writable layer")
		// Attach takes back what it did, unless that failed too.
		p.Kick()
		return nil, nil, nil
	}
	// A request that is over was given up by containerd, which then creates the container without waiting, or
	// not at all.
	if ctx.Err() == nil {
		p.mu.Lock()
		p.creating[id] = struct{}{}
		p.mu.Unlock()
	}
	fields["size"] = size.Size
	fields["duration"] = time.Since(start).String()
	log.G(ctx).WithFields(fields).Info("created block writable layer")
	return nil, nil, nil
}

// created forgets that a container is being created: containerd has it, or has given it up.
func (p *Plugin) created(id string) {
	p.mu.Lock()
	delete(p.creating, id)
	p.mu.Unlock()
}

// awaited reports whether a layer without a container may still get its container: containerd is creating it, or
// has not synchronized the plugin yet and so may be creating a container whose layer an earlier run attached.
func (p *Plugin) awaited(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, creating := p.creating[id]
	return creating || !p.synchronized
}

// RemoveContainer takes a container's block writable layer away. containerd calls it before it deletes the
// container's snapshot, and when it gives up creating a container, and goes on whatever the answer. The image is
// unmounted here, before the snapshotter deletes the directory it covers, and deleted in the background.
func (p *Plugin) RemoveContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	id := ctr.GetId()
	if !p.serving.TryRLock() {
		return fmt.Errorf("unmount the block writable layer of container %s: %w", id, errDraining)
	}
	defer p.serving.RUnlock()
	p.created(id)
	unlock := p.locks.lock(id)
	err := p.layers.Unmount(id)
	unlock()
	if err != nil {
		return fmt.Errorf("unmount the block writable layer of container %s: %w", id, err)
	}
	p.removeLater(context.WithoutCancel(ctx), id, containerFields(pod, ctr))
	return nil
}

// Wait waits for the layers being deleted in the background.
func (p *Plugin) Wait() { p.removals.Wait() }

func (p *Plugin) removeLater(ctx context.Context, id string, fields log.Fields) {
	p.removals.Add(1)
	go func() {
		defer p.removals.Done()
		unlock := p.locks.lock(id)
		defer unlock()
		// The image of a container without a layer does not exist, and Remove then does nothing.
		removed, err := p.layers.Remove(id)
		if err != nil {
			log.G(ctx).WithFields(fields).WithError(err).Warn("failed to delete a block writable layer; reconciling will retry")
			p.Kick()
		} else if removed {
			log.G(ctx).WithFields(fields).Info("removed block writable layer")
		}
	}()
}

// declaration reads a container's ephemeral-storage from the Pod annotation. A container whose declaration cannot
// be read counts as declaring nothing, so that it still starts; note says why, for the log.
func declaration(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (sizing.Resources, string) {
	raw, ok := pod.GetAnnotations()[annotation.Key]
	if !ok {
		return sizing.Resources{}, "no " + annotation.Key + " annotation"
	}
	v, err := annotation.Decode(raw)
	if err != nil {
		log.G(ctx).WithFields(containerFields(pod, ctr)).WithError(err).Warn("sizing the container as declaring nothing")
		return sizing.Resources{}, "unreadable " + annotation.Key + " annotation"
	}
	res, ok := v.Resources(ctr.GetName())
	if !ok {
		// Ephemeral containers are added after the Pod is created and cannot declare resources anyway.
		return sizing.Resources{}, "container not listed in the " + annotation.Key + " annotation"
	}
	return res, ""
}

func containerFields(pod *api.PodSandbox, ctr *api.Container) log.Fields {
	return log.Fields{
		"pod":       pod.GetNamespace() + "/" + pod.GetName(),
		"container": ctr.GetName(),
		"id":        ctr.GetId(),
	}
}

// keyedLocker hands out one mutex per key.
type keyedLocker struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocker) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
