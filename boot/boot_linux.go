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

// mountPerm mounts the PERM-labeled ext4 partition at /perm and ensures the
// well-known subdirectories exist (/perm/capsule, /perm/containerd/root).
// The by-label resolution uses /dev/disk/by-label, which devtmpfs + blkid
// populate automatically; we retry briefly in case it hasn't landed yet.
func mountPerm() error {
	const target = "/perm"
	const label = "PERM"

	// Directory must already exist — rootfs may be read-only at this point.
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("mount point %s missing (rootfs must pre-create it): %w", target, err)
	}

	// Already mounted (e.g. from initramfs)?
	if mounted, _ := isMounted(target); mounted {
		slog.Info("/perm already mounted")
		return ensurePermDirs(target)
	}

	device, err := resolvePermDevice(label)
	if err != nil {
		return err
	}

	if err := unix.Mount(device, target, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount %s -> %s: %w", device, target, err)
	}
	slog.Info("/perm mounted", "device", device)

	return ensurePermDirs(target)
}

// resolvePermDevice returns the block device for the PERM partition. We try
// /dev/disk/by-label/PERM first; if devtmpfs hasn't created it yet, fall
// back to scanning /sys/class/block for a child partition whose fs label is
// PERM — but for phase 1 we keep it simple and hard-code /dev/vda3 as a
// second fallback (matches the packer's partition order).
func resolvePermDevice(label string) (string, error) {
	byLabel := "/dev/disk/by-label/" + label
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(byLabel); err == nil {
			resolved, err := os.Readlink(byLabel)
			if err != nil {
				return byLabel, nil
			}
			// Symlink is relative (e.g. ../../vda3).
			if !strings.HasPrefix(resolved, "/") {
				resolved = "/dev/" + filepath.Base(resolved)
			}
			return resolved, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Fallback: hard-code the expected partition when udev-style label
	// links are absent (minimal Alpine initramfs may not populate them).
	if _, err := os.Stat("/dev/vda3"); err == nil {
		slog.Warn("by-label not found, falling back to /dev/vda3")
		return "/dev/vda3", nil
	}
	return "", fmt.Errorf("no device with label %s and /dev/vda3 absent", label)
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
