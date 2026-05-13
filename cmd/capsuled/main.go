package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/geekgonecrazy/capsule/auth"
	"github.com/geekgonecrazy/capsule/boot"
	"github.com/geekgonecrazy/capsule/controllers"
	coreimage "github.com/geekgonecrazy/capsule/core/image"
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

// resetAuthSentinel is the file an operator with local console access
// touches to wipe the authorized_keys table on next boot. Capsuled only
// honors it when the file's mtime is within resetAuthMaxAge — power-
// cycling the box is not enough to wipe identity, which is the point.
const (
	resetAuthSentinel = "/perm/capsule/RESET_AUTH"
	resetAuthMaxAge   = 5 * time.Minute
)

// claimWindowDuration is how long a fresh (zero-keys-enrolled) capsule
// accepts unauthenticated Adopt calls before slamming the gate shut.
// 30 min covers a typical "boot + alt-tab + remember to adopt" flow
// without leaving the window open indefinitely.
const claimWindowDuration = 30 * time.Minute

// version is overridden at build time via -ldflags '-X main.version=...'.
var version = "dev"

// CapsuleLogPath is where capsuled tees its slog output for
// CapsuleService.StreamLogs to tail. Lives on tmpfs so it vanishes at
// reboot; persistent operator logs are a future addition.
const CapsuleLogPath = "/run/capsule/capsuled.log"

func main() {
	addr := flag.String("addr", ":50000", "gRPC listen address")
	enableReflection := flag.Bool("enable-reflection", false, "expose grpc reflection (dev only — leaks wire schema)")
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

	// Honor the operator-triggered RESET_AUTH sentinel before opening
	// the SQLite DB so the wipe path is one path, not two. Recent mtime
	// is required: power-cycling alone must not wipe credentials.
	if isPID1 && bootResult.MountedPerm {
		if info, err := os.Stat(resetAuthSentinel); err == nil {
			if time.Since(info.ModTime()) <= resetAuthMaxAge {
				slog.Warn("RESET_AUTH sentinel honored — wiping authorized keys", "mtime", info.ModTime())
				if err := wipeAuthOnDisk("/perm/capsule/state.db"); err != nil {
					slog.Error("RESET_AUTH wipe failed", "err", err)
				}
				_ = os.Remove(resetAuthSentinel)
			} else {
				slog.Warn("RESET_AUTH sentinel ignored — too old", "mtime", info.ModTime(), "max_age", resetAuthMaxAge)
			}
		}
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

	// --- identity + TLS + auth ---
	// Seed the singleton CapsuleIdentity row on first boot. UUIDv4 lives
	// for the life of the disk; pinned by JWT `aud` so a token minted
	// for one capsule can't be replayed on another.
	identity, err := ensureIdentity(ctx, st.Identity())
	if err != nil {
		slog.Error("ensure identity failed; capsule will refuse all RPCs", "err", err)
	}
	tlsCertPath, tlsKeyPath := "/perm/tls/server.crt", "/perm/tls/server.key"
	if !bootResult.MountedPerm {
		// Dev / non-PID-1 run — keep TLS material in tmpfs so the dev
		// loop doesn't litter the operator's host with leftover certs.
		dir, derr := os.MkdirTemp("", "capsule-tls-*")
		if derr == nil {
			tlsCertPath = dir + "/server.crt"
			tlsKeyPath = dir + "/server.key"
		}
	}
	tlsCert, err := auth.LoadOrGenerate(tlsCertPath, tlsKeyPath, identity.CapsuleID)
	if err != nil {
		slog.Error("TLS keypair load/generate failed", "err", err)
	}
	tlsFingerprint, _ := auth.LeafFingerprint(tlsCert)

	enrolledCount, _ := st.AuthorizedKeys().Count(ctx)
	claim := auth.NewClaimWindow(enrolledCount, claimWindowDuration)
	if claim.Open() {
		slog.Warn("claim window OPEN — first capsulectl adopt within "+claimWindowDuration.String()+" wins",
			"capsule_id", identity.CapsuleID, "tls_fingerprint", tlsFingerprint)
	} else {
		slog.Info("capsule adopted",
			"capsule_id", identity.CapsuleID, "enrolled_keys", enrolledCount, "tls_fingerprint", tlsFingerprint)
	}

	stopAuth := make(chan struct{})
	defer close(stopAuth)
	authn := auth.NewAuthenticator(identity.CapsuleID,
		func(ctx context.Context, kid string) (ed25519.PublicKey, bool) {
			k, err := st.AuthorizedKeys().Get(ctx, kid)
			if err != nil {
				return nil, false
			}
			pub, perr := auth.ParsePubkey(k.Pubkey)
			if perr != nil {
				return nil, false
			}
			return pub, true
		}, claim, stopAuth)

	identityCtl := &controllers.IdentityController{
		Identity:       st.Identity(),
		Keys:           st.AuthorizedKeys(),
		Claim:          claim,
		TLSFingerprint: tlsFingerprint,
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

	// ImageService backs `capsulectl image list / push`. The same
	// containerd driver that runs containers also implements the
	// ImageStore port — when containerDriver is nil (dev mode) the
	// service surfaces FailedPrecondition.
	var imageStore runtime.ImageStore
	if cd, ok := containerDriver.(runtime.ImageStore); ok {
		imageStore = cd
	}
	imageSvc := coreimage.New(imageStore)
	imageCtl := &controllers.ImageController{Service: imageSvc}

	if containerDriver != nil || vmDriver != nil {
		rec := reconciler.New(reconciler.Config{
			Service:  workloadSvc,
			Driver:   containerDriver,
			VM:       vmDriver,
			Interval: 2 * time.Second,
		})
		// Wake the reconciler immediately on Apply/Start/Stop/Restart/Delete
		// so the operator doesn't pay a tick interval (~2s) of latency on
		// every desired-state change. Idle drift between ticks is still
		// caught by the regular timer.
		workloadSvc.SetOnChange(rec.Kick)
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
			n, _ := st.AuthorizedKeys().Count(ctx)
			boot.PrintBanner(boot.BannerInfo{
				GRPCPort:       port,
				TLSFingerprint: tlsFingerprint,
				ClaimOpen:      claim.Open(),
				EnrolledKeys:   n,
			})
		}()
	}

	if err := router.Serve(ctx, router.Config{
		Addr:             *addr,
		TLSCert:          tlsCert,
		Auth:             authn,
		EnableReflection: *enableReflection,
		Capsule:          capsuleCtl,
		Workload:         workloadCtl,
		Volume:           volumeCtl,
		Image:            imageCtl,
		Identity:         identityCtl,
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

// ensureIdentity returns the singleton CapsuleIdentity row, generating
// it on first boot. Called before the gRPC server starts so the JWT
// audience is stable from the very first RPC.
func ensureIdentity(ctx context.Context, ids store.IdentityStore) (*store.CapsuleIdentity, error) {
	id, err := ids.Get(ctx)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	fresh := &store.CapsuleIdentity{
		CapsuleID:     uuid.NewString(),
		CreatedAtUnix: time.Now().Unix(),
	}
	if err := ids.Put(ctx, fresh); err != nil {
		return nil, err
	}
	slog.Info("generated capsule_id on first boot", "capsule_id", fresh.CapsuleID)
	return fresh, nil
}

// wipeAuthOnDisk handles the RESET_AUTH recovery path before the main
// SQLite handle opens. Opens its own short-lived handle, deletes every
// row in authorized_keys, and clears adopted_at_unix / adopted_by_kid
// on the singleton identity row so the next claim window opens.
func wipeAuthOnDisk(dbPath string) error {
	s, err := sqlite.Open(dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.AuthorizedKeys().DeleteAll(ctx); err != nil {
		return err
	}
	id, err := s.Identity().Get(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if id != nil {
		id.AdoptedAtUnix = 0
		id.AdoptedByKid = ""
		if err := s.Identity().Put(ctx, id); err != nil {
			return err
		}
	}
	return nil
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
