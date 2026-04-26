// Package boot handles PID 1 duties on a capsule: early filesystem
// mounts, loopback setup, zombie reaping, and signal handling for clean
// shutdown. Init should only be called when running as PID 1.
package boot

import (
	"context"
	"fmt"
	"net"
	"os"
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
}

// Init performs PID 1 setup. On non-Linux it is a no-op (allows the daemon
// to be built and run on macOS during development). On Linux it mounts the
// early filesystems and brings up loopback; the caller is then responsible
// for starting supervised children and entering its own signal loop.
func Init(ctx context.Context) (Result, error) {
	return initPlatform(ctx)
}

// PrintBanner writes a CAPSULE ASCII banner + current IP to /dev/tty0
// (HDMI on real hardware) and /dev/console. Operator's "where do I point
// capsulectl?" answer when the only output is HDMI. Best-effort: failing
// to open either device is silent (running off PID 1, on macOS, etc.).
func PrintBanner(grpcPort int) {
	ip := defaultIPv4()
	target := ip
	if ip == "" {
		ip = "(no IP — DHCP failed?)"
		target = "<capsule-ip>"
	}
	banner := fmt.Sprintf(`
   ____    _    ____  ____  _   _ _     _____
  / ___|  / \  |  _ \/ ___|| | | | |   | ____|
 | |     / _ \ | |_) \___ \| | | | |   |  _|
 | |___ / ___ \|  __/ ___) | |_| | |___| |___
  \____/_/   \_\_|   |____/ \___/|_____|_____|

  ip: %s
  capsulectl --capsule %s:%d capsule info

`, ip, target, grpcPort)
	for _, p := range []string{"/dev/tty0", "/dev/console"} {
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		_, _ = f.WriteString(banner)
		_ = f.Close()
	}
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
