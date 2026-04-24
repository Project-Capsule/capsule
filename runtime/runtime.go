// Package runtime defines the runtime adapter port: the interface core
// uses to make workloads actually run on this capsule. Concrete drivers
// live in runtime/container (containerd) and, in later phases,
// runtime/microvm/firecracker and runtime/microvm/smolvm.
package runtime

import (
	"context"
	"io"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// Phase describes the observed lifecycle state of a runtime-managed workload.
type Phase int

const (
	PhaseUnknown Phase = iota
	PhasePending
	PhaseRunning
	PhaseStopped
	PhaseFailed
)

func (p Phase) String() string {
	switch p {
	case PhasePending:
		return "Pending"
	case PhaseRunning:
		return "Running"
	case PhaseStopped:
		return "Stopped"
	case PhaseFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// Status is a snapshot of the observed runtime state for a workload.
type Status struct {
	Phase        Phase
	Message      string
	RestartCount uint32
}

// ContainerDriver runs Container-kind workloads. EnsureRunning is the
// idempotent desired-state operation the reconciler calls every tick;
// Remove is the idempotent teardown; Status returns observed state.
type ContainerDriver interface {
	// EnsureRunning makes sure a container matching spec.Container is
	// running under the workload's Name. Pulls the image if missing.
	// Must be idempotent: calling it while a container is already
	// running should do nothing.
	EnsureRunning(ctx context.Context, w *capsulev1.Workload) error

	// Remove stops and deletes the container for the named workload.
	// Returns nil if nothing is there (idempotent).
	Remove(ctx context.Context, name string) error

	// Status returns the observed runtime state for the named workload.
	// Returns a Status with PhaseUnknown and no error when the workload
	// is not known to the runtime.
	Status(ctx context.Context, name string) (Status, error)

	// LogPath returns the filesystem path of the combined stdout+stderr
	// log for the named workload. The file may not exist yet if the
	// workload has never been started.
	LogPath(name string) string

	// Exec runs a one-shot process inside a running container. It
	// pipes stdio through the provided Streams and returns the process
	// exit code when it terminates. The ResizeChan, if non-nil, is
	// drained and forwarded to the PTY while the process is running.
	Exec(ctx context.Context, req ExecRequest) (int, error)
}

// ExecRequest is the parameter bag for ContainerDriver.Exec.
type ExecRequest struct {
	// Workload name — the container must already be running.
	Name string
	// Command is argv, required, first element is the executable.
	Command []string
	// Env are extra environment variables, merged with the container's.
	Env map[string]string
	// TTY: if true, allocate a PTY (stdout-only from the driver).
	TTY bool

	// Stdin is read until closed (nil means no stdin).
	Stdin io.Reader
	// Stdout and Stderr receive process output. In TTY mode only Stdout
	// is written; Stderr can be nil.
	Stdout io.Writer
	Stderr io.Writer

	// ResizeCh receives terminal resize events when TTY is true.
	// The driver reads from this channel until Ctx is cancelled or the
	// channel is closed.
	ResizeCh <-chan TermSize
}

// TermSize is a terminal geometry hint used by Exec's PTY mode.
type TermSize struct {
	Cols uint32
	Rows uint32
}

// VMDriver runs MicroVM-kind workloads. Shape mirrors ContainerDriver
// so the reconciler can treat both uniformly. Logs/Exec for VMs flow
// through the capsule-guest agent inside the VM (vsock) rather than
// containerd tasks, but the caller-facing semantics are the same.
type VMDriver interface {
	// EnsureRunning idempotently starts (or leaves running) the VM for
	// this workload. If a VM already exists for the name and is alive,
	// returns nil. Otherwise creates resources, launches the hypervisor,
	// and returns once the VM's init sequence has been commanded to start.
	EnsureRunning(ctx context.Context, w *capsulev1.Workload) error

	// Remove stops and tears down all VM resources for the given name.
	// Idempotent.
	Remove(ctx context.Context, name string) error

	// Status returns the observed runtime state for the named VM.
	Status(ctx context.Context, name string) (Status, error)

	// Logs streams the payload's combined stdout+stderr from the guest
	// agent. Follow tails until the payload exits or ctx is cancelled.
	Logs(ctx context.Context, name string, follow bool, tailLines int) (io.ReadCloser, error)

	// SerialLogs streams the VM's serial console (kernel boot, capsule-guest
	// bootstrap, Firecracker's own logs). Useful for debugging VMs that
	// never became reachable. Data comes from the capsule-side file
	// Firecracker tees to, so this is readable even if the guest agent
	// never came up.
	SerialLogs(ctx context.Context, name string, follow bool) (io.ReadCloser, error)

	// Exec runs a one-shot process inside the VM's payload mount
	// namespace via the guest agent. Mirrors ContainerDriver.Exec.
	Exec(ctx context.Context, req ExecRequest) (int, error)
}
