//go:build linux

// Package firecracker is the Firecracker-backed VMDriver implementation.
// One firecracker-vmm process per VM, launched by capsule and managed over
// its Unix API socket via firecracker-go-sdk.
package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd"
	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	fcmodels "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"google.golang.org/grpc"

	"github.com/geekgonecrazy/capsule/boot"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
)

// prefixedWriter prepends a prefix to each line so teed VM serial output
// is distinguishable from capsuled's own slog stream on the capsule console.
type prefixedWriter struct {
	prefix string
	w      io.Writer
	mu     sync.Mutex
	tail   []byte
}

func (p *prefixedWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(b)
	buf := append(p.tail, b...)
	for {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			break
		}
		_, _ = p.w.Write([]byte(p.prefix))
		_, _ = p.w.Write(buf[:idx+1])
		buf = buf[idx+1:]
	}
	p.tail = append(p.tail[:0], buf...)
	return n, nil
}

// Default per-VM knobs.
const (
	defaultVCPUs       = 1
	defaultMemMiB      = 256
	stateDir           = "/run/capsule/vms"
	bridgeName         = "br0"
	vmSubnetGateway    = "172.20.0.1"
	vmSubnetMask       = "255.255.0.0"
	vmThirdOctet       = 254 // 172.20.254.0/24 — isolates VM IPs from CNI's container pool
	firecrackerBinPath = "/usr/bin/firecracker"
)

// Default kernel cmdline. The VM's kernel must have virtio_blk + virtio_net
// compiled in. `ip=` is appended per-VM so the guest configures eth0
// statically without needing DHCP. root=/dev/vda is the shared capsule VM
// rootfs (image-mode); BYO-kernel callers can override by supplying
// kernel_cmdline_extra.
const defaultKernelCmdline = "ro root=/dev/vda console=ttyS0 reboot=k panic=1 pci=off nomodule"

// guestReadyTimeout is how long we wait after firecracker start for
// capsule-guest's vsock listener to come up. The 6.1 kernel + capsule-guest
// boot takes ~10s on a quiet KVM host and longer on contended cloud
// VMs; 60s is generous but still bounded.
const guestReadyTimeout = 60 * time.Second

// Driver implements runtime.VMDriver for Firecracker.
type Driver struct {
	mu   sync.Mutex
	vms  map[string]*vmState
	ctrd *containerd.Client // lazy-dialled containerd client for image pulls
}

type vmState struct {
	machine   *fc.Machine
	cancel    context.CancelFunc
	tap       string
	ip        string
	guestConn *grpc.ClientConn // host→guest gRPC over Firecracker vsock UDS
}

// New returns a fresh Driver with no VMs tracked.
func New() *Driver {
	return &Driver{vms: map[string]*vmState{}}
}

// EnsureRunning is the idempotent "desired state == running" op.
func (d *Driver) EnsureRunning(parentCtx context.Context, w *capsulev1.Workload) error {
	spec := w.GetMicroVm()
	if spec == nil {
		return fmt.Errorf("workload %q has no micro_vm spec", w.GetName())
	}
	if err := validateSpec(spec); err != nil {
		return err
	}
	name := w.GetName()

	desiredHash, err := runtime.SpecHash(w)
	if err != nil {
		return err
	}

	d.mu.Lock()
	_, alreadyTracked := d.vms[name]
	d.mu.Unlock()
	if alreadyTracked {
		// Compare the on-disk spec hash to the desired one. If the operator
		// re-applied with changes (image, env, mounts, ...), tear down so
		// the recreate path picks up the new spec. Same Remove the operator
		// would call — handles tap, port rules, vsock, payload disk, etc.
		// Empty runningHash means "started by a pre-spec-hash build" —
		// don't churn it; operator can `workload restart` to force.
		runningHash := readSpecHash(filepath.Join(stateDir, name))
		if runningHash == "" || runningHash == desiredHash {
			// TODO: real liveness check via Firecracker API.
			return nil
		}
		slog.Info("microvm spec changed — recreating", "workload", name, "old_hash", runningHash, "new_hash", desiredHash)
		if err := d.Remove(parentCtx, name); err != nil {
			return fmt.Errorf("remove stale vm %q: %w", name, err)
		}
	}

	// Prepare per-VM state directory (tmpfs; recreated every run).
	vmDir := filepath.Join(stateDir, name)
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", vmDir, err)
	}

	// Resolve which kernel + rootfs to boot from.
	//
	// Image mode (smolvm-style, default): boot the shared capsule VM rootfs
	// (contains capsule-guest as PID 1) and attach the user's OCI image as a
	// second block device (/dev/vdb). capsule-guest mounts vdb and execs the
	// payload after the host calls StartPayload over vsock.
	//
	// BYO-kernel mode: operator supplies kernel + a single root ext4;
	// this is escape-hatch territory, the guest won't have capsule-guest and
	// we don't talk to it over vsock.
	kernelPath := spec.GetKernelPath()
	rootfsPath := spec.GetRootfsPath()
	var payloadDisk string
	var payload *OCIPayload
	imageMode := spec.GetImage() != ""
	if imageMode {
		if kernelPath == "" {
			kernelPath = SharedKernel
		}
		rootfsPath = SharedRootfs
		pd, pl, err := d.preparePayloadDisk(parentCtx, w, vmDir)
		if err != nil {
			return fmt.Errorf("prepare payload disk: %w", err)
		}
		payloadDisk = pd
		payload = pl
	}

	tap := tapName(name)
	guestIP := guestIPFor(name)

	if err := setupTAP(tap); err != nil {
		return fmt.Errorf("tap setup %s: %w", tap, err)
	}

	cmdline := defaultKernelCmdline + " " + kernelIPArg(guestIP)
	if extra := strings.TrimSpace(spec.GetKernelCmdlineExtra()); extra != "" {
		cmdline += " " + extra
	}

	vcpus := int64(spec.GetVcpus())
	if vcpus <= 0 {
		vcpus = defaultVCPUs
	}
	mem := int64(spec.GetMemoryMib())
	if mem <= 0 {
		mem = defaultMemMiB
	}

	socket := filepath.Join(vmDir, "api.sock")
	logFile := filepath.Join(vmDir, "combined.log")
	vsockUDS := filepath.Join(vmDir, "vsock.uds")

	// Firecracker doesn't unlink its vsock UDS on exit, so a crashed prior
	// attempt leaves the file behind and the next Start fails with
	// EADDRINUSE when re-binding. Same for api.sock. Best-effort pre-clean.
	_ = os.Remove(vsockUDS)
	_ = os.Remove(socket)

	drives := []fcmodels.Drive{{
		DriveID:      fc.String("rootfs"),
		PathOnHost:   fc.String(rootfsPath),
		IsRootDevice: fc.Bool(true),
		// Shared rootfs is attached read-only so the same image backs every
		// running VM. capsule-guest's writable state lives on tmpfs.
		IsReadOnly: fc.Bool(imageMode),
	}}
	if payloadDisk != "" {
		drives = append(drives, fcmodels.Drive{
			DriveID:      fc.String("payload"),
			PathOnHost:   fc.String(payloadDisk),
			IsRootDevice: fc.Bool(false),
			IsReadOnly:   fc.Bool(false),
		})
	}

	// Attach declared volumes as additional block devices (/dev/vdc onward).
	// Firecracker orders virtio-blk devices by their attach order, so we
	// track the device letter we'll hand to capsule-guest for each one.
	guestMounts, err := d.prepareVolumeDrives(spec.GetMounts(), &drives)
	if err != nil {
		return fmt.Errorf("prepare volumes: %w", err)
	}

	cfg := fc.Config{
		SocketPath:      socket,
		LogPath:         logFile,
		KernelImagePath: kernelPath,
		KernelArgs:      cmdline,
		Drives:          drives,
		NetworkInterfaces: []fc.NetworkInterface{{
			StaticConfiguration: &fc.StaticNetworkConfiguration{
				HostDevName: tap,
				MacAddress:  macForName(name),
			},
		}},
		VsockDevices: []fc.VsockDevice{{
			ID:   "agent",
			Path: vsockUDS,
			CID:  3, // guest CID; 0/1/2 are reserved.
		}},
		MachineCfg: fcmodels.MachineConfiguration{
			VcpuCount:  fc.Int64(vcpus),
			MemSizeMib: fc.Int64(mem),
			Smt:        fc.Bool(false),
		},
	}

	vmCtx, cancel := context.WithCancel(context.Background())

	// Capture the VM's serial console to a file in the VM state dir AND
	// tee to capsuled's stderr so it surfaces on the capsule console (useful
	// when the capsule is itself under a hypervisor for dev). Firecracker
	// forwards the guest ttyS0 to its own stdout.
	serialFile := filepath.Join(vmDir, "vm-serial.log")
	serialW, err := os.OpenFile(serialFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		cancel()
		return fmt.Errorf("open serial log %s: %w", serialFile, err)
	}
	tee := io.MultiWriter(serialW, &prefixedWriter{prefix: "[vm " + name + "] ", w: os.Stderr})

	cmd := fc.VMCommandBuilder{}.
		WithBin(firecrackerBinPath).
		WithSocketPath(socket).
		WithStdout(tee).
		WithStderr(tee).
		Build(vmCtx)

	m, err := fc.NewMachine(vmCtx, cfg, fc.WithProcessRunner(cmd))
	if err != nil {
		cancel()
		_ = teardownTAP(tap)
		return fmt.Errorf("new machine %q: %w", name, err)
	}
	if err := m.Start(vmCtx); err != nil {
		cancel()
		_ = teardownTAP(tap)
		return fmt.Errorf("start machine %q: %w", name, err)
	}

	var guestConn *grpc.ClientConn
	if imageMode {
		// Dial capsule-guest over the Firecracker vsock UDS. Retry until ready.
		readyCtx, readyCancel := context.WithTimeout(parentCtx, guestReadyTimeout)
		conn, err := waitForGuestReady(readyCtx, vsockUDS, guestReadyTimeout)
		readyCancel()
		if err != nil {
			_ = m.Shutdown(context.Background())
			_ = m.StopVMM()
			cancel()
			_ = teardownTAP(tap)
			return fmt.Errorf("guest agent on %s: %w", vsockUDS, err)
		}
		guestConn = conn

		agent := capsulev1.NewGuestAgentClient(guestConn)
		startCtx, startCancel := context.WithTimeout(parentCtx, 10*time.Second)
		_, err = agent.StartPayload(startCtx, &capsulev1.StartPayloadRequest{
			PayloadBlockDevice: "/dev/vdb",
			Command:            payload.Command,
			Env:                payload.Env,
			WorkingDir:         payload.Cwd,
			Chroot:             true,
			Mounts:             guestMounts,
		})
		startCancel()
		if err != nil {
			_ = guestConn.Close()
			_ = m.Shutdown(context.Background())
			_ = m.StopVMM()
			cancel()
			_ = teardownTAP(tap)
			return fmt.Errorf("guest StartPayload: %w", err)
		}
	}

	// Install DNAT rules for declared port mappings now that we know the
	// guest IP is up and the VM is accepting traffic. Rule tear-down on
	// Remove uses the same "capsule-vm:<name>" comment to find them.
	if err := applyPortMappings(name, guestIP, spec.GetPorts()); err != nil {
		slog.Warn("port mapping install failed", "workload", name, "err", err)
	}

	// Stash the spec hash so a later EnsureRunning can detect drift and
	// recreate. Best-effort: if write fails we still mark the VM as
	// tracked, just future drift detection won't fire (operator can
	// `workload restart` to force).
	if err := writeSpecHash(vmDir, desiredHash); err != nil {
		slog.Warn("write spec hash", "workload", name, "err", err)
	}

	d.mu.Lock()
	d.vms[name] = &vmState{
		machine:   m,
		cancel:    cancel,
		tap:       tap,
		ip:        guestIP,
		guestConn: guestConn,
	}
	d.mu.Unlock()

	slog.Info("microvm started",
		"workload", name,
		"tap", tap,
		"ip", guestIP,
		"vcpus", vcpus,
		"memory_mib", mem,
		"image_mode", imageMode,
		"ports", len(spec.GetPorts()),
	)
	return nil
}

// Remove stops and deletes a VM. Idempotent.
func (d *Driver) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	s, ok := d.vms[name]
	delete(d.vms, name)
	d.mu.Unlock()

	// TAP name is deterministic from the workload name, so we can clean
	// it up even if we have no in-memory record (e.g. after capsuled restart).
	tap := tapName(name)

	// Always attempt to tear down port mapping rules (identified by
	// iptables comment), even if the in-memory state is gone.
	teardownPortMappings(name)

	if ok && s.machine != nil {
		// Ask the guest agent (if any) to stop the payload gracefully before
		// we pull the VM out from under it. Best effort; ignore errors.
		if s.guestConn != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = capsulev1.NewGuestAgentClient(s.guestConn).Stop(stopCtx, &capsulev1.StopRequest{GraceSeconds: 5})
			stopCancel()
			_ = s.guestConn.Close()
		}
		shutdownCtx, cancel := context.WithCancel(context.Background())
		_ = s.machine.Shutdown(shutdownCtx)
		if err := s.machine.StopVMM(); err != nil {
			slog.Warn("stop vmm", "workload", name, "err", err)
		}
		// Wait for firecracker to actually exit before teardownTAP.
		// StopVMM sends SIGTERM and returns immediately; without Wait()
		// the TAP is still open when we try to delete it, and the next
		// Start sees "Resource busy" on fresh `ip tuntap add`.
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = s.machine.Wait(waitCtx)
		waitCancel()
		cancel()
		if s.cancel != nil {
			s.cancel()
		}
	}

	if err := teardownTAP(tap); err != nil {
		slog.Warn("tap teardown", "tap", tap, "err", err)
	}

	// Tear down the devmapper snapshot so its thin LV is returned to the
	// pool. Safe to call unconditionally — idempotent when no snapshot.
	d.releasePayloadSnapshot(context.Background(), name)

	// Remove state dir.
	_ = os.RemoveAll(filepath.Join(stateDir, name))
	return nil
}

// Status returns the observed lifecycle state.
func (d *Driver) Status(_ context.Context, name string) (runtime.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.vms[name]; !ok {
		return runtime.Status{Phase: runtime.PhaseUnknown}, nil
	}
	// TODO: verify liveness against the Firecracker API instead of just
	// assuming Running once we've called Start.
	return runtime.Status{Phase: runtime.PhaseRunning}, nil
}

// --- helpers ---------------------------------------------------------------

func validateSpec(s *capsulev1.MicroVMSpec) error {
	// Image mode: no paths required; we'll pull + build the rootfs.
	if s.GetImage() != "" {
		if s.GetKernelPath() != "" && !strings.HasPrefix(s.GetKernelPath(), "/") {
			return fmt.Errorf("kernel_path must be absolute, got %q", s.GetKernelPath())
		}
		return nil
	}
	// BYO mode: operator must supply kernel + rootfs files.
	if s.GetKernelPath() == "" {
		return fmt.Errorf("either image or kernel_path is required")
	}
	if !strings.HasPrefix(s.GetKernelPath(), "/") {
		return fmt.Errorf("kernel_path must be absolute, got %q", s.GetKernelPath())
	}
	if s.GetRootfsPath() == "" {
		return fmt.Errorf("rootfs_path is required when image is not set")
	}
	if !strings.HasPrefix(s.GetRootfsPath(), "/") {
		return fmt.Errorf("rootfs_path must be absolute, got %q", s.GetRootfsPath())
	}
	if _, err := os.Stat(s.GetKernelPath()); err != nil {
		return fmt.Errorf("kernel_path: %w", err)
	}
	if _, err := os.Stat(s.GetRootfsPath()); err != nil {
		return fmt.Errorf("rootfs_path: %w", err)
	}
	return nil
}

// tapName returns a <=15 char Linux interface name for a VM, derived
// deterministically from the workload name so teardown works even after
// capsuled restarts.
func tapName(workload string) string {
	sum := sha256.Sum256([]byte(workload))
	return "kv-" + hex.EncodeToString(sum[:4]) // 11 chars total
}

// macForName returns a locally-administered MAC derived deterministically
// from the workload name. First byte sets the locally-administered bit
// and clears the multicast bit (0x02).
func macForName(workload string) string {
	sum := sha256.Sum256([]byte(workload))
	b := sum[:6]
	b[0] = (b[0] & 0xFE) | 0x02
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5])
}

// guestIPFor deterministically picks 172.20.254.X for a VM based on its
// name. Collisions are possible at homelab scale but unlikely; if we hit
// one in practice we'll add a real IPAM.
func guestIPFor(workload string) string {
	sum := sha256.Sum256([]byte(workload))
	// Use two bytes of hash to pick .2 .. .253.
	host := int(sum[0])<<8 | int(sum[1])
	host = (host % 252) + 2
	return fmt.Sprintf("172.20.%d.%d", vmThirdOctet, host)
}

// kernelIPArg builds the value for the kernel's ip= option.
// Format: client-ip::server-ip:netmask:hostname:device:autoconf
func kernelIPArg(guestIP string) string {
	return fmt.Sprintf("ip=%s::%s:%s::eth0:off", guestIP, vmSubnetGateway, vmSubnetMask)
}

// setupTAP creates a TAP device and attaches it to br0. Idempotent:
// an existing TAP or bridge is fine. Ensures br0 itself exists.
func setupTAP(name string) error {
	if err := ensureBridge(bridgeName); err != nil {
		return fmt.Errorf("ensure bridge %s: %w", bridgeName, err)
	}
	// Try tuntap add; if the name is taken we fall through (could be a
	// stale device from a crashed VMM). "File exists"/"Resource busy"
	// are both success-ish — we'll still re-set master/up below so a
	// dangling TAP attached to some other bridge gets corrected.
	addErr := runIP("tuntap", "add", "mode", "tap", "name", name)
	if addErr != nil {
		es := addErr.Error()
		if !strings.Contains(es, "File exists") && !strings.Contains(es, "Resource busy") && !strings.Contains(es, "exists") {
			return addErr
		}
	}
	if err := runIP("link", "set", name, "master", bridgeName); err != nil {
		// If the master-set fails because the TAP is still owned by a
		// dying Firecracker, nuke + recreate once and try again.
		if strings.Contains(err.Error(), "Resource busy") || strings.Contains(err.Error(), "does not exist") {
			_ = runIP("link", "del", name)
			if err2 := runIP("tuntap", "add", "mode", "tap", "name", name); err2 != nil {
				return fmt.Errorf("tap recreate %s: %w", name, err2)
			}
			if err2 := runIP("link", "set", name, "master", bridgeName); err2 != nil {
				return err2
			}
		} else {
			return err
		}
	}
	if err := runIP("link", "set", name, "up"); err != nil {
		return err
	}
	return nil
}

// ensureBridge creates the capsule bridge with the CNI gateway IP if it
// doesn't already exist. CNI's bridge plugin creates it on first use for
// containers, but VM workloads may run before any bridge container has.
// Also installs the MASQUERADE rule so VM traffic reaches the outside.
func ensureBridge(name string) error {
	if _, err := os.Stat("/sys/class/net/" + name); err != nil {
		if err := runIP("link", "add", "name", name, "type", "bridge"); err != nil {
			if !strings.Contains(err.Error(), "File exists") {
				return err
			}
		}
		_ = runIP("addr", "add", vmSubnetGateway+"/16", "dev", name)
		if err := runIP("link", "set", name, "up"); err != nil {
			return err
		}
	}
	// MASQUERADE VM traffic going out any non-bridge interface. Idempotent
	// via iptables -C: only add if not present. CNI's bridge plugin adds a
	// similar rule for containers but it's scoped to container IPs; VMs
	// have their own .254.x range that CNI doesn't know about.
	return ensureMasquerade(name)
}

// ensureMasquerade adds `iptables -t nat -A POSTROUTING -s 172.20.0.0/16
// ! -o br0 -j MASQUERADE` if not already present. Without this, ping
// from inside a VM times out at the bridge.
func ensureMasquerade(bridge string) error {
	src := "172.20.0.0/16"
	args := []string{"-t", "nat", "-s", src, "!", "-o", bridge, "-j", "MASQUERADE"}
	// -C (check) exits 0 if present, non-zero otherwise.
	checkArgs := append([]string{"-t", "nat", "-C", "POSTROUTING"}, args[2:]...)
	if err := exec.Command("/usr/sbin/iptables", checkArgs...).Run(); err == nil {
		return nil
	}
	appendArgs := append([]string{"-t", "nat", "-A", "POSTROUTING"}, args[2:]...)
	out, err := exec.Command("/usr/sbin/iptables", appendArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables masquerade: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func teardownTAP(name string) error {
	// `ip link del` returns non-zero if the link isn't there; treat as
	// success so Remove is idempotent.
	_ = runIP("link", "del", name)
	return nil
}

func runIP(args ...string) error {
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	cmd := exec.Command("/sbin/ip", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// specHashFile is where each VM's runtime.SpecHash gets stashed so a
// later EnsureRunning tick can detect spec drift after an operator
// re-apply. Lives in vmDir alongside api.sock / vsock.uds and disappears
// with the rest of that dir on Remove.
const specHashFile = "spec.hash"

func readSpecHash(vmDir string) string {
	b, err := os.ReadFile(filepath.Join(vmDir, specHashFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeSpecHash(vmDir, hash string) error {
	return os.WriteFile(filepath.Join(vmDir, specHashFile), []byte(hash), 0o644)
}
