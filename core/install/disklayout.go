// Package install implements the on-disk side of `capsulectl install`.
// It writes a fresh Capsule image to an internal disk by reusing the
// running USB's partitions as the source of truth — boot partition,
// slot_a squashfs, and slot_b squashfs are copied byte-for-byte onto
// the target. The PERM partition is initialized as a blank LVM PV and
// (optionally) seeded with a firstboot.json bundle so the disk-booted
// capsule comes up already adopted.
//
// We deliberately do NOT shell out to sfdisk / grub-install / mkfs.fat.
// The MBR is a fixed 512-byte structure; writing it directly in Go is
// faster, has zero external dependencies, and matches the layout
// image/pack.sh produces byte-for-byte. The FAT32 boot partition and
// the squashfs slot partitions are copied wholesale from the running
// installer, so all the tricky bits (GRUB EFI binary, grub.cfg, the
// per-slot kernel + initramfs pairs, the squashfs contents) come along
// for free without re-running grub-install.
package install

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// CapsuleMBRSig is the fixed MBR disk signature image/pack.sh stamps
// onto every Capsule install. Kept in sync with the constant in
// boot/install_linux.go and the hex literal in pack.sh.
const CapsuleMBRSig uint32 = 0xb1a570ff

// Sizes that match image/pack.sh. The slot size is fixed forever once
// a capsule is installed (changing it would shift PERM and destroy the
// LVM PV), so we hard-code the same default here. Boot + slot sizes
// must equal what pack.sh produces because we copy partitions
// byte-for-byte from the running USB.
const (
	sectorSize       = 512
	bootPartStart    = 2048                          // sectors; same as pack.sh
	bootPartSizeMiB  = 256                           // FAT32 CAPSULEBOOT
	slotPartSizeMiB  = 2048                          // squashfs slot
	minPermSizeMiB   = 2048                          // floor; below this the install is useless
	bootPartSectors  = bootPartSizeMiB * 1024 * 2    // MiB → 512-byte sectors
	slotPartSectors  = slotPartSizeMiB * 1024 * 2    // same
)

// Partition types (matches pack.sh's sfdisk script).
const (
	partTypeESP   = 0xEF // EFI System Partition (FAT32, bootable)
	partTypeLinux = 0x83 // Linux native (squashfs slot)
	partTypeLVM   = 0x8E // Linux LVM (PERM PV)
)

// Layout describes the target disk's partition map. PERM size is
// computed from the device size at install time; everything else is
// fixed by pack.sh's defaults.
type Layout struct {
	BootStart   uint32 // sectors
	BootSectors uint32
	SlotAStart  uint32
	SlotASectors uint32
	SlotBStart  uint32
	SlotBSectors uint32
	PermStart   uint32
	PermSectors uint32
}

// LayoutForDisk computes the partition layout for a target of the
// given byte size. The first three partitions are fixed; PERM gets the
// remainder. Returns an error if the disk is too small.
func LayoutForDisk(sizeBytes uint64) (Layout, error) {
	totalSectors := sizeBytes / sectorSize
	if totalSectors > 0xFFFFFFFF {
		// MBR's 32-bit LBA caps at ~2 TiB. Larger disks would need GPT,
		// which is a future-work item — capsule's current layout is
		// MBR-only and pack.sh produces the same 32-bit-bounded shape.
		// Truncate so PERM stays within the addressable range.
		totalSectors = 0xFFFFFFFF
	}
	l := Layout{
		BootStart:    bootPartStart,
		BootSectors:  bootPartSectors,
		SlotAStart:   bootPartStart + bootPartSectors,
		SlotASectors: slotPartSectors,
	}
	l.SlotBStart = l.SlotAStart + l.SlotASectors
	l.SlotBSectors = slotPartSectors
	l.PermStart = l.SlotBStart + l.SlotBSectors
	if uint64(l.PermStart) >= totalSectors {
		return Layout{}, fmt.Errorf("target too small for capsule layout (need at least %d MiB)",
			bootPartSizeMiB+2*slotPartSizeMiB+minPermSizeMiB+8)
	}
	l.PermSectors = uint32(totalSectors - uint64(l.PermStart))
	if l.PermSectors < minPermSizeMiB*1024*2 {
		return Layout{}, fmt.Errorf("target too small: %d MiB PERM would be available, need >= %d MiB",
			l.PermSectors/(1024*2), minPermSizeMiB)
	}
	return l, nil
}

// WriteMBR builds the 512-byte MBR and writes it to the start of dev.
// Layout: 446-byte boot code (zeroed; UEFI doesn't read it), 4-byte
// disk signature at 0x1B8, 16-byte partition entries from 0x1BE, and
// the magic 0x55AA at 0x1FE.
//
// The boot flag is set on partition 1 (the FAT32 ESP) to match what
// pack.sh produces. CHS fields are filled with 0xFEFFFF — the sentinel
// for "LBA only, don't trust CHS" — which is what every modern OS
// uses.
func WriteMBR(devPath string, layout Layout) error {
	mbr := make([]byte, sectorSize)

	// Disk signature (little-endian) at 0x1B8.
	binary.LittleEndian.PutUint32(mbr[0x1B8:0x1BC], CapsuleMBRSig)

	parts := []struct {
		typ      byte
		start    uint32
		size     uint32
		bootable bool
	}{
		{partTypeESP, layout.BootStart, layout.BootSectors, true},
		{partTypeLinux, layout.SlotAStart, layout.SlotASectors, false},
		{partTypeLinux, layout.SlotBStart, layout.SlotBSectors, false},
		{partTypeLVM, layout.PermStart, layout.PermSectors, false},
	}
	for i, p := range parts {
		offset := 0x1BE + i*16
		if p.bootable {
			mbr[offset] = 0x80
		}
		// CHS start: 0xFE 0xFF 0xFF — "LBA only" sentinel.
		mbr[offset+1] = 0xFE
		mbr[offset+2] = 0xFF
		mbr[offset+3] = 0xFF
		mbr[offset+4] = p.typ
		// CHS end: same sentinel.
		mbr[offset+5] = 0xFE
		mbr[offset+6] = 0xFF
		mbr[offset+7] = 0xFF
		binary.LittleEndian.PutUint32(mbr[offset+8:offset+12], p.start)
		binary.LittleEndian.PutUint32(mbr[offset+12:offset+16], p.size)
	}
	// Boot signature.
	mbr[0x1FE] = 0x55
	mbr[0x1FF] = 0xAA

	f, err := os.OpenFile(devPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	if _, err := f.WriteAt(mbr, 0); err != nil {
		return fmt.Errorf("write MBR to %s: %w", devPath, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync MBR on %s: %w", devPath, err)
	}
	return nil
}

// CopyPartition copies count sectors from srcDev[srcStart:] to
// dstDev[dstStart:]. Used by the installer to replicate the running
// USB's boot + slot_a + slot_b partitions onto the target.
//
// Progress is reported via the optional onProgress callback, which is
// invoked roughly every 16 MiB with (bytesCopied, totalBytes). Pass
// nil to disable.
func CopyPartition(srcDev string, srcStart uint32, dstDev string, dstStart uint32, count uint32, onProgress func(done, total uint64)) error {
	src, err := os.Open(srcDev)
	if err != nil {
		return fmt.Errorf("open src %s: %w", srcDev, err)
	}
	defer src.Close()
	dst, err := os.OpenFile(dstDev, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open dst %s: %w", dstDev, err)
	}
	defer dst.Close()

	if _, err := src.Seek(int64(srcStart)*sectorSize, io.SeekStart); err != nil {
		return fmt.Errorf("seek src: %w", err)
	}
	if _, err := dst.Seek(int64(dstStart)*sectorSize, io.SeekStart); err != nil {
		return fmt.Errorf("seek dst: %w", err)
	}

	// 4 MiB chunks: big enough for high throughput, small enough to
	// give progress updates that feel responsive.
	const chunkBytes = 4 * 1024 * 1024
	totalBytes := uint64(count) * sectorSize
	buf := make([]byte, chunkBytes)
	var copied uint64
	tick := uint64(0)
	for copied < totalBytes {
		remaining := totalBytes - copied
		want := uint64(chunkBytes)
		if remaining < want {
			want = remaining
		}
		n, rerr := io.ReadFull(src, buf[:want])
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return fmt.Errorf("write dst: %w", werr)
			}
			copied += uint64(n)
			if onProgress != nil && copied-tick >= 16*1024*1024 {
				onProgress(copied, totalBytes)
				tick = copied
			}
		}
		if rerr != nil {
			// Short read at end-of-source is acceptable for partitions
			// like the squashfs slot, where the FS itself is bounded by
			// the superblock — trailing zeros aren't required.
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			return fmt.Errorf("read src: %w", rerr)
		}
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("sync dst: %w", err)
	}
	if onProgress != nil {
		onProgress(copied, totalBytes)
	}
	return nil
}
