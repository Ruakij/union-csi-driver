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
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
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
COPY hack/mergerfs.pin /tmp/mergerfs.pin
RUN set -eu; \
    version=$(sed -n 's/^version=//p' /tmp/mergerfs.pin); \
    sha=$(sed -n "s/^${TARGETARCH}=//p" /tmp/mergerfs.pin); \
    [ -n "${version}" ] && [ -n "${sha}" ] || \
      { echo "no mergerfs build pinned for ${TARGETARCH}, run make update-mergerfs" >&2; exit 1; }; \
    apk add --no-cache curl; \
    curl -fsSL -o /tmp/mergerfs.tar.gz \
      "https://github.com/trapexit/mergerfs/releases/download/${version}/mergerfs-${version}-static-linux_${TARGETARCH}.tar.gz"; \
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
