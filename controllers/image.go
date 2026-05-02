package controllers

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	coreimage "github.com/geekgonecrazy/capsule/core/image"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// ImageController implements capsule.v1.ImageServiceServer. It owns
// only the gRPC translation; cache lookups + import live in core/image.
type ImageController struct {
	capsulev1.UnimplementedImageServiceServer
	Service *coreimage.Service
}

func (c *ImageController) List(ctx context.Context, _ *capsulev1.ImageListRequest) (*capsulev1.ImageListResponse, error) {
	imgs, err := c.Service.List(ctx)
	if err != nil {
		if errors.Is(err, coreimage.ErrNoRuntime) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &capsulev1.ImageListResponse{Images: imgs}, nil
}

// Push receives metadata + streamed tar bytes, pipes them into the
// runtime's image store, and replies with the imported refs. We use an
// io.Pipe so containerd can consume the archive while we keep reading
// gRPC chunks — Import wants a Reader and we don't want to buffer a
// (potentially multi-GiB) image in memory.
func (c *ImageController) Push(stream capsulev1.ImageService_PushServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetMetadata() == nil {
		return status.Error(codes.InvalidArgument, "first Push message must carry metadata")
	}
	totalBytes := first.GetMetadata().GetTotalBytes()

	pr, pw := io.Pipe()

	// Recv goroutine: gRPC chunks -> pipe writer. Close the writer when
	// the client closes its send side (or on any recv error) so Import
	// observes EOF and returns.
	var received uint64
	recvDone := make(chan error, 1)
	go func() {
		defer pw.Close()
		for {
			msg, rerr := stream.Recv()
			if rerr == io.EOF {
				recvDone <- nil
				return
			}
			if rerr != nil {
				_ = pw.CloseWithError(rerr)
				recvDone <- rerr
				return
			}
			chunk := msg.GetChunk()
			if len(chunk) == 0 {
				continue
			}
			if _, werr := pw.Write(chunk); werr != nil {
				recvDone <- werr
				return
			}
			received += uint64(len(chunk))
		}
	}()

	refs, importErr := c.Service.Import(stream.Context(), pr)
	// Drain the recv goroutine: Import has either consumed everything or
	// errored; in either case we want pw closed before we return so the
	// goroutine can exit. CloseWithError is idempotent.
	_ = pr.CloseWithError(io.EOF)
	rerr := <-recvDone

	if importErr != nil {
		if errors.Is(importErr, coreimage.ErrNoRuntime) {
			return status.Error(codes.FailedPrecondition, importErr.Error())
		}
		return status.Error(codes.Internal, importErr.Error())
	}
	if rerr != nil {
		return status.Error(codes.Internal, rerr.Error())
	}
	slog.Info("image push", "refs", refs, "bytes", received, "metadata_total", totalBytes)
	return stream.SendAndClose(&capsulev1.ImagePushResponse{
		ImageRefs:     refs,
		BytesReceived: received,
	})
}
