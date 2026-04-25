#!/bin/bash
# pack.sh — runs inside the packer container.
#
# Inputs (bind-mounted /work):
#   /work/rootfs.tar    Capsule rootfs as a tar (from `docker export`).
#
# Outputs (written to /work):
#   /work/rootfs.sqsh   squashfs of the rootfs (the actual rootfs image written
#                       to the ROOTFS partition — the boot chain is
#                       squashfs -> overlayfs(tmpfs upper) -> switch_root,
#                       so the rootfs is immutable on disk).
#   /work/disk.raw      bootable raw disk image.
#
# Partition layout (Phase 2 — pre-A/B):
#   1. FAT32  KEELBOOT  256 MiB   kernel + custom initramfs + syslinux
#   2. raw    ROOTFS    squashfs size + ~50 MiB headroom
#   3. raw    PERM      PERM_SIZE_MIB (default 2048) — LVM PV, formatted on first boot
#
# The rootfs is written as a raw squashfs image (no filesystem wrapper).
# Our custom initramfs (built in Dockerfile.packer) mounts it ro and layers
# a tmpfs overlay before switch_root'ing into /sbin/init (capsuled).

set -euo pipefail

WORK=/work
ROOTFS_TAR="$WORK/rootfs.tar"
SQSH="$WORK/rootfs.sqsh"
DISK="$WORK/disk.raw"
BOOT_IMG="$WORK/boot.fat"
PERM_IMG="$WORK/perm.ext4"
PERM_SIZE_MIB="${PERM_SIZE_MIB:-2048}"

[ -f "$ROOTFS_TAR" ] || { echo "pack.sh: missing $ROOTFS_TAR"; exit 1; }

# ---- 1. extract rootfs, produce the squashfs that is the rootfs image ------
rm -rf /tmp/rootfs && mkdir -p /tmp/rootfs
tar -C /tmp/rootfs -xf "$ROOTFS_TAR"
[ -x /tmp/rootfs/sbin/init ] || { echo "pack.sh: /sbin/init missing"; exit 1; }

rm -f "$SQSH"
mksquashfs /tmp/rootfs "$SQSH" -noappend -comp zstd -all-root -quiet
SQSH_BYTES=$(stat -c%s "$SQSH")
echo "pack.sh: squashfs size = ${SQSH_BYTES} bytes"

# ---- 2. size the rootfs partition to fit the squashfs + headroom -----------
# 50 MiB headroom so a future update whose squashfs is a bit larger still
# fits without re-partitioning. The partition holds the raw squashfs bytes;
# excess space is ignored by the initramfs mount.
ROOTFS_SIZE=$(( SQSH_BYTES + 50 * 1024 * 1024 ))
ROOTFS_MIB=$(( (ROOTFS_SIZE + 1024*1024 - 1) / (1024*1024) ))
ROOTFS_SIZE=$(( ROOTFS_MIB * 1024 * 1024 ))
echo "pack.sh: rootfs partition size = ${ROOTFS_MIB} MiB (squashfs inside)"

# ---- 3. build (or keep) the PERM partition --------------------------------
# PERM holds the LVM physical volume for VG "capsule": meta LV (ext4,
# mounted at /perm for state.db + containerd/root) plus the thin pool
# backing user volumes and containerd devmapper snapshots. We don't
# mkfs anything here — capsuled initializes the VG at first boot if the
# partition is unformatted, and preserves it across rebuilds otherwise.
#
# Strategy:
#   - If an existing disk.raw has a PERM partition, extract it byte-for-byte
#     so LVM metadata + thin pool extents survive the rebuild.
#   - Else create a zeroed partition; first boot will pvcreate/vgcreate it.
#
# Run `make clean` (which rm's build/) to force a fresh PERM.
PERM_BYTES=$(( PERM_SIZE_MIB * 1024 * 1024 ))

if [ -f "$DISK" ]; then
  # Find the existing PERM partition offset in disk.raw. sfdisk -d prints
  # lines like "disk.raw3 : start=     1138688, size=     4194304, type=83".
  # Pull the two numbers with a sed that strips labels and whitespace.
  existing_line=$(sfdisk -d "$DISK" 2>/dev/null | grep -E '[[:space:]]3[[:space:]]*:' || true)
  existing_start=$(echo "$existing_line" | sed -n 's/.*start=[[:space:]]*\([0-9][0-9]*\).*/\1/p')
  existing_size=$(echo "$existing_line" | sed -n 's/.*size=[[:space:]]*\([0-9][0-9]*\).*/\1/p')
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

# ---- 4. build the FAT32 /boot image ----------------------------------------
BOOT_SIZE_MIB=256
BOOT_BYTES=$(( BOOT_SIZE_MIB * 1024 * 1024 ))
rm -f "$BOOT_IMG"
truncate -s "$BOOT_BYTES" "$BOOT_IMG"
mkfs.fat -F 32 -n KEELBOOT "$BOOT_IMG" >/dev/null

mcopy -i "$BOOT_IMG" /bootfiles/vmlinuz   ::/vmlinuz
mcopy -i "$BOOT_IMG" /bootfiles/initramfs ::/initramfs
mmd    -i "$BOOT_IMG" ::/syslinux || true
mcopy -i "$BOOT_IMG" /usr/share/syslinux/ldlinux.c32  ::/syslinux/ldlinux.c32
mcopy -i "$BOOT_IMG" /usr/share/syslinux/libcom32.c32 ::/syslinux/libcom32.c32
mcopy -i "$BOOT_IMG" /usr/share/syslinux/libutil.c32  ::/syslinux/libutil.c32
mcopy -i "$BOOT_IMG" /usr/share/syslinux/menu.c32     ::/syslinux/menu.c32

# Our custom initramfs (built in Dockerfile.packer) mounts the root partition
# as squashfs ro, layers a tmpfs overlay, and switch_roots. Kernel cmdline
# passes rootfstype=squashfs so the initramfs doesn't have to guess.
cat >/tmp/syslinux.cfg <<'EOF'
DEFAULT capsule
TIMEOUT 20
PROMPT 0
LABEL capsule
  MENU LABEL Capsule (SLOT_A)
  KERNEL /vmlinuz
  INITRD /initramfs
  APPEND root=/dev/vda2 rootfstype=squashfs ro random.trust_cpu=on console=ttyS0,115200n8
EOF
mcopy -i "$BOOT_IMG" /tmp/syslinux.cfg ::/syslinux/syslinux.cfg

syslinux --install "$BOOT_IMG"

# ---- 5. build the raw disk with MBR partition table -----------------------
BOOT_START=2048
BOOT_SECTORS=$(( BOOT_SIZE_MIB * 1024 * 2 ))
ROOT_START=$(( BOOT_START + BOOT_SECTORS ))
ROOT_SECTORS=$(( ROOTFS_MIB * 1024 * 2 ))
PERM_START=$(( ROOT_START + ROOT_SECTORS ))
PERM_SECTORS=$(( PERM_SIZE_MIB * 1024 * 2 ))
TOTAL_MIB=$(( 1 + BOOT_SIZE_MIB + ROOTFS_MIB + PERM_SIZE_MIB + 4 ))
TOTAL_BYTES=$(( TOTAL_MIB * 1024 * 1024 ))

rm -f "$DISK"
truncate -s "$TOTAL_BYTES" "$DISK"

sfdisk "$DISK" <<EOF
label: dos
unit: sectors
$DISK-part1 : start=$BOOT_START, size=$BOOT_SECTORS, type=c, bootable
$DISK-part2 : start=$ROOT_START, size=$ROOT_SECTORS, type=83
$DISK-part3 : start=$PERM_START, size=$PERM_SECTORS, type=83
EOF

dd if="$BOOT_IMG" of="$DISK" bs=512 seek="$BOOT_START" count="$BOOT_SECTORS" conv=notrunc status=none
# ROOTFS partition is raw squashfs — dd the .sqsh directly. The count is
# capped to SQSH's actual size (not the partition's full allocated size),
# so leftover bytes in the partition stay zeroed. squashfs's mount code
# only reads up to the embedded superblock size regardless.
dd if="$SQSH"     of="$DISK" bs=512 seek="$ROOT_START" conv=notrunc status=none
dd if="$PERM_IMG" of="$DISK" bs=512 seek="$PERM_START" count="$PERM_SECTORS" conv=notrunc status=none

dd if=/usr/share/syslinux/mbr.bin of="$DISK" bs=440 count=1 conv=notrunc status=none

echo "pack.sh: wrote $DISK ($TOTAL_MIB MiB)"
