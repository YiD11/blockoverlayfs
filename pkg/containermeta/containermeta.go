package containermeta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/anypb"
)

// io.cri-containerd.kind is set by containerd 1.7 CRI on every container record it creates; the io.kubernetes.*
// labels are the container labels kubelet passes in CreateContainer.
const (
	LabelKind          = "io.cri-containerd.kind"
	LabelPodUID        = "io.kubernetes.pod.uid"
	LabelPodNamespace  = "io.kubernetes.pod.namespace"
	LabelPodName       = "io.kubernetes.pod.name"
	LabelContainerName = "io.kubernetes.container.name"

	KindSandbox   = "sandbox"
	KindContainer = "container"
)

type Info struct {
	Namespace     string
	ID            string
	Kind          string
	PodUID        string
	PodNamespace  string
	PodName       string
	ContainerName string
	// SnapshotKey is the client key of the snapshot the container uses as its rootfs.
	SnapshotKey string
	// Annotations are those of the container's OCI spec. CRI puts the Pod annotations that the runtime handler's
	// pod_annotations setting lists there.
	Annotations map[string]string
}

func FromLabels(namespace, id string, labels map[string]string) Info {
	return Info{
		Namespace:     namespace,
		ID:            id,
		Kind:          labels[LabelKind],
		PodUID:        labels[LabelPodUID],
		PodNamespace:  labels[LabelPodNamespace],
		PodName:       labels[LabelPodName],
		ContainerName: labels[LabelContainerName],
	}
}

type Client struct {
	conn       *grpc.ClientConn
	containers containersapi.ContainersClient
	timeout    time.Duration
}

// Dial must not block: the snapshotter starts before containerd, and lookups only run once containerd calls in.
// A positive timeout bounds every lookup, which runs while containerd waits for Mounts or Commit.
func Dial(address string, timeout time.Duration) (*Client, error) {
	conn, err := grpc.Dial("unix://"+address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, containers: containersapi.NewContainersClient(conn), timeout: timeout}, nil
}

func (c *Client) Lookup(ctx context.Context, namespace, id string) (Info, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	resp, err := c.containers.Get(namespaces.WithNamespace(ctx, namespace), &containersapi.GetContainerRequest{ID: id})
	if err != nil {
		return Info{}, errdefs.FromGRPC(err)
	}
	info := FromLabels(namespace, id, resp.GetContainer().GetLabels())
	info.SnapshotKey = resp.GetContainer().GetSnapshotKey()
	// An unreadable spec only loses the annotations; failing here would keep every container from starting.
	if info.Annotations, err = SpecAnnotations(resp.GetContainer().GetSpec()); err != nil {
		log.G(ctx).WithError(err).WithField("container", id).Warn("cannot read the container's OCI spec annotations")
	}
	return info, nil
}

// SpecAnnotations reads the annotations of an OCI runtime spec stored in a container record. containerd stores
// the spec, which is not a protobuf message, as JSON inside the Any.
func SpecAnnotations(spec *anypb.Any) (map[string]string, error) {
	if len(spec.GetValue()) == 0 {
		return nil, nil
	}
	var s struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(spec.GetValue(), &s); err != nil {
		return nil, fmt.Errorf("decode spec %s: %w", spec.GetTypeUrl(), err)
	}
	return s.Annotations, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}
