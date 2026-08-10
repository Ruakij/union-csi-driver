# union-csi-driver

A Kubernetes CSI driver that unions several sibling volumes already declared on a pod
into a single merged mount point, using either the kernels `overlay` filesystem or
`mergerfs` as the merge backend.

It does not proxy or re-implement other CSI drivers. It waits for kubelet to publish the
sibling volumes a pod already declares, then unions their node-local publish paths at its
own target path.

## Install

One release runs one backend. Installing both means installing the chart twice.

```sh
helm install mergerfs-csi charts/union-csi-driver --set backend=mergerfs
helm install overlay-csi  charts/union-csi-driver --set backend=overlay
```

The driver name defaults to `<backend>.csi.example.io`, and that is what pods put in
`volumes[].csi.driver`. If the cluster is k3s, RKE2 or MicroK8s, set `kubeletRootDir`
to the node's real kubelet directory.

## Use

```yaml
volumes:
  - name: data
    persistentVolumeClaim: {claimName: data}
  - name: archive
    persistentVolumeClaim: {claimName: archive}
  - name: merged
    csi:
      driver: mergerfs.csi.example.io
      volumeAttributes:
        sourceVolumes: "data=RW,archive=RO"
```

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
