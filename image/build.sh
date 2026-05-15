#!/bin/bash
# build.sh — build the Capsule rootfs Docker image, export it as a tarball,
# and run the packer container to produce build/rootfs.sqsh and build/disk.raw.
#
# Designed to run from a macOS/Linux host with Docker Desktop. No root or
# loop devices required on the host.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build"
ROOTFS_TAG="capsule/rootfs:latest"
PACKER_TAG="capsule/packer:latest"
CAPSULE_VERSION="${CAPSULE_VERSION:-$(date +%Y%m%d-%H%M%S)}"

# LINUX_LTS_VERSION is the single source of truth for the kernel
# version. Both Dockerfile (rootfs /lib/modules/<kver>/) and
# Dockerfile.packer (/boot/vmlinuz) must pin to exactly the same value
# — drift means the booted kernel can't find its modules and every
# modprobe silently fails (no LVM, no NIC, etc.). The Dockerfiles
# default the ARG to this same value as a safety net for direct
# `docker build`, but build.sh overrides it for every CI / make build.
# Bump this when Alpine deletes the current version from their mirror;
# verify the new version exists at:
#   https://dl-cdn.alpinelinux.org/alpine/v3.23/main/x86_64/
LINUX_LTS_VERSION="${LINUX_LTS_VERSION:-6.18.29-r0}"

mkdir -p "$BUILD_DIR"

echo "==> Building $ROOTFS_TAG (linux/amd64, CAPSULE_VERSION=$CAPSULE_VERSION, LINUX_LTS_VERSION=$LINUX_LTS_VERSION)"
docker buildx build \
  --platform=linux/amd64 \
  --build-arg "CAPSULE_VERSION=$CAPSULE_VERSION" \
  --build-arg "LINUX_LTS_VERSION=$LINUX_LTS_VERSION" \
  -f "$REPO_ROOT/image/Dockerfile" \
  -t "$ROOTFS_TAG" \
  --load \
  "$REPO_ROOT"

echo "==> Exporting rootfs tarball"
CID=$(docker create --platform=linux/amd64 "$ROOTFS_TAG" /bin/true)
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true' EXIT
docker export "$CID" > "$BUILD_DIR/rootfs.tar"
docker rm -f "$CID" >/dev/null
trap - EXIT

echo "==> Building $PACKER_TAG (linux/amd64, LINUX_LTS_VERSION=$LINUX_LTS_VERSION)"
docker buildx build \
  --platform=linux/amd64 \
  --build-arg "LINUX_LTS_VERSION=$LINUX_LTS_VERSION" \
  -f "$REPO_ROOT/image/Dockerfile.packer" \
  -t "$PACKER_TAG" \
  --load \
  "$REPO_ROOT"

echo "==> Running packer"
docker run --rm \
  --platform=linux/amd64 \
  -v "$BUILD_DIR:/work" \
  -e "BUNDLE_ONLY=${BUNDLE_ONLY:-0}" \
  -e "CAPSULE_VERSION=${CAPSULE_VERSION}" \
  "$PACKER_TAG"

echo "==> Done."
if [ "${BUNDLE_ONLY:-0}" = "1" ]; then
  ls -lh "$BUILD_DIR/rootfs.sqsh" "$BUILD_DIR/update.tar"
else
  ls -lh "$BUILD_DIR/rootfs.sqsh" "$BUILD_DIR/update.tar" "$BUILD_DIR/disk.raw"
fi
