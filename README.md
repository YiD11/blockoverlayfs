# blockoverlayfs

Hard disk limits for container writable layers on Kubernetes.

A container that writes to its root filesystem keeps growing the node's disk until kubelet notices and evicts the Pod. With blockoverlayfs each container's writable layer lives in its own ext4 image, sized from the container's `ephemeral-storage`, so a container that fills it gets `ENOSPC` instead of filling the node's disk.

blockoverlayfs is an [NRI](https://github.com/containerd/nri) plugin for containerd 2.x. containerd keeps its built-in overlayfs snapshotter and the images it has already unpacked, and kubelet and containerd run unmodified.

## How it works

```
kubelet ── CreateContainer ──▶ containerd
                                 ├─ overlayfs snapshotter prepares the rootfs: snapshots/<n>/fs and work
                                 ├─ NRI CreateContainer ──▶ blockoverlayfs
                                 │                            ├─ reads the size from the Pod annotation
                                 │                            ├─ creates and formats a sparse ext4 image
                                 │                            └─ loop-mounts it over snapshots/<n> and recreates fs and work in it
                                 └─ StartContainer: the overlay mounted from snapshots/<n> writes into the image

kubelet ── RemoveContainer ──▶ containerd
                                 ├─ NRI RemoveContainer ──▶ blockoverlayfs unmounts the image and deletes it
                                 └─ overlayfs snapshotter deletes snapshots/<n>
```

containerd calls the plugin after the snapshotter has prepared the container's rootfs and before anything mounts it. The plugin covers the snapshot's directory with the image, giving the new `fs` and `work` directories the owner, mode and extended attributes of the ones it covers. The snapshot's mounts keep naming the same paths, and those paths now lead into the image.

- CRI has no field for ephemeral-storage, so the size comes from a Pod annotation that your platform writes. NRI passes the plugin every Pod annotation, so no containerd setting is needed for it.
- Only containers that kubelet creates get a block layer. Sandbox (pause) containers and image layers stay plain overlayfs directories.
- The images, `<root>/<container ID>.img`, are the plugin's only state. Which of them are in use is read back from the mount table, so restarting the plugin, containerd or the node needs no repair. The plugin never talks to the Kubernetes API.
- If containerd fails to create a container after the plugin attached its layer, it tells the plugin, which takes the layer away again. Whatever the plugin missed while it was not registered, it cleans up when it registers again and every 10 minutes.

## Requirements

- Linux with overlayfs, ext4 and loop devices, and `mkfs.ext4` (e2fsprogs).
- containerd 2.x with NRI enabled, which is the default, and its overlayfs snapshotter.

## Install

1. Build and install the binary:

   ```bash
   make binaries
   sudo make install
   ```

   `make binaries GOARCH=amd64` cross-compiles.

2. Install the configuration and the systemd unit, and start the plugin:

   ```bash
   sudo install -D -m 644 deploy/config.toml /etc/blockoverlayfs/config.toml
   sudo install -D -m 644 deploy/systemd/blockoverlayfs.service /etc/systemd/system/blockoverlayfs.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now blockoverlayfs
   ```

   The unit reports ready once the plugin has registered with containerd, and starts before kubelet. Do not add mount sandboxing such as `PrivateMounts=` to it: the plugin has to mount in the host's mount namespace.

3. Optionally, merge [deploy/containerd/config.toml](deploy/containerd/config.toml) into `/etc/containerd/config.toml` and restart containerd. It sets `plugin_request_timeout`: raise it if creating a block layer can take longer than containerd's default of 2 seconds. See [Limits](#limits).

4. Add the annotation to your Pods:

   ```yaml
   metadata:
     annotations:
       blockoverlayfs.io/ephemeral-storage: '{"app":{"request":"10Gi","limit":"20Gi"}}'
   ```

Running containers are left as they are: only containers created after the plugin registered get block layers, so nothing needs draining. The plugin can be stopped at any time. Mounted layers stay in place for their containers, and layers of containers removed meanwhile are cleaned up once it runs again.

## The annotation

`blockoverlayfs.io/ephemeral-storage` is a JSON object keyed by container name. Each entry holds an optional `request` and `limit`, written as Kubernetes quantities:

```json
{"app": {"request": "10Gi", "limit": "20Gi"}, "init": {"limit": "1Gi"}}
```

- Put it on the Pod, or for a workload on its Pod template (`spec.template.metadata.annotations`).
- Copy the values from the containers' `resources` for `ephemeral-storage`. Nothing checks that they match: a larger limit means kubelet evicts the Pod before the container sees `ENOSPC`, and larger values let block layers outgrow what the scheduler reserved on the node.
- A container that is not listed, or whose entry is empty or 0, is treated as declaring nothing.
- Malformed JSON, or an unknown field such as `limits`, makes the whole annotation unreadable, and every container in the Pod is treated as declaring nothing.
- Anyone who can create or patch a Pod can set this annotation. Let only your platform or an admission webhook write it.

## Sizing

| The container declares | Block layer size |
| --- | --- |
| a limit | `limit - limit_offset` |
| only a request | `ceil(request × request_percent / 100)` |
| nothing, or the result is below 16Mi | `default_size`, or a plain overlayfs directory if `default_size` is unset |

The size is fixed when the container is created. kubelet restarts a container by creating a new one, which picks up the annotation's current value.

## Configuration

`/etc/blockoverlayfs/config.toml`; see [deploy/config.toml](deploy/config.toml). Every key is optional. Sizes are Kubernetes quantities such as `256Mi` or `20Gi`. The flags are `--config`, `--log-level`, which overrides `log_level`, and `--version`.

| Key | Default | Description |
| --- | --- | --- |
| `log_level` | `info` | `trace`, `debug`, `info`, `warn`, `error`, `fatal` or `panic`. |
| `root` | `/var/lib/blockoverlayfs` | Where the images are kept. Keep it on the filesystem that holds containerd's data, which kubelet watches as the image filesystem. |
| `nri.socket_path` | `/var/run/nri/nri.sock` | containerd's NRI socket. |
| `nri.plugin_index` | `10` | Two digits that order the plugin among other NRI plugins. |
| `containerd.address` | `/run/containerd/containerd.sock` | containerd's socket, used to find a container's snapshot. |
| `containerd.namespace` | `k8s.io` | The containerd namespace of Kubernetes containers. |
| `containerd.snapshotter` | `overlayfs` | The snapshotter whose containers get block layers. |
| `containerd.snapshotter_root` | `/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs` | That snapshotter's root. The plugin mounts only over its `snapshots/<n>` directories. |
| `size.limit_offset` | `0` | Subtracted from a limit, to leave room for logs and emptyDir volumes, which kubelet counts against the same limit. |
| `size.request_percent` | `105` | Applied to a request. At least 100. |
| `size.default_size` | unset | Size for containers that declare nothing. At least 16Mi. |

## Limits

- containerd passes container creations to NRI plugins one at a time for the whole node. Formatting the image adds to every container's creation, and it takes longer the larger the image.
- containerd waits `plugin_request_timeout`, 2 seconds by default, for each request. A layer that takes longer is abandoned: containerd drops the plugin and creates the container without a block layer. The plugin then registers again. If your largest layers come close to the timeout, raise it.
- While the plugin is not registered, containerd creates containers as usual, on plain overlayfs directories. They keep those directories: the plugin never adds a layer to a container that already exists.
- Only the configured snapshotter is covered. A container whose runtime handler uses another snapshotter keeps its writable layer as it is.
- Do not commit a container that has a block layer into an image, as `nerdctl commit` does. The committed layer's files would stay in the container's ext4 image and disappear when the container is removed.

## Troubleshooting

The plugin logs one line per container when it is created: `created block writable layer` with `size`, `reason` and `duration`, or `container keeps a directory writable layer` with `reason`. Both carry `pod`, `container` and `id`:

```bash
journalctl -u blockoverlayfs | grep 'pod=<namespace>/<pod>'
```

| `reason` starts with | Meaning |
| --- | --- |
| `no blockoverlayfs.io/ephemeral-storage annotation` | The Pod has no annotation. |
| `unreadable blockoverlayfs.io/ephemeral-storage annotation` | The annotation is malformed. A warning logged just before says why. |
| `container not listed in the ...` | The annotation does not name this container. |

If a container got no line at all, the plugin was not registered when containerd created it. containerd logs `closing plugin` with the reason when it drops the plugin, for example after a timeout:

```bash
journalctl -u containerd | grep -i nri
```

`ls /var/lib/blockoverlayfs` lists the images by container ID. To see where one is mounted:

```bash
losetup -j /var/lib/blockoverlayfs/<container-id>.img
findmnt /dev/loopN
```

`findmnt -t ext4` lists every mounted layer, under the overlayfs snapshotter's `snapshots` directory.

## Development

```bash
make test       # unit tests, run on macOS too
make vet        # go vet for the host and for linux
make lint       # golangci-lint v2
sudo -E env "PATH=$PATH" make test-root   # Linux and root: block layers on loop devices, ext4 and overlayfs
```

Building needs Go 1.25 or newer.

| Package | Contents |
| --- | --- |
| `cmd/blockoverlayfs` | Entry point: configuration, NRI registration |
| `pkg/plugin` | NRI handlers and reconciliation |
| `pkg/block` | ext4 images: create, mount, unmount, delete |
| `pkg/containerd` | Reads snapshots and containers from containerd |
| `pkg/sizing` | Size policy |
| `pkg/annotation` | Annotation format |
| `pkg/config` | Configuration file |

## License

Apache-2.0. See [LICENSE](LICENSE).
