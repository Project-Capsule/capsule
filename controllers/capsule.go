package controllers

import (
	"context"
	"io"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// CapsuleController implements capsule.v1.CapsuleServiceServer.
// For phase 0 it reads live info directly from the kernel; later phases
// will move the data-source bits into core/ and accept them via fields.
type CapsuleController struct {
	capsulev1.UnimplementedCapsuleServiceServer

	// CapsuleVersion is the build-time version string baked into the binary.
	CapsuleVersion string
	// ActiveSlot is the currently active A/B slot identifier. Empty until
	// A/B updates ship (phase 3).
	ActiveSlot string
	// LogPath is the file capsuled tees its slog output to. StreamLogs
	// opens and tails this.
	LogPath string
}

// StreamLogs tails CapsuleLogPath for the client. Honors follow + tail_lines.
// Stops when the client cancels the stream (Ctrl-C in capsulectl).
func (c *CapsuleController) StreamLogs(req *capsulev1.CapsuleLogsRequest, stream capsulev1.CapsuleService_StreamLogsServer) error {
	path := c.LogPath
	if path == "" {
		return status.Error(codes.FailedPrecondition, "capsuled log path not configured")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return status.Error(codes.NotFound, "no capsule logs yet")
		}
		return status.Error(codes.Internal, err.Error())
	}
	defer f.Close()

	if n := int(req.GetTailLines()); n > 0 {
		if err := seekTail(f, n); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}

	ctx := stream.Context()
	buf := make([]byte, 16*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&capsulev1.CapsuleLogChunk{Data: append([]byte(nil), buf[:n]...)}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			if !req.GetFollow() {
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

func (c *CapsuleController) GetInfo(ctx context.Context, _ *capsulev1.GetInfoRequest) (*capsulev1.GetInfoResponse, error) {
	hostname, _ := os.Hostname()
	u := uname()
	uptime := uptimeSeconds()

	return &capsulev1.GetInfoResponse{
		Hostname:      hostname,
		KernelRelease: u.release,
		KernelVersion: u.version,
		Architecture:  u.machine,
		UptimeSeconds: uptime,
		CapsuleVersion:   c.CapsuleVersion,
		ActiveSlot:    c.ActiveSlot,
	}, nil
}

type unameInfo struct {
	release string
	version string
	machine string
}
