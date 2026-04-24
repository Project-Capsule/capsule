package boot

import (
	"context"
	"log/slog"
)

// ReapZombies runs a zombie-reaper loop until ctx is cancelled. It is a
// no-op on non-Linux platforms. Should be started in its own goroutine
// by the daemon when it is running as PID 1.
func ReapZombies(ctx context.Context) {
	if err := reapLoop(ctx); err != nil {
		slog.Error("reap loop exited with error", "err", err)
	}
}
