// Package reconciler is the level-triggered loop that drives the runtime
// to match the desired state persisted in the store. It's called on a
// fixed tick and is idempotent: running it twice in a row on a steady
// state does nothing the second time.
package reconciler

import (
	"context"
	"log/slog"
	"time"

	"github.com/geekgonecrazy/capsule/core/workload"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
)

// Config wires a Reconciler.
type Config struct {
	Service  *workload.Service
	Driver   runtime.ContainerDriver
	VM       runtime.VMDriver
	Interval time.Duration
}

// Reconciler runs a desired→actual reconciliation loop.
type Reconciler struct {
	cfg  Config
	wake chan struct{}
}

// New returns a Reconciler. Missing Interval defaults to 2 seconds.
func New(cfg Config) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	return &Reconciler{cfg: cfg, wake: make(chan struct{}, 1)}
}

// Kick requests an immediate reconciliation pass without waiting for the
// next tick. Safe to call from any goroutine; non-blocking — when a wake
// is already queued, additional Kicks coalesce into the queued one. Used
// by core/workload.Service to make Apply/Start/Stop/Restart/Delete feel
// responsive instead of paying up to one tick interval (~2s) of latency.
func (r *Reconciler) Kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run drives the reconciliation loop until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()

	// Tick once up front so applying a workload feels responsive.
	r.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		case <-r.wake:
			r.Tick(ctx)
		}
	}
}

// Tick runs one reconciliation pass. Exported for tests.
func (r *Reconciler) Tick(ctx context.Context) {
	ws, err := r.cfg.Service.List(ctx)
	if err != nil {
		slog.Error("reconciler: list workloads", "err", err)
		return
	}
	for _, w := range ws {
		r.reconcileOne(ctx, w)
	}
}

func (r *Reconciler) reconcileOne(ctx context.Context, w *capsulev1.Workload) {
	name := w.GetName()

	// Tombstone — Service.Delete is in the middle of tearing this down
	// and will remove the row when it's safe. Don't touch.
	if w.GetDesiredState() == capsulev1.DesiredState_DESIRED_STATE_DELETING {
		return
	}

	// Honor desired_state=STOPPED: ensure runtime state is torn down and
	// keep it that way. Default UNSPECIFIED is treated as RUNNING so
	// existing workloads keep working after the upgrade.
	if w.GetDesiredState() == capsulev1.DesiredState_DESIRED_STATE_STOPPED {
		switch w.GetKind() {
		case capsulev1.WorkloadKind_WORKLOAD_KIND_CONTAINER:
			if r.cfg.Driver != nil {
				_ = r.cfg.Driver.Remove(ctx, name)
			}
		case capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM:
			if r.cfg.VM != nil {
				_ = r.cfg.VM.Remove(ctx, name)
			}
		}
		r.writeStatus(ctx, name, capsulev1.WorkloadPhase_WORKLOAD_PHASE_STOPPED, "stopped by operator")
		return
	}

	var status runtime.Status
	var err error

	switch w.GetKind() {
	case capsulev1.WorkloadKind_WORKLOAD_KIND_CONTAINER:
		if r.cfg.Driver == nil {
			r.writeStatus(ctx, name, capsulev1.WorkloadPhase_WORKLOAD_PHASE_FAILED, "no container runtime")
			return
		}
		if ensureErr := r.cfg.Driver.EnsureRunning(ctx, w); ensureErr != nil {
			slog.Error("reconciler: ensure running", "workload", name, "err", ensureErr)
			r.writeStatus(ctx, name, capsulev1.WorkloadPhase_WORKLOAD_PHASE_FAILED, ensureErr.Error())
			return
		}
		status, err = r.cfg.Driver.Status(ctx, name)
	case capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM:
		if r.cfg.VM == nil {
			r.writeStatus(ctx, name, capsulev1.WorkloadPhase_WORKLOAD_PHASE_FAILED, "no vm runtime")
			return
		}
		if ensureErr := r.cfg.VM.EnsureRunning(ctx, w); ensureErr != nil {
			slog.Error("reconciler: ensure vm", "workload", name, "err", ensureErr)
			r.writeStatus(ctx, name, capsulev1.WorkloadPhase_WORKLOAD_PHASE_FAILED, ensureErr.Error())
			return
		}
		status, err = r.cfg.VM.Status(ctx, name)
	default:
		r.writeStatus(ctx, name, capsulev1.WorkloadPhase_WORKLOAD_PHASE_FAILED, "unsupported kind")
		return
	}
	if err != nil {
		slog.Error("reconciler: status", "workload", name, "err", err)
		return
	}
	r.writeStatus(ctx, name, toProtoPhase(status.Phase), status.Message)
}

func (r *Reconciler) writeStatus(ctx context.Context, name string, phase capsulev1.WorkloadPhase, msg string) {
	ps := &capsulev1.WorkloadStatus{Phase: phase, Message: msg}
	if err := r.cfg.Service.SetStatus(ctx, name, ps); err != nil {
		slog.Debug("reconciler: set status", "workload", name, "err", err)
	}
}

func toProtoPhase(p runtime.Phase) capsulev1.WorkloadPhase {
	switch p {
	case runtime.PhaseRunning:
		return capsulev1.WorkloadPhase_WORKLOAD_PHASE_RUNNING
	case runtime.PhasePending:
		return capsulev1.WorkloadPhase_WORKLOAD_PHASE_PENDING
	case runtime.PhaseStopped:
		return capsulev1.WorkloadPhase_WORKLOAD_PHASE_STOPPED
	case runtime.PhaseFailed:
		return capsulev1.WorkloadPhase_WORKLOAD_PHASE_FAILED
	default:
		return capsulev1.WorkloadPhase_WORKLOAD_PHASE_UNSPECIFIED
	}
}
