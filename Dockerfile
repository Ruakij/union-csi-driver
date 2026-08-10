# Copyright 2021 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Cross-compiling from the build platform, so a multi-arch build needs no
# emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETARCH
ARG version=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags "-X main.version=${version}" -o /out/union-csi-driver ./cmd/union-csi-driver

# Alpine has no mergerfs package, so take the upstream static build. Pinned by
# digest because it is fetched from outside the package manager.
FROM alpine AS mergerfs
ARG TARGETARCH
ARG MERGERFS_VERSION=2.42.0
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) sha=0cf8692e1687c8a1140c714966c6f5f4b498a1537f1a0bef5665082ecb35fc12 ;; \
      arm64) sha=da318afbf109f025a41e9be86de5ebbdfb879546abf4cb5176c8c15881b7cf05 ;; \
      *) echo "no mergerfs static build pinned for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    apk add --no-cache curl; \
    curl -fsSL -o /tmp/mergerfs.tar.gz \
      "https://github.com/trapexit/mergerfs/releases/download/${MERGERFS_VERSION}/mergerfs-${MERGERFS_VERSION}-static-linux_${TARGETARCH}.tar.gz"; \
    echo "${sha}  /tmp/mergerfs.tar.gz" | sha256sum -c -; \
    mkdir /mergerfs; \
    tar -xzf /tmp/mergerfs.tar.gz -C /mergerfs

FROM alpine
LABEL description="union-csi-driver"

# mergerfs and fusermount ship in every image regardless of --backend: the
# container is privileged either way, so a binary the overlay backend never execs
# is not meaningful attack surface, and it keeps this to one build lane.
RUN apk add --no-cache util-linux coreutils fuse && apk update && apk upgrade
COPY --from=mergerfs /mergerfs/usr/local/bin/mergerfs /usr/local/bin/mergerfs
COPY --from=build /out/union-csi-driver /union-csi-driver
ENTRYPOINT ["/union-csi-driver"]
