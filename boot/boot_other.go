//go:build !linux

package boot

import (
	"context"
	"log/slog"
)

func initPlatform(_ context.Context) (Result, error) {
	slog.Info("boot.Init: non-Linux build, skipping PID 1 setup")
	return Result{}, nil
}
