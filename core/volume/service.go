// Package volume is the business logic for managing persistent volumes.
// Each volume is a thin-provisioned LVM logical volume in the capsule VG,
// formatted ext4 directly on the block device. Containers mount the LV at
// /run/capsule/mounts/<workload>/<name> then bind-mount into the container;
// MicroVMs attach the LV as a virtio-blk device. Same block device, one
// mounter at a time.
//
// Thin provisioning means `capsulectl volume create foo --size 10GiB`
// declares a 10 GiB capacity but consumes only filesystem metadata on disk
// (~a few MiB) until the guest writes to it. Multiple volumes share the
// pool's free extents until one fills it.
package volume

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/geekgonecrazy/capsule/boot"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
)

// LVM layout assumptions. Set up at image-build time by pack.sh and
// activated at boot by boot.mountPerm. The driver never creates the VG or
// thinpool itself — if they're missing, volume ops fail with a clear error
// rather than silently reformatting the operator's disk.
const (
	// VGName is the capsule volume group. Contains meta LV (ext4 at /perm)
	// and the thin pool that backs user volumes plus containerd snapshots.
	VGName = "capsule"
	// ThinPoolName is the thin pool inside VGName that backs user volumes
	// and containerd devmapper-snapshotter LVs.
	ThinPoolName = "thinpool"
	// VolPrefix is prepended to every user-volume LV name so we can
	// distinguish them from internal LVs (meta, containerd layers) when
	// listing. A volume named "mydata" lives at /dev/capsule/vol-mydata.
	VolPrefix = "vol-"
)

// DefaultSizeMiB is the provisioned size applied when a Create request
// omits size_mib. Thin-provisioned; on-disk cost is negligible until the
// guest writes.
const DefaultSizeMiB uint64 = 512

// Size bounds enforced by validateSize. Min avoids mkfs.ext4 metadata
// overhead unhappiness; max is a sanity guardrail (capsule /perm pools
// are usually far smaller anyway).
const (
	MinSizeMiB uint64 = 32
	MaxSizeMiB uint64 = 1024 * 1024 // 1 TiB
)

// ErrInUse is returned by Delete/Resize when workloads still reference the
// volume (and force was not set, for Delete).
var ErrInUse = errors.New("volume in use by one or more workloads")

// ErrShrink is returned by Resize when new_size_mib < current size.
var ErrShrink = errors.New("cannot shrink volume (grow-only)")

// Service manages Volume CRUD. It owns the SQLite row and the LVM LV;
// they must stay in sync.
type Service struct {
	store store.Store
}

// New returns a Service bound to the given store.
func New(s store.Store) *Service { return &Service{store: s} }

// Create materializes /dev/capsule/vol-<name> as a thin LV of the given
// size (MiB), formats it ext4, and persists the metadata row. sizeMiB == 0
// means DefaultSizeMiB. Errors if the volume already exists.
func (s *Service) Create(ctx context.Context, name string, sizeMiB uint64) (*capsulev1.Volume, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if sizeMiB == 0 {
		sizeMiB = DefaultSizeMiB
	}
	if err := validateSize(sizeMiB); err != nil {
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
	if err := createVolumeLV(name, sizeMiB); err != nil {
		return nil, err
	}

	v := &capsulev1.Volume{
		Name:          name,
		HostPath:      hostPath,
		CreatedAtUnix: time.Now().Unix(),
	}
	if err := s.store.Volumes().Put(ctx, v); err != nil {
		_ = removeVolumeLV(name)
		return nil, err
	}
	slog.Info("volume create", "name", name, "size_mib", sizeMiB)
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
		v.SizeBytes = blockDeviceSize(v.GetHostPath())
	}
	return vs, nil
}

// Delete removes the volume. Fails if any workload still references it,
// unless force is true. Removes both the store row and the LV.
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
	if err := removeVolumeLV(name); err != nil {
		return fmt.Errorf("lvremove %s: %w", v.GetHostPath(), err)
	}
	if err := s.store.Volumes().Delete(ctx, name); err != nil {
		return err
	}
	slog.Info("volume delete", "name", name, "force", force)
	return nil
}

// Resize grows an existing volume to newSizeMiB MiB. Grow-only; shrink
// rejected. Volume must be detached (no workload references) — ext4
// resize on a mounted filesystem may work in-kernel but detaching keeps
// the semantics predictable and matches the Delete contract.
func (s *Service) Resize(ctx context.Context, name string, newSizeMiB uint64) (*capsulev1.Volume, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if err := validateSize(newSizeMiB); err != nil {
		return nil, err
	}
	v, err := s.store.Volumes().Get(ctx, name)
	if err != nil {
		return nil, err
	}
	users, err := s.refsTo(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(users) > 0 {
		return nil, fmt.Errorf("%w: %v", ErrInUse, users)
	}

	current := blockDeviceSize(v.GetHostPath())
	want := newSizeMiB * 1024 * 1024
	if want < current {
		return nil, fmt.Errorf("%w: current=%d MiB, requested=%d MiB", ErrShrink, current/1024/1024, newSizeMiB)
	}
	if want == current {
		// Nothing to do. Still populate derived and return.
		s.populateDerived(ctx, v)
		return v, nil
	}
	if err := resizeVolumeLV(name, newSizeMiB); err != nil {
		return nil, err
	}
	slog.Info("volume resize", "name", name, "from_mib", current/1024/1024, "to_mib", newSizeMiB)
	s.populateDerived(ctx, v)
	return v, nil
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

// HostPath returns /dev/<vg>/<VolPrefix><name>. Does not check existence.
// /dev/<vg>/<lv> is the LVM udev-generated symlink to the dm node and
// is stable across reboots.
func HostPath(name string) string {
	return "/dev/" + VGName + "/" + VolPrefix + name
}

// LVName returns the LV's short name (VolPrefix + user name) as used with
// lvcreate/lvremove/lvresize's -n / --name flag.
func LVName(name string) string { return VolPrefix + name }

// --- LVM shell-out helpers ------------------------------------------------
//
// All LVM commands are gated by boot.ExecMu so their child-exit signals
// don't race capsuled's PID-1 reaper loop.

func createVolumeLV(name string, sizeMiB uint64) error {
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()

	// lvcreate -V <size>M -T <vg>/<thinpool> -n <vol>
	lv := LVName(name)
	args := []string{
		"-V", fmt.Sprintf("%dM", sizeMiB),
		"-T", VGName + "/" + ThinPoolName,
		"-n", lv,
	}
	slog.Info("createVolumeLV", "name", name, "size_mib", sizeMiB, "args", args)
	cmd := exec.Command("/sbin/lvcreate", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("lvcreate %s: %w: %s", lv, err, strings.TrimSpace(string(out)))
	}

	// mkfs.ext4 -q -F /dev/<vg>/<lv>
	dev := HostPath(name)
	if out, err := exec.Command("/usr/sbin/mkfs.ext4", "-q", "-F", dev).CombinedOutput(); err != nil {
		if out2, err2 := exec.Command("/sbin/mkfs.ext4", "-q", "-F", dev).CombinedOutput(); err2 != nil {
			// Roll back the LV so Create leaves no partial state.
			_ = exec.Command("/sbin/lvremove", "-y", VGName+"/"+lv).Run()
			return fmt.Errorf("mkfs.ext4 %s: %w / %w: %s / %s",
				dev, err, err2, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

func removeVolumeLV(name string) error {
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	lv := VGName + "/" + LVName(name)
	out, err := exec.Command("/sbin/lvremove", "-y", lv).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// Already gone — idempotent path; don't fail Delete.
		if strings.Contains(msg, "not found") || strings.Contains(msg, "Failed to find") {
			return nil
		}
		return fmt.Errorf("lvremove %s: %w: %s", lv, err, msg)
	}
	return nil
}

func resizeVolumeLV(name string, newSizeMiB uint64) error {
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	lv := VGName + "/" + LVName(name)
	dev := HostPath(name)
	// lvresize -L <size>M <vg>/<lv>. Grow-only is enforced above; the
	// kernel will also refuse a shrink without -f even if we asked for one.
	if out, err := exec.Command("/sbin/lvresize",
		"-L", fmt.Sprintf("%dM", newSizeMiB), lv,
	).CombinedOutput(); err != nil {
		return fmt.Errorf("lvresize %s: %w: %s", lv, err, strings.TrimSpace(string(out)))
	}
	// e2fsck -f is required by resize2fs whenever the ext4 has any
	// unclean/needs-recovery flag — common if the last mounter was
	// killed rather than cleanly unmounted. -y auto-answers prompts
	// from a trusted filesystem image (we own it; no interactive mode
	// makes sense here).
	if err := runExt4Bin("e2fsck", "-f", "-y", dev); err != nil {
		// e2fsck exit codes: 0 = no errors, 1 = errors corrected, 2 =
		// system should reboot. 1 is fine for our purposes.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			// corrected, keep going
		} else {
			return fmt.Errorf("e2fsck %s: %w", dev, err)
		}
	}
	// resize2fs grows the ext4 inside to the new LV size.
	if err := runExt4Bin("resize2fs", dev); err != nil {
		return fmt.Errorf("resize2fs %s: %w", dev, err)
	}
	return nil
}

// runExt4Bin runs an e2fsprogs tool by unqualified name, trying /usr/sbin
// and /sbin in turn. Returns the first success, or the last error with
// combined output from both attempts.
func runExt4Bin(bin string, args ...string) error {
	for _, prefix := range []string{"/usr/sbin/", "/sbin/"} {
		cmd := exec.Command(prefix+bin, args...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		// If the binary exists but exited non-zero, return that error
		// directly (caller may inspect exit code). If path didn't
		// exist, try the next prefix.
		if !strings.Contains(err.Error(), "no such file") {
			return fmt.Errorf("%s%s %v: %w: %s", prefix, bin, args, err, strings.TrimSpace(string(out)))
		}
	}
	return fmt.Errorf("%s not found in /usr/sbin or /sbin", bin)
}

// blockDeviceSize returns the size in bytes of a block device via
// BLKGETSIZE64. Zero on any error (missing device, not a block device,
// permission denied) — matches the old fileSize semantics for callers
// that treat 0 as "unknown".
func blockDeviceSize(path string) uint64 {
	if path == "" {
		return 0
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0
	}
	defer unix.Close(fd)
	var n uint64
	// BLKGETSIZE64: size in bytes, as uint64. ioctl number defined by
	// the kernel as _IOR(0x12, 114, size_t).
	const BLKGETSIZE64 = 0x80081272
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(BLKGETSIZE64), uintptr(unsafe.Pointer(&n))); errno != 0 {
		// Fallback to lvs for callers that might hand us a path that's
		// only a symlink until udev catches up.
		if size, ok := lvmReportedSize(path); ok {
			return size
		}
		return 0
	}
	return n
}

// lvmReportedSize consults `lvs --units b --nosuffix -o lv_size` for the
// LV behind path. Used as a fallback for blockDeviceSize before the device
// node is ready. path is expected as /dev/<vg>/<lv>; any other shape → 0.
func lvmReportedSize(path string) (uint64, bool) {
	// /dev/<vg>/<lv>
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "dev" {
		return 0, false
	}
	vg, lv := parts[2], parts[3]
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	out, err := exec.Command("/sbin/lvs",
		"--units", "b", "--nosuffix", "--noheadings",
		"-o", "lv_size", vg+"/"+lv,
	).Output()
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(out))
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// --- populate / refs ------------------------------------------------------

func (s *Service) populateDerived(ctx context.Context, v *capsulev1.Volume) {
	ws, _ := s.store.Workloads().List(ctx)
	v.MountedBy = usersFromWorkloads(ws, v.GetName())
	v.SizeBytes = blockDeviceSize(v.GetHostPath())
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

// --- validation -----------------------------------------------------------

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

func validateSize(sizeMiB uint64) error {
	if sizeMiB < MinSizeMiB {
		return fmt.Errorf("size %d MiB below minimum %d MiB", sizeMiB, MinSizeMiB)
	}
	if sizeMiB > MaxSizeMiB {
		return fmt.Errorf("size %d MiB above maximum %d MiB", sizeMiB, MaxSizeMiB)
	}
	return nil
}
