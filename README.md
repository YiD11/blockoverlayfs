# blockoverlayfs

A containerd proxy snapshotter that wraps containerd's built-in overlayfs snapshotter. It puts each Kubernetes app container's writable layer (the overlay `upperdir` and `workdir`) in its own sparse ext4 image, loop-mounted and sized from the container's `ephemeral-storage` limit or request. A container that fills its layer gets `ENOSPC` instead of growing the node's disk until kubelet evicts the pod. Neither kubelet nor containerd is patched.

## How it works

kubelet 1.29 does not pass ephemeral-storage over CRI, so the platform that creates a Pod also writes each container's request and limit into the Pod annotation `scitix.ai/ephemeral-storage`. kubelet hands Pod annotations to CRI unchanged, and containerd's `pod_annotations` setting copies this one into every container's OCI spec. The snapshotter reads it from the container record when the container's task is created. Nothing runs in the cluster for this, and the snapshotter never talks to kube-apiserver.

```
platform ──▶ Pod: resources ephemeral-storage, and the annotation scitix.ai/ephemeral-storage
                  = {"app":{"request":"10Gi","limit":"20Gi"},"init":{"limit":"1Gi"}}
kubelet ──CRI (Pod annotations)──▶ containerd 1.7 ── pod_annotations ──▶ container's OCI spec
                                        │
                                        └─gRPC (proxy_plugins)──▶ containerd-blockoverlayfs-grpc
                                            ├─ inner: containerd's overlay snapshotter, used as a library
                                            ├─ Mounts(): container record (labels, OCI spec annotation)
                                            │            → size → sparse ext4 image, loop-mounted
                                            │            → upperdir and workdir moved into the image
                                            ├─ Usage():  blocks and inodes the image's filesystem has in use
                                            └─ Remove(): inner remove → unmount → delete in the background
```

Block layer size for a container:

| The container declares | Block layer size |
| --- | --- |
| a positive ephemeral-storage limit | `limit - limit_offset` |
| only a positive ephemeral-storage request | `ceil(request * request_percent / 100)` |
| neither, or a result below 16Mi | `default_size`; an empty `default_size` keeps a directory upper |

A container whose OCI spec has no readable annotation counts as declaring neither, and the snapshotter logs a warning. That covers static pods and any other Pod the platform did not create, Pods whose annotation is malformed, and runtime handlers missing `pod_annotations`. Sandbox (pause) containers, image unpacking, views and committed layers never get a block layer. The size is decided once per container, when its task is first created, and recorded in `<root>/blocks/<id>/state.json`.

## The annotation

The platform writes it when it creates the Pod:

```yaml
metadata:
  annotations:
    scitix.ai/ephemeral-storage: '{"app":{"request":"10Gi","limit":"20Gi"},"init":{"limit":"1Gi"}}'
```

- The value is a JSON object keyed by container name. Init containers are listed the same way as other containers. Each entry may hold `request` and `limit` as Kubernetes quantities; an empty entry, or a quantity of 0, declares nothing. A container missing from the object counts as declaring nothing.
- Any other field, such as `limits`, or malformed JSON makes the whole annotation unreadable, and every container of the Pod is sized as undeclared.
- Put it on the Pod's own metadata. For a workload it goes in the pod template, e.g. `spec.template.metadata.annotations` of a Deployment or `spec.tasks[].template.metadata.annotations` of a Volcano Job. kubelet passes only the Pod's annotations.
- Use the values the platform writes into `resources.requests` and `resources.limits` for `ephemeral-storage`. Nothing checks the annotation against the Pod spec, the scheduler, ResourceQuota or LimitRange:
  - A limit above the spec's limit makes the block layer bigger than kubelet's eviction threshold, so the Pod is evicted before the container gets `ENOSPC`.
  - Values above what the spec requests let block layers outgrow the space the scheduler reserved on the node.
- Only the platform should write it: whoever can create or patch a Pod can set this value. kubelet rebuilds the Pod's sandbox configuration each time it creates a container, so a change made after creation applies to containers created afterwards, such as a restarted container.

## Requirements

- Linux with overlayfs, ext4 and loop devices, and `mkfs.ext4` from e2fsprogs. containerd 1.7.x, Kubernetes 1.29.
- The daemon runs in the host's mount namespace: containerd's shims mount container rootfs there, on top of the images this daemon mounts.
- The platform writes the annotation above on the Pods it creates.
- Go 1.25 or newer to build.

## Build

```bash
make binaries                 # bin/containerd-blockoverlayfs-grpc for linux/$(go env GOARCH)
make binaries GOARCH=amd64    # cross-compile for x86_64
```

## Install on a node

Drain the node first: switching snapshotters only affects containers created afterwards.

1. Install the binary:
   ```bash
   sudo make install
   ```
   Or copy `bin/containerd-blockoverlayfs-grpc` to `/usr/local/bin/`.
2. Install the configuration and adjust `writable_layer`:
   ```bash
   sudo install -D -m 644 deploy/config.toml /etc/containerd-blockoverlayfs-grpc/config.toml
   ```
3. Start the service. The unit uses `Type=notify` and `Before=containerd.service`, and must not be given mount sandboxing such as `PrivateMounts=`.
   ```bash
   sudo install -D -m 644 deploy/systemd/containerd-blockoverlayfs-grpc.service /etc/systemd/system/containerd-blockoverlayfs-grpc.service
   sudo systemctl daemon-reload && sudo systemctl enable --now containerd-blockoverlayfs-grpc
   ```
4. Merge [deploy/containerd/config.toml](deploy/containerd/config.toml) into `/etc/containerd/config.toml` and restart containerd.
   - Keep `exports = { root = ... }` equal to the snapshotter's `root`: CRI reports that path to kubelet as the image filesystem, and kubelet's eviction manager stops working if it cannot read it.
   - Add `pod_annotations = ["scitix.ai/ephemeral-storage"]` to every runtime handler Pods use, the default one and any others such as `nvidia`.

After the switch containerd unpacks images again for `blockoverlayfs`. Content blobs are shared, so nothing is downloaded twice, but the unpacked layers take space a second time until the old overlayfs snapshots are garbage collected.

## Configuration

Flags `--address`, `--config`, `--log-level` and `--root` override the file. A missing file at the default path means built-in defaults are used. A missing file passed with `--config` is an error. Sizes are Kubernetes quantities such as `256Mi` or `20Gi`; `256MiB` is rejected.

| Key | Default | Meaning |
| --- | --- | --- |
| `root` | `/var/lib/containerd/io.containerd.snapshotter.v1.blockoverlayfs` | State directory: the inner overlay in `<root>/overlay`, block layers in `<root>/blocks`. Keep it on the filesystem that holds containerd's data. |
| `address` | `/run/containerd-blockoverlayfs-grpc/containerd-blockoverlayfs-grpc.sock` | gRPC socket that containerd's `proxy_plugins` entry points to. |
| `log_level` | `info` | `trace`, `debug`, `info`, `warn`, `error`, `fatal` or `panic`. |
| `containerd.address` | `/run/containerd/containerd.sock` | Used to read the record of the container that owns a snapshot: its CRI labels and its OCI spec annotations. |
| `containerd.lookup_timeout` | `3s` | Bounds that read. |
| `overlay.sync_remove` | `false` | As in containerd's built-in overlayfs plugin. |
| `overlay.upperdir_label` | `false` | As in the built-in plugin. For a block layer the label points at its upper directory inside the image. |
| `overlay.mount_options` | empty | As in the built-in plugin. |
| `writable_layer.source` | `off` | `off` or `ephemeral-storage`. |
| `writable_layer.limit_offset` | empty (0) | Subtracted from a limit. Reserves room for logs and emptyDir volumes. |
| `writable_layer.request_percent` | `105` (when 0) | Applied to a request. At least 100. |
| `writable_layer.default_size` | empty | For containers that declare nothing, have no annotation, or whose computed size is below 16Mi. At least 16Mi. |

With `source = "off"` every new container keeps a directory upper, and block layers created earlier keep being served until their containers are removed.

## Operating

- What a Pod declared: `kubectl get pod <pod> -o jsonpath='{.metadata.annotations.scitix\.ai/ephemeral-storage}'`. What a container received: `ctr -n k8s.io containers info <container id>` shows the annotation under `Spec.annotations`; if it is missing there but present on the Pod, the runtime handler lacks `pod_annotations`.
- A container's block layer: `ctr -n k8s.io snapshots --snapshotter blockoverlayfs info <container id>` shows the label `containerd.io/snapshot/writable-mountpoint`. `<root>/blocks/<id>/state.json` records the size and why it was chosen, with the pod and container. A reason starting with `no scitix.ai/ephemeral-storage annotation` means the annotation did not reach the container; `unreadable` means it is malformed, and the snapshotter's log says why.
- Attached images: `losetup -a`. Mounted layers: `findmnt -t ext4`. Do not use `/sys/module/loop/parameters/max_loop` for capacity: the kernel creates loop devices on demand.
- A removed container's image is deleted once containerd's garbage collection removes its snapshot.
- kubelet: a container's `rootfs.usedBytes` is its block layer's usage, which is what eviction compares. The summary's rootfs `capacityBytes` and `availableBytes` stay those of the image filesystem; eviction does not read them.

## Development

| Command | What it does |
| --- | --- |
| `make test` | Unit tests. They run on macOS too. |
| `make vet` | `go vet` for the host platform and for linux. |
| `make lint` | golangci-lint v2, linting the linux build. |
| `sudo -E env "PATH=$PATH" make test-root` | Block layer tests, a gRPC round trip, and containerd's snapshotter conformance suite twice: as a plain overlay, and with a block layer under every active snapshot. Needs Linux and root; CI runs it. `ROOT_TESTFLAGS=` drops `-race`. |

Layout:

```
cmd/containerd-blockoverlayfs-grpc/  snapshotter daemon entry point
pkg/annotation/                     the Pod annotation's key and format
pkg/config/                         TOML configuration, defaults and validation
pkg/sizing/                         block layer size policy
pkg/block/                          sparse ext4 images: create, mount, measure, copy, detach, reap
pkg/snapshot/                       wrapper snapshotter, CRI decider, key parsing, mount rewriting
pkg/containermeta/                  container records from containerd: CRI labels and OCI spec annotations
version/                            version info injected at link time
deploy/                             snapshotter config, systemd unit, containerd snippet
```

## License

Apache-2.0, see [LICENSE](LICENSE).
