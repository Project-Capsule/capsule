SHELL := /bin/bash
BUILD_DIR := build
DISK_IMAGE := $(BUILD_DIR)/disk.raw
UPDATE_BUNDLE := $(BUILD_DIR)/update.tar

.PHONY: all proto tools capsuled capsulectl image update-bundle qemu clean test

all: image

tools:
	@command -v buf >/dev/null || (echo "install buf: brew install bufbuild/buf/buf" && exit 1)
	@command -v protoc-gen-go >/dev/null || go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@command -v protoc-gen-go-grpc >/dev/null || go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

proto: tools
	buf generate

# Cross-compile capsuled for the capsule target platform (linux/amd64).
capsuled:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o $(BUILD_DIR)/capsuled ./cmd/capsuled

# Build capsulectl for the host (operator's laptop).
capsulectl:
	go build -trimpath -ldflags='-s -w' -o $(BUILD_DIR)/capsulectl ./cmd/capsulectl

test:
	go test ./...

# Build the full Capsule image: rootfs Docker image -> squashfs -> bootable
# disk + streaming update bundle. pack.sh writes both build/disk.raw and
# build/update.tar in a single pass.
image:
	bash image/build.sh

# Build only the streaming update bundle — skip disk.raw assembly. Use this
# for iteration on a running capsule (push + reboot). Full-image build still
# happens via `make image` for fresh installs.
update-bundle:
	BUNDLE_ONLY=1 bash image/build.sh
	@test -f $(UPDATE_BUNDLE) || (echo "missing $(UPDATE_BUNDLE)"; exit 1)
	@ls -lh $(UPDATE_BUNDLE)

# Phase 0 QEMU boot: BIOS (SeaBIOS default), virtio disk + net, serial console on
# host stdio. KVM on Linux hosts accelerates; on macOS this falls back to TCG
# (software) — slow but sufficient for smoke tests.
qemu: $(DISK_IMAGE)
	qemu-system-x86_64 \
	  -m 2G -smp 2 \
	  -drive if=virtio,format=raw,file=$(DISK_IMAGE) \
	  -netdev user,id=n0,hostfwd=tcp::50000-:50000 \
	  -device virtio-net-pci,netdev=n0 \
	  -device virtio-rng-pci \
	  -serial mon:stdio \
	  -nographic

$(DISK_IMAGE):
	$(MAKE) image

clean:
	rm -rf $(BUILD_DIR)
