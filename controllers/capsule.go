package controllers

import (
	"context"
	stderrors "errors"
	"io"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	coreupdate "github.com/geekgonecrazy/capsule/core/update"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
)

// CapsuleController implements capsule.v1.CapsuleServiceServer.
type CapsuleController struct {
	capsulev1.UnimplementedCapsuleServiceServer

	// CapsuleVersion is the build-time version string baked into the binary.
	CapsuleVersion string
	// ActiveSlot is the currently active A/B slot identifier. Empty in
	// dev mode or for old single-slot builds.
	ActiveSlot string
	// LogPath is the file capsuled tees its slog output to. StreamLogs
	// opens and tails this.
	LogPath string
	// OSStateStore exposes pending-slot bookkeeping for GetInfo.
	OSStateStore store.OSStateStore
	// UpdateService handles the heavy lifting for UpdateOS / UpdateConfirm.
	UpdateService *coreupdate.Service
	// RebootDelay defers `unix.Reboot` after responding to UpdateOS so the
	// gRPC client gets a clean response before the kernel restarts. 1s is
	// usually enough; tests can shrink it.
	RebootDelay time.Duration
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

	memTotal, memAvail := memInfo()
	cpuCores, cpuModel := cpuInfo()
	diskPath, diskTotal := diskInfo()
	poolTotal, poolUsed := thinpoolUsage()

	resp := &capsulev1.GetInfoResponse{
		Hostname:             hostname,
		KernelRelease:        u.release,
		KernelVersion:        u.version,
		Architecture:         u.machine,
		UptimeSeconds:        uptime,
		CapsuleVersion:       c.CapsuleVersion,
		ActiveSlot:           c.ActiveSlot,
		MemoryTotalBytes:     memTotal,
		MemoryAvailableBytes: memAvail,
		CpuCores:             cpuCores,
		CpuModel:             cpuModel,
		BootDisk:             diskPath,
		DiskTotalBytes:       diskTotal,
		ThinpoolTotalBytes:   poolTotal,
		ThinpoolUsedBytes:    poolUsed,
		LocalTimeUnix:        time.Now().Unix(),
	}
	if c.OSStateStore != nil {
		st, err := c.OSStateStore.Get(ctx)
		if err == nil {
			resp.PendingSlot = st.PendingSlot
			resp.PendingDeadlineUnix = st.PendingDeadlineUnix
			resp.LastVersion = st.LastVersion
		}
	}
	return resp, nil
}

// UpdateOS receives a streamed update bundle: first message is metadata,
// subsequent messages are bytes. On success, schedules a reboot in the
// background and returns the slot we wrote to.
func (c *CapsuleController) UpdateOS(stream capsulev1.CapsuleService_UpdateOSServer) error {
	if c.UpdateService == nil {
		return status.Error(codes.Unimplemented, "update service not configured")
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "no metadata: %v", err)
	}
	meta := first.GetMetadata()
	if meta == nil {
		return status.Error(codes.InvalidArgument, "first message must be UpdateOSMetadata")
	}
	chunkFn := func() ([]byte, error) {
		msg, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		if c := msg.GetChunk(); c != nil {
			return c, nil
		}
		// metadata sent twice → ignore (treat as empty chunk).
		return []byte{}, nil
	}
	slot, version, err := c.UpdateService.ReceiveBundle(stream.Context(), meta.GetTotalBytes(), meta.GetSha256Hex(), chunkFn)
	if err != nil {
		switch {
		case stderrors.Is(err, coreupdate.ErrChecksumMismatch):
			return status.Error(codes.InvalidArgument, err.Error())
		case stderrors.Is(err, coreupdate.ErrBundleTooLarge):
			return status.Error(codes.FailedPrecondition, err.Error())
		default:
			return status.Error(codes.Internal, err.Error())
		}
	}
	if err := stream.SendAndClose(&capsulev1.UpdateOSResponse{
		NextSlot:        slot,
		NextVersion:     version,
		RebootScheduled: true,
	}); err != nil {
		return err
	}
	delay := c.RebootDelay
	if delay <= 0 {
		delay = time.Second
	}
	go func() {
		time.Sleep(delay)
		if err := c.UpdateService.Reboot(); err != nil {
			// At PID 1, this should not return; if it does, just log.
			// (controllers can't slog without importing — keep silent.)
			_ = err
		}
	}()
	return nil
}

// UpdateConfirm commits a tentative slot.
func (c *CapsuleController) UpdateConfirm(ctx context.Context, _ *capsulev1.UpdateConfirmRequest) (*capsulev1.UpdateConfirmResponse, error) {
	if c.UpdateService == nil {
		return nil, status.Error(codes.Unimplemented, "update service not configured")
	}
	slot, version, err := c.UpdateService.Confirm(ctx)
	if err != nil {
		switch {
		case stderrors.Is(err, coreupdate.ErrNoPending):
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		case stderrors.Is(err, coreupdate.ErrSlotMismatch):
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		default:
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	return &capsulev1.UpdateConfirmResponse{
		CommittedSlot:    slot,
		CommittedVersion: version,
	}, nil
}

type unameInfo struct {
	release string
	version string
	machine string
}
