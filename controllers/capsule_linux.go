//go:build linux

package controllers

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/geekgonecrazy/capsule/boot"
)

func uname() unameInfo {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return unameInfo{}
	}
	return unameInfo{
		release: cstr(u.Release[:]),
		version: cstr(u.Version[:]),
		machine: cstr(u.Machine[:]),
	}
}

func uptimeSeconds() uint64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	if si.Uptime < 0 {
		return 0
	}
	return uint64(si.Uptime)
}

// memInfo reads /proc/meminfo and returns (total, available) bytes.
// Returns (0, 0) on failure (dev mode, missing /proc).
func memInfo() (total, available uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = meminfoKB(line) * 1024
		case strings.HasPrefix(line, "MemAvailable:"):
			available = meminfoKB(line) * 1024
		}
	}
	return total, available
}

// meminfoKB extracts the kB value from a /proc/meminfo line like
// "MemTotal:        2027548 kB". Returns 0 if not parseable.
func meminfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// cpuInfo returns (logical cpu count, model name). Falls back to
// runtime.NumCPU + "" if /proc/cpuinfo isn't readable.
func cpuInfo() (uint32, string) {
	count := uint32(runtime.NumCPU())
	model := ""
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if model == "" && strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					model = strings.TrimSpace(line[i+1:])
				}
			}
		}
	}
	return count, model
}

// diskInfo returns (boot disk path, total bytes). Empty + 0 on failure
// (dev mode, missing PARTUUID, etc.).
func diskInfo() (string, uint64) {
	dev, err := boot.BootDisk()
	if err != nil {
		return "", 0
	}
	size, err := blockSize(dev)
	if err != nil {
		return dev, 0
	}
	return dev, size
}

// blockSize calls BLKGETSIZE64 on a block device and returns its size in bytes.
func blockSize(dev string) (uint64, error) {
	fd, err := unix.Open(dev, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(fd)
	const BLKGETSIZE64 = 0x80081272
	var n uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(BLKGETSIZE64), uintptr(unsafe.Pointer(&n))); errno != 0 {
		return 0, errno
	}
	return n, nil
}

// thinpoolUsage queries LVM for the capsule VG's thin-pool data usage.
// Returns (total bytes, used bytes). Zeros on error or missing VG.
//
// Uses `lvs --noheadings --units b --nosuffix -o lv_size,data_percent
// capsule/thinpool` which prints e.g. "  10737418240  42.50".
func thinpoolUsage() (total, used uint64) {
	cmd := exec.Command("/sbin/lvs",
		"--noheadings", "--units", "b", "--nosuffix",
		"-o", "lv_size,data_percent",
		"capsule/thinpool")
	boot.ExecMu.Lock()
	out, err := cmd.Output()
	boot.ExecMu.Unlock()
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 {
		return 0, 0
	}
	total, err = strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0
	}
	pct, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return total, 0
	}
	used = uint64(float64(total) * pct / 100.0)
	return total, used
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
