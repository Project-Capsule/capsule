//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/runtime"
)

// Logs streams the named VM payload's combined stdout+stderr via the
// guest agent over vsock. Returns a ReadCloser the caller drains and
// closes; closing stops the server-side stream.
func (d *Driver) Logs(ctx context.Context, name string, follow bool, tailLines int) (io.ReadCloser, error) {
	d.mu.Lock()
	s, ok := d.vms[name]
	d.mu.Unlock()
	if !ok || s.guestConn == nil {
		return nil, fmt.Errorf("no running vm %q", name)
	}

	agent := capsulev1.NewGuestAgentClient(s.guestConn)
	stream, err := agent.Logs(ctx, &capsulev1.LogsRequest{
		Follow:    follow,
		TailLines: int32(tailLines),
	})
	if err != nil {
		return nil, err
	}
	return &guestLogsReader{stream: stream}, nil
}

type guestLogsReader struct {
	stream  capsulev1.GuestAgent_LogsClient
	pending []byte
}

func (r *guestLogsReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		chunk, err := r.stream.Recv()
		if err != nil {
			return 0, err
		}
		r.pending = chunk.GetData()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *guestLogsReader) Close() error {
	return r.stream.CloseSend()
}

// SerialLogs streams the VM's serial console log file. The file is
// populated by Firecracker (stdout+stderr) with tee'd kernel boot
// messages, capsule-guest output, and Firecracker's own logs. This is the
// diagnostic path for VMs that fail to boot or that can't be reached
// via the guest agent.
func (d *Driver) SerialLogs(ctx context.Context, name string, follow bool) (io.ReadCloser, error) {
	path := filepath.Join(stateDir, name, "vm-serial.log")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !follow {
		return f, nil
	}
	return &tailingReader{f: f, ctx: ctx}, nil
}

// tailingReader wraps an *os.File so reads block for new data instead
// of returning io.EOF when the writer is still appending.
type tailingReader struct {
	f   *os.File
	ctx context.Context
}

func (t *tailingReader) Read(p []byte) (int, error) {
	for {
		n, err := t.f.Read(p)
		if n > 0 || err != io.EOF {
			return n, err
		}
		// EOF: wait briefly and try again, or return EOF if ctx cancelled.
		select {
		case <-t.ctx.Done():
			return 0, io.EOF
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (t *tailingReader) Close() error { return t.f.Close() }

// Exec forwards a one-shot command to the guest agent. Mirrors
// container driver Exec semantics: stdin/stdout/stderr pumped through
// the provided request streams, int return is the exit code.
func (d *Driver) Exec(ctx context.Context, req runtime.ExecRequest) (int, error) {
	d.mu.Lock()
	s, ok := d.vms[req.Name]
	d.mu.Unlock()
	if !ok || s.guestConn == nil {
		return -1, fmt.Errorf("no running vm %q", req.Name)
	}

	agent := capsulev1.NewGuestAgentClient(s.guestConn)
	stream, err := agent.Exec(ctx)
	if err != nil {
		return -1, err
	}

	// First message: config.
	if err := stream.Send(&capsulev1.ExecClientMessage{
		Payload: &capsulev1.ExecClientMessage_Config{
			Config: &capsulev1.ExecConfig{
				Command: req.Command,
				Tty:     req.TTY,
				Env:     req.Env,
			},
		},
	}); err != nil {
		return -1, err
	}

	// stdin pump: req.Stdin → stream.
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		if req.Stdin == nil {
			_ = stream.CloseSend()
			return
		}
		buf := make([]byte, 4096)
		for {
			n, err := req.Stdin.Read(buf)
			if n > 0 {
				if serr := stream.Send(&capsulev1.ExecClientMessage{
					Payload: &capsulev1.ExecClientMessage_Stdin{Stdin: append([]byte(nil), buf[:n]...)},
				}); serr != nil {
					return
				}
			}
			if err != nil {
				_ = stream.CloseSend()
				return
			}
		}
	}()

	// Drain server messages until Exit.
	exit := int32(-1)
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return int(exit), err
		}
		switch p := msg.Payload.(type) {
		case *capsulev1.ExecServerMessage_Stdout:
			if req.Stdout != nil {
				_, _ = req.Stdout.Write(p.Stdout)
			}
		case *capsulev1.ExecServerMessage_Stderr:
			if req.Stderr != nil {
				_, _ = req.Stderr.Write(p.Stderr)
			}
		case *capsulev1.ExecServerMessage_Exit:
			exit = p.Exit.GetExitCode()
		}
	}
	<-stdinDone
	return int(exit), nil
}
