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

func uptimeSeconds() uint64                        { return 0 }
func memInfo() (uint64, uint64)                    { return 0, 0 }
func cpuInfo() (uint32, string)                    { return uint32(runtime.NumCPU()), "" }
func diskInfo() (string, uint64)                   { return "", 0 }
func thinpoolUsage() (uint64, uint64)              { return 0, 0 }
