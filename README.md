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
unreferenced source never appears on the node. The driver rejects the union volume with
a clear error instead of waiting for a source that will never arrive.

`sourceVolumes` names volumes of the same pod. Leftmost wins on lookup, and the mode
suffix says whether writes may land there: `RW`, `RO`, or (mergerfs only) `NC`. A bare
name is `RW`. Setting `readOnly: true` on the CSI volume makes the whole merge
read-only regardless.

Backend options may be passed as further `volumeAttributes`, subject to the option
policy the admin configured on the DaemonSet. Path-bearing and process-shaping options
are not settable from a pod at all: they are computed by the driver.

### Backend differences that show up in the manifest

- **mergerfs** resolves every lookup across branches at request time, so branches may be
  edited out-of-band while mounted. Any number of branches may be `RW`. It has no
  copy-on-write: it cannot express read-only lowers plus one writable top layer.
- **overlay** is kernel-side, with no daemon, and survives a driver restart. It accepts
  at most one `RW` entry and it must be listed first, since the kernel always stacks the
  single upperdir on top. That entry's merged content lives in `<volume>/.union-csi/upper`,
  so data already at the root of the RW volume is not part of the merge. Editing a lower
  layer while mounted is undefined behaviour per the kernel docs.

## Build

```sh
go build ./cmd/union-csi-driver
docker build -t union-csi-driver .
helm template test charts/union-csi-driver
```

Mount code is Linux-only; on a non-Linux machine, build and vet with `GOOS=linux`.
