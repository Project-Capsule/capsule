//go:build linux

package boot

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

func reapLoop(ctx context.Context) error {
	ch := make(chan os.Signal, 32)
	signal.Notify(ch, unix.SIGCHLD)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			drainZombies()
		}
	}
}

func drainZombies() {
	// Wait for any in-flight exec.Cmd to finish reaping its own child
	// before we wait4(-1). Without this, the reaper races with
	// exec.Cmd.Wait and the caller sees "waitid: no child processes".
	ExecMu.Lock()
	defer ExecMu.Unlock()
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		slog.Debug("reaped child", "pid", pid, "exit_status", ws.ExitStatus())
	}
}
