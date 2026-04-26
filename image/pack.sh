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
#   /work/disk.raw      Bootable raw disk image (4 partitions, MBR).
#
# Partition layout (Phase 3 — A/B):
#   1. FAT32     CAPSULEBOOT  256 MiB        kernels + initramfs (per slot) + grubx64.efi
#   2. raw       SLOT_A       SLOT_SIZE_MIB  squashfs (active rootfs by default)
#   3. raw       SLOT_B       SLOT_SIZE_MIB  squashfs (seeded identical to A on first build)
#   4. type 8e   PERM         PERM_SIZE_MIB  LVM PV (capsule VG: meta LV + thinpool)
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
BOOT_IMG="$WORK/boot.fat"
PERM_IMG="$WORK/perm.ext4"
UPDATE_TAR="$WORK/update.tar"

PERM_SIZE_MIB="${PERM_SIZE_MIB:-2048}"
DISK_SIG_HEX="b1a570ff"

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

# update-bundle path: skip disk.raw assembly (sfdisk + boot.fat + dd of all
# four partitions). The Makefile's `update-bundle` target sets BUNDLE_ONLY=1
# so iteration on a running capsule (push the bundle, watch the reboot) is
# fast. Full-image build (`make image`) leaves it unset.
if [ "${BUNDLE_ONLY:-0}" = "1" ]; then
  echo "pack.sh: BUNDLE_ONLY=1 — skipping disk.raw assembly"
  exit 0
fi

# ---- 3. size the slot partitions to fit the squashfs + headroom ------------
# 50 MiB headroom so a future update whose squashfs grows a bit still fits
# without re-partitioning. Both slots are sized identically.
SLOT_BYTES=$(( SQSH_BYTES + 50 * 1024 * 1024 ))
SLOT_MIB=$(( (SLOT_BYTES + 1024*1024 - 1) / (1024*1024) ))
SLOT_BYTES=$(( SLOT_MIB * 1024 * 1024 ))
echo "pack.sh: slot partition size = ${SLOT_MIB} MiB each (squashfs inside)"

# ---- 4. PERM partition: preserve across rebuilds if possible --------------
# PERM holds the LVM PV for VG "capsule": meta LV (mounted at /perm) +
# thinpool LV (user volumes + future containerd devmapper snapshots).
#
# Strategy: if an existing disk.raw is present, find the highest-numbered
# partition (was p3 in the old 3-partition layout, is p4 in the new 4-partition
# layout — either way that's PERM) and dd those bytes into the new disk so
# capsule state + volumes survive.
PERM_BYTES=$(( PERM_SIZE_MIB * 1024 * 1024 ))

if [ -f "$DISK" ]; then
  existing_line=$(sfdisk -d "$DISK" 2>/dev/null \
                 | grep -E "$DISK[0-9]+ : " \
                 | tail -1 || true)
  existing_start=$(echo "$existing_line" | sed -n 's/.*start=[[:space:]]*\([0-9][0-9]*\).*/\1/p')
  existing_size=$(echo "$existing_line"  | sed -n 's/.*size=[[:space:]]*\([0-9][0-9]*\).*/\1/p')
  if [ -n "$existing_start" ] && [ -n "$existing_size" ]; then
    echo "pack.sh: preserving PERM from existing disk.raw (start=$existing_start sectors, size=$existing_size sectors)"
    rm -f "$PERM_IMG"
    dd if="$DISK" of="$PERM_IMG" bs=512 skip="$existing_start" count="$existing_size" status=none
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

# ---- 5. per-slot kernels + GRUB EFI in /boot ------------------------------
# CAPSULEBOOT is an EFI System Partition (FAT32, MBR type 0xEF). UEFI firmware
# boots /EFI/BOOT/BOOTX64.EFI by fallback — no NVRAM entry needed, so the
# image works on any machine without efibootmgr setup.
#
# We use GRUB EFI rather than syslinux-efi: Alpine's syslinux-efi handoff to
# the kernel hangs on some consumer firmwares (Beelink N150 confirmed) before
# the framebuffer console initializes. GRUB is more battle-tested across
# consumer UEFI implementations.
BOOT_SIZE_MIB=256
BOOT_BYTES=$(( BOOT_SIZE_MIB * 1024 * 1024 ))
rm -f "$BOOT_IMG"
truncate -s "$BOOT_BYTES" "$BOOT_IMG"
mkfs.fat -F 32 -n CAPSULEBOOT "$BOOT_IMG" >/dev/null

# Both slots get the same kernel + initramfs at first build. Updates rewrite
# only the inactive slot's pair (skip-if-unchanged compares hashes first).
mcopy -i "$BOOT_IMG" /bootfiles/vmlinuz   ::/vmlinuz_a
mcopy -i "$BOOT_IMG" /bootfiles/initramfs ::/initramfs_a
mcopy -i "$BOOT_IMG" /bootfiles/vmlinuz   ::/vmlinuz_b
mcopy -i "$BOOT_IMG" /bootfiles/initramfs ::/initramfs_b

# Build a standalone grubx64.efi: a single self-contained EFI binary with a
# tiny embedded grub.cfg that searches for our ESP by FAT label and chains to
# the external /EFI/BOOT/grub.cfg. The external cfg has the actual menu —
# capsuled rewrites it at update-time to flip slots.
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

# DEFAULT is 0 (slot_a). Updates flip it to 1 (slot_b) by rewriting the
# `set default=` line. `panic=10` auto-reboots on kernel panic so a bad
# kernel rolls back to the committed slot via the bootloader's default.
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

mmd    -i "$BOOT_IMG" ::/EFI               || true
mmd    -i "$BOOT_IMG" ::/EFI/BOOT          || true
mcopy -i "$BOOT_IMG" /tmp/grub-stage/grubx64.efi ::/EFI/BOOT/BOOTX64.EFI
mcopy -i "$BOOT_IMG" /tmp/grub-stage/grub.cfg    ::/EFI/BOOT/grub.cfg

# ---- 6. build the raw disk with MBR + 4 partitions + stable disk-id -------
BOOT_START=2048
BOOT_SECTORS=$(( BOOT_SIZE_MIB * 1024 * 2 ))
SLOT_A_START=$(( BOOT_START + BOOT_SECTORS ))
SLOT_A_SECTORS=$(( SLOT_MIB * 1024 * 2 ))
SLOT_B_START=$(( SLOT_A_START + SLOT_A_SECTORS ))
SLOT_B_SECTORS=$SLOT_A_SECTORS
PERM_START=$(( SLOT_B_START + SLOT_B_SECTORS ))
PERM_SECTORS=$(( PERM_SIZE_MIB * 1024 * 2 ))
TOTAL_MIB=$(( 1 + BOOT_SIZE_MIB + 2 * SLOT_MIB + PERM_SIZE_MIB + 4 ))
TOTAL_BYTES=$(( TOTAL_MIB * 1024 * 1024 ))

rm -f "$DISK"
truncate -s "$TOTAL_BYTES" "$DISK"

# label: dos + the explicit `label-id` line gives PARTUUIDs we control.
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
# Both slots get the same squashfs bytes on first build. They diverge as
# soon as an update lands. dd has no count= cap; squashfs's superblock
# bounds the actual filesystem size so trailing zero bytes are ignored.
dd if="$SQSH"     of="$DISK" bs=512 seek="$SLOT_A_START"                       conv=notrunc status=none
dd if="$SQSH"     of="$DISK" bs=512 seek="$SLOT_B_START"                       conv=notrunc status=none
dd if="$PERM_IMG" of="$DISK" bs=512 seek="$PERM_START"   count="$PERM_SECTORS" conv=notrunc status=none

# UEFI-only — no MBR boot code. Firmware boots /EFI/BOOT/BOOTX64.EFI from
# partition 1 (type 0xEF) directly.

echo "pack.sh: wrote $DISK ($TOTAL_MIB MiB) — slot_a + slot_b seeded identically"
