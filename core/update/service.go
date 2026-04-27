// Package update is the business logic for OS updates: receiving a
// streamed bundle, writing it to the inactive A/B slot, flipping the
// bootloader's one-shot, persisting tentative state, and committing or
// rolling back based on operator action.
//
// The wire protocol is a streaming `UpdateOS` RPC followed by a
// non-streaming `UpdateConfirm`. The on-disk shape is:
//
//	/perm/updates/incoming.tar             // streamed bundle, sha256-verified
//	/perm/updates/staging/{VERSION,vmlinuz,initramfs,rootfs.sqsh}
//
// All shell-outs (mount, dd, etc.) are gated by boot.ExecMu so they
// don't race the PID-1 reaper.
package update

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/geekgonecrazy/capsule/boot"
	"github.com/geekgonecrazy/capsule/store"
)

// Sentinel errors. Map these in the controller to gRPC codes.
var (
	// ErrNoPending is returned by Confirm when there's no pending slot.
	ErrNoPending = errors.New("update: no pending slot")
	// ErrSlotMismatch is returned by Confirm when the active slot isn't
	// the pending slot — we likely rolled back already.
	ErrSlotMismatch = errors.New("update: active slot does not match pending slot")
	// ErrChecksumMismatch is returned by ReceiveBundle when the streamed
	// bytes don't hash to the metadata's sha256.
	ErrChecksumMismatch = errors.New("update: bundle sha256 mismatch")
	// ErrBundleTooLarge is returned by ReceiveBundle when the bundle's
	// rootfs.sqsh would not fit in the inactive slot partition.
	ErrBundleTooLarge = errors.New("update: bundle too large for slot")
)

// DefaultDeadline is how long capsuled waits for a Confirm before it
// auto-reboots to roll back.
const DefaultDeadline = 10 * time.Minute

// Service performs the heavy lifting for OS updates. It depends only on:
//   - an OSStateStore for persistent A/B bookkeeping
//   - the active slot (provided at construction by capsuled)
//   - a staging directory under /perm
//   - an injectable rebooter (so tests can swap in a fake)
type Service struct {
	State      store.OSStateStore
	ActiveSlot string // "slot_a" / "slot_b"
	StagingDir string // typically /perm/updates
	BootMount  string // typically /run/capsule/boot-mount
	Reboot     func() error
	// Now lets tests fast-forward the deadline timer.
	Now func() time.Time

	deadlineTimer *time.Timer
}

// New constructs a Service with reasonable defaults.
func New(s store.OSStateStore, activeSlot string) *Service {
	return &Service{
		State:      s,
		ActiveSlot: activeSlot,
		StagingDir: "/perm/updates",
		BootMount:  "/run/capsule/boot-mount",
		Reboot:     defaultReboot,
		Now:        time.Now,
	}
}

// OnStartup is called by capsuled once /perm is mounted and the store is
// open. It reconciles os_state with the booted slot:
//
//   - first boot ever (no row): seed `active_slot = ActiveSlot, last_good_slot = ActiveSlot`.
//   - no pending: nothing to do.
//   - pending && active == pending: tentative mode. If the deadline has already
//     passed (e.g. clock skew, power loss during tentative boot), reboot
//     immediately for rollback. Otherwise arm a deadline goroutine that
//     reboots if Confirm hasn't cleared pending by then.
//   - pending && active != pending: bootloader rolled back — the kernel
//     panicked or the previous tentative run timed out and self-rebooted.
//     Clear pending and log the failure for visibility via GetInfo.
func (s *Service) OnStartup(ctx context.Context) error {
	if s.ActiveSlot == "" {
		// dev mode (capsuled not running as PID 1, or pre-A/B image). Skip.
		return nil
	}
	st, err := s.State.Get(ctx)
	if errors.Is(err, store.ErrNotFound) {
		seed := &store.OSState{
			ActiveSlot:   s.ActiveSlot,
			LastGoodSlot: s.ActiveSlot,
		}
		if err := s.State.Put(ctx, seed); err != nil {
			return fmt.Errorf("seed os_state: %w", err)
		}
		slog.Info("os_state seeded on first boot", "slot", s.ActiveSlot)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read os_state: %w", err)
	}

	// Always update active_slot to what we actually booted into.
	st.ActiveSlot = s.ActiveSlot

	switch {
	case st.PendingSlot == "":
		// Steady state.
		return s.State.Put(ctx, st)

	case st.PendingSlot != s.ActiveSlot:
		// Bootloader rolled back: we asked for X, ended up on Y.
		slog.Warn("bootloader rolled back update",
			"requested", st.PendingSlot,
			"booted", s.ActiveSlot,
			"failed_version", st.LastVersion)
		st.PendingSlot = ""
		st.PendingDeadlineUnix = 0
		// LastGoodSlot stays on whatever was committed before the failed try.
		return s.State.Put(ctx, st)

	default:
		// Tentative mode: pending == active, awaiting Confirm.
		if err := s.State.Put(ctx, st); err != nil {
			return err
		}
		now := s.Now().Unix()
		if st.PendingDeadlineUnix == 0 || now >= st.PendingDeadlineUnix {
			slog.Warn("tentative deadline already passed; reverting DEFAULT and rebooting",
				"to", st.LastGoodSlot,
				"failed_version", st.LastVersion)
			if err := s.rollbackToLastGood(ctx, st.LastGoodSlot); err != nil {
				slog.Error("rollback: revert DEFAULT failed; rebooting anyway", "err", err)
			}
			return s.Reboot()
		}
		until := time.Unix(st.PendingDeadlineUnix, 0).Sub(s.Now())
		slog.Info("tentative boot — awaiting UpdateConfirm or auto-rollback",
			"slot", s.ActiveSlot,
			"deadline_in", until.Round(time.Second).String())
		s.deadlineTimer = time.AfterFunc(until, func() {
			cur, err := s.State.Get(context.Background())
			if err != nil {
				slog.Warn("deadline check: read os_state failed", "err", err)
				return
			}
			if cur.PendingSlot == "" {
				return // confirmed in the meantime
			}
			slog.Warn("auto-rollback: deadline expired; reverting DEFAULT and rebooting",
				"to", cur.LastGoodSlot, "from", cur.ActiveSlot, "failed_version", cur.LastVersion)
			if err := s.rollbackToLastGood(context.Background(), cur.LastGoodSlot); err != nil {
				slog.Error("auto-rollback: revert DEFAULT failed; rebooting anyway", "err", err)
			}
			if err := s.Reboot(); err != nil {
				slog.Error("auto-rollback reboot failed", "err", err)
			}
		})
		return nil
	}
}

// ReceiveBundle streams an update bundle from the supplied chunk reader,
// stream-extracts the tar on the fly (squashfs piped directly to the
// inactive slot device, kernel + initramfs staged to /perm/updates/staging),
// verifies the whole-bundle sha256 as it passes through, and arms the
// bootloader one-shot. Returns the slot it wrote to and the bundle's
// VERSION. Caller schedules the reboot.
//
// We avoid staging the entire tar to disk because the bundle is dominated
// by the squashfs (~700 MiB vs ~35 MiB for kernel+initramfs), and the
// meta LV that holds /perm is sized for state.db, not for full bundles.
//
// nextChunk returns ([]byte, io.EOF) when the stream is exhausted.
func (s *Service) ReceiveBundle(ctx context.Context, expectSize uint64, expectSHA256Hex string, nextChunk func() ([]byte, error)) (slot string, version string, err error) {
	if s.ActiveSlot == "" {
		return "", "", errors.New("update service: ActiveSlot not set")
	}
	stagePath := filepath.Join(s.StagingDir, "staging")
	if err := os.RemoveAll(stagePath); err != nil {
		return "", "", fmt.Errorf("clean staging: %w", err)
	}
	if err := os.MkdirAll(stagePath, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir staging: %w", err)
	}

	// Pick inactive slot; resolve its block device up-front so we can fail
	// fast if discovery is broken before consuming the stream.
	nextSlot := otherSlot(s.ActiveSlot)
	inactiveDev, err := boot.FindPartitionByNumber(slotPartitionNumber(nextSlot))
	if err != nil {
		return "", "", fmt.Errorf("find slot partition: %w", err)
	}

	// Pipe: producer goroutine pulls gRPC chunks → hash + pipe writer.
	// Consumer (tar reader) reads via tar.NewReader on the pipe reader.
	pr, pw := io.Pipe()
	hash := sha256.New()
	var bytesIn uint64
	producerErr := make(chan error, 1)
	go func() {
		defer pw.Close()
		for {
			chunk, rerr := nextChunk()
			if len(chunk) > 0 {
				hash.Write(chunk)
				bytesIn += uint64(len(chunk))
				if _, werr := pw.Write(chunk); werr != nil {
					producerErr <- werr
					return
				}
			}
			if errors.Is(rerr, io.EOF) {
				producerErr <- nil
				return
			}
			if rerr != nil {
				pw.CloseWithError(rerr)
				producerErr <- rerr
				return
			}
		}
	}()

	tr := tar.NewReader(pr)
	bundle := bundlePaths{
		kernelPath:    filepath.Join(stagePath, "vmlinuz"),
		initramfsPath: filepath.Join(stagePath, "initramfs"),
	}
	want := map[string]bool{"VERSION": true, "vmlinuz": true, "initramfs": true, "rootfs.sqsh": true}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			pr.CloseWithError(err)
			return "", "", fmt.Errorf("read tar: %w", err)
		}
		if !want[hdr.Name] {
			// Skip unknown member by reading it through to advance.
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return "", "", err
			}
			continue
		}
		switch hdr.Name {
		case "VERSION":
			b, err := io.ReadAll(tr)
			if err != nil {
				return "", "", err
			}
			bundle.version = strings.TrimSpace(string(b))
		case "vmlinuz":
			if err := writeFile(bundle.kernelPath, tr); err != nil {
				return "", "", err
			}
		case "initramfs":
			if err := writeFile(bundle.initramfsPath, tr); err != nil {
				return "", "", err
			}
		case "rootfs.sqsh":
			// Pre-flight: refuse to even start the dd if the squashfs is
			// bigger than the inactive slot. Otherwise io.Copy would write
			// up to the partition boundary and EIO partway through, leaving
			// a half-written slot. Slot sizes are frozen at install time
			// (see image/pack.sh SLOT_SIZE_MIB) — the only fix is reinstall.
			slotBytes, err := blockDeviceSize(inactiveDev)
			if err != nil {
				return "", "", fmt.Errorf("size of %s: %w", inactiveDev, err)
			}
			if hdr.Size > slotBytes {
				return "", "", fmt.Errorf("%w: rootfs.sqsh %d bytes > slot %s capacity %d bytes; bump SLOT_SIZE_MIB and reinstall to apply this update",
					ErrBundleTooLarge, hdr.Size, inactiveDev, slotBytes)
			}
			slog.Info("streaming rootfs.sqsh to inactive slot",
				"slot", nextSlot, "device", inactiveDev, "size", hdr.Size, "slot_bytes", slotBytes)
			if err := writeBlockDevice(inactiveDev, tr); err != nil {
				return "", "", fmt.Errorf("stream rootfs to %s: %w", inactiveDev, err)
			}
		}
		delete(want, hdr.Name)
	}
	// Drain any tail bytes (tar archive trailer) so the hash covers them.
	_, _ = io.Copy(io.Discard, pr)
	if perr := <-producerErr; perr != nil {
		return "", "", fmt.Errorf("stream: %w", perr)
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for k := range want {
			missing = append(missing, k)
		}
		return "", "", fmt.Errorf("bundle missing required members: %v", missing)
	}
	if expectSize != 0 && bytesIn != expectSize {
		return "", "", fmt.Errorf("size mismatch: expected %d got %d", expectSize, bytesIn)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, expectSHA256Hex) {
		return "", "", fmt.Errorf("%w: expected %s got %s", ErrChecksumMismatch, expectSHA256Hex, got)
	}

	// Mount /boot rw, copy per-slot kernel + initramfs (skip-if-unchanged),
	// then flip GRUB's `set default=` to the new slot. There's no true
	// one-shot equivalent in our GRUB setup, so the next boot's `default`
	// controls which slot loads — on tentative boot the deadline timer +
	// UpdateConfirm dance handles "kernel boots, userland sad" rollback.
	// Kernel panic during tentative boot will loop on the new slot until
	// manual recovery via the GRUB menu.
	bootPart, err := boot.FindPartitionByNumber(1) // CAPSULEBOOT (ESP)
	if err != nil {
		return "", "", fmt.Errorf("find boot partition: %w", err)
	}
	if err := s.withBootMount(bootPart, func() error {
		dst := func(name string) string { return filepath.Join(s.BootMount, name) }
		if err := copyIfChanged(bundle.kernelPath, dst("vmlinuz_"+slotLetter(nextSlot))); err != nil {
			return err
		}
		if err := copyIfChanged(bundle.initramfsPath, dst("initramfs_"+slotLetter(nextSlot))); err != nil {
			return err
		}
		return rewriteGrubDefault(filepath.Join(s.BootMount, "EFI", "BOOT", "grub.cfg"), nextSlot)
	}); err != nil {
		return "", "", err
	}

	// Persist pending + deadline + version. Caller reboots.
	st, err := s.State.Get(ctx)
	if err != nil {
		return "", "", fmt.Errorf("read os_state: %w", err)
	}
	st.PendingSlot = nextSlot
	st.PendingDeadlineUnix = s.Now().Add(DefaultDeadline).Unix()
	st.LastVersion = bundle.version
	if err := s.State.Put(ctx, st); err != nil {
		return "", "", fmt.Errorf("write os_state: %w", err)
	}
	return nextSlot, bundle.version, nil
}

// writeFile writes the contents of r to path with fsync at the end.
func writeFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return f.Sync()
}

// blockDeviceSize returns the byte length of a block device. os.Stat
// reports 0 for block specials, so seek-to-end is the cheap portable
// trick (works on Linux for any seekable fd).
func blockDeviceSize(devPath string) (int64, error) {
	f, err := os.OpenFile(devPath, os.O_RDONLY, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Seek(0, io.SeekEnd)
}

// writeBlockDevice opens devPath and copies bytes from r, fsync at end.
// Used for the squashfs → slot dd-equivalent.
func writeBlockDevice(devPath string, r io.Reader) error {
	f, err := os.OpenFile(devPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return f.Sync()
}

// Confirm commits the pending slot. The bootloader DEFAULT was already
// flipped at ReceiveBundle time, so all that's left is to clear pending
// state, set last_good_slot, and stop the auto-rollback deadline timer.
func (s *Service) Confirm(ctx context.Context) (committedSlot, committedVersion string, err error) {
	st, err := s.State.Get(ctx)
	if err != nil {
		return "", "", fmt.Errorf("read os_state: %w", err)
	}
	if st.PendingSlot == "" {
		return "", "", ErrNoPending
	}
	if st.PendingSlot != s.ActiveSlot {
		return "", "", ErrSlotMismatch
	}
	if s.deadlineTimer != nil {
		s.deadlineTimer.Stop()
	}
	committedVersion = st.LastVersion
	committedSlot = st.ActiveSlot
	st.PendingSlot = ""
	st.PendingDeadlineUnix = 0
	st.LastGoodSlot = st.ActiveSlot
	if err := s.State.Put(ctx, st); err != nil {
		return "", "", fmt.Errorf("write os_state: %w", err)
	}
	slog.Info("update committed", "slot", committedSlot, "version", committedVersion)
	return committedSlot, committedVersion, nil
}

// rollbackToLastGood rewrites GRUB's `set default=` back to the
// last-known-good slot before triggering a reboot. Used by the deadline
// goroutine when the operator hasn't confirmed in time.
func (s *Service) rollbackToLastGood(ctx context.Context, lastGood string) error {
	bootPart, err := boot.FindPartitionByNumber(1)
	if err != nil {
		return fmt.Errorf("find boot partition: %w", err)
	}
	return s.withBootMount(bootPart, func() error {
		return rewriteGrubDefault(filepath.Join(s.BootMount, "EFI", "BOOT", "grub.cfg"), lastGood)
	})
}

// --------------------------------------------------------------------
// Internals
// --------------------------------------------------------------------

type bundlePaths struct {
	version       string
	kernelPath    string
	initramfsPath string
}

// withBootMount mounts the FAT32 boot partition rw at s.BootMount,
// invokes fn, then unmounts. Shells out via /bin/mount + /bin/umount —
// matches the rest of capsuled's /perm/LVM discipline.
func (s *Service) withBootMount(devPath string, fn func() error) error {
	if err := os.MkdirAll(s.BootMount, 0o755); err != nil {
		return fmt.Errorf("mkdir boot mount: %w", err)
	}
	if mounted, _ := isMounted(s.BootMount); !mounted {
		boot.ExecMu.Lock()
		out, err := exec.Command("/bin/mount", "-t", "vfat", "-o", "umask=0022", devPath, s.BootMount).CombinedOutput()
		boot.ExecMu.Unlock()
		if err != nil {
			return fmt.Errorf("mount %s -> %s: %w: %s", devPath, s.BootMount, err, strings.TrimSpace(string(out)))
		}
	}
	defer func() {
		boot.ExecMu.Lock()
		out, err := exec.Command("/bin/umount", s.BootMount).CombinedOutput()
		boot.ExecMu.Unlock()
		if err != nil {
			slog.Warn("umount boot mount failed", "err", err, "out", strings.TrimSpace(string(out)))
		}
	}()
	return fn()
}

// rewriteGrubDefault replaces the `set default=N` line in grub.cfg so the
// next reboot picks the requested slot. We author the file in pack.sh in a
// fixed two-entry order (slot_a=0, slot_b=1) so a string replace is safe.
func rewriteGrubDefault(path, slot string) error {
	idx, err := slotIndex(slot)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read grub.cfg: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "set default=") {
			lines[i] = fmt.Sprintf("set default=%d", idx)
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no `set default=` line in %s", path)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

func slotIndex(slot string) (int, error) {
	switch slot {
	case "slot_a":
		return 0, nil
	case "slot_b":
		return 1, nil
	default:
		return -1, fmt.Errorf("unknown slot %q", slot)
	}
}

func copyIfChanged(src, dst string) error {
	srcSum, err := fileSHA256(src)
	if err != nil {
		return fmt.Errorf("hash src: %w", err)
	}
	if dstSum, err := fileSHA256(dst); err == nil && dstSum == srcSum {
		slog.Info("skip-if-unchanged", "file", filepath.Base(dst))
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// defaultReboot is implemented per-platform — see service_linux.go for
// the real PID-1 reboot syscall, and service_other.go for a non-Linux stub.

func otherSlot(s string) string {
	if s == "slot_a" {
		return "slot_b"
	}
	return "slot_a"
}

func slotPartitionNumber(slot string) int {
	if slot == "slot_a" {
		return 2
	}
	return 3
}

func slotLetter(slot string) string {
	if slot == "slot_a" {
		return "a"
	}
	return "b"
}

func isMounted(path string) (bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true, nil
		}
	}
	return false, nil
}

