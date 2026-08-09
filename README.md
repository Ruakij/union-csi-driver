# union-csi-driver

A Kubernetes CSI driver that unions several sibling volumes already declared on a pod
into a single merged mount point, using either the kernels `overlay` filesystem or
`mergerfs` as the merge backend.

It does not proxy or re-implement other CSI drivers. It waits for kubelet to publish the
sibling volumes a pod already declares, then unions their node-local publish paths at its
own target path.
