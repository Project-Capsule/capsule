package supervise

import (
	"io"
	"log/slog"
	"os"
)

// stdoutSink returns a Writer that forwards lines to slog.Info with a child
// name prefix. For phase 1 we use os.Stderr directly so containerd's output
// lands on the serial console verbatim; the slog-backed sink is kept here
// for future use when we want structured logs per child.
func stdoutSink(name string) io.Writer {
	_ = name // reserved for slog-based routing
	_ = slog.LevelInfo
	return os.Stderr
}
