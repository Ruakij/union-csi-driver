# union-csi-driver Helm repository

This branch holds nothing but the Helm chart index. chart-releaser writes to it on
every stable release; the source lives on `main`.

```sh
helm repo add union-csi https://ruakij.github.io/union-csi-driver
helm repo update
helm install mergerfs-csi union-csi/union-csi-driver --set backend=mergerfs
```
