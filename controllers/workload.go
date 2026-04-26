package controllers

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/geekgonecrazy/capsule/core/workload"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
	"github.com/geekgonecrazy/capsule/store"
)

// WorkloadController implements capsule.v1.WorkloadServiceServer
// by delegating to core/workload.Service. It owns only the gRPC
// translation — validation and storage policy live in core.
type WorkloadController struct {
	capsulev1.UnimplementedWorkloadServiceServer
	Service *workload.Service
}

func (c *WorkloadController) Apply(ctx context.Context, req *capsulev1.WorkloadApplyRequest) (*capsulev1.WorkloadApplyResponse, error) {
	if req.GetWorkload() == nil {
		return nil, status.Error(codes.InvalidArgument, "workload is required")
	}
	w, err := c.Service.Apply(ctx, req.GetWorkload())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &capsulev1.WorkloadApplyResponse{Workload: w}, nil
}

func (c *WorkloadController) Get(ctx context.Context, req *capsulev1.WorkloadGetRequest) (*capsulev1.Workload, error) {
	w, err := c.Service.Get(ctx, req.GetName())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "workload %q not found", req.GetName())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return w, nil
}

func (c *WorkloadController) List(ctx context.Context, _ *capsulev1.WorkloadListRequest) (*capsulev1.WorkloadListResponse, error) {
	ws, err := c.Service.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.WorkloadListResponse{Workloads: ws}, nil
}

func (c *WorkloadController) Delete(ctx context.Context, req *capsulev1.WorkloadDeleteRequest) (*capsulev1.WorkloadDeleteResponse, error) {
	if err := c.Service.Delete(ctx, req.GetName()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.WorkloadDeleteResponse{}, nil
}

func (c *WorkloadController) Restart(ctx context.Context, req *capsulev1.WorkloadRestartRequest) (*capsulev1.WorkloadRestartResponse, error) {
	if err := c.Service.Restart(ctx, req.GetName()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "workload %q not found", req.GetName())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.WorkloadRestartResponse{}, nil
}

func (c *WorkloadController) Stop(ctx context.Context, req *capsulev1.WorkloadStopRequest) (*capsulev1.WorkloadStopResponse, error) {
	if err := c.Service.Stop(ctx, req.GetName()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "workload %q not found", req.GetName())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.WorkloadStopResponse{}, nil
}

func (c *WorkloadController) Start(ctx context.Context, req *capsulev1.WorkloadStartRequest) (*capsulev1.WorkloadStartResponse, error) {
	if err := c.Service.Start(ctx, req.GetName()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "workload %q not found", req.GetName())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.WorkloadStartResponse{}, nil
}

// --- Logs ------------------------------------------------------------------

// Logs streams a workload's combined stdout+stderr to the client.
// Container workloads read from the on-disk log file (with tail_lines
// seek support). MicroVM workloads stream via the guest agent over
// vsock; tail_lines is forwarded but has no effect today (TODO in
// capsule-guest). follow tails forever until the client cancels.
func (c *WorkloadController) Logs(req *capsulev1.WorkloadLogsRequest, stream capsulev1.WorkloadService_LogsServer) error {
	name := req.GetName()
	if name == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}

	ctx := stream.Context()

	source := workload.LogSourcePayload
	if req.GetSource() == capsulev1.LogSource_LOG_SOURCE_SERIAL {
		source = workload.LogSourceSerial
	}

	// shouldStop ends the follow loop once the workload is no longer
	// running — without it, `logs -f` hangs forever after a workload
	// exits/stops/is deleted (the file just stops growing; EOF + sleep
	// + retry forever). Throttled so we don't query the store on every
	// idle tick. Nil for non-follow reads since the loop returns at EOF
	// anyway.
	var shouldStop func() bool
	if req.GetFollow() {
		shouldStop = makeLogsShouldStop(c.Service, name)
	}

	// For containers (payload logs only), we apply tail_lines by seeking
	// in the file before streaming. That needs *os.File semantics, so
	// keep the file path fast-path for that case; fall back to OpenLogs
	// for VMs and for serial logs.
	if source == workload.LogSourcePayload && req.GetTailLines() > 0 {
		if path := c.Service.LogPath(name); path != "" {
			f, err := os.Open(path)
			if err == nil {
				defer f.Close()
				if err := seekTail(f, int(req.GetTailLines())); err != nil {
					return status.Error(codes.Internal, err.Error())
				}
				return streamReaderToLogs(ctx, f, req.GetFollow(), shouldStop, stream)
			}
		}
	}

	rc, err := c.Service.OpenLogs(ctx, name, req.GetFollow(), int(req.GetTailLines()), source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return status.Errorf(codes.NotFound, "no logs yet for %q", name)
		}
		return status.Error(codes.Internal, err.Error())
	}
	defer rc.Close()
	return streamReaderToLogs(ctx, rc, req.GetFollow(), shouldStop, stream)
}

// makeLogsShouldStop returns a closure that reports whether a follow-mode
// log stream should give up. Returns true once the named workload's
// observed phase is no longer RUNNING or PENDING (so STOPPED, FAILED,
// DELETING, or "row vanished" all end the stream cleanly). Internally
// throttled to one store lookup per check interval — the server can be
// woken on every 250 ms EOF tick and we don't want to thrash SQLite.
func makeLogsShouldStop(svc *workload.Service, name string) func() bool {
	const interval = 2 * time.Second
	var lastCheck time.Time
	var lastResult bool
	return func() bool {
		if time.Since(lastCheck) < interval {
			return lastResult
		}
		lastCheck = time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		w, err := svc.Get(ctx, name)
		if err != nil {
			// Workload was deleted (or store hiccup). Treat both as "stop"
			// — better to bail than to spin against a missing target.
			lastResult = true
			return true
		}
		switch w.GetStatus().GetPhase() {
		case capsulev1.WorkloadPhase_WORKLOAD_PHASE_RUNNING,
			capsulev1.WorkloadPhase_WORKLOAD_PHASE_PENDING:
			lastResult = false
		default:
			lastResult = true
		}
		return lastResult
	}
}

// streamReaderToLogs pumps r's bytes into the gRPC Logs stream. If
// follow is true and r returns EOF, checks shouldStop (if provided) to
// see if the source has gone permanently quiet — workload stopped,
// failed, or deleted — and exits cleanly if so; otherwise sleeps
// briefly and retries so we tail file-backed sources. Without
// shouldStop, follow loops until the client cancels.
func streamReaderToLogs(ctx context.Context, r io.Reader, follow bool, shouldStop func() bool, stream capsulev1.WorkloadService_LogsServer) error {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&capsulev1.WorkloadLogChunk{Data: append([]byte(nil), buf[:n]...)}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			if !follow {
				return nil
			}
			if shouldStop != nil && shouldStop() {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(250 * time.Millisecond):
				continue
			}
		}
		if rerr != nil {
			return status.Error(codes.Internal, rerr.Error())
		}
	}
}

// seekTail repositions f so that the next Read starts at the Nth-from-last
// newline. If the file is shorter, rewinds to the start.
func seekTail(f *os.File, lines int) error {
	const block = 4096
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()
	if size == 0 || lines <= 0 {
		return nil
	}
	want := lines + 1 // include partial last line
	off := size
	buf := make([]byte, block)
	found := 0
	for off > 0 {
		chunk := int64(block)
		if off < chunk {
			chunk = off
		}
		off -= chunk
		if _, err := f.ReadAt(buf[:chunk], off); err != nil {
			return err
		}
		for i := int(chunk) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				found++
				if found == want {
					_, err := f.Seek(off+int64(i)+1, io.SeekStart)
					return err
				}
			}
		}
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

// --- Exec ------------------------------------------------------------------

// Exec implements the bidi-streaming exec RPC. The first client message
// must carry an ExecConfig; subsequent messages are stdin bytes or PTY
// resize events. Server streams stdout/stderr and finally an Exit message.
func (c *WorkloadController) Exec(stream capsulev1.WorkloadService_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	cfg := first.GetConfig()
	if cfg == nil {
		return status.Error(codes.InvalidArgument, "first Exec message must carry config")
	}
	if cfg.GetName() == "" {
		return status.Error(codes.InvalidArgument, "config.name is required")
	}
	if len(cfg.GetCommand()) == 0 {
		return status.Error(codes.InvalidArgument, "config.command is required")
	}

	// Wire bidi streams.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	var stderrForReq io.Writer = stderrW
	if cfg.GetTty() {
		// PTY mode collapses stdout+stderr into stdout; stderr side is
		// intentionally unused in that mode.
		stderrForReq = nil
		_ = stderrR.Close()
		_ = stderrW.Close()
	}

	resizeCh := make(chan runtime.TermSize, 8)

	// Goroutine: pump stdout/stderr pipes → stream.
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		pumpReaderToStream(stdoutR, func(b []byte) error {
			return stream.Send(&capsulev1.WorkloadExecServerMessage{
				Payload: &capsulev1.WorkloadExecServerMessage_Stdout{Stdout: b},
			})
		})
	}()
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		if cfg.GetTty() {
			return
		}
		pumpReaderToStream(stderrR, func(b []byte) error {
			return stream.Send(&capsulev1.WorkloadExecServerMessage{
				Payload: &capsulev1.WorkloadExecServerMessage_Stderr{Stderr: b},
			})
		})
	}()

	// Goroutine: receive stream messages → stdin pipe / resize.
	recvDone := make(chan error, 1)
	go func() {
		defer stdinW.Close()
		defer close(resizeCh)
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				recvDone <- nil
				return
			}
			if err != nil {
				recvDone <- err
				return
			}
			switch p := msg.Payload.(type) {
			case *capsulev1.WorkloadExecClientMessage_Stdin:
				if _, werr := stdinW.Write(p.Stdin); werr != nil {
					recvDone <- werr
					return
				}
			case *capsulev1.WorkloadExecClientMessage_Resize:
				select {
				case resizeCh <- runtime.TermSize{Cols: p.Resize.GetCols(), Rows: p.Resize.GetRows()}:
				default:
				}
			case *capsulev1.WorkloadExecClientMessage_Config:
				// Ignore duplicate configs.
			}
		}
	}()

	// Build env map.
	envMap := cfg.GetEnv()

	exitCode, execErr := c.Service.Exec(stream.Context(), runtime.ExecRequest{
		Name:     cfg.GetName(),
		Command:  cfg.GetCommand(),
		Env:      envMap,
		TTY:      cfg.GetTty(),
		Stdin:    stdinR,
		Stdout:   stdoutW,
		Stderr:   stderrForReq,
		ResizeCh: resizeCh,
	})

	// Close writers so pumpers finish.
	stdoutW.Close()
	if stderrW != nil {
		stderrW.Close()
	}
	<-sendDone
	<-stderrDone

	if execErr != nil {
		return status.Error(codes.Internal, execErr.Error())
	}
	return stream.Send(&capsulev1.WorkloadExecServerMessage{
		Payload: &capsulev1.WorkloadExecServerMessage_Exit{
			Exit: &capsulev1.WorkloadExecExit{ExitCode: int32(exitCode)},
		},
	})
}

// pumpReaderToStream reads r in chunks and forwards each chunk via send.
// Stops on EOF or send error.
func pumpReaderToStream(r io.Reader, send func([]byte) error) {
	buf := make([]byte, 16*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := send(append([]byte(nil), buf[:n]...)); serr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
