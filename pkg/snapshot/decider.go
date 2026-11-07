package snapshot

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/YiD11/blockoverlayfs/pkg/annotation"
	"github.com/YiD11/blockoverlayfs/pkg/containermeta"
	"github.com/YiD11/blockoverlayfs/pkg/sizing"
)

// ContainerLookup reads a container record from containerd.
type ContainerLookup interface {
	Lookup(ctx context.Context, namespace, id string) (containermeta.Info, error)
}

// Decider decides whether an active snapshot gets a block writable layer, and how large.
type Decider interface {
	Decide(ctx context.Context, key string) (Decision, error)
}

// Decision is a Decider's answer. Final is false when the snapshot needs no decision, not being a container's
// rootfs, or cannot be decided yet; nothing is recorded then and the next Mounts asks again. A final decision
// with Size zero keeps the directory upper.
type Decision struct {
	Final  bool
	Size   int64
	Reason string
	// Pod, as namespace/name, and Container name the container in the log line; neither is recorded.
	Pod       string
	Container string
}

// CRIDecider sizes the rootfs of Kubernetes containers. containerd's metadata layer names a backend snapshot
// <namespace>/<sequence>/<key>, CRI uses the container ID as the key and labels the container record with its kind
// and kubelet's pod labels, and the ephemeral-storage declaration arrives in the annotation.Key annotation of the
// container's OCI spec.
type CRIDecider struct {
	Lookup ContainerLookup
	Policy sizing.Policy
}

func (d *CRIDecider) Decide(ctx context.Context, key string) (Decision, error) {
	if !d.Policy.Enabled {
		return Decision{}, nil
	}
	ref, ok := ParseKey(key)
	if !ok || ref.IsUnpack() {
		return Decision{}, nil
	}
	c, err := d.Lookup.Lookup(ctx, ref.Namespace, ref.Name)
	if errdefs.IsNotFound(err) {
		// CRI mounts a new rootfs to copy image volumes out of it before the container record exists. Task create
		// calls Mounts again once it does.
		return Decision{}, nil
	}
	if err != nil {
		return Decision{}, fmt.Errorf("look up container %s in namespace %s: %w", ref.Name, ref.Namespace, err)
	}
	// Sandbox (pause) containers and containers created outside CRI keep a directory upper, and so does a
	// container record that merely shares the key's name.
	if c.Kind != containermeta.KindContainer || c.SnapshotKey != ref.Name {
		return Decision{}, nil
	}

	res, note := declaration(ctx, c)
	r, err := d.Policy.Size(res)
	if err != nil {
		return Decision{}, fmt.Errorf("size the writable layer of container %s: %w", ref.Name, err)
	}
	reason := r.Reason
	if note != "" {
		reason = note + ": " + reason
	}
	return Decision{
		Final:     true,
		Size:      r.Size,
		Reason:    reason,
		Pod:       c.PodNamespace + "/" + c.PodName,
		Container: c.ContainerName,
	}, nil
}

// declaration reads what the container declares from the annotation. A container it cannot read counts as
// declaring nothing, so it still starts; note says why, for the logged reason.
func declaration(ctx context.Context, c containermeta.Info) (sizing.Resources, string) {
	fields := log.Fields{"pod": c.PodNamespace + "/" + c.PodName, "container": c.ContainerName}
	raw, ok := c.Annotations[annotation.Key]
	if !ok {
		// The platform did not write it, as for static pods, or the runtime handler lacks pod_annotations.
		log.G(ctx).WithFields(fields).Warnf("no %s annotation in the container's OCI spec, sizing it as undeclared", annotation.Key)
		return sizing.Resources{}, "no " + annotation.Key + " annotation"
	}
	v, err := annotation.Decode(raw)
	if err != nil {
		log.G(ctx).WithError(err).WithFields(fields).Warn("sizing the container as undeclared")
		return sizing.Resources{}, "unreadable " + annotation.Key + " annotation"
	}
	res, ok := v.Resources(c.ContainerName)
	if !ok {
		// Ephemeral containers are added after the Pod is created and cannot declare resources anyway.
		log.G(ctx).WithFields(fields).Infof("container not listed in the %s annotation, sizing it as undeclared", annotation.Key)
		return sizing.Resources{}, "container not listed in the " + annotation.Key + " annotation"
	}
	return res, ""
}
