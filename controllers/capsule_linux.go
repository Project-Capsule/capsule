//go:build linux

package controllers

import (
	"golang.org/x/sys/unix"
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

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
