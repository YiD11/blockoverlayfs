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
- The images, `<root>/<container ID>.img`, are the plugin's only state. An image is a container's layer while it is mounted over the container's snapshot, holds the upper directory, and containerd has the container. The plugin reads the first two from the mount table and the third from containerd, so restarting the plugin, containerd or the node needs no repair. The plugin never talks to the Kubernetes API.
- Any other image is what is left of something that was cut short: the node restarted, or the plugin or containerd stopped while a container was being created or removed. No container writes to such an image, and none will, because kubelet replaces a container instead of starting it again. The plugin deletes these images when it starts, when it registers again, and every 10 minutes. See [Restarts](#restarts).
- If containerd fails to create a container after the plugin attached its layer, it tells the plugin, which takes the layer away again.

## Requirements

- Kubernetes on containerd 2.x with NRI enabled, which is the default, and containerd's overlayfs snapshotter.
- Linux 5.15 or newer with overlayfs, ext4 and loop devices.
- A node whose `/` is a shared mount, which it is under systemd: the plugin mounts in its container, and the mounts have to reach the node.
- Helm 3 or newer.

## Install

1. Build the image and push it to a registry your nodes pull from:

   ```bash
   make image IMAGE=registry.example.com/blockoverlayfs:0.1.0
   docker push registry.example.com/blockoverlayfs:0.1.0
   ```

   `make image CONTAINER_TOOL=nerdctl` builds with another tool. The image holds the plugin and the `mkfs.ext4` it formats images with, so the nodes need neither.

2. Install the chart:

   ```bash
   helm install blockoverlayfs charts/blockoverlayfs --namespace kube-system \
     --set image.repository=registry.example.com/blockoverlayfs --set image.tag=0.1.0
   ```

   The chart runs the plugin on every node as a privileged DaemonSet. Its Pod mounts the snapshotter's directory with bidirectional mount propagation, so the layers it mounts there appear on the node, where containerd's shims mount container rootfs. A Pod is ready once the plugin has registered with containerd. See [Configuration](#configuration) for the chart's values.

3. Optionally, merge [deploy/containerd/config.toml](deploy/containerd/config.toml) into `/etc/containerd/config.toml` on the nodes and restart containerd. It sets `plugin_request_timeout`: raise it if creating a block layer can take longer than containerd's default of 2 seconds. See [Limits](#limits).

4. Add the annotation to your Pods:

   ```yaml
   metadata:
     annotations:
       blockoverlayfs.io/ephemeral-storage: '{"app":{"request":"10Gi","limit":"20Gi"}}'
   ```

Running containers are left as they are: only containers created after the plugin registered get block layers, so nothing needs draining. The plugin's Pod can be deleted at any time: it finishes the layer it is attaching or removing first. Mounted layers stay in place for their containers, and layers of containers removed meanwhile are cleaned up once the plugin runs again.

## Uninstall

```bash
helm uninstall blockoverlayfs --namespace kube-system
```

Delete the Pods that have block layers first, or drain the node. A layer that is mounted when the plugin goes stays mounted for its container, and nothing takes it away when the container is removed: the image stays, and containerd cannot delete the snapshot's directory under it. Installing the chart again clears what is left; so does restarting the node and then deleting `/var/lib/containerd/blockoverlayfs`.

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

## Restarts

Nothing needs to be done after a restart. What each one does to block layers:

| What restarts | Layers of running containers | A container being created or removed at that moment |
| --- | --- | --- |
| The plugin's Pod, deleted or replaced by `helm upgrade` | Stay mounted. | The plugin finishes that layer before it stops. |
| The plugin, killed | Stay mounted. | containerd goes on without the plugin: a container being created gets a plain overlayfs directory, and one being removed is removed. The plugin deletes what is left of the layer when kubelet has started it again. |
| containerd | Stay mounted. | kubelet asks again. A container being created is created anew and gets a new layer, and the plugin deletes the layer of the attempt that was cut short as soon as it has registered again. |
| The node | Gone with their containers: kubelet creates new containers, with new layers. | The same. |

- **After a node restart, what the old containers wrote is gone.** The restart unmounts every layer, and the plugin deletes the images when it starts. A plain overlayfs directory would have kept the files of an exited container until kubelet removed the container.
- **While the plugin is not registered, containers are created without block layers**, and they keep running without one: the plugin never adds a layer to a container that already exists. The plugin is not registered:
  - after the node booted, until kubelet has started the plugin's Pod, which it does alongside the node's other Pods;
  - while its Pod is being replaced, by `helm upgrade` or a changed configuration, which takes as long as stopping the old Pod and starting the new one, pulling its image included;
  - after it was killed, until kubelet has restarted it: at once the first time, and after a growing delay if it keeps failing;
  - for about 100 milliseconds after containerd has come back from a restart.
- **A Pod can ask to wait for the plugin instead.** Enable NRI's validator in containerd, as shown in [deploy/containerd/config.toml](deploy/containerd/config.toml), and annotate the Pod:

  ```yaml
  metadata:
    annotations:
      required-plugins.noderesource.dev: '["blockoverlayfs"]'
  ```

  While the plugin is not registered, containerd then refuses to create that Pod's containers, and kubelet tries again until the plugin is back. Pods without the annotation are created as before. The same file shows how to require the plugin for every container on a node.
- **A plugin that is killed in the few milliseconds between mounting an image and completing the layer** leaves the snapshot without an upper directory until kubelet has restarted it. If kubelet starts the container in that time, the start fails, and kubelet then follows the Pod's restart policy.

## Configuration

The plugin is configured through the chart's values; see [charts/blockoverlayfs/values.yaml](charts/blockoverlayfs/values.yaml). The chart renders `config` into the plugin's configuration file and mounts every path in it into the plugin's container, so a path names the same directory on the node and in the container. Sizes are Kubernetes quantities such as `256Mi` or `20Gi`.

| Value | Default | Description |
| --- | --- | --- |
| `config.logLevel` | `info` | `trace`, `debug`, `info`, `warn`, `error`, `fatal` or `panic`. |
| `config.root` | `/var/lib/containerd/blockoverlayfs` | Where the images are kept. Keep it on the filesystem that holds containerd's data, which kubelet watches as the image filesystem. |
| `config.nri.socketPath` | `/var/run/nri/nri.sock` | containerd's NRI socket. |
| `config.nri.pluginIndex` | `10` | Two digits that order the plugin among other NRI plugins. |
| `config.containerd.address` | `/run/containerd/containerd.sock` | containerd's socket, used to find a container's snapshot. |
| `config.containerd.namespace` | `k8s.io` | The containerd namespace of Kubernetes containers. |
| `config.containerd.snapshotter` | `overlayfs` | The snapshotter whose containers get block layers. |
| `config.containerd.snapshotterRoot` | `/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs` | That snapshotter's root. The plugin mounts only over its `snapshots/<n>` directories. |
| `config.size.limitOffset` | `0` | Subtracted from a limit, to leave room for logs and emptyDir volumes, which kubelet counts against the same limit. |
| `config.size.requestPercent` | `105` | Applied to a request. At least 100. |
| `config.size.defaultSize` | unset | Size for containers that declare nothing. At least 16Mi. |
| `containerdStateDir` | `/run/containerd` | containerd's state directory, where its shims mount container rootfs. The plugin looks there before it covers a snapshot. |

The other values place and run the DaemonSet: `image`, `nodeSelector`, `affinity`, `tolerations`, `priorityClassName`, `resources`, `updateStrategy`, `podAnnotations` and `podLabels`. By default the plugin runs on every Linux node and tolerates every taint.

Changing a value with `helm upgrade` replaces the plugin's Pods, one node at a time. Containers created on a node while its Pod is being replaced get no block layer; see [Restarts](#restarts).

## Limits

- containerd passes container creations to NRI plugins one at a time for the whole node. Formatting the image adds to every container's creation, and it takes longer the larger the image.
- containerd waits `plugin_request_timeout`, 2 seconds by default, for each request. A layer that takes longer is abandoned: containerd drops the plugin and creates the container without a block layer. The plugin then registers again. If your largest layers come close to the timeout, raise it.
- While the plugin is not registered, containerd creates containers as usual, on plain overlayfs directories, and a node restart discards what its containers wrote to their block layers. See [Restarts](#restarts).
- Only the configured snapshotter is covered. A container whose runtime handler uses another snapshotter keeps its writable layer as it is.
- Committing a container that has a block layer works as usual: `nerdctl commit` reads the container's files through the ext4 image and copies the difference into a new image layer, which outlives the container.

## Troubleshooting

The plugin logs one line per container when it is created: `created block writable layer` with `size`, `reason` and `duration`, or `container keeps a directory writable layer` with `reason`. When a container is removed it logs `removed block writable layer`. All carry `pod`, `container` and `id`. Look in the logs of the plugin's Pod on the container's node:

```bash
kubectl -n kube-system get pods -l app.kubernetes.io/name=blockoverlayfs -o wide
```

```bash
kubectl -n kube-system logs <plugin-pod> | grep 'pod=<namespace>/<pod>'
```

| `reason` starts with | Meaning |
| --- | --- |
| `no blockoverlayfs.io/ephemeral-storage annotation` | The Pod has no annotation. |
| `unreadable blockoverlayfs.io/ephemeral-storage annotation` | The annotation is malformed. A warning logged just before says why. |
| `container not listed in the ...` | The annotation does not name this container. |

If a container got no line at all, the plugin was not registered when containerd created it. containerd logs `closing plugin` with the reason when it drops the plugin, for example after a timeout. On the node:

```bash
journalctl -u containerd | grep -i nri
```

When the plugin deletes an image that is no container's layer, it logs `deleted a block writable layer` with the container's `id` and a `reason`:

| `reason` | Meaning |
| --- | --- |
| `it is mounted nowhere` | The node restarted, or attaching or removing the layer was cut short before the image was mounted or after it was unmounted. |
| `it holds no upper directory` | Attaching the layer was cut short after the image was mounted, or the container was removed while the plugin was not registered. |
| `it has no container` | The container was removed, or containerd gave its creation up, while the plugin was not registered. |

A plugin Pod that does not become ready has not registered with containerd, and its log says why. One that stays in `ContainerCreating` is missing a directory on the node, which the Pod's events name: `/var/run/nri` is missing when NRI is disabled in containerd.

On the node, `ls /var/lib/containerd/blockoverlayfs` lists the images by container ID. To see where one is mounted:

```bash
losetup -j /var/lib/containerd/blockoverlayfs/<container-id>.img
findmnt /dev/loopN
```

`findmnt -t ext4` lists every mounted layer, under the overlayfs snapshotter's `snapshots` directory.

## Development

```bash
make test       # unit tests, run on macOS too
make vet        # go vet for the host and for linux
make lint       # golangci-lint v2
sudo -E env "PATH=$PATH" make test-root   # Linux and root: block layers on loop devices, ext4 and overlayfs
make helm-lint  # lint and render the chart
make image      # build the container image
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

The chart is in `charts/blockoverlayfs`, and the image is built from `Dockerfile`.

## License

Apache-2.0. See [LICENSE](LICENSE).
