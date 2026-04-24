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

	// Alpine's initramfs mounts / read-only even when the kernel cmdline
	// says 'rw'. Remount it read-write up front so we can install
	// /etc/resolv.conf, write logs, and generally behave like a normal init.
	// A/B updates (Phase 3) will switch to squashfs and we'll revisit: in
	// that world the squashfs stays read-only and only tmpfs overlays / PERM
	// are writeable.
	if err := unix.Mount("", "/", "", unix.MS_REMOUNT, ""); err != nil {
		slog.Warn("remount / rw failed", "err", err)
	}

	for _, m := range earlyMounts {
		if err := mountOne(m); err != nil {
			if m.required {
				return res, fmt.Errorf("mount %s -> %s: %w", m.source, m.target, err)
			}
			slog.Warn("optional mount failed", "target", m.target, "err", err)
		}
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

	// Bring up the first non-loopback ethernet interface and DHCP for an
	// address. This is deliberately minimal for phase 0 — a proper
	// declarative network config comes with later phases.
	if err := bringUpDefaultEth(); err != nil {
		slog.Warn("failed to configure ethernet", "err", err)
	}

	if err := mountPerm(); err != nil {
		slog.Error("failed to mount /perm", "err", err)
	} else {
		res.MountedPerm = true
	}

	// Bridge networking needs a few kernel modules loaded and IP forwarding
	// enabled. These are best-effort: if they fail, only bridge-mode
	// workloads will break; host mode and default mode keep working.
	loadBridgeModules()
	if err := enableIPForward(); err != nil {
		slog.Warn("failed to enable IP forwarding", "err", err)
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
		"virtio_rng",
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
//	VG "capsule" on /dev/vda3
//	  ├─ LV "meta"      (plain LV, ext4)          → mounted here
//	  └─ LV "thinpool"  (dm-thin pool)            → volumes + snapshots
func mountPerm() error {
	const (
		target  = "/perm"
		vg      = "capsule"
		metaLV  = "meta"
		permDev = "/dev/vda3"
	)

	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("mount point %s missing (rootfs must pre-create it): %w", target, err)
	}

	if mounted, _ := isMounted(target); mounted {
		slog.Info("/perm already mounted")
		return ensurePermDirs(target)
	}

	if _, err := os.Stat(permDev); err != nil {
		return fmt.Errorf("PERM device %s missing: %w", permDev, err)
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
