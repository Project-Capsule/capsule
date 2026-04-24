//go:build !linux

// Stub on non-Linux platforms. capsuled only runs on Linux, but capsulectl and
// unit tests should still compile on macOS for development.
package firecracker

import (
	"context"
	"errors"
	"io"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
)

// ErrUnsupported is returned from every call on non-Linux platforms.
var ErrUnsupported = errors.New("firecracker driver is Linux-only")

type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) EnsureRunning(_ context.Context, _ *capsulev1.Workload) error {
	return ErrUnsupported
}
func (d *Driver) Remove(_ context.Context, _ string) error { return ErrUnsupported }
func (d *Driver) Status(_ context.Context, _ string) (runtime.Status, error) {
	return runtime.Status{Phase: runtime.PhaseUnknown}, nil
}
func (d *Driver) Logs(_ context.Context, _ string, _ bool, _ int) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}
func (d *Driver) SerialLogs(_ context.Context, _ string, _ bool) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}
func (d *Driver) Exec(_ context.Context, _ runtime.ExecRequest) (int, error) {
	return -1, ErrUnsupported
}
