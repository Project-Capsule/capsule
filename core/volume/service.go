// Package volume is the business logic for managing persistent volumes.
// Every volume is a raw ext4 file at /perm/volumes/<name>.ext4. VM
// workloads get the file attached as a virtio-blk device; container
// workloads get it loop-mounted on the capsule and the mount point
// bind-mounted into the container. Same backing store, same data, one
// mounter at a time.
package volume

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/geekgonecrazy/capsule/boot"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
)

// Root is where volume ext4 files live on the capsule.
const Root = "/perm/volumes"

// DefaultSizeMiB is the size of a freshly-created volume ext4. Can grow
// later via `resize2fs` on the detached file — the wire API will expose
// that as `capsulectl volume resize` in a future pass.
const DefaultSizeMiB = 512

// ErrInUse is returned by Delete when workloads still reference the volume
// and force was not set.
var ErrInUse = errors.New("volume in use by one or more workloads")

// Service manages Volume CRUD. It owns both the SQLite row and the
// on-disk ext4 file; they must stay in sync.
type Service struct {
	store store.Store
}

// New returns a Service bound to the given store.
func New(s store.Store) *Service { return &Service{store: s} }

// Create materializes /perm/volumes/<name>.ext4 (sized DefaultSizeMiB,
// formatted ext4) and persists the metadata row. Errors if the volume
// already exists.
func (s *Service) Create(ctx context.Context, name string) (*capsulev1.Volume, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	existing, err := s.store.Volumes().Get(ctx, name)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("volume %q already exists", name)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	hostPath := HostPath(name)
	if err := os.MkdirAll(Root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", Root, err)
	}
	if err := createExt4Volume(hostPath, DefaultSizeMiB); err != nil {
		return nil, err
	}

	v := &capsulev1.Volume{
		Name:          name,
		HostPath:      hostPath,
		CreatedAtUnix: time.Now().Unix(),
	}
	if err := s.store.Volumes().Put(ctx, v); err != nil {
		_ = os.Remove(hostPath)
		return nil, err
	}
	return v, nil
}

// Get returns a volume plus populated mounted_by / size_bytes fields.
func (s *Service) Get(ctx context.Context, name string) (*capsulev1.Volume, error) {
	v, err := s.store.Volumes().Get(ctx, name)
	if err != nil {
		return nil, err
	}
	s.populateDerived(ctx, v)
	return v, nil
}

// List returns all volumes with derived fields populated.
func (s *Service) List(ctx context.Context) ([]*capsulev1.Volume, error) {
	vs, err := s.store.Volumes().List(ctx)
	if err != nil {
		return nil, err
	}
	ws, _ := s.store.Workloads().List(ctx)
	for _, v := range vs {
		v.MountedBy = usersFromWorkloads(ws, v.GetName())
		v.SizeBytes = fileSize(v.GetHostPath())
	}
	return vs, nil
}

// Delete removes the volume. Fails if any workload still references it,
// unless force is true. Removes both the store row and the ext4 file.
func (s *Service) Delete(ctx context.Context, name string, force bool) error {
	v, err := s.store.Volumes().Get(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	users, err := s.refsTo(ctx, name)
	if err != nil {
		return err
	}
	if len(users) > 0 && !force {
		return fmt.Errorf("%w: %v", ErrInUse, users)
	}
	if err := os.Remove(v.GetHostPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rm %s: %w", v.GetHostPath(), err)
	}
	return s.store.Volumes().Delete(ctx, name)
}

// Exists reports whether a volume with the given name is registered.
// Used by the container runtime driver to validate mounts.
func (s *Service) Exists(ctx context.Context, name string) (bool, error) {
	if _, err := s.store.Volumes().Get(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// HostPath returns /perm/volumes/<name>.ext4. Does not check existence.
func HostPath(name string) string { return filepath.Join(Root, name+".ext4") }

// --- internals -------------------------------------------------------------

// createExt4Volume truncates an empty file to the desired size and
// formats it ext4. Idempotent: if the file already exists at the right
// size and format, no-op.
func createExt4Volume(path string, sizeMiB int) error {
	if st, err := os.Stat(path); err == nil && st.Size() == int64(sizeMiB)*1024*1024 {
		return nil
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return err
	}
	if err := os.Truncate(path, int64(sizeMiB)*1024*1024); err != nil {
		return err
	}
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	cmd := exec.Command("/usr/sbin/mkfs.ext4", "-q", "-F", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		cmd = exec.Command("/sbin/mkfs.ext4", "-q", "-F", path)
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			_ = os.Remove(path)
			return fmt.Errorf("mkfs.ext4 %s: %w: %s / %s", path, err, string(out), string(out2))
		}
	}
	return nil
}

func (s *Service) populateDerived(ctx context.Context, v *capsulev1.Volume) {
	ws, _ := s.store.Workloads().List(ctx)
	v.MountedBy = usersFromWorkloads(ws, v.GetName())
	v.SizeBytes = fileSize(v.GetHostPath())
}

func (s *Service) refsTo(ctx context.Context, name string) ([]string, error) {
	ws, err := s.store.Workloads().List(ctx)
	if err != nil {
		return nil, err
	}
	return usersFromWorkloads(ws, name), nil
}

// usersFromWorkloads scans workload mount references for volume name
// and returns the list of workload names that use it. Handles both
// Container and MicroVM workloads.
func usersFromWorkloads(ws []*capsulev1.Workload, volume string) []string {
	var out []string
	for _, w := range ws {
		var mounts []*capsulev1.VolumeMount
		if c := w.GetContainer(); c != nil {
			mounts = c.GetMounts()
		}
		if vm := w.GetMicroVm(); vm != nil {
			mounts = append(mounts, vm.GetMounts()...)
		}
		for _, m := range mounts {
			if m.GetVolumeName() == volume {
				out = append(out, w.GetName())
				break
			}
		}
	}
	return out
}

func fileSize(path string) uint64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return uint64(st.Size())
}

// --- validation ------------------------------------------------------------

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validateName(name string) error {
	if name == "" {
		return errors.New("volume name is required")
	}
	if len(name) > 63 || !nameRE.MatchString(name) {
		return fmt.Errorf("volume name %q is invalid (DNS-1123 label, <=63 chars)", name)
	}
	return nil
}
