# blockoverlayfs

Hard disk limits for container writable layers on Kubernetes.

By default a container that writes to its root filesystem keeps growing the node's disk until kubelet notices and evicts the Pod. With blockoverlayfs each container's writable layer lives in its own ext4 image sized from its `ephemeral-storage` limit, so a container that fills it gets `ENOSPC` instead of filling the node's disk.

blockoverlayfs is a containerd [proxy snapshotter](https://github.com/containerd/containerd/blob/release/1.7/docs/PLUGINS.md#proxy-plugins) built on containerd's own overlayfs snapshotter. kubelet and containerd run unmodified.

## How it works

```
Pod annotation ──kubelet──▶ containerd ──pod_annotations──▶ container OCI spec
                                │
                                └──proxy_plugins──▶ blockoverlayfs
                                                     ├─ reads the container's size from its OCI spec
                                                     ├─ creates a sparse ext4 image and loop-mounts it
                                                     └─ places the overlay upperdir and workdir inside it
```

CRI has no field for ephemeral-storage, so the size reaches the snapshotter through a Pod annotation. containerd's `pod_annotations` setting copies it into each container's OCI spec, and blockoverlayfs reads it there when the container starts. blockoverlayfs never talks to the Kubernetes API.

Only containers that kubelet creates get a block layer. Sandbox (pause) containers, image layers and views stay plain overlayfs directories.

## Quick start

Requirements: Linux with overlayfs, ext4 and loop devices, `mkfs.ext4` (e2fsprogs), and containerd 1.7.

1. Build and install the binary:

   ```bash
   make binaries
   sudo make install
   ```

2. Install the configuration and the systemd unit:

   ```bash
   sudo install -D -m 644 deploy/config.toml /etc/containerd-blockoverlayfs-grpc/config.toml
   sudo install -D -m 644 deploy/systemd/containerd-blockoverlayfs-grpc.service /etc/systemd/system/containerd-blockoverlayfs-grpc.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now containerd-blockoverlayfs-grpc
   ```

   Do not add mount sandboxing such as `PrivateMounts=` to the unit. The daemon has to mount block layers in the host's mount namespace.

3. Merge [deploy/containerd/config.toml](deploy/containerd/config.toml) into `/etc/containerd/config.toml`, then restart containerd. Drain the node first: only containers created after the switch use the new snapshotter.

4. Add the annotation to your Pods:

   ```yaml
   metadata:
     annotations:
       blockoverlayfs.io/ephemeral-storage: '{"app":{"request":"10Gi","limit":"20Gi"}}'
   ```

After the switch containerd unpacks images again for blockoverlayfs. Nothing is downloaded twice, but unpacked layers take disk space twice until containerd garbage-collects the old overlayfs snapshots.

## The annotation

`blockoverlayfs.io/ephemeral-storage` is a JSON object keyed by container name. Each entry holds an optional `request` and `limit`, written as Kubernetes quantities:

```json
{"app": {"request": "10Gi", "limit": "20Gi"}, "init": {"limit": "1Gi"}}
```

- Put it on the Pod itself, or for a workload on its Pod template (`spec.template.metadata.annotations`).
- Copy the values from the containers' `resources` for `ephemeral-storage`. Nothing checks that they match: a larger limit means kubelet evicts the Pod before the container sees `ENOSPC`, and larger values let block layers outgrow what the scheduler reserved on the node.
- A container that is not listed, or whose entry is empty or 0, is treated as declaring nothing.
- Malformed JSON or an unknown field such as `limits` makes the whole annotation unreadable, and every container in the Pod is treated as declaring nothing.
- Anyone who can create or patch a Pod can set this annotation. Let only your platform or an admission webhook write it.

## Sizing

| The container declares | Block layer size |
| --- | --- |
| a limit | `limit - limit_offset` |
| only a request | `ceil(request × request_percent / 100)` |
| nothing, or the result is below 16Mi | `default_size`, or a plain overlayfs directory if `default_size` is empty |

The size is fixed when the container first starts. A restarted container picks up the annotation's current value.

## Configuration

`/etc/containerd-blockoverlayfs-grpc/config.toml`. See [deploy/config.toml](deploy/config.toml) for an example. The flags `--config`, `--root`, `--address` and `--log-level` override the file. Sizes are Kubernetes quantities such as `256Mi` or `20Gi`.

| Key | Default | Description |
| --- | --- | --- |
| `root` | `/var/lib/containerd/io.containerd.snapshotter.v1.blockoverlayfs` | State directory. Keep it on the filesystem that holds containerd's data. |
| `address` | `/run/containerd-blockoverlayfs-grpc/containerd-blockoverlayfs-grpc.sock` | gRPC socket that containerd's `proxy_plugins` entry points to. |
| `log_level` | `info` | `trace`, `debug`, `info`, `warn`, `error`, `fatal` or `panic`. |
| `containerd.address` | `/run/containerd/containerd.sock` | containerd socket, used to read container records. |
| `containerd.lookup_timeout` | `3s` | Timeout for reading a container record. |
| `overlay.sync_remove` | `false` | Same as containerd's overlayfs snapshotter. |
| `overlay.upperdir_label` | `false` | Same as containerd's overlayfs snapshotter. |
| `overlay.mount_options` | `[]` | Same as containerd's overlayfs snapshotter. |
| `writable_layer.source` | `off` | `ephemeral-storage` enables block layers. `off` keeps new containers on plain directories. |
| `writable_layer.limit_offset` | `0` | Subtracted from a limit, to leave room for logs and emptyDir volumes. |
| `writable_layer.request_percent` | `105` | Applied to a request. At least 100. |
| `writable_layer.default_size` | empty | Size for containers that declare nothing. At least 16Mi. |

When merging the containerd snippet:

- `exports.root` in `proxy_plugins.blockoverlayfs` must equal the snapshotter's `root`. kubelet reads this path as the image filesystem, and eviction stops working if it cannot.
- Set `pod_annotations` on every runtime handler your Pods use, such as `runc` and `nvidia`. A handler without it passes no annotation, so its containers are sized as declaring nothing.

## Troubleshooting

Check the annotation on the Pod and in the container:

```bash
kubectl get pod <pod> -o jsonpath='{.metadata.annotations.blockoverlayfs\.io/ephemeral-storage}'
ctr -n k8s.io containers info <container-id>   # under Spec.annotations
```

If it is on the Pod but not in the container, the runtime handler is missing `pod_annotations`.

The daemon logs each container's writable layer once, the first time the container starts, as `created block writable layer` or `container keeps a directory writable layer`, with the fields `pod`, `container`, `size` and `reason`:

```bash
journalctl -u containerd-blockoverlayfs-grpc | grep 'pod=<namespace>/<pod>'
```

| `reason` starts with | Meaning |
| --- | --- |
| `no blockoverlayfs.io/ephemeral-storage annotation` | The annotation did not reach the container. |
| `unreadable blockoverlayfs.io/ephemeral-storage annotation` | The annotation is malformed. A warning logged just before says why. |
| `container not listed in the ...` | The annotation does not name this container. |

To find a container's block layer, run `ctr -n k8s.io snapshots --snapshotter blockoverlayfs info <container-id>` and look for the label `containerd.io/snapshot/writable-mountpoint`. `losetup -a` lists attached images and `findmnt -t ext4` lists mounted layers.

## Development

```bash
make test       # unit tests, run on macOS too
make vet        # go vet for the host and for linux
make lint       # golangci-lint v2
sudo -E env "PATH=$PATH" make test-root   # Linux and root: block layers and containerd's snapshotter test suite
```

Building needs Go 1.25 or newer. `make binaries GOARCH=amd64` cross-compiles.

| Package | Contents |
| --- | --- |
| `cmd/containerd-blockoverlayfs-grpc` | Daemon entry point |
| `pkg/snapshot` | The wrapping snapshotter |
| `pkg/block` | ext4 images: create, mount, measure, remove |
| `pkg/sizing` | Size policy |
| `pkg/annotation` | Annotation format |
| `pkg/containermeta` | Reads container records from containerd |
| `pkg/config` | Configuration file |

## License

Apache-2.0. See [LICENSE](LICENSE).
