// Package plugin is the NRI plugin. When CRI creates a container, after the container's rootfs snapshot is prepared
// and before anything mounts it, the plugin gives the snapshot's upper and work directories a size-limited block
// device. When CRI removes the container, the plugin takes the device away again, before containerd deletes the
// snapshot.
package plugin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/nri/pkg/api"

	"github.com/YiD11/blockoverlayfs/pkg/annotation"
	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

// Runtime is what the plugin reads from containerd.
type Runtime interface {
	// SnapshotDir returns the directory of a container's rootfs snapshot. The error satisfies errdefs.IsNotFound
	// when the configured snapshotter has no snapshot for the container.
	SnapshotDir(ctx context.Context, id string) (string, error)
	SnapshotExists(ctx context.Context, id string) (bool, error)
	ContainerExists(ctx context.Context, id string) (bool, error)
}

// Layers keeps the block writable layers, one per container.
type Layers interface {
	// Attach mounts a new layer of size bytes over a container's snapshot directory.
	Attach(ctx context.Context, id, dir string, size int64) error
	// Unmount takes a container's layer off wherever it is mounted.
	Unmount(id string) error
	// Remove deletes a container's layer, refusing while it is mounted.
	Remove(id string) error
	// Mounted reports whether a container's layer is mounted.
	Mounted(id string) (bool, error)
	// IDs lists the containers that have a layer, mounted or not.
	IDs() ([]string, error)
}

// Plugin handles NRI's Synchronize, CreateContainer and RemoveContainer. NRI serializes these; the plugin also
// reconciles in the background, and a lock per container keeps the two apart.
type Plugin struct {
	runtime Runtime
	layers  Layers
	policy  sizing.Policy

	locks keyedLocker
	kick  chan struct{}
	// removals counts the layers being deleted in the background.
	removals sync.WaitGroup
}

func New(runtime Runtime, layers Layers, policy sizing.Policy) *Plugin {
	return &Plugin{
		runtime: runtime,
		layers:  layers,
		policy:  policy,
		kick:    make(chan struct{}, 1),
	}
}

// Synchronize is called whenever the plugin registers with the runtime, which it does again after containerd
// restarts or drops the connection. Containers may have been removed in the meantime without the plugin hearing of
// it, so it reconciles, in the background: NRI waits for Synchronize before it lets the plugin see new containers.
func (p *Plugin) Synchronize(ctx context.Context, _ []*api.PodSandbox, containers []*api.Container) ([]*api.ContainerUpdate, error) {
	log.G(ctx).WithField("containers", len(containers)).Info("synchronized with the runtime")
	p.Kick()
	return nil, nil
}

// CreateContainer gives a new container its block writable layer. A container that declares no size, and gets no
// default size, keeps the snapshotter's directory. So does one whose rootfs is not on the configured snapshotter.
// Any other failure fails the container's creation: it would otherwise run without the limit it asked for.
func (p *Plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	start := time.Now()
	id := ctr.GetId()
	fields := containerFields(pod, ctr)

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
		return nil, nil, fmt.Errorf("attach a block writable layer of %d bytes to container %s: %w", size.Size, id, err)
	}
	fields["size"] = size.Size
	fields["duration"] = time.Since(start).String()
	log.G(ctx).WithFields(fields).Info("created block writable layer")
	return nil, nil, nil
}

// RemoveContainer takes a container's block writable layer away. containerd calls it before it deletes the
// container's snapshot, and also when it gives up creating a container. The layer must be unmounted before the
// snapshotter deletes the directory it covers, or the snapshotter would delete the layer's contents and fail on the
// mount point; deleting the image can wait, so it happens in the background.
func (p *Plugin) RemoveContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) error {
	id := ctr.GetId()
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
		if err := p.layers.Remove(id); err != nil {
			log.G(ctx).WithFields(fields).WithError(err).Warn("failed to delete a block writable layer; reconciling will retry")
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
