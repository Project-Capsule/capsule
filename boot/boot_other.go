//go:build !linux

package boot

import (
	"context"
	"errors"
	"log/slog"
)

func initPlatform(_ context.Context) (Result, error) {
	slog.Info("boot.Init: non-Linux build, skipping PID 1 setup")
	return Result{}, nil
}

// FindPartitionByNumber is a no-op on non-Linux. Capsule's PID-1 paths
// only run on Linux; this stub keeps the package importable for tooling
// builds (e.g. capsulectl on macOS).
func FindPartitionByNumber(int) (string, error) {
	return "", errors.New("not implemented on non-Linux")
}

// BootDisk is a no-op on non-Linux. See FindPartitionByNumber.
func BootDisk() (string, error) {
	return "", errors.New("not implemented on non-Linux")
}
