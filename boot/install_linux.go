//go:build linux

package boot

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CapsuleMBRSig is the fixed MBR disk signature pack.sh stamps onto
// every Capsule install ("BLASTOFF" in little-endian hex). The installer
// uses the same constant; presence on a disk means "already installed."
const CapsuleMBRSig = "b1a570ff"

// minTargetSizeBytes is the minimum capacity a disk needs to be a
// viable install target. The image lays out 256 MiB boot + 2x2 GiB
// slots + 2 GiB PERM = ~6.5 GiB minimum; 16 GiB gives headroom for the
// thinpool to be useful for actual workloads.
const minTargetSizeBytes = 16 * 1024 * 1024 * 1024

// DetectInstallerMode decides whether capsuled should boot in
// installer mode. The contract:
//
//   - Returns (true, candidates) when the kernel cmdline contains
//     capsule.mode=installer (set by pack.sh for dedicated installer
//     images), OR when we're on removable media (USB stick / SD card)
//     AND at least one non-removable internal disk exists.
//   - Returns (false, nil) in every other case — including dev mode
//     (no /proc/cmdline PARTUUID) and the common case of running on
//     internal disk.
//
// Errors are swallowed and logged at debug; failing to detect installer
// mode degrades to runtime mode, which is the safer default (no
// destructive writes to a misidentified target).
func DetectInstallerMode() (bool, []TargetDisk) {
	// Dedicated installer images stamp capsule.mode=installer into the
	// kernel cmdline at build time — this is the authoritative signal.
	// It's more reliable than the removable-media heuristic, which
	// breaks when the target disk already has a Capsule MBR signature
	// (reinstall) or when hardware doesn't set the sysfs removable flag.
	if cmdlineHasInstallerFlag() {
		return true, scanTargetDisks(false)
	}

	bootDev, err := BootDisk()
	if err != nil {
		// No usable cmdline PARTUUID — almost always means dev mode
		// (capsuled run off PID 1). Definitely not installer mode.
		slog.Debug("installer detect: cannot resolve boot disk", "err", err)
		return false, nil
	}
	bootBase := filepath.Base(bootDev)
	if !isRemovableBlock(bootBase) {
		// Booted from a non-removable disk — that's a normal runtime
		// boot, not an installer.
		return false, nil
	}

	targets := scanTargetDisks(true)
	if len(targets) == 0 {
		return false, nil
	}
	return true, targets
}

// cmdlineHasInstallerFlag returns true when the kernel cmdline contains
// the capsule.mode=installer token stamped by pack.sh into dedicated
// installer images. This is the authoritative installer signal.
func cmdlineHasInstallerFlag() bool {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	for _, tok := range strings.Fields(string(b)) {
		if tok == "capsule.mode=installer" {
			return true
		}
	}
	return false
}

// scanTargetDisks returns all non-removable internal disks that are large
// enough to be viable install targets. When skipBootDisk is true the disk
// hosting the current boot slot is excluded (needed for the removable-media
// fallback path; not needed when the installer flag is set since the boot
// disk is already the installer image on a different physical device class).
func scanTargetDisks(skipBootDisk bool) []TargetDisk {
	var bootBase string
	if skipBootDisk {
		if dev, err := BootDisk(); err == nil {
			bootBase = filepath.Base(dev)
		}
	}
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		slog.Debug("installer detect: read /sys/class/block", "err", err)
		return nil
	}
	var targets []TargetDisk
	for _, e := range entries {
		name := e.Name()
		if name == bootBase {
			continue
		}
		if _, err := os.Stat("/sys/class/block/" + name + "/partition"); err == nil {
			continue
		}
		if isVirtualBlock(name) {
			continue
		}
		if isRemovableBlock(name) {
			continue
		}
		size, err := blockSizeBytes(name)
		if err != nil || size < minTargetSizeBytes {
			continue
		}
		path := "/dev/" + name
		reinstall := hasCapsuleMBR(path)
		if reinstall {
			slog.Info("installer detect: target has existing Capsule install (reinstall)", "disk", path)
		}
		targets = append(targets, TargetDisk{Path: path, SizeBytes: size, Reinstall: reinstall})
	}
	return targets
}

// isRemovableBlock reads /sys/block/<name>/removable. Returns false on
// any error so a missing sysfs node degrades to "not removable" — we'd
// rather skip a weird device than misclassify the boot disk as USB.
func isRemovableBlock(name string) bool {
	b, err := os.ReadFile("/sys/block/" + name + "/removable")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == "1"
}

// isVirtualBlock filters out devices that have no physical backing
// (loop, dm-N, ram-N, zram, md*, etc.). The heuristic is the symlink
// target under /sys/class/block: a real disk symlinks via /sys/devices/
// pci…/, while a virtual one lives under /sys/devices/virtual/.
func isVirtualBlock(name string) bool {
	target, err := filepath.EvalSymlinks("/sys/class/block/" + name)
	if err != nil {
		return false
	}
	return strings.Contains(target, "/devices/virtual/")
}

// blockSizeBytes reads /sys/block/<name>/size (in 512-byte sectors)
// and converts to bytes.
func blockSizeBytes(name string) (uint64, error) {
	b, err := os.ReadFile("/sys/block/" + name + "/size")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * 512, nil
}

// hasCapsuleMBR returns true iff the disk's MBR signature matches
// CapsuleMBRSig. A best-effort read: missing/unreadable device returns
// false, which lets the installer treat the disk as a fresh target.
// That's the right default for a fresh install — pack.sh always stamps
// the signature, so absence means "not Capsule."
func hasCapsuleMBR(devPath string) bool {
	got, err := readMBRDiskSig(devPath)
	if err != nil {
		return false
	}
	return got == CapsuleMBRSig
}
