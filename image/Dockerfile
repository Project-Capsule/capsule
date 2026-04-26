# Stage 1: build capsuled for linux/amd64 (static, no CGO).
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CAPSULE_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.version=${CAPSULE_VERSION}" \
      -o /out/capsuled ./cmd/capsuled
# capsule-guest: PID 1 inside image-mode microVMs. Static, tiny, injected
# into the per-VM ext4 rootfs by the firecracker driver at apply time.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
      -o /out/capsule-guest ./cmd/capsule-guest

# Stage 2: assemble the Capsule rootfs. Alpine provides userland; capsuled is /sbin/init.
FROM alpine:3.23 AS rootfs
RUN apk add --no-cache \
      iproute2 \
      iptables \
      ca-certificates tzdata \
      containerd runc \
      # linux-lts brings /lib/modules/<kver>/ — the matching module tree for
      # the kernel installed in /boot by Dockerfile.packer. We use linux-lts
      # (not linux-virt) so the same kernel works on real hardware (Beelink
      # NVMe / NICs / USB) and under QEMU. Required: containerd's overlayfs
      # snapshotter, bridge networking, KVM all need modules from here.
      linux-lts \
      # CNI plugins for bridge networking + portmap + firewall.
      cni-plugins \
      # mkfs.ext4 + resize2fs run at build time (vm-shared.ext4) and at
      # runtime (user volume LV format / grow). e2fsprogs-extra ships
      # `debugfs` which we use to inject device nodes into the ext4 at
      # build time (overlayfs can't mknod).
      e2fsprogs e2fsprogs-extra \
      # LVM2 + thin-provisioning-tools back the storage layer: /perm is a
      # plain LV in VG "capsule"; user volumes and containerd devmapper
      # snapshots share a thin pool in the same VG. Without these the boot
      # path can't activate the VG and volume ops fail.
      lvm2 lvm2-dmeventd device-mapper \
      thin-provisioning-tools
# Alpine ships CNI plugins at /usr/libexec/cni; CNI's default lookup is
# /opt/cni/bin. Symlink so standard tooling (go-cni) finds them.
RUN mkdir -p /opt/cni && ln -sf /usr/libexec/cni /opt/cni/bin
# Firecracker for MicroVM workloads. Alpine's testing repo doesn't
# reliably ship it, so pull a static release binary from upstream.
ARG FC_VERSION=v1.10.1
RUN set -eux; \
    wget -q "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz" -O /tmp/fc.tgz; \
    tar -xzf /tmp/fc.tgz -C /tmp; \
    install -m 0755 "/tmp/release-${FC_VERSION}-x86_64/firecracker-${FC_VERSION}-x86_64" /usr/bin/firecracker; \
    install -m 0755 "/tmp/release-${FC_VERSION}-x86_64/jailer-${FC_VERSION}-x86_64"       /usr/bin/jailer; \
    rm -rf /tmp/fc.tgz "/tmp/release-${FC_VERSION}-x86_64"
# Bundled Firecracker-compatible vmlinux. We use the Firecracker CI kernel
# (6.1.128) instead of the older quickstart kernel (4.14, 2021) because
# runc 1.2+ requires CONFIG_CGROUP_BPF=y to set up the cgroup v2 device
# controller — missing in the quickstart kernel. The CI build has
# CGROUP_BPF, CGROUP_DEVICE, VSOCKETS, VIRTIO_VSOCKETS, DEVTMPFS,
# UNIX98_PTYS, plus all the virtio block/net drivers we need.
#
# Source: https://github.com/firecracker-microvm/firecracker/blob/main/docs/kernel-policy.md
ARG VMLINUX_VERSION=6.1.128
ARG VMLINUX_URL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.12/x86_64/vmlinux-${VMLINUX_VERSION}
RUN set -eux; \
    mkdir -p /usr/share/capsule; \
    wget -q "${VMLINUX_URL}" -O /usr/share/capsule/vmlinux; \
    chmod 0644 /usr/share/capsule/vmlinux

# Bundled runc for running the OCI payload inside the microVM. capsule-guest
# shells out to this binary for StartPayload/Exec/Stop. The static release
# from upstream is what we want — no CGO, no dynamic libs.
ARG RUNC_VERSION=v1.2.6
ARG RUNC_URL=https://github.com/opencontainers/runc/releases/download/${RUNC_VERSION}/runc.amd64
RUN set -eux; \
    wget -q "${RUNC_URL}" -O /usr/share/capsule/runc; \
    chmod 0755 /usr/share/capsule/runc
# Strip things we never use, in-place, to keep the squashfs small.
RUN rm -rf /var/cache/apk /usr/share/doc /usr/share/man /usr/share/locale \
           /usr/share/info /etc/ssh
COPY image/etc/ /etc/
RUN mkdir -p /run/cni/ipam /opt/cni
# Pre-create /perm and runtime state dirs so capsuled can mount onto them
# even when rootfs is read-only.
RUN mkdir -p /perm /run/containerd /run/capsule/vms /run/capsule/workloads
# Docker buildkit zeroes /etc/hostname and /etc/resolv.conf in the final
# image (they're bind-mounted at container runtime). Ship the Capsule copies
# at /etc/capsule/ so they survive into the rootfs tarball.
RUN mkdir -p /etc/capsule
COPY image/etc/hostname    /etc/capsule/hostname
COPY image/etc/resolv.conf /etc/capsule/resolv.conf
# containerd config: root lives on /perm (survives A/B updates), state on
# tmpfs (tasks don't survive reboot anyway).
RUN mkdir -p /etc/containerd
COPY image/etc/containerd-config.toml /etc/containerd/config.toml
# Alpine ships /sbin/init as a symlink to /bin/busybox. If we COPY through
# that symlink we overwrite busybox — and with it every shell utility.
# Remove the symlink first so the COPY creates a regular file.
RUN rm -f /sbin/init
COPY --from=build /out/capsuled /sbin/init
# capsule-guest is the microVM PID 1. Lives both as a standalone binary (kept
# around for debugging and, in the future, live update flows) AND packed
# into /usr/share/capsule/vm-shared.ext4 below as /sbin/init.
COPY --from=build /out/capsule-guest /usr/share/capsule/capsule-guest
RUN chmod 0755 /sbin/init /usr/share/capsule/capsule-guest

# Stage 3: the shared microVM rootfs. Every Capsule-managed VM boots from
# this same ext4. It contains:
#   - busybox for a minimal userland + debug shell
#   - capsule-guest as /sbin/init (vsock gRPC agent, runs runc for payload)
#   - runc at /usr/bin/runc
#   - the minimal /dev device nodes the kernel expects BEFORE devtmpfs is
#     mounted — without /dev/console the kernel can't connect init's
#     stdio and you get a silent hang. devtmpfs fills in the rest at
#     boot, but these have to already be there when the kernel exec's init.
# The user's OCI payload is attached separately as /dev/vdb and mounted
# by capsule-guest on demand.
FROM alpine:3.23 AS vmroot
RUN rm -rf /var/cache/apk /usr/share/doc /usr/share/man /usr/share/locale \
           /usr/share/info /etc/ssh /etc/init.d /etc/runlevels /etc/conf.d \
           /etc/network /etc/crontabs /media /mnt
RUN rm -f /sbin/init
COPY --from=build /out/capsule-guest /sbin/init
COPY --from=rootfs /usr/share/capsule/runc /usr/bin/runc
RUN chmod 0755 /sbin/init /usr/bin/runc && \
    mkdir -p /oci /proc /sys /dev /run /tmp /sys/fs/cgroup

# Back in the main rootfs: pack the vmroot tree into a single ext4 image
# that the firecracker driver passes as the root block device for every
# microVM. 128 MiB is plenty for busybox + capsule-guest + mount points.
#
# Device nodes are created via `debugfs` AFTER mkfs, because `mknod` for
# character devices fails over Docker's overlayfs (no CAP_MKNOD). debugfs
# edits the ext4 image file directly — no mount, no privilege required.
FROM rootfs AS rootfs-final
COPY --from=vmroot / /tmp/vmroot/
RUN truncate -s 128M /usr/share/capsule/vm-shared.ext4 && \
    mkfs.ext4 -q -F -d /tmp/vmroot /usr/share/capsule/vm-shared.ext4 && \
    rm -rf /tmp/vmroot && \
    printf '%s\n' \
      "cd /dev" \
      "mknod console c 5 1" \
      "mknod null c 1 3" \
      "mknod zero c 1 5" \
      "mknod full c 1 7" \
      "mknod tty c 5 0" \
      "mknod random c 1 8" \
      "mknod urandom c 1 9" \
      | debugfs -w /usr/share/capsule/vm-shared.ext4 >/dev/null
