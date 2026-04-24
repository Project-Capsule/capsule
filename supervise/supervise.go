// Package supervise runs child processes under an exponential-backoff
// restart policy. capsuled uses it for containerd and, later, for per-VM
// firecracker processes.
package supervise

import (
	"context"
	"log/slog"
	"os/exec"
	"time"
)

// Config describes a supervised child.
type Config struct {
	// Name is a human-friendly identifier used only in logs.
	Name string
	// Path is the absolute path of the binary to exec.
	Path string
	// Args are arguments excluding argv[0].
	Args []string
	// Env is the environment; nil inherits from capsuled.
	Env []string

	// Backoff bounds. Zero values are replaced with sensible defaults.
	MinRestart time.Duration
	MaxRestart time.Duration
}

const (
	defaultMinRestart = 200 * time.Millisecond
	defaultMaxRestart = 30 * time.Second
)

// Run supervises cfg until ctx is cancelled. It blocks; callers typically
// invoke it in its own goroutine.
func Run(ctx context.Context, cfg Config) {
	minBackoff := cfg.MinRestart
	if minBackoff <= 0 {
		minBackoff = defaultMinRestart
	}
	maxBackoff := cfg.MaxRestart
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxRestart
	}

	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		cmd := exec.CommandContext(ctx, cfg.Path, cfg.Args...)
		cmd.Env = cfg.Env
		// Stdout/stderr are wired to capsuled's stdio so output lands in the
		// serial console / journald / whatever wraps capsuled.
		cmd.Stdout = stdoutSink(cfg.Name)
		cmd.Stderr = stdoutSink(cfg.Name)

		err := cmd.Run()
		// Context cancellation is the clean shutdown path — treat it as done.
		if ctx.Err() != nil {
			return
		}

		// Reset backoff if the child ran for a while before dying.
		if time.Since(started) > 10*time.Second {
			backoff = minBackoff
		}

		slog.Error("supervised child exited; restarting",
			"name", cfg.Name,
			"err", err,
			"backoff", backoff.String(),
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxBackoff)
	}
}
