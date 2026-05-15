// Package boot handles PID 1 duties on a capsule: early filesystem
// mounts, loopback setup, zombie reaping, and signal handling for clean
// shutdown. Init should only be called when running as PID 1.
package boot

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
)

// Result reports the outcome of running Init.
type Result struct {
	// MountedPerm is true if the /perm partition was mounted successfully.
	MountedPerm bool
	// ActiveSlot is "slot_a" / "slot_b" on a real (Linux PID-1) boot, set
	// from the `capsule.slot=a|b` token on the kernel cmdline (written by
	// the bootloader's per-slot menuentry). Empty in dev mode (running off
	// PID 1) or for old single-slot images.
	ActiveSlot string
	// InstallerMode is true when capsuled detected it booted from
	// removable media AND at least one viable internal install target
	// exists. In this mode capsuled exposes only InstallService and
	// IdentityService; the reconciler, scheduler, containerd, and all
	// workload paths stay dark. See boot.DetectInstallerMode.
	InstallerMode bool
	// Targets are the candidate internal disks where the installer can
	// write a fresh Capsule install. Populated only when InstallerMode
	// is true. Auto-pick rule: if len(Targets) == 1, take it; otherwise
	// the operator must supply --target on `capsulectl install`.
	Targets []TargetDisk
}

// TargetDisk is one candidate install destination — a non-removable
// internal block device with no existing Capsule MBR signature.
type TargetDisk struct {
	// Path is the absolute /dev path ("/dev/nvme0n1", "/dev/sda").
	Path string
	// SizeBytes is the capacity from /sys/block/<name>/size * 512.
	SizeBytes uint64
}

// Init performs PID 1 setup. On non-Linux it is a no-op (allows the daemon
// to be built and run on macOS during development). On Linux it mounts the
// early filesystems and brings up loopback; the caller is then responsible
// for starting supervised children and entering its own signal loop.
func Init(ctx context.Context) (Result, error) {
	return initPlatform(ctx)
}

// BannerInfo carries the dynamic bits the banner shows the HDMI operator:
// the listening port, the SHA-256 fingerprint of the TLS leaf cert (so
// `capsulectl adopt` can be visually confirmed), and whether the capsule
// is awaiting adoption or already enrolled.
type BannerInfo struct {
	GRPCPort       int
	TLSFingerprint string // hex, lowercase, no separators (formatted for display here)
	ShortID        string // stable handle ("capsule-a3f2"); empty pre-short_id
	ClaimOpen      bool
	EnrolledKeys   int
	// Installer fields. When InstallerMode is true the banner switches
	// to the installer template: prints INSTALLER + target disk and tells
	// the operator to run `capsulectl install <short-id>`. The runtime
	// adoption hint is suppressed.
	InstallerMode bool
	TargetDisk    string // "/dev/nvme0n1"; only used when InstallerMode is true
	TargetSize    string // human-formatted ("512 GB"); only used when InstallerMode is true
	// StoreBroken signals that /perm was mounted but SQLite refused to
	// open — capsuled fell back to an in-memory store and the claim
	// window was NOT opened. The banner switches to a recovery template
	// so the operator immediately knows this is not a normal "awaiting
	// adoption" state.
	StoreBroken bool
	StoreError  string
}

// PrintBanner writes a CAPSULE ASCII banner + current IP + adoption
// status to /dev/tty0 (HDMI on real hardware) and /dev/console.
// Operator's "where do I point capsulectl?" answer when the only output
// is HDMI. Best-effort: failing to open either device is silent
// (running off PID 1, on macOS, etc.).
func PrintBanner(info BannerInfo) {
	ip := defaultIPv4()
	if ip == "" {
		ip = "(no IP — DHCP failed?)"
	}
	// First line under the ASCII art: short ID (if set) + IP. Falling
	// back to just IP keeps existing capsules legible after upgrade.
	id := info.ShortID
	if id == "" {
		id = "(short_id pending — reboot to generate)"
	}
	var status string
	switch {
	case info.StoreBroken:
		// Recovery banner: /perm is mounted but SQLite refused to open.
		// Adoption is intentionally NOT offered — the operator must
		// restore state.db or run RESET_AUTH at the console first.
		errMsg := info.StoreError
		if errMsg == "" {
			errMsg = "(see capsuled logs)"
		}
		status = fmt.Sprintf("  status: STATE UNREADABLE — refusing adoption\n  error:  %s\n  recover: restore /perm/capsule/state.db, or touch /perm/capsule/RESET_AUTH and reboot\n", errMsg)
	case info.InstallerMode:
		// Installer banner: target disk + the exact command to run from
		// the operator's laptop. Adoption hint suppressed — the install
		// command seals the operator key in one pass.
		tgt := info.TargetDisk
		if tgt == "" {
			tgt = "(no target disk detected)"
		}
		if info.TargetSize != "" {
			tgt += "  (" + info.TargetSize + ")"
		}
		status = fmt.Sprintf("  status: INSTALLER  ready to flash internal disk\n  target: %s\n  capsulectl install %s --name <name>\n",
			tgt, info.ShortID)
	case info.ClaimOpen:
		status = fmt.Sprintf("  status: AWAITING ADOPTION\n  capsulectl adopt --capsule %s:%d\n", ipForCmd(ip), info.GRPCPort)
	default:
		status = fmt.Sprintf("  status: adopted (%d enrolled key(s))\n  capsulectl --capsule %s:%d capsule info\n",
			info.EnrolledKeys, ipForCmd(ip), info.GRPCPort)
	}
	fp := formatFingerprintForBanner(info.TLSFingerprint)
	banner := fmt.Sprintf(`
   ____    _    ____  ____  _   _ _     _____
  / ___|  / \  |  _ \/ ___|| | | | |   | ____|
 | |     / _ \ | |_) \___ \| | | | |   |  _|
 | |___ / ___ \|  __/ ___) | |_| | |___| |___
  \____/_/   \_\_|   |____/ \___/|_____|_____|

  %s  %s:%d
%s  tls fingerprint (sha256):
%s

`, id, ip, info.GRPCPort, status, fp)
	for _, p := range []string{"/dev/tty0", "/dev/console"} {
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		_, _ = f.WriteString(banner)
		_ = f.Close()
	}
}

// ipForCmd returns the IP shown in command examples. Falls back to a
// placeholder when DHCP hasn't produced an address — keeps the command
// in the banner copy-pasteable rather than literally printing "(no IP)".
func ipForCmd(ip string) string {
	if strings.HasPrefix(ip, "(") {
		return "<capsule-ip>"
	}
	return ip
}

// formatFingerprintForBanner groups a hex fingerprint into colon-
// separated bytes, 8 bytes per row, indented for the banner. Avoids
// importing the auth package here (boot is a lower layer).
func formatFingerprintForBanner(hex string) string {
	if hex == "" {
		return "    (none)"
	}
	if len(hex)%2 != 0 {
		return "    " + hex
	}
	var (
		out []byte
		col int
	)
	out = append(out, []byte("    ")...)
	for i := 0; i < len(hex); i += 2 {
		if col == 8 {
			out = append(out, '\n')
			out = append(out, []byte("    ")...)
			col = 0
		} else if col > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[i], hex[i+1])
		col++
	}
	return string(out)
}

// defaultIPv4 returns the first non-loopback IPv4 address in dotted-quad
// form, or "" if none. Used by PrintBanner so HDMI shows the IP an
// operator should point capsulectl at.
func defaultIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ipv4 := ipnet.IP.To4()
			if ipv4 != nil && !ipv4.IsLoopback() {
				return ipv4.String()
			}
		}
	}
	return ""
}
