SHELL := /bin/bash
BUILD_DIR := build
DISK_IMAGE := $(BUILD_DIR)/disk.raw
UPDATE_BUNDLE := $(BUILD_DIR)/update.tar

.PHONY: all proto tools capsuled capsulectl image installer update-bundle qemu clean test

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

# Build the installer image: single rootfs slot, no PERM partition, ~3 GiB
# smaller than disk.raw. Writes build/installer.raw. Flash to USB with dd.
installer:
	INSTALLER_IMAGE=1 bash image/build.sh

# Build only the streaming update bundle — skip disk.raw assembly. Use this
# for iteration on a running capsule (push + reboot). Full-image build still
# happens via `make image` for fresh installs.
update-bundle:
	BUNDLE_ONLY=1 bash image/build.sh
	@test -f $(UPDATE_BUNDLE) || (echo "missing $(UPDATE_BUNDLE)"; exit 1)
	@ls -lh $(UPDATE_BUNDLE)

# UEFI QEMU boot via OVMF/edk2 firmware (pflash). brew ships the x86_64 code
# blob and a shared i386 vars blob (compatible with x86_64 firmware). Override
# these paths if your firmware lives elsewhere.
OVMF_CODE ?= /opt/homebrew/share/qemu/edk2-x86_64-code.fd
OVMF_VARS ?= /opt/homebrew/share/qemu/edk2-i386-vars.fd
EFI_VARS  := $(BUILD_DIR)/efi-vars.fd

$(EFI_VARS): $(OVMF_VARS)
	cp $(OVMF_VARS) $(EFI_VARS)

qemu: $(DISK_IMAGE) $(EFI_VARS)
	qemu-system-x86_64 \
	  -m 2G -smp 2 \
	  -drive if=pflash,format=raw,unit=0,readonly=on,file=$(OVMF_CODE) \
	  -drive if=pflash,format=raw,unit=1,file=$(EFI_VARS) \
	  -drive if=virtio,format=raw,file=$(DISK_IMAGE) \
	  -netdev user,id=n0,hostfwd=tcp::50000-:50000 \
	  -device virtio-net-pci,netdev=n0 \
	  -device virtio-rng-pci \
	  -serial mon:stdio \
	  -nographic

# qemu-installer: boots the disk image as a *USB stick* (removable) and
# attaches an empty 16 GiB scratch disk as the install target. This
# triggers boot.DetectInstallerMode -> installer mode, so `capsulectl
# install` can be driven end-to-end against a local VM.
#
# build/install-target.raw is created lazily; rm -f it to start fresh
# between install attempts.
INSTALL_TARGET := $(BUILD_DIR)/install-target.raw
$(INSTALL_TARGET):
	truncate -s 16G $(INSTALL_TARGET)

qemu-installer: $(DISK_IMAGE) $(INSTALL_TARGET) $(EFI_VARS)
	qemu-system-x86_64 \
	  -m 2G -smp 2 \
	  -drive if=pflash,format=raw,unit=0,readonly=on,file=$(OVMF_CODE) \
	  -drive if=pflash,format=raw,unit=1,file=$(EFI_VARS) \
	  -drive if=none,format=raw,id=usb-stick,file=$(DISK_IMAGE) \
	  -device usb-ehci,id=ehci \
	  -device usb-storage,bus=ehci.0,drive=usb-stick,removable=on,bootindex=1 \
	  -drive if=virtio,format=raw,file=$(INSTALL_TARGET),bootindex=2 \
	  -netdev user,id=n0,hostfwd=tcp::50000-:50000 \
	  -device virtio-net-pci,netdev=n0 \
	  -device virtio-rng-pci \
	  -serial mon:stdio \
	  -nographic

$(DISK_IMAGE):
	$(MAKE) image

clean:
	rm -rf $(BUILD_DIR)
