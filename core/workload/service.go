// Package workload is the business logic for managing Workload records.
// It owns the rules — what's a valid workload, how Apply mutates state —
// and has no knowledge of gRPC or SQL. It drives the runtime via the
// interfaces in package runtime and persists state via package store.
package workload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
	"github.com/geekgonecrazy/capsule/store"
	"google.golang.org/protobuf/proto"
)

// Service owns Workload lifecycle: persist desired state; tear down runtime
// state on delete. Reconcile (desired → actual) lives in core/reconciler.
type Service struct {
	store  store.Store
	driver runtime.ContainerDriver
	vm     runtime.VMDriver
}

// New returns a Service wired to the given store and drivers. Either
// driver may be nil; if so, Delete still cleans up store state but
// Apply of workloads whose kind requires that driver will fail.
func New(s store.Store, d runtime.ContainerDriver, vm runtime.VMDriver) *Service {
	return &Service{store: s, driver: d, vm: vm}
}

// Apply validates the workload, persists it as-is (without status), and
// returns the stored record. The reconciler picks up the new desired
// state on its next tick.
func (s *Service) Apply(ctx context.Context, w *capsulev1.Workload) (*capsulev1.Workload, error) {
	if err := validate(w); err != nil {
		return nil, err
	}

	// Preserve any existing Status — Apply is about desired spec, not
	// observed status. If we just Put the request we'd blow away status
	// that the reconciler has populated.
	existing, err := s.store.Workloads().Get(ctx, w.GetName())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	merged := proto.Clone(w).(*capsulev1.Workload)
	if existing != nil {
		merged.Status = existing.GetStatus()
	}
	if err := s.store.Workloads().Put(ctx, merged); err != nil {
		return nil, err
	}
	return merged, nil
}

// Get returns a workload by name.
func (s *Service) Get(ctx context.Context, name string) (*capsulev1.Workload, error) {
	return s.store.Workloads().Get(ctx, name)
}

// List returns every persisted workload.
func (s *Service) List(ctx context.Context) ([]*capsulev1.Workload, error) {
	return s.store.Workloads().List(ctx)
}

// Delete removes a workload from the store AND asks the appropriate
// runtime to tear down any corresponding resources. Safe on missing name.
func (s *Service) Delete(ctx context.Context, name string) error {
	// Look up kind so we know which driver to call. Missing in store
	// (already-deleted row, or never existed) → best-effort against
	// the container driver.
	w, err := s.store.Workloads().Get(ctx, name)
	var kind capsulev1.WorkloadKind
	if err == nil && w != nil {
		kind = w.GetKind()
	}

	// Mark the row as DELETING before touching the runtime so the
	// reconciler stops trying to keep it Running. This avoids the
	// classic race: driver.Remove takes a couple of seconds to SIGKILL
	// + unmount + delete; the reconciler ticks in the gap, sees
	// desired=Running, and starts a fresh container before our Remove
	// completes. If Remove fails, the row stays at DELETING — operator
	// can retry `workload delete` and it finishes cleanly without
	// re-applying the marker.
	if w != nil && w.GetDesiredState() != capsulev1.DesiredState_DESIRED_STATE_DELETING {
		w.DesiredState = capsulev1.DesiredState_DESIRED_STATE_DELETING
		if err := s.store.Workloads().Put(ctx, w); err != nil {
			return fmt.Errorf("mark workload %q deleting: %w", name, err)
		}
	}

	switch kind {
	case capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM:
		if s.vm != nil {
			if err := s.vm.Remove(ctx, name); err != nil {
				return fmt.Errorf("runtime vm remove %q: %w", name, err)
			}
		}
	default:
		if s.driver != nil {
			if err := s.driver.Remove(ctx, name); err != nil {
				return fmt.Errorf("runtime remove %q: %w", name, err)
			}
		}
	}
	// Runtime is gone (or never existed). Drop the store row.
	return s.store.Workloads().Delete(ctx, name)
}

// LogPath returns the path to a workload's combined stdout+stderr log
// file, or "" if no runtime driver is wired. Only meaningful for
// container workloads — VM workloads stream logs via OpenLogs.
func (s *Service) LogPath(name string) string {
	if s.driver == nil {
		return ""
	}
	return s.driver.LogPath(name)
}

// LogSource selects which log stream OpenLogs serves.
type LogSource int

const (
	LogSourcePayload LogSource = iota // default: app stdout+stderr
	LogSourceSerial                   // VM serial console (MicroVM only)
)

// OpenLogs returns a ReadCloser streaming the requested log for a
// workload. Dispatches by kind + source: containers open the on-disk
// log file from the container driver; VMs stream from the guest agent
// (payload) or the Firecracker serial capture (serial). Caller must
// Close when done.
func (s *Service) OpenLogs(ctx context.Context, name string, follow bool, tailLines int, source LogSource) (io.ReadCloser, error) {
	w, err := s.store.Workloads().Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if w.GetKind() == capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM {
		if s.vm == nil {
			return nil, ErrNoRuntime
		}
		if source == LogSourceSerial {
			return s.vm.SerialLogs(ctx, name, follow)
		}
		return s.vm.Logs(ctx, name, follow, tailLines)
	}
	if source == LogSourceSerial {
		return nil, fmt.Errorf("--serial is only valid for MicroVM workloads")
	}
	if s.driver == nil {
		return nil, ErrNoRuntime
	}
	path := s.driver.LogPath(name)
	if path == "" {
		return nil, os.ErrNotExist
	}
	return os.Open(path)
}

// Exec runs a one-shot process inside a workload. Dispatches by kind:
// containers use containerd tasks; VMs forward to the guest agent.
// Returns ErrNoRuntime when no appropriate driver is wired.
var ErrNoRuntime = errors.New("no runtime driver configured")

func (s *Service) Exec(ctx context.Context, req runtime.ExecRequest) (int, error) {
	w, err := s.store.Workloads().Get(ctx, req.Name)
	if err == nil && w != nil && w.GetKind() == capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM {
		if s.vm == nil {
			return -1, ErrNoRuntime
		}
		return s.vm.Exec(ctx, req)
	}
	if s.driver == nil {
		return -1, ErrNoRuntime
	}
	return s.driver.Exec(ctx, req)
}

// Restart tears down the runtime state for a workload. Desired state
// is unchanged (still RUNNING, so the reconciler recreates it on the
// next tick). Returns the underlying store error if the workload
// doesn't exist.
func (s *Service) Restart(ctx context.Context, name string) error {
	w, err := s.store.Workloads().Get(ctx, name)
	if err != nil {
		return err
	}
	switch w.GetKind() {
	case capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM:
		if s.vm == nil {
			return ErrNoRuntime
		}
		return s.vm.Remove(ctx, name)
	default:
		if s.driver == nil {
			return ErrNoRuntime
		}
		return s.driver.Remove(ctx, name)
	}
}

// Stop flips desired_state to STOPPED and tears down runtime state.
// The reconciler respects the flag and won't recreate the workload
// until Start is called. Idempotent.
func (s *Service) Stop(ctx context.Context, name string) error {
	w, err := s.store.Workloads().Get(ctx, name)
	if err != nil {
		return err
	}
	w.DesiredState = capsulev1.DesiredState_DESIRED_STATE_STOPPED
	if err := s.store.Workloads().Put(ctx, w); err != nil {
		return err
	}
	switch w.GetKind() {
	case capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM:
		if s.vm != nil {
			_ = s.vm.Remove(ctx, name)
		}
	default:
		if s.driver != nil {
			_ = s.driver.Remove(ctx, name)
		}
	}
	return nil
}

// Start flips desired_state to RUNNING. The reconciler picks it up and
// brings the workload back on its next tick. Idempotent. Also clears
// the previous "stopped by operator" status so `workload list` shows
// PENDING until the reconciler refreshes (instead of lingering on
// STOPPED while the VM is mid-boot).
func (s *Service) Start(ctx context.Context, name string) error {
	w, err := s.store.Workloads().Get(ctx, name)
	if err != nil {
		return err
	}
	w.DesiredState = capsulev1.DesiredState_DESIRED_STATE_RUNNING
	w.Status = &capsulev1.WorkloadStatus{
		Phase:   capsulev1.WorkloadPhase_WORKLOAD_PHASE_PENDING,
		Message: "starting",
	}
	return s.store.Workloads().Put(ctx, w)
}

// SetStatus is used by the reconciler to record observed state.
func (s *Service) SetStatus(ctx context.Context, name string, status *capsulev1.WorkloadStatus) error {
	w, err := s.store.Workloads().Get(ctx, name)
	if err != nil {
		return err
	}
	// Avoid a needless write if nothing changed.
	if proto.Equal(w.GetStatus(), status) {
		return nil
	}
	w.Status = status
	return s.store.Workloads().Put(ctx, w)
}

// --- validation -------------------------------------------------------------

// Names follow the Kubernetes-style DNS-1123-label rules, lightly.
// lowercase alphanumerics and '-', 1..63 chars, doesn't start or end with '-'.
var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validate(w *capsulev1.Workload) error {
	if w == nil {
		return errors.New("workload is nil")
	}
	if w.GetName() == "" {
		return errors.New("workload.name is required")
	}
	if len(w.GetName()) > 63 || !nameRE.MatchString(w.GetName()) {
		return fmt.Errorf("workload.name %q is invalid (expect DNS-1123 label, <=63 chars)", w.GetName())
	}
	switch w.GetKind() {
	case capsulev1.WorkloadKind_WORKLOAD_KIND_CONTAINER:
		if w.GetContainer() == nil {
			return errors.New("kind=Container requires container spec")
		}
		if w.GetContainer().GetImage() == "" {
			return errors.New("container.image is required")
		}
	case capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM:
		if w.GetMicroVm() == nil {
			return errors.New("kind=MicroVM requires micro_vm spec")
		}
		// Either image mode (easy) or BYO kernel+rootfs mode.
		if w.GetMicroVm().GetImage() == "" {
			if w.GetMicroVm().GetKernelPath() == "" {
				return errors.New("micro_vm: either image or kernel_path is required")
			}
			if w.GetMicroVm().GetRootfsPath() == "" {
				return errors.New("micro_vm.rootfs_path is required when image is not set")
			}
		}
	default:
		return errors.New("workload.kind is required (Container or MicroVM)")
	}
	return nil
}
