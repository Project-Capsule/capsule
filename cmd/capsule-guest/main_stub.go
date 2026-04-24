//go:build !linux

// capsule-guest only runs on Linux (it's a microVM PID 1). On macOS etc.
// we still want `go build ./...` to succeed for tooling and capsulectl, so
// ship a trivial stub.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "capsule-guest only runs on Linux microVMs")
	os.Exit(1)
}
