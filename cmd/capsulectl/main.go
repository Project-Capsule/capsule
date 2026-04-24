package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	sigsyaml "sigs.k8s.io/yaml"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

func main() {
	// Global flags first: --capsule host:port. Then subcommand and its args.
	global := flag.NewFlagSet("capsulectl", flag.ExitOnError)
	addr := global.String("capsule", "localhost:50000", "capsule gRPC address")
	if err := global.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	rest := global.Args()
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
	case "workload apply":
		fs := flag.NewFlagSet("workload apply", flag.ExitOnError)
		file := fs.String("f", "", "workload manifest file (- for stdin)")
		_ = fs.Parse(subArgs)
		if *file == "" {
			err = errors.New("workload apply requires -f <file>")
			break
		}
		err = workloadApply(*addr, *file)
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
		if len(subArgs) < 1 {
			err = errors.New("volume create requires a name")
			break
		}
		err = volumeCreate(*addr, subArgs[0])
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

Usage:
  capsulectl [--capsule host:port] capsule info
  capsulectl [--capsule host:port] workload apply -f <manifest.yaml>
  capsulectl [--capsule host:port] workload list
  capsulectl [--capsule host:port] workload get <name>
  capsulectl [--capsule host:port] workload delete <name>
  capsulectl [--capsule host:port] workload restart <name>
  capsulectl [--capsule host:port] workload stop <name>
  capsulectl [--capsule host:port] workload start <name>
  capsulectl [--capsule host:port] workload logs [-f] [-n N] [--serial] <name>
  capsulectl [--capsule host:port] workload exec [-t] <name> -- <cmd> [args...]
  capsulectl [--capsule host:port] volume create <name>
  capsulectl [--capsule host:port] volume list
  capsulectl [--capsule host:port] volume get <name>
  capsulectl [--capsule host:port] volume delete [--force] <name>
`)
}

func dial(addr string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func withCtx(fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return fn(ctx)
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
		fmt.Printf("  uptime_seconds:  %d\n", resp.UptimeSeconds)
		fmt.Printf("  capsule_version: %s\n", resp.CapsuleVersion)
		fmt.Printf("  active_slot:     %s\n", resp.ActiveSlot)
		return nil
	})
}

// --- workload apply --------------------------------------------------------

func workloadApply(addr, path string) error {
	raw, err := readManifest(path)
	if err != nil {
		return err
	}
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

func workloadExec(addr, name string, command []string, tty bool) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewWorkloadServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.Exec(ctx)
	if err != nil {
		return err
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
		return err
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
			return err
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
	// Hard-exit so the stdin goroutine (blocked on os.Stdin.Read in raw
	// mode) doesn't delay process teardown. With exitCode=0 we still
	// exit 0, matching "return nil" semantics.
	os.Exit(exitCode)
	return nil
}

// --- volume -----------------------------------------------------------------

func volumeCreate(addr, name string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := capsulev1.NewVolumeServiceClient(conn)
	return withCtx(func(ctx context.Context) error {
		v, err := client.Create(ctx, &capsulev1.VolumeCreateRequest{Name: name})
		if err != nil {
			return err
		}
		fmt.Printf("created volume %q at %s\n", v.GetName(), v.GetHostPath())
		return nil
	})
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
