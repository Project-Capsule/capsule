package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"
	sigsyaml "sigs.k8s.io/yaml"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

func main() {
	// Global flags first: --capsule host:port. Then subcommand and its args.
	// CAPSULE_HOST env var sets the default so daily use doesn't repeat it.
	global := flag.NewFlagSet("capsulectl", flag.ExitOnError)
	defaultAddr := os.Getenv("CAPSULE_HOST")
	if defaultAddr == "" {
		defaultAddr = "localhost:50000"
	}
	addr := global.String("capsule", defaultAddr, "capsule gRPC address (overrides $CAPSULE_HOST)")
	if err := global.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	rest := global.Args()
	// `cp` is a top-level verb (no group/subcommand split), so we
	// dispatch it before the group+cmd switch.
	if len(rest) >= 1 && rest[0] == "cp" {
		if err := runCp(*addr, rest[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if len(rest) < 2 {
		usage()
		os.Exit(2)
	}
	group, cmd := rest[0], rest[1]
	subArgs := rest[2:]

	var err error
	switch group + " " + cmd {
	case "capsule info":
		err = capsuleInfo(*addr)
	case "capsule update":
		// "capsule update push <bundle> [--auto-confirm=N]" / "capsule update confirm"
		if len(subArgs) < 1 {
			err = errors.New("capsule update requires a subcommand: push | confirm")
			break
		}
		err = capsuleUpdate(*addr, subArgs[0], subArgs[1:])
	case "capsule debug":
		// `capsule debug [-i <image>] [--keep] [-- <cmd> [args...]]`
		preDash, postDash := splitAtDashDash(subArgs)
		fs := flag.NewFlagSet("capsule debug", flag.ExitOnError)
		image := fs.String("i", "docker.io/library/alpine:3.20", "image for the debug container")
		keep := fs.Bool("keep", false, "leave the debug workload running on exit")
		_ = fs.Parse(preDash)
		cmdArgs := postDash
		if len(cmdArgs) == 0 {
			cmdArgs = []string{"/bin/sh"}
		}
		err = capsuleDebug(*addr, *image, *keep, cmdArgs)
	case "capsule logs":
		fs := flag.NewFlagSet("capsule logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "stream new output until Ctrl-C")
		tail := fs.Int("n", 0, "show the last N lines before streaming")
		_ = fs.Parse(subArgs)
		err = capsuleLogs(*addr, *follow, *tail)
	case "apply -f":
		// kubectl-style: dispatches by `kind:` so a single command applies
		// either workloads (Container/MicroVM) or Volumes from one manifest.
		if len(subArgs) < 1 {
			err = errors.New("apply -f requires a manifest path")
			break
		}
		err = applyManifest(*addr, subArgs[0])
	case "workload list":
		err = workloadList(*addr)
	case "workload get":
		if len(subArgs) < 1 {
			err = errors.New("workload get requires a name")
			break
		}
		err = workloadGet(*addr, subArgs[0])
	case "workload delete":
		if len(subArgs) < 1 {
			err = errors.New("workload delete requires a name")
			break
		}
		err = workloadDelete(*addr, subArgs[0])
	case "workload restart":
		if len(subArgs) < 1 {
			err = errors.New("workload restart requires a name")
			break
		}
		err = workloadLifecycle(*addr, subArgs[0], "restart")
	case "workload stop":
		if len(subArgs) < 1 {
			err = errors.New("workload stop requires a name")
			break
		}
		err = workloadLifecycle(*addr, subArgs[0], "stop")
	case "workload start":
		if len(subArgs) < 1 {
			err = errors.New("workload start requires a name")
			break
		}
		err = workloadLifecycle(*addr, subArgs[0], "start")
	case "workload logs":
		// Accept flags either before or after the workload name
		// (Go's default parser stops at the first non-flag arg, which
		// is annoying for subcommands).
		flagTokens, positionals := partitionFlags(subArgs)
		fs := flag.NewFlagSet("workload logs", flag.ExitOnError)
		follow := fs.Bool("f", false, "stream new output until Ctrl-C")
		tail := fs.Int("n", 0, "show the last N lines before streaming")
		serial := fs.Bool("serial", false, "MicroVM only: stream the VM serial console (kernel boot, capsule-guest, Firecracker) instead of the payload log")
		_ = fs.Parse(flagTokens)
		if len(positionals) < 1 {
			err = errors.New("workload logs requires a name")
			break
		}
		err = workloadLogs(*addr, positionals[0], *follow, *tail, *serial)
	case "volume create":
		// Go's flag.Parse stops at the first non-flag token. We (a) join
		// "--foo VAL" into "--foo=VAL" so they're a single token, then
		// (b) front-load all flag tokens ahead of positionals so Parse
		// sees them before it bails.
		normalized := frontLoadFlags(joinValueFlags(subArgs, map[string]bool{"size": true}))
		fs := flag.NewFlagSet("volume create", flag.ExitOnError)
		size := fs.String("size", "", "size with unit suffix (e.g. 10GiB, 512MiB); bare int = MiB; omit for default")
		_ = fs.Parse(normalized)
		if fs.NArg() < 1 {
			err = errors.New("volume create requires a name")
			break
		}
		var sizeMiB uint64
		if *size != "" {
			sizeMiB, err = parseSize(*size)
			if err != nil {
				err = fmt.Errorf("--size: %w", err)
				break
			}
		}
		err = volumeCreate(*addr, fs.Arg(0), sizeMiB)
	case "volume list":
		err = volumeList(*addr)
	case "volume get":
		if len(subArgs) < 1 {
			err = errors.New("volume get requires a name")
			break
		}
		err = volumeGet(*addr, subArgs[0])
	case "volume delete":
		fs := flag.NewFlagSet("volume delete", flag.ExitOnError)
		force := fs.Bool("force", false, "delete even if workloads reference it")
		_ = fs.Parse(subArgs)
		if fs.NArg() < 1 {
			err = errors.New("volume delete requires a name")
			break
		}
		err = volumeDelete(*addr, fs.Arg(0), *force)
	case "image list":
		err = imageList(*addr)
	case "image push":
		if len(subArgs) < 1 {
			err = errors.New("image push requires a tarball path (or '-' for stdin)")
			break
		}
		err = imagePush(*addr, subArgs[0])
	case "volume resize":
		if len(subArgs) < 2 {
			err = errors.New("volume resize requires <name> <size>")
			break
		}
		var sizeMiB uint64
		sizeMiB, err = parseSize(subArgs[1])
		if err != nil {
			err = fmt.Errorf("size: %w", err)
			break
		}
		err = volumeResize(*addr, subArgs[0], sizeMiB)
	case "workload exec":
		// Split subArgs at `--` so the command can contain anything. Then
		// extract flag tokens (starting with '-') from the left half so
		// Go's flag parser (which stops at the first non-flag) doesn't
		// swallow them as the command.
		preDash, postDash := splitAtDashDash(subArgs)
		flagTokens, positionals := partitionFlags(preDash)
		fs := flag.NewFlagSet("workload exec", flag.ExitOnError)
		tty := fs.Bool("t", false, "allocate a PTY (for interactive shells)")
		_ = fs.Parse(flagTokens)
		if len(positionals) < 1 {
			err = errors.New("workload exec requires a name")
			break
		}
		name := positionals[0]
		cmdArgs := append(append([]string{}, positionals[1:]...), postDash...)
		if len(cmdArgs) == 0 {
			err = errors.New("workload exec requires a command (e.g. -- /bin/sh)")
			break
		}
		err = workloadExec(*addr, name, cmdArgs, *tty)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `capsulectl — capsule control CLI

Set CAPSULE_HOST in your environment to skip --capsule.

Usage:
  capsulectl [--capsule host:port] apply -f <manifest.yaml>     # any kind: Container/MicroVM/Volume
  capsulectl [--capsule host:port] capsule info
  capsulectl [--capsule host:port] capsule logs [-f] [-n N]
  capsulectl [--capsule host:port] capsule update push <bundle.tar> [--auto-confirm=N]
  capsulectl [--capsule host:port] capsule update confirm
  capsulectl [--capsule host:port] capsule debug [-i <image>] [--keep] [-- <cmd> [args...]]
  capsulectl [--capsule host:port] workload list
  capsulectl [--capsule host:port] workload get <name>
  capsulectl [--capsule host:port] workload delete <name>
  capsulectl [--capsule host:port] workload restart <name>
  capsulectl [--capsule host:port] workload stop <name>
  capsulectl [--capsule host:port] workload start <name>
  capsulectl [--capsule host:port] workload logs [-f] [-n N] [--serial] <name>
  capsulectl [--capsule host:port] workload exec [-t] <name> -- <cmd> [args...]
  capsulectl [--capsule host:port] cp <src> <dst>                # files/dirs to or from a workload
  capsulectl [--capsule host:port] volume create [--size 10GiB] <name>
  capsulectl [--capsule host:port] volume list
  capsulectl [--capsule host:port] volume get <name>
  capsulectl [--capsule host:port] volume delete [--force] <name>
  capsulectl [--capsule host:port] volume resize <name> <size>
  capsulectl [--capsule host:port] image list
  capsulectl [--capsule host:port] image push <tarball>             # '-' = stdin (pipe 'docker save')
`)
}

func dial(addr string) (*grpc.ClientConn, error) {
	// Keepalive: ping every 15 s during a stream and consider the
	// connection dead if no ack within 10 s. Without this, long-running
	// streams (logs -f, exec) just hang in Recv() forever when the
	// capsule reboots or the network drops — the OS won't notice the
	// dead TCP connection for ages. PermitWithoutStream lets us catch a
	// reboot even between RPCs. Time must stay >= the server's
	// EnforcementPolicy.MinTime (router.go) or pings get rejected.
	return grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                15 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

func withCtx(fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return fn(ctx)
}

// --- capsule logs ----------------------------------------------------------

func capsuleLogs(addr string, follow bool, tail int) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewCapsuleServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
	}()

	stream, err := client.StreamLogs(ctx, &capsulev1.CapsuleLogsRequest{
		Follow:    follow,
		TailLines: int32(tail),
	})
	if err != nil {
		return err
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		_, _ = os.Stdout.Write(chunk.GetData())
	}
}

// --- capsule info ----------------------------------------------------------

func capsuleInfo(addr string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewCapsuleServiceClient(conn)

	return withCtx(func(ctx context.Context) error {
		resp, err := client.GetInfo(ctx, &capsulev1.GetInfoRequest{})
		if err != nil {
			return err
		}
		fmt.Printf("Capsule %s\n", addr)
		fmt.Printf("  hostname:        %s\n", resp.Hostname)
		fmt.Printf("  kernel_release:  %s\n", resp.KernelRelease)
		fmt.Printf("  kernel_version:  %s\n", resp.KernelVersion)
		fmt.Printf("  architecture:    %s\n", resp.Architecture)
		fmt.Printf("  uptime:          %s\n", formatUptime(resp.UptimeSeconds))
		fmt.Printf("  capsule_version: %s\n", resp.CapsuleVersion)
		fmt.Printf("  active_slot:     %s\n", resp.ActiveSlot)
		if resp.LastVersion != "" {
			fmt.Printf("  last_version:    %s\n", resp.LastVersion)
		}
		if resp.LocalTimeUnix > 0 {
			capsuleNow := time.Unix(resp.LocalTimeUnix, 0).UTC()
			skew := time.Since(capsuleNow).Round(time.Second)
			skewStr := ""
			if abs := skew; abs < 0 {
				abs = -abs
				if abs > 5*time.Second {
					skewStr = fmt.Sprintf(" (skew: capsule is %s ahead)", abs)
				}
			} else if skew > 5*time.Second {
				skewStr = fmt.Sprintf(" (skew: capsule is %s behind)", skew)
			}
			fmt.Printf("  local_time:      %s%s\n", capsuleNow.Format(time.RFC3339), skewStr)
		}
		if resp.PendingSlot != "" {
			// Compute time-to-deadline in the *capsule's* clock frame so the
			// message stays meaningful even when the operator's laptop clock
			// drifts from the capsule's. Falls back to laptop frame if the
			// capsule didn't return its current time.
			var until time.Duration
			if resp.LocalTimeUnix > 0 {
				until = time.Duration(resp.PendingDeadlineUnix-resp.LocalTimeUnix) * time.Second
			} else {
				until = time.Until(time.Unix(resp.PendingDeadlineUnix, 0))
			}
			until = until.Round(time.Second)
			marker := "auto-rollback in " + until.String()
			if until <= 0 {
				marker = "deadline passed"
			}
			fmt.Printf("  pending_slot:    %s (%s — run `capsule update confirm` to commit)\n", resp.PendingSlot, marker)
		}
		if resp.CpuCores > 0 {
			model := resp.CpuModel
			if model == "" {
				model = "(unknown model)"
			}
			fmt.Printf("  cpu:             %d core(s) — %s\n", resp.CpuCores, model)
		}
		if resp.MemoryTotalBytes > 0 {
			used := resp.MemoryTotalBytes - resp.MemoryAvailableBytes
			fmt.Printf("  memory:          %s used / %s total (%.0f%% available)\n",
				humanBytes(used), humanBytes(resp.MemoryTotalBytes),
				100.0*float64(resp.MemoryAvailableBytes)/float64(resp.MemoryTotalBytes))
		}
		if resp.DiskTotalBytes > 0 {
			fmt.Printf("  disk:            %s — %s total\n", resp.BootDisk, humanBytes(resp.DiskTotalBytes))
		}
		if resp.ThinpoolTotalBytes > 0 {
			pct := 100.0 * float64(resp.ThinpoolUsedBytes) / float64(resp.ThinpoolTotalBytes)
			fmt.Printf("  volume pool:     %s used / %s total (%.1f%% full)\n",
				humanBytes(resp.ThinpoolUsedBytes), humanBytes(resp.ThinpoolTotalBytes), pct)
		}
		return nil
	})
}

// formatUptime renders seconds as "Xd Yh Zm Ss" trimmed to the largest
// nonzero unit.
func formatUptime(secs uint64) string {
	if secs == 0 {
		return "0s"
	}
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	parts := []string{}
	if d > 0 {
		parts = append(parts, fmt.Sprintf("%dd", d))
	}
	if h > 0 || d > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 || h > 0 || d > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	parts = append(parts, fmt.Sprintf("%ds", s))
	return strings.Join(parts, " ")
}

// --- capsule debug ---------------------------------------------------------

// capsuleDebug deploys a transient privileged container with the host's
// PID/IPC/mount namespaces shared and /perm + /sys + /dev + /run/capsule
// + /usr/sbin bind-mounted, then exec's into it for an interactive shell.
// On exit, deletes the workload unless --keep was passed. This is the
// breakglass path for "something's broken on the capsule and I need to
// poke at it" — equivalent to gokrazy's `breakglass` or the `kubectl
// debug node` pattern.
//
// Anyone who can reach :50000 can root the host this way; mTLS is a hard
// prereq before exposing the capsule on a hostile network (PLAN §3).
func capsuleDebug(addr, image string, keep bool, cmdArgs []string) error {
	const name = "capsule-debug"
	w := &capsulev1.Workload{
		Name: name,
		Kind: capsulev1.WorkloadKind_WORKLOAD_KIND_CONTAINER,
		Container: &capsulev1.ContainerSpec{
			Image:       image,
			Command:     []string{"/bin/sh", "-c"},
			Args:        []string{"sleep infinity"},
			NetworkMode: capsulev1.NetworkMode_NETWORK_MODE_HOST,
			Privileged:  true,
			HostPid:     true,
			// Deliberately NOT setting HostMount — keep the container in
			// its own mount namespace so the bind mounts below stay
			// scoped to it. With HostMount=true, OCI WithMounts runs in
			// the host ns, leaving bind mounts pinned to the rootfs that
			// containerd later refuses to unmount. The bind paths still
			// give us full visibility into /perm + LVM + capsuled state.
			HostBindPaths: []string{
				"/perm",
				"/sys",
				"/dev",
				"/run/capsule",
				// Bind /sbin so /sbin/lvs (and the host's iptables/ip/mount/
				// e2fsck) work without being shipped in the debug image.
				"/sbin",
				"/usr/sbin",
				"/usr/bin",
			},
		},
	}

	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	wsclient := capsulev1.NewWorkloadServiceClient(conn)

	// Apply (replaces if it already exists from a previous session).
	if err := withCtx(func(ctx context.Context) error {
		_, err := wsclient.Apply(ctx, &capsulev1.WorkloadApplyRequest{Workload: w})
		return err
	}); err != nil {
		return fmt.Errorf("apply debug workload: %w", err)
	}
	fmt.Fprintf(os.Stderr, "debug session — host /perm + /sys + /dev are bind-mounted; host PID ns shared.\n")
	fmt.Fprintf(os.Stderr, "for LVM/iptables/blkid run `apk add lvm2 e2fsprogs iptables iproute2` first;\n")
	fmt.Fprintf(os.Stderr, "future: a prebuilt capsule-debug image with the toolchain baked in (PLAN §).\n\n")

	// Wait for Running. Bound at 60 s — image pull + start should be quick.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		w, err := wsclient.Get(ctx, &capsulev1.WorkloadGetRequest{Name: name})
		cancel()
		if err == nil && w.GetStatus().GetPhase() == capsulev1.WorkloadPhase_WORKLOAD_PHASE_RUNNING {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// Exec into the container with a TTY. Use the Core variant — the
	// CLI wrapper calls os.Exit which would skip our cleanup defer.
	exitCode, execErr := workloadExecCore(addr, name, cmdArgs, true)

	// Cleanup (unless --keep). Service.Delete now marks DELETING first
	// so the reconciler stops touching the workload before driver.Remove
	// runs — no race that re-creates the container behind our back.
	if !keep {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := wsclient.Delete(ctx, &capsulev1.WorkloadDeleteRequest{Name: name}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: delete debug workload: %v\n", err)
		}
		cancel()
	}

	if execErr != nil {
		return execErr
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

// --- capsule update --------------------------------------------------------

func capsuleUpdate(addr, sub string, args []string) error {
	switch sub {
	case "push":
		// "capsule update push <bundle> [--auto-confirm=N]"
		normalized := frontLoadFlags(joinValueFlags(args, map[string]bool{"auto-confirm": true}))
		fs := flag.NewFlagSet("capsule update push", flag.ExitOnError)
		autoConfirm := fs.Int("auto-confirm", 0, "after push + reboot, wait N seconds, verify health, then auto-send confirm")
		_ = fs.Parse(normalized)
		if fs.NArg() < 1 {
			return errors.New("capsule update push requires a bundle path")
		}
		return capsuleUpdatePush(addr, fs.Arg(0), *autoConfirm)
	case "confirm":
		return capsuleUpdateConfirm(addr)
	default:
		return fmt.Errorf("unknown capsule update subcommand: %s", sub)
	}
}

func capsuleUpdatePush(addr, bundlePath string, autoConfirmSecs int) error {
	st, err := os.Stat(bundlePath)
	if err != nil {
		return fmt.Errorf("stat bundle: %w", err)
	}
	totalBytes := uint64(st.Size())
	sum, err := sha256File(bundlePath)
	if err != nil {
		return fmt.Errorf("sha256: %w", err)
	}

	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewCapsuleServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	stream, err := client.UpdateOS(ctx)
	if err != nil {
		return fmt.Errorf("UpdateOS: %w", err)
	}
	if err := stream.Send(&capsulev1.UpdateOSRequest{
		Msg: &capsulev1.UpdateOSRequest_Metadata{
			Metadata: &capsulev1.UpdateOSMetadata{
				TotalBytes: totalBytes,
				Sha256Hex:  sum,
			},
		},
	}); err != nil {
		return fmt.Errorf("send metadata: %w", err)
	}

	f, err := os.Open(bundlePath)
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	defer f.Close()
	buf := make([]byte, 1024*1024) // 1 MiB chunks
	var sent uint64
	lastTick := time.Now()
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&capsulev1.UpdateOSRequest{
				Msg: &capsulev1.UpdateOSRequest_Chunk{Chunk: append([]byte(nil), buf[:n]...)},
			}); err != nil {
				return fmt.Errorf("send chunk: %w", err)
			}
			sent += uint64(n)
			if time.Since(lastTick) > 500*time.Millisecond {
				fmt.Fprintf(os.Stderr, "\r  pushing... %d / %d MiB",
					sent/(1024*1024), totalBytes/(1024*1024))
				lastTick = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read bundle: %w", rerr)
		}
	}
	fmt.Fprintln(os.Stderr)

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("CloseAndRecv: %w", err)
	}
	fmt.Printf("update pushed: slot=%s version=%s\n", resp.NextSlot, resp.NextVersion)
	fmt.Printf("capsule will reboot now. wait for it to come back, then verify health.\n")

	if autoConfirmSecs <= 0 {
		fmt.Printf("when ready, run: capsulectl --capsule %s capsule update confirm\n", addr)
		return nil
	}
	return capsuleUpdateAutoConfirm(addr, resp.NextSlot, autoConfirmSecs)
}

func capsuleUpdateAutoConfirm(addr, expectSlot string, settleSecs int) error {
	fmt.Printf("auto-confirm: waiting for capsule to come back as %s, then settle for %ds\n", expectSlot, settleSecs)
	deadline := time.Now().Add(15 * time.Minute)
	// Phase 1: wait for capsule to become reachable on the expected slot.
	for time.Now().Before(deadline) {
		conn, err := dial(addr)
		if err == nil {
			client := capsulev1.NewCapsuleServiceClient(conn)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			resp, err := client.GetInfo(ctx, &capsulev1.GetInfoRequest{})
			cancel()
			conn.Close()
			if err == nil && resp.ActiveSlot == expectSlot {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	// Phase 2: settle window — re-poll periodically; bail if it goes unhealthy.
	settleEnd := time.Now().Add(time.Duration(settleSecs) * time.Second)
	for time.Now().Before(settleEnd) {
		time.Sleep(5 * time.Second)
		conn, err := dial(addr)
		if err != nil {
			return fmt.Errorf("auto-confirm: dial during settle: %w", err)
		}
		client := capsulev1.NewCapsuleServiceClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.GetInfo(ctx, &capsulev1.GetInfoRequest{})
		cancel()
		conn.Close()
		if err != nil {
			return fmt.Errorf("auto-confirm: GetInfo during settle: %w", err)
		}
		if resp.ActiveSlot != expectSlot {
			return fmt.Errorf("auto-confirm: active_slot drifted from %s to %s", expectSlot, resp.ActiveSlot)
		}
	}
	// Phase 3: send the confirm.
	return capsuleUpdateConfirm(addr)
}

func capsuleUpdateConfirm(addr string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewCapsuleServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		resp, err := client.UpdateConfirm(ctx, &capsulev1.UpdateConfirmRequest{})
		if err != nil {
			return err
		}
		fmt.Printf("update committed: slot=%s version=%s\n", resp.CommittedSlot, resp.CommittedVersion)
		return nil
	})
}

func sha256File(path string) (string, error) {
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

// --- unified apply ---------------------------------------------------------

// applyManifest reads a YAML/JSON manifest, peeks the `kind:` field, and
// dispatches to the matching service. Supports the same kinds the rest of
// the CLI does: `Container` / `MicroVM` (workloads) and `Volume`.
func applyManifest(addr, path string) error {
	raw, err := readManifest(path)
	if err != nil {
		return err
	}
	kind, err := manifestKind(raw)
	if err != nil {
		return err
	}
	switch kind {
	case "Volume":
		return volumeApplyRaw(addr, raw)
	case "Container", "MicroVM", "WORKLOAD_KIND_CONTAINER", "WORKLOAD_KIND_MICRO_VM":
		return workloadApplyRaw(addr, raw)
	default:
		return fmt.Errorf("unsupported kind %q (expected Container, MicroVM, or Volume)", kind)
	}
}

// manifestKind extracts the `kind:` value from a YAML or JSON manifest
// without parsing the rest of the document — we only need enough to
// route. Tolerates whitespace/quotes around the value.
func manifestKind(raw []byte) (string, error) {
	j, err := sigsyaml.YAMLToJSON(raw)
	if err != nil {
		return "", fmt.Errorf("parse manifest: %w", err)
	}
	var stub struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(j, &stub); err != nil {
		return "", fmt.Errorf("parse kind: %w", err)
	}
	if stub.Kind == "" {
		return "", errors.New("manifest missing required `kind:` field")
	}
	return stub.Kind, nil
}

// volumeApplyRaw is idempotent: creates if missing, resizes if size grew,
// no-op if already matching. Mirrors workloadApply semantics. Volume YAML
// schema:
//
//	kind: Volume
//	name: shared
//	size: 2GiB    # optional; bare int = MiB
func volumeApplyRaw(addr string, raw []byte) error {
	var m struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Size string `json:"size"`
	}
	j, err := sigsyaml.YAMLToJSON(raw)
	if err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	if err := json.Unmarshal(j, &m); err != nil {
		return fmt.Errorf("parse volume: %w", err)
	}
	if m.Name == "" {
		return errors.New("volume.name is required")
	}
	var sizeMiB uint64
	if m.Size != "" {
		sizeMiB, err = parseSize(m.Size)
		if err != nil {
			return fmt.Errorf("size: %w", err)
		}
	}

	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewVolumeServiceClient(conn)

	return withCtx(func(ctx context.Context) error {
		existing, err := client.Get(ctx, &capsulev1.VolumeGetRequest{Name: m.Name})
		if err == nil && existing != nil {
			currentMiB := existing.GetSizeBytes() / (1024 * 1024)
			switch {
			case sizeMiB == 0 || sizeMiB == currentMiB:
				fmt.Printf("volume %q unchanged (%d MiB)\n", m.Name, currentMiB)
				return nil
			case sizeMiB > currentMiB:
				if _, err := client.Resize(ctx, &capsulev1.VolumeResizeRequest{Name: m.Name, NewSizeMib: sizeMiB}); err != nil {
					return fmt.Errorf("resize: %w", err)
				}
				fmt.Printf("volume %q resized %d MiB -> %d MiB\n", m.Name, currentMiB, sizeMiB)
				return nil
			default:
				return fmt.Errorf("volume %q is %d MiB; resize is grow-only (requested %d MiB)", m.Name, currentMiB, sizeMiB)
			}
		}
		// Get failed — assume not-found and create. (gRPC doesn't expose a
		// stable NotFound discriminator at the client without status, but
		// any other error from Create will surface a clear message.)
		if _, err := client.Create(ctx, &capsulev1.VolumeCreateRequest{Name: m.Name, SizeMib: sizeMiB}); err != nil {
			return fmt.Errorf("create: %w", err)
		}
		fmt.Printf("volume %q created (%d MiB)\n", m.Name, sizeMiB)
		return nil
	})
}

// workloadApplyRaw is the byte-input variant of workloadApply, used by
// applyManifest after it has already read the file.
func workloadApplyRaw(addr string, raw []byte) error {
	w, err := parseWorkload(raw)
	if err != nil {
		return err
	}
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		resp, err := client.Apply(ctx, &capsulev1.WorkloadApplyRequest{Workload: w})
		if err != nil {
			return err
		}
		fmt.Printf("applied workload %q (kind=%s)\n", resp.GetWorkload().GetName(), resp.GetWorkload().GetKind())
		return nil
	})
}

func readManifest(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// parseWorkload accepts either JSON or YAML. It converts YAML to JSON, then
// unmarshals via protojson so the result matches the server's shape exactly.
// Also accepts friendly kind aliases ("Container"/"MicroVM") in addition to
// the canonical enum values.
func parseWorkload(raw []byte) (*capsulev1.Workload, error) {
	// Normalize kind aliases before handing to protojson.
	s := string(raw)
	s = strings.ReplaceAll(s, "kind: Container", "kind: WORKLOAD_KIND_CONTAINER")
	s = strings.ReplaceAll(s, "kind: MicroVM", "kind: WORKLOAD_KIND_MICRO_VM")
	raw = []byte(s)

	j, err := sigsyaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	w := &capsulev1.Workload{}
	if err := protojson.Unmarshal(j, w); err != nil {
		return nil, fmt.Errorf("parse workload: %w", err)
	}
	return w, nil
}

// --- workload list ---------------------------------------------------------

func workloadList(addr string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	return withCtx(func(ctx context.Context) error {
		resp, err := client.List(ctx, &capsulev1.WorkloadListRequest{})
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKIND\tIMAGE\tDESIRED\tPHASE\tMESSAGE")
		for _, w := range resp.GetWorkloads() {
			image := w.GetContainer().GetImage()
			if image == "" {
				image = w.GetMicroVm().GetImage()
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				w.GetName(),
				kindShort(w.GetKind()),
				image,
				desiredShort(w.GetDesiredState()),
				phaseShort(w.GetStatus().GetPhase()),
				w.GetStatus().GetMessage(),
			)
		}
		return tw.Flush()
	})
}

func desiredShort(d capsulev1.DesiredState) string {
	switch d {
	case capsulev1.DesiredState_DESIRED_STATE_STOPPED:
		return "Stopped"
	default:
		return "Running"
	}
}

// --- workload get ----------------------------------------------------------

func workloadGet(addr, name string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	return withCtx(func(ctx context.Context) error {
		w, err := client.Get(ctx, &capsulev1.WorkloadGetRequest{Name: name})
		if err != nil {
			return err
		}
		j, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(w)
		if err != nil {
			return err
		}
		fmt.Println(string(j))
		return nil
	})
}

// --- workload delete -------------------------------------------------------

func workloadDelete(addr, name string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	return withCtx(func(ctx context.Context) error {
		if _, err := client.Delete(ctx, &capsulev1.WorkloadDeleteRequest{Name: name}); err != nil {
			return err
		}
		fmt.Printf("deleted workload %q\n", name)
		return nil
	})
}

// workloadLifecycle dispatches Restart/Stop/Start RPCs by verb.
func workloadLifecycle(addr, name, verb string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		switch verb {
		case "restart":
			if _, err := client.Restart(ctx, &capsulev1.WorkloadRestartRequest{Name: name}); err != nil {
				return err
			}
		case "stop":
			if _, err := client.Stop(ctx, &capsulev1.WorkloadStopRequest{Name: name}); err != nil {
				return err
			}
		case "start":
			if _, err := client.Start(ctx, &capsulev1.WorkloadStartRequest{Name: name}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown verb %q", verb)
		}
		past := map[string]string{"restart": "restarted", "stop": "stopped", "start": "started"}[verb]
		fmt.Printf("%s workload %q\n", past, name)
		return nil
	})
}

// --- workload logs ---------------------------------------------------------

func workloadLogs(addr, name string, follow bool, tail int, serial bool) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ctrl-C stops the stream without exiting non-zero.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		cancel()
	}()

	source := capsulev1.LogSource_LOG_SOURCE_PAYLOAD
	if serial {
		source = capsulev1.LogSource_LOG_SOURCE_SERIAL
	}
	stream, err := client.Logs(ctx, &capsulev1.WorkloadLogsRequest{
		Name:      name,
		Follow:    follow,
		TailLines: int32(tail),
		Source:    source,
	})
	if err != nil {
		return err
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		_, _ = os.Stdout.Write(chunk.GetData())
	}
}

// --- workload exec ---------------------------------------------------------

// workloadExec is the convenience wrapper for the `workload exec`
// subcommand: runs the exec, then os.Exit's with the remote exit code so
// shell pipelines see the right status. Calls workloadExecCore under the
// hood. Uses os.Exit because the stdin goroutine (in raw-tty mode) is
// blocked on Read and would delay normal return — but that means defers
// in callers DO NOT run. If you need cleanup before exit (e.g. capsule
// debug), call workloadExecCore directly and handle the exit yourself.
func workloadExec(addr, name string, command []string, tty bool) error {
	exitCode, err := workloadExecCore(addr, name, command, tty)
	if err != nil {
		return err
	}
	os.Exit(exitCode)
	return nil
}

// workloadExecCore does the actual exec stream work and returns the
// remote exit code + any transport error. Caller is responsible for
// propagating the exit code (via os.Exit) AFTER any cleanup it needs.
func workloadExecCore(addr, name string, command []string, tty bool) (int, error) {
	conn, err := dial(addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.Exec(ctx)
	if err != nil {
		return 0, err
	}

	// First message: config.
	cfg := &capsulev1.WorkloadExecConfig{
		Name:    name,
		Command: command,
		Tty:     tty,
	}
	if err := stream.Send(&capsulev1.WorkloadExecClientMessage{
		Payload: &capsulev1.WorkloadExecClientMessage_Config{Config: cfg},
	}); err != nil {
		return 0, err
	}

	// If we asked for a TTY and stdin is a terminal, put it in raw mode.
	var restore func()
	if tty && term.IsTerminal(int(os.Stdin.Fd())) {
		prev, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err == nil {
			restore = func() { _ = term.Restore(int(os.Stdin.Fd()), prev) }
		}
		// Send initial resize.
		if cols, rows, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			_ = stream.Send(&capsulev1.WorkloadExecClientMessage{
				Payload: &capsulev1.WorkloadExecClientMessage_Resize{
					Resize: &capsulev1.WorkloadExecResize{Cols: uint32(cols), Rows: uint32(rows)},
				},
			})
		}
		// SIGWINCH → resize message.
		winchCh := make(chan os.Signal, 1)
		signal.Notify(winchCh, syscall.SIGWINCH)
		go func() {
			for range winchCh {
				cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil {
					continue
				}
				_ = stream.Send(&capsulev1.WorkloadExecClientMessage{
					Payload: &capsulev1.WorkloadExecClientMessage_Resize{
						Resize: &capsulev1.WorkloadExecResize{Cols: uint32(cols), Rows: uint32(rows)},
					},
				})
			}
		}()
	}
	if restore != nil {
		defer restore()
	}

	// Goroutine: stdin → stream.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&capsulev1.WorkloadExecClientMessage{
					Payload: &capsulev1.WorkloadExecClientMessage_Stdin{Stdin: append([]byte(nil), buf[:n]...)},
				}); sendErr != nil {
					return
				}
			}
			if err != nil {
				_ = stream.CloseSend()
				return
			}
		}
	}()

	// Main: server stream → stdout/stderr; bail immediately on Exit.
	exitCode := 0
Loop:
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		switch p := msg.Payload.(type) {
		case *capsulev1.WorkloadExecServerMessage_Stdout:
			_, _ = os.Stdout.Write(p.Stdout)
		case *capsulev1.WorkloadExecServerMessage_Stderr:
			_, _ = os.Stderr.Write(p.Stderr)
		case *capsulev1.WorkloadExecServerMessage_Exit:
			exitCode = int(p.Exit.GetExitCode())
			_ = stream.CloseSend()
			break Loop
		}
	}
	if restore != nil {
		restore()
	}
	return exitCode, nil
}

// --- volume -----------------------------------------------------------------

func volumeCreate(addr, name string, sizeMiB uint64) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewVolumeServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		v, err := client.Create(ctx, &capsulev1.VolumeCreateRequest{Name: name, SizeMib: sizeMiB})
		if err != nil {
			return err
		}
		fmt.Printf("created volume %q at %s\n", v.GetName(), v.GetHostPath())
		return nil
	})
}

func volumeResize(addr, name string, newSizeMiB uint64) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewVolumeServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		v, err := client.Resize(ctx, &capsulev1.VolumeResizeRequest{Name: name, NewSizeMib: newSizeMiB})
		if err != nil {
			return err
		}
		fmt.Printf("resized volume %q to %s\n", v.GetName(), humanBytes(v.GetSizeBytes()))
		return nil
	})
}

// parseSize accepts size specs like "10GiB", "512MiB", "1TiB". A bare
// integer is interpreted as MiB. Returns the size in MiB. Binary units
// only (1 KiB = 1024 B); decimal suffixes would be ambiguous for storage.
func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	// Split trailing unit (letters) from numeric prefix.
	i := len(s)
	for i > 0 && !(s[i-1] >= '0' && s[i-1] <= '9') {
		i--
	}
	num, unit := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	n, err := strconv.ParseUint(strings.TrimSpace(num), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	switch unit {
	case "", "m", "mi", "mib":
		return n, nil
	case "k", "ki", "kib":
		if n%1024 != 0 {
			return 0, fmt.Errorf("%s not a whole MiB", s)
		}
		return n / 1024, nil
	case "g", "gi", "gib":
		return n * 1024, nil
	case "t", "ti", "tib":
		return n * 1024 * 1024, nil
	}
	return 0, fmt.Errorf("unknown size unit %q (use KiB/MiB/GiB/TiB)", unit)
}

func volumeList(addr string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewVolumeServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		resp, err := client.List(ctx, &capsulev1.VolumeListRequest{})
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSIZE\tHOST_PATH\tMOUNTED_BY")
		for _, v := range resp.GetVolumes() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				v.GetName(),
				humanBytes(v.GetSizeBytes()),
				v.GetHostPath(),
				strings.Join(v.GetMountedBy(), ","),
			)
		}
		return tw.Flush()
	})
}

func volumeGet(addr, name string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewVolumeServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		v, err := client.Get(ctx, &capsulev1.VolumeGetRequest{Name: name})
		if err != nil {
			return err
		}
		j, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(v)
		if err != nil {
			return err
		}
		fmt.Println(string(j))
		return nil
	})
}

func volumeDelete(addr, name string, force bool) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewVolumeServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		if _, err := client.Delete(ctx, &capsulev1.VolumeDeleteRequest{Name: name, Force: force}); err != nil {
			return err
		}
		fmt.Printf("deleted volume %q\n", name)
		return nil
	})
}

// --- image -----------------------------------------------------------------

func imageList(addr string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewImageServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		resp, err := client.List(ctx, &capsulev1.ImageListRequest{})
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tDIGEST\tSIZE\tUPDATED")
		for _, img := range resp.GetImages() {
			digest := img.GetDigest()
			// Trim "sha256:" + middle of the hash so the column stays
			// readable in a terminal — keeps the first 12 hex chars,
			// same shape as `docker images`.
			if len(digest) > 19 && strings.HasPrefix(digest, "sha256:") {
				digest = digest[:19]
			}
			updated := "-"
			if img.GetCreatedUnix() > 0 {
				updated = time.Unix(img.GetCreatedUnix(), 0).Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				img.GetName(),
				digest,
				humanBytes(uint64(img.GetSizeBytes())),
				updated,
			)
		}
		return tw.Flush()
	})
}

// imagePush streams an OCI / docker-save tarball to the capsule's
// containerd image store. path == "-" means stdin (so you can pipe
// `docker save myimage:tag | capsulectl image push -`); otherwise we
// open the file and stat it for a totalBytes hint.
func imagePush(addr, path string) error {
	var src io.Reader
	var totalBytes uint64
	if path == "-" {
		src = os.Stdin
	} else {
		st, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat tarball: %w", err)
		}
		totalBytes = uint64(st.Size())
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open tarball: %w", err)
		}
		defer f.Close()
		src = f
	}

	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewImageServiceClient(conn)

	// Long timeout: importing a multi-GiB image over a slow link is
	// legitimate. UpdateOS uses 30 minutes for the same reason.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	stream, err := client.Push(ctx)
	if err != nil {
		return fmt.Errorf("Push: %w", err)
	}
	if err := stream.Send(&capsulev1.ImagePushRequest{
		Msg: &capsulev1.ImagePushRequest_Metadata{
			Metadata: &capsulev1.ImagePushMetadata{TotalBytes: totalBytes},
		},
	}); err != nil {
		return fmt.Errorf("send metadata: %w", err)
	}

	buf := make([]byte, 1024*1024) // 1 MiB chunks — same as UpdateOS
	var sent uint64
	lastTick := time.Now()
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if err := stream.Send(&capsulev1.ImagePushRequest{
				Msg: &capsulev1.ImagePushRequest_Chunk{Chunk: append([]byte(nil), buf[:n]...)},
			}); err != nil {
				return fmt.Errorf("send chunk: %w", err)
			}
			sent += uint64(n)
			if time.Since(lastTick) > 500*time.Millisecond {
				if totalBytes > 0 {
					fmt.Fprintf(os.Stderr, "\r  pushing... %d / %d MiB",
						sent/(1024*1024), totalBytes/(1024*1024))
				} else {
					fmt.Fprintf(os.Stderr, "\r  pushing... %d MiB", sent/(1024*1024))
				}
				lastTick = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read tarball: %w", rerr)
		}
	}
	if sent > 0 {
		fmt.Fprintln(os.Stderr)
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("CloseAndRecv: %w", err)
	}
	if len(resp.GetImageRefs()) == 0 {
		fmt.Printf("imported (no manifests in archive — bytes: %d)\n", resp.GetBytesReceived())
		return nil
	}
	fmt.Printf("imported %d ref(s) (%d bytes):\n", len(resp.GetImageRefs()), resp.GetBytesReceived())
	for _, ref := range resp.GetImageRefs() {
		fmt.Printf("  %s\n", ref)
	}
	return nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// splitAtDashDash splits args around the first "--" token. The "--" itself
// is dropped. When no "--" is present, everything goes to the left side.
func splitAtDashDash(args []string) (pre, post []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// frontLoadFlags reorders args so every leading-dash token comes before
// any positional, preserving within-group order. Use AFTER joinValueFlags
// so multi-token "--flag value" pairs are already a single "--flag=value"
// token — otherwise this would separate them.
func frontLoadFlags(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
		} else {
			positionals = append(positionals, a)
		}
	}
	return append(flags, positionals...)
}

// joinValueFlags rewrites sequences like "--foo VAL" into "--foo=VAL" for
// the named value-taking flags, so flag.Parse can recognize them even
// when they appear after positional tokens (Go's flag.Parse stops at the
// first non-flag without this).
func joinValueFlags(args []string, valueFlags map[string]bool) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") && !strings.Contains(a, "=") {
			name := a[2:]
			if valueFlags[name] && i+1 < len(args) {
				out = append(out, a+"="+args[i+1])
				i++
				continue
			}
		} else if strings.HasPrefix(a, "-") && len(a) > 1 && !strings.HasPrefix(a, "--") && !strings.Contains(a, "=") {
			name := a[1:]
			if valueFlags[name] && i+1 < len(args) {
				out = append(out, a+"="+args[i+1])
				i++
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// partitionFlags separates leading-dash tokens from positional tokens while
// preserving order within each group. Used so flags can appear either
// before or after positional arguments without Go's flag parser choking.
// Only single/double-dash tokens are treated as flags; lone "-" is kept as
// a positional.
func partitionFlags(args []string) (flags, positionals []string) {
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
		} else {
			positionals = append(positionals, a)
		}
	}
	return flags, positionals
}

// --- formatting helpers ----------------------------------------------------

func kindShort(k capsulev1.WorkloadKind) string {
	switch k {
	case capsulev1.WorkloadKind_WORKLOAD_KIND_CONTAINER:
		return "Container"
	case capsulev1.WorkloadKind_WORKLOAD_KIND_MICRO_VM:
		return "MicroVM"
	default:
		return "?"
	}
}

func phaseShort(p capsulev1.WorkloadPhase) string {
	switch p {
	case capsulev1.WorkloadPhase_WORKLOAD_PHASE_PENDING:
		return "Pending"
	case capsulev1.WorkloadPhase_WORKLOAD_PHASE_RUNNING:
		return "Running"
	case capsulev1.WorkloadPhase_WORKLOAD_PHASE_STOPPED:
		return "Stopped"
	case capsulev1.WorkloadPhase_WORKLOAD_PHASE_FAILED:
		return "Failed"
	default:
		return "-"
	}
}
