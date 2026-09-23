// Package e2e installs the chart once per backend into a kind cluster and checks
// the merged view from real pods. The tests are behind the e2e build tag; run them
// with make e2e. E2E_KEEP=1 leaves the cluster running, E2E_SKIP_BUILD=1 reuses the
// last built union-csi-driver:e2e.
package e2e
