# Test image for "make test-mount": a Go toolchain plus the same mergerfs build
# the driver image ships, taken from that image's fetch stage so the pinned
# version lives in one place.
ARG MERGERFS_IMAGE
FROM ${MERGERFS_IMAGE} AS mergerfs

FROM golang:1.27-alpine
RUN apk add --no-cache fuse
COPY --from=mergerfs /mergerfs/usr/local/bin/mergerfs /usr/local/bin/mergerfs
