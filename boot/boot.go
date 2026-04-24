// Package boot handles PID 1 duties on a capsule: early filesystem
// mounts, loopback setup, zombie reaping, and signal handling for clean
// shutdown. Init should only be called when running as PID 1.
package boot

import "context"

// Result reports the outcome of running Init.
type Result struct {
	// MountedPerm is true if the /perm partition was mounted successfully.
	MountedPerm bool
}

// Init performs PID 1 setup. On non-Linux it is a no-op (allows the daemon
// to be built and run on macOS during development). On Linux it mounts the
// early filesystems and brings up loopback; the caller is then responsible
// for starting supervised children and entering its own signal loop.
func Init(ctx context.Context) (Result, error) {
	return initPlatform(ctx)
}
