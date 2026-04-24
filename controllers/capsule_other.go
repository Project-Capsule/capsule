//go:build !linux

package controllers

import "runtime"

func uname() unameInfo {
	return unameInfo{
		release: "dev",
		version: "dev",
		machine: runtime.GOARCH,
	}
}

func uptimeSeconds() uint64 { return 0 }
