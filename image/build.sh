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

mkdir -p "$BUILD_DIR"

echo "==> Building $ROOTFS_TAG (linux/amd64, CAPSULE_VERSION=$CAPSULE_VERSION)"
docker buildx build \
  --platform=linux/amd64 \
  --build-arg "CAPSULE_VERSION=$CAPSULE_VERSION" \
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

echo "==> Building $PACKER_TAG (linux/amd64)"
docker buildx build \
  --platform=linux/amd64 \
  -f "$REPO_ROOT/image/Dockerfile.packer" \
  -t "$PACKER_TAG" \
  --load \
  "$REPO_ROOT"

echo "==> Running packer"
docker run --rm \
  --platform=linux/amd64 \
  -v "$BUILD_DIR:/work" \
  "$PACKER_TAG"

echo "==> Done."
ls -lh "$BUILD_DIR/rootfs.sqsh" "$BUILD_DIR/disk.raw"
