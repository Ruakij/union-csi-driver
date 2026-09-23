# union-csi-driver

[![CI](https://github.com/Ruakij/union-csi-driver/actions/workflows/ci.yaml/badge.svg)](https://github.com/Ruakij/union-csi-driver/actions/workflows/ci.yaml)
[![Version](https://img.shields.io/github/v/release/Ruakij/union-csi-driver?label=Version&color=green)](https://github.com/Ruakij/union-csi-driver/releases)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25%2B-blue)](charts/union-csi-driver/Chart.yaml)
[![Backends](https://img.shields.io/badge/Backends-overlayfs%20%7C%20mergerfs-orange)](#backend-differences-that-show-up-in-the-manifest)
[![Go](https://img.shields.io/github/go-mod/go-version/Ruakij/union-csi-driver?label=Go)](go.mod)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

**An overlayfs and mergerfs CSI driver for Kubernetes.**

It unions several sibling volumes already declared on a pod into a single merged mount
point, using either the kernel's `overlay` filesystem (overlayfs) or `mergerfs` (FUSE)
as the merge backend. The backend is picked per DaemonSet, so a cluster can run either
or both.

It does not proxy or re-implement other CSI drivers. It waits for kubelet to publish the
sibling volumes a pod already declares, then unions their node-local publish paths at its
own target path.

Without it, merging volumes in Kubernetes means a privileged sidecar running `mount` into
a shared `emptyDir` with `Bidirectional` propagation, or an init container copying data
around. A union volume replaces both with an ordinary pod volume.

## Features

On top of what overlayfs and mergerfs do themselves:

- **Mounts survive driver restarts, upgrades and evictions**, including the FUSE-based
  mergerfs ones. See [Surviving restarts](#surviving-restarts).
- **Most volume types as sources.** PVCs (bound to CSI, `local`, `hostPath`, `nfs`,
  `iscsi` or `fc` PVs), generic ephemeral volumes, inline CSI volumes, `emptyDir`,
  `configMap`, `secret`, `downwardAPI`, `projected`, `hostPath`, `nfs`, `iscsi` and
  `fc`, mixed freely in one merge.
- **Sources by pod volume name.** No node paths, PV names or PVC names in the manifest,
  and no ambiguity when two volumes point at the same claim.
- **Kubernetes-native waiting.** Sibling volumes are waited for before merging. If they
  are not ready in time, the mount fails with a message naming the pending volumes and
  kubelet's own retry and backoff take over. Errors are ordinary `FailedMount` events in
  `kubectl describe pod`.
- **Per-branch write modes** in each backend's own vocabulary (`RW`, `RO`, `NC`), plus a
  whole-volume `readOnly` switch.
- **Admin-controlled option policy.** Pods may tune backend options, but only within a
  per-backend schema and an admin allowlist, denylist, defaults and forced set.
- **Safe by construction.** Pods never choose the backend and never pass paths or
  process-shaping options. Everything that ends up on a mount command line or in a
  syscall is computed on the node.
- **One binary, one image, one chart.** Both backends ship in the same image. Pick one
  per Helm release, or install twice to offer both.
- **Node-only.** No controller, provisioner or attacher, just a DaemonSet with
  `node-driver-registrar`.

## Surviving restarts

Many FUSE-based CSI drivers run their FUSE daemons inside the driver pod. Restarting,
upgrading or evicting that pod kills every daemon on the node, and every workload using
those mounts is left with `Transport endpoint is not connected` until it is recreated.
The usual workaround is a separate service installed on the host, such as
[blob-csi-driver's blobfuse-proxy](https://github.com/kubernetes-sigs/blob-csi-driver/tree/master/deploy/blobfuse-proxy).

Here, nothing has to be installed on the node. Union volumes stay up across a driver
restart, upgrade or eviction:

| Backend                   | Driver pod restarts                                                 | Daemon crash or OOM kill                                    |
| ------------------------- | ------------------------------------------------------------------- | ----------------------------------------------------------- |
| overlay                   | Unaffected: a kernel mount with no process behind it.               | No daemon.                                                  |
| mergerfs, host systemd    | Unaffected: the daemon belongs to host systemd, not the driver pod. | Remounted within 30s; open file descriptors see `ENOTCONN`. |
| mergerfs, no host systemd | Remounted within 30s; open file descriptors see `ENOTCONN`.         | Remounted within 30s; open file descriptors see `ENOTCONN`. |

- **overlay** mounts are created in the host's mount namespace through the
  `Bidirectional` kubelet pods mount, so they live on regardless of the driver pod.
- **mergerfs with host systemd** (the default wherever `/run/systemd` exists): each
  daemon runs from the driver image in its own session and is then adopted into a
  transient host systemd scope, `union-csi-<volumeID>.scope`, the same mechanism as
  `systemd-run --scope` and kubelet's own mount helpers. From then on the daemon belongs
  to host systemd, not the driver pod's cgroup. The scope name is derived from the
  volume ID, so a restarted driver still finds and stops it on unmount.
- **mergerfs without systemd** (Talos, other non-systemd nodes, or
  `mergerfs.daemonLifetime=in-container`): daemons die with the driver pod and are
  remounted as below. Both the driver log and the chart's install notes warn about this
  mode.

Each mergerfs daemon's target and exact argv are recorded in
`<kubeletRoot>/plugins/<driverName>/state/<volumeID>.json` before it starts. On startup
and every 30s after, the driver remounts every mount whose daemon died, and drops
records of targets kubelet has removed. The mount comes back at the same path, but file
descriptors opened before it died keep returning `ENOTCONN` until the workload reopens
them.

With `mergerfs.daemonLifetime=systemd`, the driver pod does not start at all on nodes
without systemd, instead of silently falling back.

While no driver pod is running, for example mid-rollout, new pods with a union volume
wait in `ContainerCreating` with a `FailedMount` event and start once it is back.
Running pods are unaffected.

## How it works

Only CSI ephemeral inline volumes are supported. For each union volume, kubelet calls
`NodePublishVolume`, which:

1. Parses `volumeAttributes` against a fixed grammar and rejects unknown keys.
2. Resolves backend options through the admin policy: defaults, then pod options, then
   forced options.
3. Reads the pod from the API server, checks its UID against the one kubelet injected,
   and maps each named volume to its kubelet publish path under
   `<kubeletRoot>/pods/<podUID>/volumes/`. PVCs are followed to their PV name, since
   kubelet names their directories that way. Generic ephemeral volumes are followed
   through their `<pod>-<volume>` claim, which must be owned by the pod. hostPath
   volumes and PVs are looked up under the host root bind-mounted at `/host`. Every
   resolved path must stay inside its root.
4. Waits up to `publishTimeout` for each source: a real mountpoint for CSI, `local` and
   network volumes, an existing directory for the rest. On timeout it returns a
   retryable error and kubelet tries again later.
5. Mounts the union at the target path:
   - **overlay**: a kernel overlay mount through the `fsopen`/`fsconfig` API where
     available (one argument per layer, no option-length limit), otherwise classic
     `mount(2)`. A single source becomes a bind mount.
   - **mergerfs**: starts the `mergerfs` daemon without a shell and waits until the
     target is a live FUSE mount.

A target that is already mounted counts as published, so kubelet's repeated calls,
including those after a driver restart, are no-ops. `NodeUnpublishVolume` unmounts the
union, cleans up, and succeeds if the volume is already gone.

## Install

One release runs one backend. Installing both means installing the chart twice, with
different release names.

From the Helm repository:

```sh
helm repo add union-csi https://ruakij.github.io/union-csi-driver
helm repo update
helm install mergerfs-csi union-csi/union-csi-driver --set backend=mergerfs
helm install overlay-csi  union-csi/union-csi-driver --set backend=overlay
```

Or straight from the OCI registry, which is also where prereleases go:

```sh
helm install mergerfs-csi oci://ghcr.io/ruakij/charts/union-csi-driver --set backend=mergerfs
```

Or from a checkout, to run an unreleased revision:

```sh
helm install mergerfs-csi charts/union-csi-driver --set backend=mergerfs
```

The driver name defaults to `<backend>.csi.ruekov.eu`, and that is what pods put in
`volumes[].csi.driver`. If the cluster is k3s, RKE2 or MicroK8s, set `kubeletRootDir`
to the node's real kubelet directory.

## Use

```yaml
containers:
  - name: app
    image: alpine
    volumeMounts:
      - {name: merged, mountPath: /merged}
      - {name: data, mountPath: /sources/data}
      - {name: archive, mountPath: /sources/archive}
volumes:
  - name: data
    persistentVolumeClaim: {claimName: data}
  - name: archive
    persistentVolumeClaim: {claimName: archive}
  - name: merged
    csi:
      driver: mergerfs.csi.ruekov.eu
      volumeAttributes:
        sourceVolumes: "data=RW,archive=RO"
```

Every source volume needs a `volumeMounts` entry in some container of the pod, even if
nothing reads it there: kubelet only sets up volumes a container mounts, so an
unreferenced source never appears on the node. The union volume is rejected with a
clear error instead of waiting for a source that will never arrive.

`sourceVolumes` names volumes of the same pod. Leftmost wins on lookup, and the mode
suffix says whether writes may land there: `RW`, `RO`, or (mergerfs only) `NC`. A bare
name is `RW`. Setting `readOnly: true` on the CSI volume makes the whole merge
read-only regardless.

### Backend differences that show up in the manifest

- **mergerfs** resolves every lookup across branches at request time, so branches may be
  edited out-of-band while mounted. Any number of branches may be `RW`. It has no
  copy-on-write: it cannot express read-only lowers plus one writable top layer.
- **overlay** is kernel-side, with no daemon. It accepts at most one `RW` entry and it
  must be listed first, since the kernel always stacks the single upperdir on top. A
  bare name means `RW`, so mark every other entry `=RO`. The RW entry's merged content
  lives in `<volume>/.union-csi/upper`, so data already at the root of the RW volume is
  not part of the merge. Editing a lower layer while mounted is undefined behaviour per
  the kernel docs.

## Configuration

### Volume attributes

Set by pods in `volumes[].csi`:

| Field                            | Default  | Description                                                                                                 |
| -------------------------------- | -------- | ----------------------------------------------------------------------------------------------------------- |
| `volumeAttributes.sourceVolumes` | required | Comma-separated pod volume names, highest priority first, each with an optional `=RW`, `=RO` or `=NC` mode. |
| `volumeAttributes.options`       | `""`     | Comma-separated `key=value` backend options, see [Backend options](#backend-options).                       |
| `readOnly`                       | `false`  | Mounts the whole merge read-only, whatever the per-source modes say.                                        |

Any other `volumeAttributes` key is rejected.

### Backend options

Pods pass these through `options`, subject to the admin policy below. "Default" is the
value applied when neither the pod nor the admin sets the option; a dash means the
backend's own default applies.

#### mergerfs:

| Option                 | Values                                                                            | Default  | Description                                                                          |
| ---------------------- | --------------------------------------------------------------------------------- | -------- | ------------------------------------------------------------------------------------ |
| `cache.entry`          | seconds                                                                           | `1`      | How long the kernel caches name lookups.                                             |
| `cache.attr`           | seconds                                                                           | `1`      | How long the kernel caches file attributes.                                          |
| `cache.negative_entry` | seconds                                                                           | `0`      | How long the kernel caches failed lookups. Keep at 0 if branches change out-of-band. |
| `cache.readdir`        | `true`, `false`                                                                   | -        | Cache directory listings in the kernel.                                              |
| `cache.files`          | `off`, `partial`, `full`, `auto-full`, `per-process`, `libfuse`                   | -        | Page cache mode for file contents.                                                   |
| `func.getattr`         | `ff`, `newest`                                                                    | `newest` | Which branch's attributes are reported when a file exists in several.                |
| `category.search`      | `ff`, `all`, `newest`                                                             | -        | Policy for finding a file across branches.                                           |
| `category.create`      | `ff`, `mfs`, `lfs`, `lus`, `pfrd`, `rand`, `newest`, `all`, `msp*`, `ep*` forms   | -        | Which `RW` branch a new file lands on.                                               |
| `dropcacheonclose`     | `true`, `false`                                                                   | -        | Drop a file's page cache when it is closed.                                          |
| `inodecalc`            | `passthrough`, `path-hash`, `devino-hash`, `hybrid-hash`, and their `32` variants | -        | How inode numbers of merged files are computed.                                      |
| `threads`              | -16 to 1024                                                                       | -        | Worker threads: 0 for one per CPU, negative to divide the CPU count.                 |
| `minfreespace`         | size, e.g. `4G`                                                                   | -        | Minimum free space for a branch to receive new files.                                |

#### overlay:

| Option                | Values                      | Default    | Description                                                                        |
| --------------------- | --------------------------- | ---------- | ---------------------------------------------------------------------------------- |
| `default_permissions` | flag                        | -          | Check permissions against the merged inode only.                                   |
| `redirect_dir`        | `nofollow`, `off`, `follow` | `nofollow` | Handling of renamed directories.                                                   |
| `index`               | `off`                       | `off`      | Inode index; only `off` is accepted.                                               |
| `xino`                | `off`, `auto`, `on`         | `off`      | Encode the layer in inode numbers for unique inodes across layers.                 |
| `uuid`                | `auto`, `null`, `off`, `on` | -          | How the filesystem UUID is used for file handles.                                  |
| `verity`              | `off`, `on`, `require`      | -          | fs-verity digest checks for metacopy files.                                        |
| `metacopy`            | `on`, `off`                 | `off`      | Copy up metadata only. Denied by default.                                          |
| `userxattr`           | flag                        | -          | Store overlay metadata in `user.` instead of `trusted.` xattrs. Denied by default. |
| `nfs_export`          | `on`, `off`                 | -          | Make the merge exportable over NFS. Denied by default.                             |

The overlay defaults are the only combination under which the kernel docs sanction
editing lower layers at all. `metacopy` together with `userxattr` would let an
unprivileged writer forge overlay redirects on a lower layer, hence the denylist.

Some options cannot be set at all, by pods or admins: branch lists, `lowerdir`,
`upperdir`, `workdir`, `allow_other` and anything else that carries a path or shapes the
process. These are always computed on the node.

### Chart values

| Value                         | Default                                                         | Description                                                                                          |
| ----------------------------- | --------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `backend`                     | `mergerfs`                                                      | Merge backend of this release: `mergerfs` or `overlay`.                                              |
| `driverName`                  | `<backend>.csi.ruekov.eu`                                       | CSI driver name pods put in `volumes[].csi.driver`.                                                  |
| `nameOverride`                | `""`                                                            | Overrides the chart name in resource names.                                                          |
| `image.repository`            | `ghcr.io/ruakij/union-csi-driver`                               | Driver image.                                                                                        |
| `image.tag`                   | chart `appVersion`                                              | Driver image tag.                                                                                    |
| `image.pullPolicy`            | `IfNotPresent`                                                  | Driver image pull policy.                                                                            |
| `registrar.image.*`           | `registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.17.0` | `node-driver-registrar` image, same fields as `image`.                                               |
| `kubeletRootDir`              | `/var/lib/kubelet`                                              | The node's kubelet directory. MicroK8s uses `/var/snap/microk8s/common/var/lib/kubelet`.             |
| `hostRootMount`               | `true`                                                          | Bind-mount the host root into the driver container. Required for `hostPath` sources.                 |
| `hostRootDir`                 | `/host`                                                         | Where the host root is mounted inside the driver container.                                          |
| `publishTimeout`              | `30s`                                                           | How long a mount waits for its source volumes before failing and letting kubelet retry.              |
| `maxSourceVolumes`            | `32`                                                            | Maximum `sourceVolumes` entries per union volume.                                                    |
| `options.allowlist`           | `""`                                                            | Backend options pods may set. Empty allows every schema option that is not denied.                   |
| `options.denylist`            | `""` (backend default)                                          | Backend options pods may not set. Empty uses the backend's default denylist.                         |
| `options.denylistMode`        | `refuse`                                                        | What happens to a denied pod option: `refuse` fails the mount, `strip` drops the option and logs it. |
| `options.defaults`            | `""` (backend default)                                          | `key=value` options applied unless the pod sets them. Empty uses the backend defaults.               |
| `options.forced`              | `""`                                                            | `key=value` options applied last, overriding the pod.                                                |
| `mergerfs.daemonLifetime`     | `auto`                                                          | `auto` uses host systemd where present, `systemd` requires it, `in-container` never uses it.         |
| `logLevel`                    | `2`                                                             | klog verbosity of both containers.                                                                   |
| `rbac.create`                 | `true`                                                          | Create the ClusterRole and binding.                                                                  |
| `serviceAccount.create`       | `true`                                                          | Create the ServiceAccount.                                                                           |
| `serviceAccount.name`         | `""`                                                            | ServiceAccount to use; defaults to the release's full name.                                          |
| `priorityClassName`           | `system-node-critical`                                          | Priority class of the DaemonSet pods.                                                                |
| `tolerations`                 | tolerate everything                                             | DaemonSet tolerations.                                                                               |
| `nodeSelector`, `affinity`    | `{}`                                                            | DaemonSet scheduling constraints.                                                                    |
| `resources`                   | `{}`                                                            | Driver container resources.                                                                          |
| `podAnnotations`, `podLabels` | `{}`                                                            | Extra metadata on the DaemonSet pods.                                                                |
| `updateStrategy`              | `RollingUpdate`                                                 | DaemonSet update strategy.                                                                           |

Admin `defaults` and `forced` values bypass the allowlist and denylist but are checked
against the backend's option schema at startup, so a typo stops the DaemonSet instead of
failing every mount.

## Build

```sh
go build ./cmd/union-csi-driver
docker build -t union-csi-driver .
helm template test charts/union-csi-driver
```

Mount code is Linux-only; on a non-Linux machine, build and vet with `GOOS=linux`.
