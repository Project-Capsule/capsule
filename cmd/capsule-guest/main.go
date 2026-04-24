//go:build linux

// capsule-guest is PID 1 inside every Capsule-managed microVM. It does the
// minimum required to turn a booted Firecracker VM into something capsuled
// on the host can drive: early mounts, a vsock gRPC listener on port 52,
// and implementations of the capsule.guest.v1.GuestAgent RPCs. The payload
// OCI image lives on a separate block device (/dev/vdb) and is mounted
// on demand when StartPayload is called.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"
)

// vsockPort is the AF_VSOCK port the agent listens on. capsuled on the host
// dials it via the per-VM vsock unix socket at /run/capsule/vms/<id>/vsock_52.
const vsockPort = 52

func main() {
	// Unambiguous first line — written via syscall directly to fd 2 so we
	// can confirm capsule-guest is running even if log/slog setup is broken.
	syscall.Write(2, []byte("capsule-guest: pid1 entered main\n"))
	// Also spray to /dev/kmsg so it lands in the kernel log regardless of
	// where stderr ends up.
	if f, ferr := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); ferr == nil {
		_, _ = f.WriteString("capsule-guest: pid1 entered main\n")
		_ = f.Close()
	}

	// PID 1 must never return. On any fatal error, log and block forever
	// so the Firecracker serial console captures the reason.
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "capsule-guest: %v\n", err)
		fmt.Fprintln(os.Stderr, "capsule-guest: init failed; blocking (reboot the VM to retry)")
		// time.Sleep keeps a runtime timer scheduled, so Go's deadlock
		// detector stays quiet even when no other goroutines are alive.
		// (Plain `select {}` panics when this is the only goroutine.)
		for {
			time.Sleep(time.Hour)
		}
	}
}

func run() error {
	if err := earlyMounts(); err != nil {
		return fmt.Errorf("early mounts: %w", err)
	}

	agent := newAgent()
	go agent.reapLoop()

	lis, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		return fmt.Errorf("vsock listen :%d: %w", vsockPort, err)
	}
	log.Printf("capsule-guest: vsock agent listening on :%d", vsockPort)

	srv := grpc.NewServer()
	capsulev1.RegisterGuestAgentServer(srv, agent)

	// Handle shutdown signals so we can at least log them, even though
	// Firecracker usually kills us without warning.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		log.Printf("capsule-guest: got %s, stopping gRPC", s)
		srv.GracefulStop()
	}()

	return srv.Serve(lis)
}

func earlyMounts() error {
	type m struct {
		source, target, fstype string
		flags                  uintptr
		data                   string
		fatal                  bool
	}
	mounts := []m{
		// Critical mounts — without these capsule-guest can't function.
		{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, "", true},
		{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, "", true},
		{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755", true},
		{"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=0755", true},
		{"tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777", true},
		// Best-effort — features that fail here degrade gracefully:
		// devpts: needed for `exec -t` PTY allocation.
		// cgroup2: needed for runc to create containers.
		{"devpts", "/dev/pts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "ptmxmode=0666,mode=0620,gid=5", false},
		{"cgroup2", "/sys/fs/cgroup", "cgroup2", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, "", false},
	}
	for _, mt := range mounts {
		_ = os.MkdirAll(mt.target, 0o755)
		err := syscall.Mount(mt.source, mt.target, mt.fstype, mt.flags, mt.data)
		if err == nil || err == syscall.EBUSY {
			continue
		}
		if mt.fatal {
			return fmt.Errorf("mount %s -> %s: %w", mt.source, mt.target, err)
		}
		fmt.Fprintf(os.Stderr, "capsule-guest: mount %s -> %s (non-fatal): %v\n", mt.source, mt.target, err)
	}
	// devpts with ptmxmode exposes the ptmx multiplexer at /dev/pts/ptmx;
	// tooling (runc) also expects /dev/ptmx. Symlink it in for both paths.
	if _, err := os.Lstat("/dev/ptmx"); os.IsNotExist(err) {
		_ = os.Symlink("pts/ptmx", "/dev/ptmx")
	}
	return nil
}
