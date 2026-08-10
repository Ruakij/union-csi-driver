# Copyright 2019 The Kubernetes Authors.
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

CMDS=union-csi-driver
all: build

include release-tools/build.make

# Mount-level tests need a real Linux kernel, CAP_SYS_ADMIN, /dev/fuse and the
# mergerfs binary, so they run in a privileged container. On a non-Linux machine
# that container is inside colima or another Linux VM.
MOUNTTEST_IMAGE ?= union-csi-mounttest:local
MOUNTTEST_PKGS ?= ./pkg/backend/...

.PHONY: test-mount-image
test-mount-image:
	docker build --target mergerfs -t union-csi-mergerfs:local .
	docker build -f hack/mounttest.Dockerfile --build-arg MERGERFS_IMAGE=union-csi-mergerfs:local -t $(MOUNTTEST_IMAGE) hack

.PHONY: test-mount
test-mount: test-mount-image
	docker run --rm --privileged -v $(CURDIR):/src -w /src -e GOFLAGS=-buildvcs=false \
		$(MOUNTTEST_IMAGE) go test -tags mounttest -count=1 $(MOUNTTEST_PKGS)
