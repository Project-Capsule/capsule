#!/bin/bash
# pack.sh — runs inside the packer container.
#
# Inputs (bind-mounted /work):
#   /work/rootfs.tar    Capsule rootfs as a tar (from `docker export`).
#
# Outputs (written to /work):
#   /work/rootfs.sqsh   squashfs of the rootfs (raw block written to both
#                       slots; mounted ro by the initramfs and stacked under
#                       a tmpfs overlay).
#   /work/update.tar    Streaming update bundle (VERSION + vmlinuz +
#                       initramfs + rootfs.sqsh) — pushed via
#                       `capsulectl capsule update push`.
#   /work/disk.raw      Bootable raw disk image (normal: 4 partitions;
#                       installer: 2 partitions, no PERM, single slot).
#
# Normal partition layout (A/B runtime image):
#   1. FAT32     CAPSULEBOOT  256 MiB        kernels + initramfs (per slot) + grubx64.efi
#   2. raw       SLOT_A       SLOT_SIZE_MIB  squashfs (active rootfs by default)
#   3. raw       SLOT_B       SLOT_SIZE_MIB  squashfs (seeded identical to A on first build)
#   4. type 8e   PERM         PERM_SIZE_MIB  LVM PV (capsule VG: meta LV + thinpool)
#
# Installer partition layout (INSTALLER_IMAGE=1):
#   1. FAT32     CAPSULEBOOT  256 MiB        kernel + initramfs + grubx64.efi
#   2. raw       SLOT         SLOT_SIZE_MIB  squashfs (single slot, no B)
#   No PERM partition — the installer has no persistent state.
#   Kernel cmdline includes capsule.mode=installer so capsuled enters
#   installer mode unconditionally without relying on sysfs removable.
#
# MBR disk-id is fixed at 0xb1a570ff (BLASTOFF) so PARTUUIDs are stable across
# rebuilds. Kernel cmdline references the slot by PARTUUID, not /dev path, so
# Beelink NVMe and QEMU virtio both resolve cleanly via /dev/disk/by-partuuid/.
#
# Slot identity is communicated to capsuled via `capsule.slot=a|b` on the
# kernel cmdline (parsed by detectActiveSlot in boot/boot_linux.go).

set -euo pipefail

WORK=/work
ROOTFS_TAR="$WORK/rootfs.tar"
SQSH="$WORK/rootfs.sqsh"
DISK="$WORK/disk.raw"
INSTALLER_DISK="$WORK/installer.raw"
BOOT_IMG="$WORK/boot.fat"
PERM_IMG="$WORK/perm.ext4"
UPDATE_TAR="$WORK/update.tar"

# Set INSTALLER_IMAGE=1 (via `make installer`) to produce a dedicated
# installer image: single rootfs slot, no PERM partition, cmdline flag.
INSTALLER_IMAGE="${INSTALLER_IMAGE:-0}"

PERM_SIZE_MIB="${PERM_SIZE_MIB:-2048}"
# Slot size is FIXED, not dynamic. Once a capsule is installed, slot
# partition offsets are frozen — growing them would shift PERM and
# destroy the LVM PV. So pick a size with enough headroom for the next
# several years of OS growth on day one. 2 GiB matches Talos's per-side
# budget (after they bumped from 1 GiB in 1.11) and gives ~75% headroom
# over today's ~1.1 GiB squashfs. To override at build time:
#   SLOT_SIZE_MIB=3072 make image
SLOT_SIZE_MIB="${SLOT_SIZE_MIB:-2048}"
# Runtime disk ID (BLASTOFF). Installer uses a different ID so the two images
# don't produce conflicting PARTUUID=b1a570ff-02 entries when both are visible
# to the kernel simultaneously — without distinct IDs the kernel may boot the
# installed NVMe squashfs instead of the USB installer squashfs.
DISK_SIG_HEX="b1a570ff"
INSTALLER_DISK_SIG_HEX="b005ab1e"

if [ "$INSTALLER_IMAGE" = "1" ]; then
  DISK_SIG_HEX="$INSTALLER_DISK_SIG_HEX"
fi

[ -f "$ROOTFS_TAR" ] || { echo "pack.sh: missing $ROOTFS_TAR"; exit 1; }

# ---- 1. extract rootfs, produce squashfs (the rootfs image, also the bundle's payload) ----
rm -rf /tmp/rootfs && mkdir -p /tmp/rootfs
tar -C /tmp/rootfs -xf "$ROOTFS_TAR"
[ -x /tmp/rootfs/sbin/init ] || { echo "pack.sh: /sbin/init missing"; exit 1; }

rm -f "$SQSH"
mksquashfs /tmp/rootfs "$SQSH" -noappend -comp zstd -all-root -quiet
SQSH_BYTES=$(stat -c%s "$SQSH")
echo "pack.sh: squashfs size = ${SQSH_BYTES} bytes"

# ---- 2. compose the update bundle ------------------------------------------
# update.tar contains everything an OS update push needs: VERSION (a single
# build identifier line), the kernel + initramfs, and the squashfs rootfs.
# Capsuled (in a future commit) will receive this stream, dd the squashfs
# to the inactive slot, and write per-slot kernels into CAPSULEBOOT.
VERSION="${CAPSULE_VERSION:-$(date +%Y%m%d-%H%M%S)}"
mkdir -p /tmp/bundle
echo "$VERSION"             > /tmp/bundle/VERSION
cp /bootfiles/vmlinuz         /tmp/bundle/vmlinuz
cp /bootfiles/initramfs       /tmp/bundle/initramfs
cp "$SQSH"                    /tmp/bundle/rootfs.sqsh
( cd /tmp/bundle && tar -cf "$UPDATE_TAR" VERSION vmlinuz initramfs rootfs.sqsh )
echo "pack.sh: update bundle = $UPDATE_TAR ($(stat -c%s "$UPDATE_TAR") bytes, version=$VERSION)"

# update-bundle path: skip disk.raw assembly. BUNDLE_ONLY=1 is set by
# `make update-bundle` for fast iteration on a running capsule.
# INSTALLER_IMAGE=1 also skips the normal disk.raw (builds installer.raw instead).
if [ "${BUNDLE_ONLY:-0}" = "1" ]; then
  echo "pack.sh: BUNDLE_ONLY=1 — skipping disk.raw assembly"
  exit 0
fi

if [ "$INSTALLER_IMAGE" = "1" ]; then
  DISK="$INSTALLER_DISK"
fi

# ---- 3. validate squashfs fits in the fixed slot size ----------------------
# Slot size is fixed (see SLOT_SIZE_MIB at the top). If the squashfs grew
# past the slot, fail loud at build time — this is a hard ceiling, not a
# warning, because every future update.tar built from this image will
# refuse to apply on existing capsules with smaller slots.
SLOT_MIB="$SLOT_SIZE_MIB"
SLOT_BYTES=$(( SLOT_MIB * 1024 * 1024 ))
if [ "$SQSH_BYTES" -gt "$SLOT_BYTES" ]; then
  echo "pack.sh: ERROR squashfs ${SQSH_BYTES} bytes > slot ${SLOT_BYTES} bytes (${SLOT_MIB} MiB)"
  echo "pack.sh: bump SLOT_SIZE_MIB or shrink the rootfs"
  exit 1
fi
echo "pack.sh: slot partition size = ${SLOT_MIB} MiB each (squashfs ${SQSH_BYTES} bytes, $(( 100 * SQSH_BYTES / SLOT_BYTES ))% full)"

# ---- 4. PERM partition (normal image only) ---------------------------------
# PERM holds the LVM PV for VG "capsule": meta LV (mounted at /perm) +
# thinpool LV (user volumes + future containerd devmapper snapshots).
# Installer images have no PERM — the installer has no persistent state.
#
# Strategy: if an existing disk.raw is present, preserve the PERM bytes so
# capsule state + volumes survive a rebuild.
if [ "$INSTALLER_IMAGE" != "1" ]; then
  PERM_BYTES=$(( PERM_SIZE_MIB * 1024 * 1024 ))
  if [ -f "$WORK/disk.raw" ]; then
    existing_line=$(sfdisk -d "$WORK/disk.raw" 2>/dev/null \
                   | grep -E "$WORK/disk.raw[0-9]+ : " \
                   | tail -1 || true)
    existing_start=$(echo "$existing_line" | sed -n 's/.*start=[[:space:]]*\([0-9][0-9]*\).*/\1/p')
    existing_size=$(echo "$existing_line"  | sed -n 's/.*size=[[:space:]]*\([0-9][0-9]*\).*/\1/p')
    if [ -n "$existing_start" ] && [ -n "$existing_size" ]; then
      echo "pack.sh: preserving PERM from existing disk.raw (start=$existing_start sectors, size=$existing_size sectors)"
      rm -f "$PERM_IMG"
      dd if="$WORK/disk.raw" of="$PERM_IMG" bs=512 skip="$existing_start" count="$existing_size" status=none
      if [ "$(stat -c%s "$PERM_IMG" 2>/dev/null)" != "$PERM_BYTES" ]; then
        echo "pack.sh: perm size changed (now ${PERM_SIZE_MIB} MiB); recreating"
        rm -f "$PERM_IMG"
      fi
    fi
  fi
  if [ ! -f "$PERM_IMG" ]; then
    echo "pack.sh: creating fresh (unformatted) perm image (${PERM_SIZE_MIB} MiB); first boot will initialize LVM"
    truncate -s "$PERM_BYTES" "$PERM_IMG"
  fi
fi

# ---- 5. per-slot kernels + GRUB EFI in /boot ------------------------------
# CAPSULEBOOT is an EFI System Partition (FAT32, MBR type 0xEF). UEFI firmware
# boots /EFI/BOOT/BOOTX64.EFI by fallback — no NVRAM entry needed, so the
# image works on any machine without efibootmgr setup.
BOOT_SIZE_MIB=256
BOOT_BYTES=$(( BOOT_SIZE_MIB * 1024 * 1024 ))
rm -f "$BOOT_IMG"
truncate -s "$BOOT_BYTES" "$BOOT_IMG"
mkfs.fat -F 32 -n CAPSULEBOOT "$BOOT_IMG" >/dev/null

mcopy -i "$BOOT_IMG" /bootfiles/vmlinuz   ::/vmlinuz_a
mcopy -i "$BOOT_IMG" /bootfiles/initramfs ::/initramfs_a
if [ "$INSTALLER_IMAGE" != "1" ]; then
  # Runtime image: both slots seeded identically at first build.
  mcopy -i "$BOOT_IMG" /bootfiles/vmlinuz   ::/vmlinuz_b
  mcopy -i "$BOOT_IMG" /bootfiles/initramfs ::/initramfs_b
fi

mkdir -p /tmp/grub-stage
cat >/tmp/grub-stage/embedded.cfg <<EOF
search --no-floppy --label CAPSULEBOOT --set=root
configfile /EFI/BOOT/grub.cfg
EOF
grub-mkstandalone \
    --format=x86_64-efi \
    --output=/tmp/grub-stage/grubx64.efi \
    --modules="part_msdos fat normal linux configfile all_video efi_gop efi_uga search search_label echo" \
    "boot/grub/grub.cfg=/tmp/grub-stage/embedded.cfg"

if [ "$INSTALLER_IMAGE" = "1" ]; then
  # Installer: single slot, capsule.mode=installer flag in cmdline so
  # capsuled enters installer mode unconditionally (no sysfs removable check).
  cat >/tmp/grub-stage/grub.cfg <<EOF
set timeout=2
set default=0

menuentry "Capsule Installer" {
    linux /vmlinuz_a root=PARTUUID=${DISK_SIG_HEX}-02 rootfstype=squashfs ro random.trust_cpu=on capsule.slot=a capsule.mode=installer console=tty0 console=ttyS0,115200n8 panic=10
    initrd /initramfs_a
}
EOF
else
  # Runtime image: A/B slots, timeout=2 for slot selection.
  cat >/tmp/grub-stage/grub.cfg <<EOF
set timeout=2
set default=0

menuentry "Capsule (slot_a)" {
    linux /vmlinuz_a root=PARTUUID=${DISK_SIG_HEX}-02 rootfstype=squashfs ro random.trust_cpu=on capsule.slot=a console=tty0 console=ttyS0,115200n8 panic=10
    initrd /initramfs_a
}

menuentry "Capsule (slot_b)" {
    linux /vmlinuz_b root=PARTUUID=${DISK_SIG_HEX}-03 rootfstype=squashfs ro random.trust_cpu=on capsule.slot=b console=tty0 console=ttyS0,115200n8 panic=10
    initrd /initramfs_b
}
EOF
fi

mmd    -i "$BOOT_IMG" ::/EFI               || true
mmd    -i "$BOOT_IMG" ::/EFI/BOOT          || true
mcopy -i "$BOOT_IMG" /tmp/grub-stage/grubx64.efi ::/EFI/BOOT/BOOTX64.EFI
mcopy -i "$BOOT_IMG" /tmp/grub-stage/grub.cfg    ::/EFI/BOOT/grub.cfg

# ---- 6. build the raw disk -------------------------------------------------
BOOT_START=2048
BOOT_SECTORS=$(( BOOT_SIZE_MIB * 1024 * 2 ))
SLOT_A_START=$(( BOOT_START + BOOT_SECTORS ))
SLOT_A_SECTORS=$(( SLOT_MIB * 1024 * 2 ))

rm -f "$DISK"

if [ "$INSTALLER_IMAGE" = "1" ]; then
  # Installer: 2 partitions — EFI + single rootfs slot. No PERM.
  TOTAL_MIB=$(( 1 + BOOT_SIZE_MIB + SLOT_MIB + 4 ))
  TOTAL_BYTES=$(( TOTAL_MIB * 1024 * 1024 ))
  truncate -s "$TOTAL_BYTES" "$DISK"
  sfdisk "$DISK" <<EOF
label: dos
label-id: 0x${DISK_SIG_HEX}
unit: sectors
${DISK}1 : start=$BOOT_START,   size=$BOOT_SECTORS,   type=ef, bootable
${DISK}2 : start=$SLOT_A_START, size=$SLOT_A_SECTORS, type=83
EOF
  dd if="$BOOT_IMG" of="$DISK" bs=512 seek="$BOOT_START" count="$BOOT_SECTORS" conv=notrunc status=none
  dd if="$SQSH"     of="$DISK" bs=512 seek="$SLOT_A_START"                      conv=notrunc status=none
  echo "pack.sh: wrote $DISK ($TOTAL_MIB MiB) — installer image (single slot, no PERM)"
else
  # Runtime: 4 partitions — EFI + slot_a + slot_b + PERM.
  SLOT_B_START=$(( SLOT_A_START + SLOT_A_SECTORS ))
  SLOT_B_SECTORS=$SLOT_A_SECTORS
  PERM_START=$(( SLOT_B_START + SLOT_B_SECTORS ))
  PERM_SECTORS=$(( PERM_SIZE_MIB * 1024 * 2 ))
  TOTAL_MIB=$(( 1 + BOOT_SIZE_MIB + 2 * SLOT_MIB + PERM_SIZE_MIB + 4 ))
  TOTAL_BYTES=$(( TOTAL_MIB * 1024 * 1024 ))
  truncate -s "$TOTAL_BYTES" "$DISK"
  sfdisk "$DISK" <<EOF
label: dos
label-id: 0x${DISK_SIG_HEX}
unit: sectors
${DISK}1 : start=$BOOT_START,   size=$BOOT_SECTORS,   type=ef, bootable
${DISK}2 : start=$SLOT_A_START, size=$SLOT_A_SECTORS, type=83
${DISK}3 : start=$SLOT_B_START, size=$SLOT_B_SECTORS, type=83
${DISK}4 : start=$PERM_START,   size=$PERM_SECTORS,   type=8e
EOF
  dd if="$BOOT_IMG" of="$DISK" bs=512 seek="$BOOT_START"   count="$BOOT_SECTORS" conv=notrunc status=none
  dd if="$SQSH"     of="$DISK" bs=512 seek="$SLOT_A_START"                       conv=notrunc status=none
  dd if="$SQSH"     of="$DISK" bs=512 seek="$SLOT_B_START"                       conv=notrunc status=none
  dd if="$PERM_IMG" of="$DISK" bs=512 seek="$PERM_START"   count="$PERM_SECTORS" conv=notrunc status=none
  echo "pack.sh: wrote $DISK ($TOTAL_MIB MiB) — slot_a + slot_b seeded identically"
fi
