//go:build linux

package boot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type mountSpec struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
	// optional: if true, a failure is fatal
	required bool
}

var earlyMounts = []mountSpec{
	{source: "proc", target: "/proc", fstype: "proc", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, required: true},
	{source: "sysfs", target: "/sys", fstype: "sysfs", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, required: true},
	{source: "devtmpfs", target: "/dev", fstype: "devtmpfs", flags: unix.MS_NOSUID, data: "mode=0755", required: true},
	// devpts gets us /dev/pts/<N> + /dev/ptmx for PTY allocation. Required
	// for any `exec -t` call, including the debug container's interactive
	// shell when it shares the host mount namespace.
	{source: "devpts", target: "/dev/pts", fstype: "devpts", flags: unix.MS_NOSUID | unix.MS_NOEXEC, data: "newinstance,ptmxmode=0666,mode=0620,gid=5"},
	{source: "tmpfs", target: "/run", fstype: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, data: "mode=0755"},
	{source: "tmpfs", target: "/tmp", fstype: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, data: "mode=1777"},
	{source: "cgroup2", target: "/sys/fs/cgroup", fstype: "cgroup2", flags: unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
}

func initPlatform(ctx context.Context) (Result, error) {
	var res Result

	// Rootfs is overlayfs (lower=squashfs ro, upper=tmpfs rw) — already rw,
	// no remount needed. Anything we write under / lands on the tmpfs upper
	// layer and is gone after reboot; persistent state goes under /perm.

	for _, m := range earlyMounts {
		if err := mountOne(m); err != nil {
			if m.required {
				return res, fmt.Errorf("mount %s -> %s: %w", m.source, m.target, err)
			}
			slog.Warn("optional mount failed", "target", m.target, "err", err)
		}
	}

	// /dev/ptmx is the canonical path programs use to allocate a PTY
	// (openpty(3) etc.). Convention is for it to symlink to
	// /dev/pts/ptmx, which devpts creates. devtmpfs's /dev doesn't
	// pre-create this symlink; we do it after mounting devpts.
	if _, err := os.Lstat("/dev/ptmx"); os.IsNotExist(err) {
		if err := os.Symlink("/dev/pts/ptmx", "/dev/ptmx"); err != nil {
			slog.Warn("create /dev/ptmx symlink failed", "err", err)
		}
	}

	// /proc is mounted now → safe to read /proc/cmdline for the slot marker.
	res.ActiveSlot = detectActiveSlot()
	if res.ActiveSlot != "" {
		slog.Info("active slot", "slot", res.ActiveSlot)
	}

	if err := installResolvConf("/etc/capsule/resolv.conf", "/etc/resolv.conf"); err != nil {
		slog.Warn("failed to install resolv.conf", "err", err)
	}

	if err := linkUp("lo"); err != nil {
		slog.Warn("failed to bring up loopback", "err", err)
	}

	// Load networking + bridging + KVM + virtio_net kernel modules. Must run
	// BEFORE bringUpDefaultEth — our minimal initramfs only loads virtio_blk
	// (enough to mount the rootfs); virtio_net + everything bridge/VM-side is
	// pulled in here. Best-effort: failures only break the corresponding
	// workload kind.
	loadBridgeModules()
	if err := enableIPForward(); err != nil {
		slog.Warn("failed to enable IP forwarding", "err", err)
	}

	// Bring up the first non-loopback ethernet interface with the QEMU SLIRP
	// static defaults. A proper declarative network config lands later.
	if err := bringUpDefaultEth(); err != nil {
		slog.Warn("failed to configure ethernet", "err", err)
	}

	if err := mountPerm(); err != nil {
		slog.Error("failed to mount /perm", "err", err)
	} else {
		res.MountedPerm = true
	}

	// Hostname must come after mountPerm (so /perm/capsule/hostname is
	// readable if the operator set it) and after loadBridgeModules (so the
	// MAC-derived fallback can see the real ethernet, not just lo).
	if err := applyResolvedHostname(res.MountedPerm); err != nil {
		slog.Warn("failed to set hostname", "err", err)
	}

	return res, nil
}

func loadBridgeModules() {
	// virtio_rng fills the kernel entropy pool from a host-side RNG device
	// (QEMU's -device virtio-rng-pci). Without it, Go's crypto/rand blocks
	// for up to 60s on first TLS handshake (which is what the first image
	// pull does), stalling the whole reconciler.
	// bridge/br_netfilter/veth/nf_nat are required for NETWORK_MODE_BRIDGE.
	// kvm/kvm_intel/kvm_amd are needed for MicroVM workloads via
	// Firecracker. On non-virt hosts they'll fail to load — that's fine,
	// only MicroVM workloads will fail later.
	// NIC drivers: try every common consumer-mini-PC chip. Modprobe is a
	// no-op for drivers whose hardware isn't present, so a shotgun load is
	// fine and keeps the same image bootable on Beelink (Intel I225/I226
	// or Realtek 8125 typically) and on QEMU (virtio_net) without a
	// host-specific build.
	for _, m := range []string{
		"virtio_net", "virtio_rng",
		"igc", "e1000e", "e1000", "igb", // Intel
		"r8169", "r8125",                // Realtek
		"bridge", "br_netfilter", "veth", "nf_nat",
		"kvm", "kvm_intel", "kvm_amd",
		"tun",
	} {
		cmd := exec.Command("/sbin/modprobe", m)
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("modprobe", "module", m, "err", err, "out", strings.TrimSpace(string(out)))
		}
	}
}

func enableIPForward() error {
	// Also bridge netfilter so iptables rules apply to bridged traffic.
	return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644)
}

// mountPerm activates the capsule volume group and mounts its meta LV at
// /perm. The meta LV holds state.db, logs, and containerd's root; user
// volumes + containerd devmapper snapshots live in the sibling thin pool
// inside the same VG (not mounted here — those are block devices handed
// out by the volume service and devmapper snapshotter).
//
// First-boot path: if the PERM partition (pack.sh creates it zeroed)
// has no LVM PV signature yet, we pvcreate/vgcreate/lvcreate here so the
// operator doesn't have to. Subsequent boots see the existing PV and
// just activate.
//
// Layout on disk (built by image/pack.sh, initialized on first boot):
//
//	VG "capsule" on partition 4 of the boot disk (MBR type 0x8e)
//	  ├─ LV "meta"      (plain LV, ext4)          → mounted here
//	  └─ LV "thinpool"  (dm-thin pool)            → volumes + snapshots
//
// The boot disk varies by host (`/dev/vda` in QEMU, `/dev/nvme0n1` on the
// Beelink, `/dev/sdX` on USB). We resolve PERM dynamically by parsing the
// kernel cmdline for `root=PARTUUID=<sig>-<NN>` to get the disk signature,
// then scanning /sys/class/block for the matching disk's partition 4.
func mountPerm() error {
	const (
		target = "/perm"
		vg     = "capsule"
		metaLV = "meta"
	)

	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("mount point %s missing (rootfs must pre-create it): %w", target, err)
	}

	if mounted, _ := isMounted(target); mounted {
		slog.Info("/perm already mounted")
		return ensurePermDirs(target)
	}

	permDev, err := findPermPartition()
	if err != nil {
		return fmt.Errorf("locate PERM partition: %w", err)
	}

	if !pvExists(permDev) {
		slog.Info("initializing capsule VG on first boot", "device", permDev)
		if err := initializeCapsuleVG(permDev); err != nil {
			return fmt.Errorf("initialize VG %s: %w", vg, err)
		}
	}

	if err := activateVG(vg); err != nil {
		return fmt.Errorf("activate vg %s: %w", vg, err)
	}

	device := "/dev/" + vg + "/" + metaLV
	if err := waitForBlockDevice(device, 2*time.Second); err != nil {
		return fmt.Errorf("waiting for %s: %w", device, err)
	}

	if err := unix.Mount(device, target, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s -> %s: %w", device, target, err)
	}
	slog.Info("/perm mounted", "device", device)

	return ensurePermDirs(target)
}

// pvExists reports whether dev already has an LVM PV signature. Used to
// decide between "activate existing VG" and "first-boot initialize".
// `pvs <dev>` exits 0 when there's a PV, non-zero otherwise.
func pvExists(dev string) bool {
	return exec.Command("/sbin/pvs", "--noheadings", dev).Run() == nil
}

// findPermPartition locates the PERM (partition 4) block device. Wrapper
// around FindPartitionByNumber kept for the existing call site.
func findPermPartition() (string, error) {
	return FindPartitionByNumber(4)
}

// FindPartitionByNumber returns the absolute /dev path of partition N on
// the boot disk. The boot disk is identified by the MBR signature embedded
// in `root=PARTUUID=<sig>-<NN>` on the kernel cmdline; we scan
// /sys/class/block for partitions whose parent disk has that signature
// and return the one with the matching `partition` number.
//
// Works for any device naming (vda / nvme0n1 / sda / mmcblk0) — no
// hardcoded paths.
//
// Used by:
//   - mountPerm (partition 4 = LVM PV)
//   - core/update for the inactive slot's block device (2 or 3)
//   - core/update for CAPSULEBOOT (1) — ESP
func FindPartitionByNumber(partNum int) (string, error) {
	wantSig, err := bootDiskSignature()
	if err != nil {
		return "", err
	}
	want := strconv.Itoa(partNum)

	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		partNumBytes, err := os.ReadFile("/sys/class/block/" + e.Name() + "/partition")
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(partNumBytes)) != want {
			continue
		}
		target, err := filepath.EvalSymlinks("/sys/class/block/" + e.Name())
		if err != nil {
			continue
		}
		diskName := filepath.Base(filepath.Dir(target))
		gotSig, err := readMBRDiskSig("/dev/" + diskName)
		if err != nil {
			continue
		}
		if gotSig == wantSig {
			return "/dev/" + e.Name(), nil
		}
	}
	return "", fmt.Errorf("no partition %d found with disk signature %s", partNum, wantSig)
}

// BootDisk returns the absolute /dev path of the disk we booted from
// (e.g. "/dev/vda", "/dev/nvme0n1"). Resolved from the active slot's
// PARTUUID on the kernel cmdline.
func BootDisk() (string, error) {
	wantSig, err := bootDiskSignature()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir("/sys/class/block")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		// Whole disks don't have a `partition` file; partitions do.
		if _, err := os.Stat("/sys/class/block/" + e.Name() + "/partition"); err == nil {
			continue
		}
		gotSig, err := readMBRDiskSig("/dev/" + e.Name())
		if err != nil {
			continue
		}
		if gotSig == wantSig {
			return "/dev/" + e.Name(), nil
		}
	}
	return "", fmt.Errorf("no whole disk found with signature %s", wantSig)
}

// bootDiskSignature parses /proc/cmdline for `root=PARTUUID=<sig>-<NN>`
// and returns the lowercase 8-char hex signature. Shared helper for
// FindPartitionByNumber and BootDisk.
func bootDiskSignature() (string, error) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", fmt.Errorf("read /proc/cmdline: %w", err)
	}
	for _, f := range strings.Fields(string(cmdline)) {
		if pu, ok := strings.CutPrefix(f, "root=PARTUUID="); ok {
			parts := strings.SplitN(pu, "-", 2)
			if len(parts) != 2 {
				return "", fmt.Errorf("malformed PARTUUID %q", pu)
			}
			return strings.ToLower(parts[0]), nil
		}
	}
	return "", fmt.Errorf("no root=PARTUUID in /proc/cmdline (cmdline: %q)", strings.TrimSpace(string(cmdline)))
}

// readMBRDiskSig reads the 4-byte MBR disk signature at offset 0x1B8 and
// formats it the way Linux exposes PARTUUID prefixes (big-endian hex).
// On-disk byte order is little-endian, which is why we reverse.
func readMBRDiskSig(devPath string) (string, error) {
	f, err := os.Open(devPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var sig [4]byte
	if _, err := f.ReadAt(sig[:], 0x1B8); err != nil {
		return "", err
	}
	return fmt.Sprintf("%02x%02x%02x%02x", sig[3], sig[2], sig[1], sig[0]), nil
}

// detectActiveSlot parses /proc/cmdline for `capsule.slot=a|b` and returns
// "slot_a" / "slot_b". Empty string if no marker (dev mode, or pre-A/B
// build). The marker is set per-menuentry in grub.cfg by pack.sh.
func detectActiveSlot() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	for f := range strings.FieldsSeq(string(data)) {
		switch f {
		case "capsule.slot=a":
			return "slot_a"
		case "capsule.slot=b":
			return "slot_b"
		}
	}
	return ""
}

// initializeCapsuleVG runs the first-boot sequence that turns a blank
// PERM partition into the capsule VG: pvcreate, vgcreate, an ext4 meta
// LV for /perm, and a thin pool for volumes + containerd snapshots.
//
// Sizes:
//   - meta LV: scaled with the VG — min(25% of VG, 32 GiB), floor 1 GiB.
//     /perm holds state.db, update staging (~2.4 GiB peak during a push),
//     AND the containerd content store (every image ever pushed/pulled),
//     so a fixed small size starves image work on big disks.
//   - thinpool: the remainder of the VG (minus a small reserve LVM wants
//     for metadata volumes), thin-provisioned so declared LV sizes don't
//     preallocate.
func initializeCapsuleVG(dev string) error {
	if err := runLVM("/sbin/pvcreate", "-ff", "-y", dev); err != nil {
		return err
	}
	if err := runLVM("/sbin/vgcreate", "capsule", dev); err != nil {
		return err
	}
	metaMiB, err := computeMetaMiB("capsule")
	if err != nil {
		return err
	}
	slog.Info("creating meta LV", "size_mib", metaMiB)
	if err := runLVM("/sbin/lvcreate", "-L", fmt.Sprintf("%dM", metaMiB), "-n", "meta", "capsule"); err != nil {
		return err
	}
	if err := runLVM("/usr/sbin/mkfs.ext4", "-q", "-F", "/dev/capsule/meta"); err != nil {
		// Fall back to /sbin path in case mkfs.ext4 is only there.
		if err2 := runLVM("/sbin/mkfs.ext4", "-q", "-F", "/dev/capsule/meta"); err2 != nil {
			return fmt.Errorf("mkfs.ext4 /dev/capsule/meta: %w / %w", err, err2)
		}
	}
	// Thin pool: use 95% of remaining VG extents so LVM has a little
	// headroom to grow pool metadata if it needs to. --monitor n because
	// we don't run dmeventd (yet); without it LVM's auto-extend doesn't
	// wire up, but activation succeeds.
	if err := runLVM("/sbin/lvcreate", "--monitor", "n", "-l", "95%FREE", "--thinpool", "thinpool", "capsule"); err != nil {
		return err
	}
	return nil
}

// activateVG scans + activates all LVs in the volume group. Idempotent:
// already-active LVs are no-ops for LVM. Without this step, the per-LV
// device nodes under /dev/<vg>/ don't exist yet because LVM doesn't
// auto-activate at kernel bringup.
//
// --monitor n suppresses the dmeventd hookup. We don't ship dmeventd on
// the capsule yet; without --monitor n, LVM prints a warning and returns
// non-zero even though the LVs came up. When we want thin-pool autoextend
// (Phase B/C on PLAN.md), wire dmeventd separately.
func activateVG(vg string) error {
	_ = runLVM("/sbin/vgscan", "--mknodes")
	if err := runLVM("/sbin/vgchange", "--monitor", "n", "-ay", vg); err != nil {
		return err
	}
	return nil
}

func runLVM(bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// computeMetaMiB returns the meta LV size for a freshly-created VG.
// Reads vg_size from `vgs` and applies min(25% of VG, 32 GiB), floor 1 GiB.
// On a 2 GiB pack.sh template image the floor wins (1 GiB); on real
// hardware (tens of GiB to TiB) the percentage takes over until the cap.
func computeMetaMiB(vg string) (uint64, error) {
	cmd := exec.Command("/sbin/vgs", "--noheadings", "--units", "m", "--nosuffix", "-o", "vg_size", vg)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("vgs %s: %w", vg, err)
	}
	field := strings.TrimSpace(string(out))
	// vgs prints e.g. "  2044.00" — accept either int or float MiB.
	vgMiBFloat, err := strconv.ParseFloat(field, 64)
	if err != nil {
		return 0, fmt.Errorf("parse vg_size %q: %w", field, err)
	}
	vgMiB := uint64(vgMiBFloat)
	meta := vgMiB / 4
	const floor, cap uint64 = 1024, 32 * 1024
	if meta < floor {
		meta = floor
	}
	if meta > cap {
		meta = cap
	}
	return meta, nil
}

// waitForBlockDevice polls for path to appear as a block device, up to
// deadline. LVM's udev rules create /dev/<vg>/<lv> symlinks asynchronously.
func waitForBlockDevice(path string, deadline time.Duration) error {
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("block device %s not present after %s", path, deadline)
}

func ensurePermDirs(root string) error {
	for _, d := range []string{
		filepath.Join(root, "capsule"),
		filepath.Join(root, "containerd"),
		filepath.Join(root, "containerd", "root"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

// isMounted reports whether path appears as a mount point in /proc/mounts.
func isMounted(path string) (bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true, nil
		}
	}
	return false, nil
}

// bringUpDefaultEth brings up the first non-loopback interface and asks
// busybox udhcpc for a lease. Works on QEMU's SLIRP (DHCP server at
// 10.0.2.2) and on a real LAN router. Caller logs but does not abort if
// this fails — gRPC still binds to 0.0.0.0 so anyone who can reach the
// box (statically configured, console session) can talk to capsuled.
func bringUpDefaultEth() error {
	name, err := firstEthIfaceWait(15 * time.Second)
	if err != nil {
		return err
	}
	if err := linkUp(name); err != nil {
		return fmt.Errorf("link up %s: %w", name, err)
	}
	// -i: interface, -q: exit on lease, -n: exit if no lease, -t 4: 4
	// retries, -A 2: 2-second between retries. Short enough that a missing
	// DHCP server doesn't stall boot indefinitely.
	cmd := exec.Command("/sbin/udhcpc", "-i", name, "-q", "-n", "-t", "4", "-A", "2")
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("udhcpc failed", "iface", name, "err", err, "out", strings.TrimSpace(string(out)))
		return nil
	}
	slog.Info("ethernet configured via DHCP", "iface", name)

	// One-shot NTP sync. Real hardware boots with whatever wall clock the
	// RTC says (often minutes off, sometimes worse). Without this, server-
	// stamped log timestamps disagree with operator clocks and the A/B
	// update deadline can fire much later in real time than it should.
	go syncTimeOnce()
	return nil
}

// syncTimeOnce runs busybox ntpd in -q mode (quit after first sync) against
// a public NTP pool. Best-effort and non-blocking — capsuled boots fine
// without it; the only consequence of failure is a wall-clock that drifts
// from real time. Run on its own goroutine so DHCP→bring-up doesn't wait.
func syncTimeOnce() {
	cmd := exec.Command("/usr/sbin/ntpd",
		"-d", "-n", "-q",
		"-p", "pool.ntp.org",
		"-p", "time.cloudflare.com",
		"-p", "time.google.com",
	)
	// Hard 30-sec timeout: ntpd can hang if all peers are unreachable.
	timer := time.AfterFunc(30*time.Second, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("ntpd sync failed", "err", err, "out", strings.TrimSpace(string(out)))
		return
	}
	slog.Info("clock synced via ntpd", "now", time.Now().UTC().Format(time.RFC3339))
}

// firstEthIfaceWait polls /sys/class/net for a non-loopback interface,
// waiting up to timeout for one to appear. PCI probe of NIC drivers
// (r8169, igc, etc.) is async — modprobe returns before the netdev is
// created, so a one-shot check often misses it.
func firstEthIfaceWait(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		name, err := firstEthIface()
		if err == nil {
			return name, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// PermHostnameFile is the operator-settable hostname override. Lives on
// /perm so it survives OS A/B updates. A SetHostname RPC (future) writes
// here; for now an operator can drop a single-line file via `capsule
// debug` to rename the box across reboots.
const PermHostnameFile = "/perm/capsule/hostname"

// applyResolvedHostname picks a hostname using this priority:
//  1. /perm/capsule/hostname (operator-set, persistent)
//  2. capsule-<6 hex chars of the first non-loopback NIC MAC>
//     (deterministic per box: same hardware → same name across reboots)
//  3. "capsule" (last-resort static fallback)
//
// permMounted=false skips step 1 — we never block on /perm being there
// (a failed VG activation must not also leave the box nameless).
func applyResolvedHostname(permMounted bool) error {
	name := resolveHostname(permMounted)
	if name == "" {
		return nil
	}
	if err := unix.Sethostname([]byte(name)); err != nil {
		return fmt.Errorf("sethostname %q: %w", name, err)
	}
	slog.Info("hostname set", "hostname", name)
	return nil
}

func resolveHostname(permMounted bool) string {
	if permMounted {
		if name, ok := readHostnameFile(PermHostnameFile); ok {
			return name
		}
	}
	if name := macDerivedHostname(); name != "" {
		return name
	}
	if name, ok := readHostnameFile("/etc/capsule/hostname"); ok {
		return name
	}
	return "capsule"
}

func readHostnameFile(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(b))
	if name == "" {
		return "", false
	}
	return name, true
}

// macDerivedHostname returns "capsule-<6hex>" using the last 3 octets of
// the first non-loopback netdev's MAC (alphabetical name order — stable
// per box). Returns "" if no eligible NIC is up yet.
func macDerivedHostname() string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name() == "lo" {
			continue
		}
		names = append(names, e.Name())
	}
	// ReadDir returns lexical order on Linux, but be explicit — we rely on
	// it for "stable per box" across kernel revs.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	for _, n := range names {
		mac, err := os.ReadFile(filepath.Join("/sys/class/net", n, "address"))
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(mac))
		// Skip all-zero MACs (uninitialized virtual interfaces) — they
		// would collide across boxes.
		if s == "" || s == "00:00:00:00:00:00" {
			continue
		}
		parts := strings.Split(s, ":")
		if len(parts) != 6 {
			continue
		}
		return "capsule-" + strings.ToLower(parts[3]+parts[4]+parts[5])
	}
	return ""
}

// installResolvConf copies src into dst. Like hostname, /etc/resolv.conf is
// wiped by Docker's build layer; boot re-creates it from the baked-in copy.
// Missing src is not an error.
func installResolvConf(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// firstEthIface returns the name of the first non-loopback network interface
// reported by the kernel. Reads /sys/class/net directly to avoid depending
// on netlink or /proc.
func firstEthIface() (string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() == "lo" {
			continue
		}
		return e.Name(), nil
	}
	return "", fmt.Errorf("no non-loopback interface found")
}

func mountOne(m mountSpec) error {
	if err := os.MkdirAll(m.target, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data)
	// Alpine's initramfs mounts /proc, /sys and /dev before switch_root,
	// so those targets are already live when capsuled starts. EBUSY (mount
	// point already occupied by a mount of the same fstype) is the normal
	// path, not an error.
	if errors.Is(err, unix.EBUSY) {
		slog.Debug("mount target already mounted", "target", m.target)
		return nil
	}
	return err
}

// linkUp brings an interface up. Uses `ip link` from the image because
// iproute2 is in our rootfs and a shell-out avoids pulling netlink deps
// into capsuled itself.
func linkUp(name string) error {
	cmd := exec.Command("/sbin/ip", "link", "set", name, "up")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}
