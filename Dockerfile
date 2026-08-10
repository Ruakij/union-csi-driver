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

FROM alpine
LABEL description="union-csi-driver"

# mergerfs and fusermount ship in every image regardless of --backend: the
# container is privileged either way, so a binary the overlay backend never execs
# is not meaningful attack surface, and it keeps this to one build lane.
RUN apk add --no-cache util-linux coreutils mergerfs fuse && apk update && apk upgrade
COPY --from=build /out/union-csi-driver /union-csi-driver
ENTRYPOINT ["/union-csi-driver"]
