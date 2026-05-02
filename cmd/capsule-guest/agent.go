//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// agent implements capsule.v1.GuestAgent. It owns the single payload
// container (there is exactly one payload per VM) plus a ring-ish buffer
// file so Logs can tail after exit.
//
// Payload runs under runc with a synthesized OCI bundle whose rootfs is
// the mounted /oci (the user's OCI image, materialized from /dev/vdb).
type agent struct {
	capsulev1.UnimplementedGuestAgentServer

	boot time.Time

	mu       sync.Mutex
	phase    capsulev1.PayloadPhase
	exitCode int32
	message  string
	logDone  bool
	logCond  *sync.Cond

	started  bool
	watching bool // a goroutine is polling runc state + reaping exit

	// volumeMounts holds the host-side mountpoints (e.g. "/oci/workspace")
	// for every user volume StartPayload mounted. These are mounted
	// OUTSIDE the runc namespace, so `runc delete` doesn't tear them down
	// — Stop must explicitly umount them so ext4 commits its journal
	// before the VM is killed. Without this, writes from the workload
	// silently vanish: dirty pages drop on the floor when Firecracker is
	// SIGTERM'd, and the next mounter sees the pre-write state.
	volumeMounts []string
}

const (
	// runcBinary is the path to the runc binary inside the shared VM rootfs.
	runcBinary = "/usr/bin/runc"
	// containerID is the single OCI container ID this VM hosts.
	containerID = "payload"
	// bundleDir is where we write the OCI runtime bundle (config.json).
	bundleDir = "/run/capsule-guest/bundle"
	// logPath is where the payload's combined stdout+stderr lands.
	logPath = "/run/capsule-guest/payload.log"
	// pidFile records the payload init PID (written by runc run -d).
	pidFile = "/run/capsule-guest/payload.pid"
)

func newAgent() *agent {
	a := &agent{
		boot:  time.Now(),
		phase: capsulev1.PayloadPhase_PAYLOAD_PHASE_UNSPECIFIED,
	}
	a.logCond = sync.NewCond(&a.mu)
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	_ = os.MkdirAll(bundleDir, 0o755)
	return a
}

// execMu serializes our own exec.Cmd calls (runc) against the reap
// loop. Without this, wait4(-1) in reapLoop wins the race and
// exec.Cmd.Wait() in our runc helpers returns "waitid: no child
// processes". Caller contract: hold execMu for cmd.Run()/Wait()
// lifetime; drainZombies takes it before reaping.
var execMu sync.Mutex

// reapLoop handles zombies — runc's auxiliary processes and anything
// orphaned into PID 1. The payload itself lives under runc, so runc
// (and the kernel) own the reaping of its subtree.
func (a *agent) reapLoop() {
	for {
		execMu.Lock()
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		execMu.Unlock()
		if pid <= 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if err != nil && err != syscall.ECHILD {
			log.Printf("capsule-guest: wait4: %v", err)
		}
	}
}

// Ping is a liveness probe.
func (a *agent) Ping(_ context.Context, _ *capsulev1.PingRequest) (*capsulev1.PingResponse, error) {
	return &capsulev1.PingResponse{
		AgentVersion:  "0.2.0-runc",
		UptimeSeconds: uint64(time.Since(a.boot).Seconds()),
	}, nil
}

// StartPayload mounts /dev/vdb at /oci, synthesizes an OCI bundle, and
// runs the container in detached mode via runc. Output is redirected
// into payload.log; Logs tails it.
func (a *agent) StartPayload(_ context.Context, req *capsulev1.StartPayloadRequest) (*capsulev1.StartPayloadResponse, error) {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return nil, fmt.Errorf("payload already started")
	}
	a.mu.Unlock()

	if len(req.GetCommand()) == 0 {
		return nil, fmt.Errorf("command is required")
	}

	dev := req.GetPayloadBlockDevice()
	if dev == "" {
		dev = "/dev/vdb"
	}
	mountPoint := "/oci"
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", mountPoint, err)
	}
	if err := syscall.Mount(dev, mountPoint, "ext4", 0, ""); err != nil && err != syscall.EBUSY {
		return nil, fmt.Errorf("mount %s -> %s: %w", dev, mountPoint, err)
	}

	// Ensure the payload has a working /etc/resolv.conf. The OCI image
	// typically doesn't ship one (they're expected to be injected at
	// container runtime — Docker bind-mounts the host's). Use public
	// resolvers as a sensible default. Skip if the image already has one.
	resolvPath := filepath.Join(mountPoint, "etc/resolv.conf")
	if _, err := os.Stat(resolvPath); os.IsNotExist(err) {
		_ = os.MkdirAll(filepath.Dir(resolvPath), 0o755)
		_ = os.WriteFile(resolvPath, []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n"), 0o644)
	}

	// Mount any declared volume block devices inside the payload rootfs.
	// Entries are collected here and passed to writeOCIConfig below so
	// runc sees them as bind mounts (we mount outside the container ns
	// and bind-propagate, rather than mounting inside where the VM may
	// lack mount caps).
	var extraMounts []extraMount
	for _, vm := range req.GetMounts() {
		dev := vm.GetDevice()
		mp := vm.GetMountPath()
		fstype := vm.GetFstype()
		if fstype == "" {
			fstype = "ext4"
		}
		hostMP := filepath.Join(mountPoint, mp)
		if err := os.MkdirAll(hostMP, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir volume %s: %w", hostMP, err)
		}
		var flags uintptr
		if vm.GetReadOnly() {
			flags |= syscall.MS_RDONLY
		}
		if err := syscall.Mount(dev, hostMP, fstype, flags, ""); err != nil && err != syscall.EBUSY {
			return nil, fmt.Errorf("mount %s -> %s: %w", dev, hostMP, err)
		}
		extraMounts = append(extraMounts, extraMount{src: hostMP, dst: mp, readOnly: vm.GetReadOnly()})
		a.mu.Lock()
		a.volumeMounts = append(a.volumeMounts, hostMP)
		a.mu.Unlock()
	}

	cwd := req.GetWorkingDir()
	if cwd == "" {
		cwd = "/"
	}
	if err := writeOCIConfig(bundleDir, req.GetCommand(), req.GetEnv(), cwd, extraMounts); err != nil {
		return nil, fmt.Errorf("write config.json: %w", err)
	}

	// Fresh log file for this run.
	_ = os.Remove(logPath)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer logFile.Close()

	// Clean up any stale container from a previous boot.
	_ = runRuncQuiet("delete", "--force", containerID)

	// runc run -d: fork + detach. Container's stdio is the fds we pass to
	// runc here (we pass the log file for stdout/stderr, /dev/null for stdin).
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()

	cmd := exec.Command(runcBinary, "run", "--bundle", bundleDir, "-d", "--pid-file", pidFile, containerID)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	execMu.Lock()
	err = cmd.Run()
	execMu.Unlock()
	if err != nil {
		// runc wrote its own error to the log file; surface what we have.
		tail, _ := os.ReadFile(logPath)
		return nil, fmt.Errorf("runc run: %w: %s", err, strings.TrimSpace(string(tail)))
	}

	pid, _ := readPID()
	a.mu.Lock()
	a.started = true
	a.phase = capsulev1.PayloadPhase_PAYLOAD_PHASE_RUNNING
	a.logDone = false
	a.exitCode = 0
	a.message = ""
	if !a.watching {
		a.watching = true
		go a.watchPayload()
	}
	a.mu.Unlock()

	log.Printf("capsule-guest: payload started pid=%d argv=%v", pid, req.GetCommand())
	return &capsulev1.StartPayloadResponse{PayloadPid: int32(pid)}, nil
}

// watchPayload polls runc state until the container exits, then records
// phase/exit_code and wakes Logs followers.
func (a *agent) watchPayload() {
	for {
		time.Sleep(500 * time.Millisecond)
		st, err := runcState()
		if err != nil {
			a.mu.Lock()
			a.phase = capsulev1.PayloadPhase_PAYLOAD_PHASE_FAILED
			a.message = "runc state: " + err.Error()
			a.logDone = true
			a.logCond.Broadcast()
			a.mu.Unlock()
			return
		}
		switch st.Status {
		case "running", "created", "paused":
			continue
		case "stopped":
			code := int32(0)
			if st.ExitStatus != nil {
				code = int32(*st.ExitStatus)
			}
			a.mu.Lock()
			a.phase = capsulev1.PayloadPhase_PAYLOAD_PHASE_EXITED
			a.exitCode = code
			a.logDone = true
			a.logCond.Broadcast()
			a.mu.Unlock()
			log.Printf("capsule-guest: payload exited code=%d", code)
			return
		default:
			a.mu.Lock()
			a.phase = capsulev1.PayloadPhase_PAYLOAD_PHASE_FAILED
			a.message = "unknown runc status: " + st.Status
			a.logDone = true
			a.logCond.Broadcast()
			a.mu.Unlock()
			return
		}
	}
}

// Status reports the last-observed payload state.
func (a *agent) Status(_ context.Context, _ *capsulev1.StatusRequest) (*capsulev1.StatusResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &capsulev1.StatusResponse{
		Phase:    a.phase,
		ExitCode: a.exitCode,
		Message:  a.message,
	}, nil
}

// Logs streams combined stdout+stderr from payload.log. follow tails
// until the container exits (watchPayload broadcasts logDone).
func (a *agent) Logs(req *capsulev1.LogsRequest, stream capsulev1.GuestAgent_LogsServer) error {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	buf := make([]byte, 4096)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&capsulev1.LogChunk{Data: append([]byte(nil), buf[:n]...)}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			if !req.GetFollow() {
				return nil
			}
			a.mu.Lock()
			done := a.logDone
			if !done {
				a.logCond.Wait()
			}
			a.mu.Unlock()
			if done {
				return nil
			}
			continue
		}
		if rerr != nil {
			return rerr
		}
	}
}

// Exec runs `runc exec` inside the payload container. TTY mode uses
// runc's --tty (runc allocates the pty and owns master/slave); non-TTY
// mode uses plain pipes. Stdio flows over the gRPC bidi stream either way.
func (a *agent) Exec(stream capsulev1.GuestAgent_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	cfg := first.GetConfig()
	if cfg == nil {
		return fmt.Errorf("first message must be config")
	}
	if len(cfg.GetCommand()) == 0 {
		return fmt.Errorf("command is required")
	}

	// Build `runc exec [--tty] [...env...] <id> <argv>`. When the caller
	// wants a TTY we both (a) allocate a pty for runc via pty.Start below
	// AND (b) pass --tty so runc sets up a controlling terminal for the
	// exec'd process (otherwise `sh -i` prints "can't access tty; job
	// control turned off" and doesn't render a prompt). Since runc's own
	// stdio is a tty (our pty slave), runc accepts --tty without needing
	// --console-socket in foreground mode.
	args := []string{"exec"}
	if cfg.GetTty() {
		args = append(args, "--tty")
	}
	for k, v := range cfg.GetEnv() {
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, containerID)
	args = append(args, cfg.GetCommand()...)

	cmd := exec.Command(runcBinary, args...)

	var (
		ptmx         *os.File
		stdinWriter  io.Writer
		stdoutReader io.Reader
		stderrReader io.Reader
	)
	// Hold execMu for the runc subprocess lifetime so the PID-1 reap
	// loop doesn't wait4 its pid before our own cmd.Wait() below can.
	execMu.Lock()
	defer execMu.Unlock()

	if cfg.GetTty() {
		var err error
		ptmx, err = pty.Start(cmd)
		if err != nil {
			return fmt.Errorf("pty start runc: %w", err)
		}
		defer ptmx.Close()
		stdinWriter = ptmx
		stdoutReader = ptmx
		// stderr is merged into stdout on a pty.
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		stdinWriter = stdin
		stdoutReader = stdout
		stderrReader = stderr
		if err := cmd.Start(); err != nil {
			return err
		}
	}

	// client → runc stdin (and resize events for tty mode)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				// Close stdin-side on EOF/disconnect so runc exits.
				if closer, ok := stdinWriter.(io.Closer); ok {
					_ = closer.Close()
				}
				return
			}
			switch p := m.Payload.(type) {
			case *capsulev1.ExecClientMessage_Stdin:
				_, _ = stdinWriter.Write(p.Stdin)
			case *capsulev1.ExecClientMessage_Resize:
				if ptmx != nil {
					_ = pty.Setsize(ptmx, &pty.Winsize{
						Rows: uint16(p.Resize.GetRows()),
						Cols: uint16(p.Resize.GetCols()),
					})
				}
			}
		}
	}()

	sendMu := sync.Mutex{}
	pump := func(r io.Reader, makeMsg func([]byte) *capsulev1.ExecServerMessage) {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sendMu.Lock()
				_ = stream.Send(makeMsg(append([]byte(nil), buf[:n]...)))
				sendMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	done := make(chan struct{}, 2)
	go func() {
		pump(stdoutReader, func(b []byte) *capsulev1.ExecServerMessage {
			return &capsulev1.ExecServerMessage{Payload: &capsulev1.ExecServerMessage_Stdout{Stdout: b}}
		})
		done <- struct{}{}
	}()
	if stderrReader != nil {
		go func() {
			pump(stderrReader, func(b []byte) *capsulev1.ExecServerMessage {
				return &capsulev1.ExecServerMessage{Payload: &capsulev1.ExecServerMessage_Stderr{Stderr: b}}
			})
			done <- struct{}{}
		}()
	} else {
		done <- struct{}{}
	}

	werr := cmd.Wait()
	<-done
	<-done

	code := int32(0)
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			code = int32(ee.ExitCode())
		} else {
			code = -1
		}
	}
	sendMu.Lock()
	defer sendMu.Unlock()
	return stream.Send(&capsulev1.ExecServerMessage{
		Payload: &capsulev1.ExecServerMessage_Exit{Exit: &capsulev1.ExecExit{ExitCode: code}},
	})
}

// Stop sends SIGTERM via runc, then SIGKILL after grace_seconds, then
// unmounts every user volume the payload had attached. The unmount step
// is the data-safety boundary: ext4 commits its journal on umount(2),
// and once umount returns the writes are durable on the underlying
// virtio-blk device. Without it, the host's Remove path SIGTERMs
// Firecracker moments later and the guest kernel's dirty pages — which
// include freshly-extracted tarball contents, sqlite writes, etc. —
// never make it back to the host's LV. Result: silent data loss visible
// to the next mounter.
func (a *agent) Stop(_ context.Context, req *capsulev1.StopRequest) (*capsulev1.StopResponse, error) {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return &capsulev1.StopResponse{}, nil
	}
	a.mu.Unlock()

	_ = runRuncQuiet("kill", containerID, "TERM")

	grace := time.Duration(req.GetGraceSeconds()) * time.Second
	if grace <= 0 {
		grace = 10 * time.Second
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		st, err := runcState()
		if err != nil || st.Status == "stopped" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = runRuncQuiet("kill", containerID, "KILL")
	_ = runRuncQuiet("delete", "--force", containerID)

	// Unmount user volumes in reverse order so a deeper mount comes off
	// before a parent. syscall.Sync() afterward catches anything the
	// kernel had outside the per-mount writeback (belt and braces — ext4
	// umount already commits the journal).
	a.mu.Lock()
	mps := a.volumeMounts
	a.volumeMounts = nil
	a.started = false
	a.mu.Unlock()
	for i := len(mps) - 1; i >= 0; i-- {
		if err := syscall.Unmount(mps[i], 0); err != nil {
			// MNT_DETACH lets the kernel clean up after the last user
			// (we just killed runc; nothing should be holding it now,
			// but guard anyway). Failure here means a leaked mount in
			// the dying VM — annoying but not data-losing on its own.
			if err2 := syscall.Unmount(mps[i], syscall.MNT_DETACH); err2 != nil {
				log.Printf("capsule-guest: umount %s: %v / %v", mps[i], err, err2)
			}
		}
	}
	syscall.Sync()
	return &capsulev1.StopResponse{}, nil
}

// --- helpers ---------------------------------------------------------------

// runcStateResult mirrors the JSON `runc state <id>` prints.
type runcStateResult struct {
	OCIVersion string    `json:"ociVersion"`
	ID         string    `json:"id"`
	PID        int       `json:"pid"`
	Status     string    `json:"status"`
	Bundle     string    `json:"bundle"`
	ExitStatus *int      `json:"exitStatus,omitempty"`
	Created    time.Time `json:"created,omitempty"`
}

func runcState() (*runcStateResult, error) {
	execMu.Lock()
	defer execMu.Unlock()
	out, err := exec.Command(runcBinary, "state", containerID).Output()
	if err != nil {
		return nil, fmt.Errorf("runc state: %w", err)
	}
	var r runcStateResult
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func runRuncQuiet(args ...string) error {
	execMu.Lock()
	defer execMu.Unlock()
	cmd := exec.Command(runcBinary, args...)
	return cmd.Run()
}

func readPID() (int, error) {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	var pid int
	_, _ = fmt.Sscanf(string(b), "%d", &pid)
	return pid, nil
}
