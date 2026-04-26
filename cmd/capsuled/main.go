package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/geekgonecrazy/capsule/boot"
	"github.com/geekgonecrazy/capsule/controllers"
	"github.com/geekgonecrazy/capsule/core/reconciler"
	coreupdate "github.com/geekgonecrazy/capsule/core/update"
	corevolume "github.com/geekgonecrazy/capsule/core/volume"
	"github.com/geekgonecrazy/capsule/core/workload"
	"github.com/geekgonecrazy/capsule/router"
	"github.com/geekgonecrazy/capsule/runtime"
	containerRT "github.com/geekgonecrazy/capsule/runtime/container"
	firecrackerRT "github.com/geekgonecrazy/capsule/runtime/microvm/firecracker"
	"github.com/geekgonecrazy/capsule/store"
	"github.com/geekgonecrazy/capsule/store/memory"
	"github.com/geekgonecrazy/capsule/store/sqlite"
	"github.com/geekgonecrazy/capsule/supervise"
)

// version is overridden at build time via -ldflags '-X main.version=...'.
var version = "dev"

// CapsuleLogPath is where capsuled tees its slog output for
// CapsuleService.StreamLogs to tail. Lives on tmpfs so it vanishes at
// reboot; persistent operator logs are a future addition.
const CapsuleLogPath = "/run/capsule/capsuled.log"

func main() {
	addr := flag.String("addr", ":50000", "gRPC listen address")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	isPID1 := os.Getpid() == 1

	// Set up slog before boot.Init so that any errors during early boot
	// (mounts, module loads, NIC bring-up) are visible. On PID 1 we tee
	// to /dev/tty0 — initramfs already mounted devtmpfs so it's open-able
	// — to ensure logs reach HDMI on hardware where the kernel cmdline
	// console=tty0 console=ttyS0 routes /dev/console at the (nonexistent
	// on this Beelink) serial port.
	earlyWriters := []io.Writer{os.Stderr}
	if isPID1 {
		if tty, err := os.OpenFile("/dev/tty0", os.O_WRONLY, 0); err == nil {
			earlyWriters = append(earlyWriters, tty)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(earlyWriters...), &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("capsule starting", "version", version, "pid1", isPID1)

	bootResult := boot.Result{}
	if isPID1 {
		r, err := boot.Init(ctx)
		if err != nil {
			// PID 1 can't just exit — the kernel would panic. Log and
			// fall through to the supervision loop so the operator can
			// at least reach a console.
			slog.Error("boot.Init failed", "err", err)
		}
		bootResult = r
		go boot.ReapZombies(ctx)
	}

	// /run is mounted now — extend the slog tee to also write the
	// capsule log file so CapsuleService.StreamLogs can tail it.
	if err := os.MkdirAll("/run/capsule", 0o755); err == nil {
		if f, err := os.OpenFile(CapsuleLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
			writers := append(earlyWriters, f)
			slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: slog.LevelInfo})))
			slog.Info("capsule log tee", "path", CapsuleLogPath)
		}
	}

	if isPID1 && bootResult.MountedPerm {
		if err := os.MkdirAll("/run/containerd", 0o711); err != nil {
			slog.Error("mkdir /run/containerd", "err", err)
		}
		go supervise.Run(ctx, supervise.Config{
			Name:       "containerd",
			Path:       "/usr/bin/containerd",
			Args:       []string{"--config", "/etc/containerd/config.toml"},
			MinRestart: 500 * time.Millisecond,
			MaxRestart: 30 * time.Second,
		})
	}

	// --- store ---
	var st store.Store
	if bootResult.MountedPerm {
		ss, err := sqlite.Open("/perm/capsule/state.db")
		if err != nil {
			slog.Error("sqlite open failed, falling back to memory store", "err", err)
			st = memory.New()
		} else {
			st = ss
		}
	} else {
		// Dev / non-PID-1 run: no /perm → use in-memory store.
		slog.Info("no /perm mount; using in-memory store")
		st = memory.New()
	}
	defer st.Close()

	// --- A/B update service: handles UpdateOS / UpdateConfirm + tentative-deadline auto-rollback ---
	updateSvc := coreupdate.New(st.OSState(), bootResult.ActiveSlot)
	if isPID1 && bootResult.ActiveSlot != "" {
		if err := updateSvc.OnStartup(ctx); err != nil {
			slog.Error("update OnStartup failed", "err", err)
		}
	}

	capsuleCtl := &controllers.CapsuleController{
		LogPath:        CapsuleLogPath,
		CapsuleVersion: version,
		ActiveSlot:     bootResult.ActiveSlot,
		OSStateStore:   st.OSState(),
		UpdateService:  updateSvc,
	}

	// --- runtime driver (best-effort; workload APIs error out if nil) ---
	var containerDriver runtime.ContainerDriver
	if isPID1 {
		// Wait briefly for containerd to come up — it needs /run/containerd
		// and the supervisor to fork it. Best-effort with a short retry.
		containerDriver = dialContainerd(ctx, "/run/containerd/containerd.sock", 30*time.Second)
	}

	// --- runtime drivers ---
	var vmDriver runtime.VMDriver
	if isPID1 {
		vmDriver = firecrackerRT.New()
	}

	// --- core + controllers ---
	workloadSvc := workload.New(st, containerDriver, vmDriver)
	workloadCtl := &controllers.WorkloadController{Service: workloadSvc}

	volumeSvc := corevolume.New(st)
	volumeCtl := &controllers.VolumeController{Service: volumeSvc}

	if containerDriver != nil || vmDriver != nil {
		rec := reconciler.New(reconciler.Config{
			Service:  workloadSvc,
			Driver:   containerDriver,
			VM:       vmDriver,
			Interval: 2 * time.Second,
		})
		go rec.Run(ctx)
	}

	// Banner with IP for HDMI operators — print after gRPC has had a moment
	// to bind. Goroutine so we don't block Serve.
	if isPID1 {
		go func() {
			time.Sleep(2 * time.Second)
			port := 50000
			if _, p, err := net.SplitHostPort(*addr); err == nil {
				if n, err := strconv.Atoi(p); err == nil {
					port = n
				}
			}
			boot.PrintBanner(port)
		}()
	}

	if err := router.Serve(ctx, router.Config{
		Addr:     *addr,
		Capsule:  capsuleCtl,
		Workload: workloadCtl,
		Volume:   volumeCtl,
	}); err != nil {
		slog.Error("gRPC server failed", "err", err)
		if isPID1 {
			// PID 1 must not exit. Block on context so the kernel doesn't panic.
			<-ctx.Done()
		} else {
			os.Exit(1)
		}
	}

	if isPID1 {
		// Clean shutdown path: hold here until ctx done, then signal the
		// kernel to reboot. For phase 0 we just block forever; proper
		// reboot(2) call comes with phase 3 (updates).
		<-ctx.Done()
		slog.Info("capsule stopped")
		select {}
	}
}

// dialContainerd polls until the containerd socket is reachable or timeout
// elapses. Returns nil on failure; callers treat that as "no runtime yet".
func dialContainerd(ctx context.Context, socket string, timeout time.Duration) runtime.ContainerDriver {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil
		}
		d, err := containerRT.New(socket)
		if err == nil {
			slog.Info("containerd connected", "socket", socket)
			return d
		}
		time.Sleep(500 * time.Millisecond)
	}
	slog.Error("containerd unreachable after timeout", "socket", socket, "timeout", timeout.String())
	return nil
}
