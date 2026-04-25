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

	// /proc is mounted now → safe to read /proc/cmdline for the slot marker.
	res.ActiveSlot = detectActiveSlot()
	if res.ActiveSlot != "" {
		slog.Info("active slot", "slot", res.ActiveSlot)
	}

	// Docker strips /etc/hostname during image build, so we keep our copy
	// at /etc/capsule/hostname and install it onto the kernel here.
	if err := setHostnameFromFile("/etc/capsule/hostname"); err != nil {
		slog.Warn("failed to set hostname", "err", err)
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
	for _, m := range []string{
		"virtio_net", "virtio_rng",
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
//   - core/update for KEELBOOT (1)
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
// build). The marker is set per-syslinux-LABEL by pack.sh.
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
//   - meta LV: 512 MiB. Enough for state.db (sqlite) + /perm/containerd/root
//     metadata; user-visible logs go to tmpfs, not /perm.
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
	if err := runLVM("/sbin/lvcreate", "-L", "512M", "-n", "meta", "capsule"); err != nil {
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

// bringUpDefaultEth brings up the first non-loopback interface and assigns
// it a static address. Phase 0 hardcodes QEMU's SLIRP default (10.0.2.15/24,
// gateway 10.0.2.2) so we can prove the gRPC path without depending on
// AF_PACKET / udhcpc, which the Alpine linux-virt kernel doesn't expose
// out of the box. A real declarative network config lands in a later phase.
func bringUpDefaultEth() error {
	name, err := firstEthIface()
	if err != nil {
		return err
	}
	if err := linkUp(name); err != nil {
		return fmt.Errorf("link up %s: %w", name, err)
	}
	if err := runIP("addr", "add", "10.0.2.15/24", "dev", name); err != nil {
		return fmt.Errorf("addr add: %w", err)
	}
	if err := runIP("route", "add", "default", "via", "10.0.2.2", "dev", name); err != nil {
		return fmt.Errorf("route add default: %w", err)
	}
	slog.Info("ethernet configured (static SLIRP defaults)", "iface", name)
	return nil
}

// setHostnameFromFile reads a single line from path and calls sethostname(2).
// Trims trailing whitespace; missing file is not an error.
func setHostnameFromFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	name := strings.TrimSpace(string(b))
	if name == "" {
		return nil
	}
	if err := unix.Sethostname([]byte(name)); err != nil {
		return err
	}
	slog.Info("hostname set", "hostname", name)
	return nil
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

func runIP(args ...string) error {
	cmd := exec.Command("/sbin/ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
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
