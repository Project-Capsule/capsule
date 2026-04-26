// Package router bootstraps the capsule gRPC server: it constructs the
// *grpc.Server, installs interceptors (auth, logging, metrics), and mounts
// controllers. It does not know what any service does.
package router

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/geekgonecrazy/capsule/controllers"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// Config wires the server's dependencies.
type Config struct {
	// Listen address, e.g. ":50000".
	Addr string

	// Controllers. Only the ones wired here are exposed.
	Capsule  *controllers.CapsuleController
	Workload *controllers.WorkloadController
	Volume   *controllers.VolumeController
}

// Serve starts the gRPC server and blocks until ctx is cancelled or the
// server errors. It returns a non-nil error only on a serve failure.
func Serve(ctx context.Context, cfg Config) error {
	if cfg.Addr == "" {
		return fmt.Errorf("router: missing Addr")
	}
	if cfg.Capsule == nil {
		return fmt.Errorf("router: missing Capsule controller")
	}

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("router: listen %s: %w", cfg.Addr, err)
	}

	// Keepalive: long-running streams (logs -f, exec) need to fail
	// fast when the underlying connection breaks (capsule reboot,
	// network blip) instead of stalling in Recv() until the OS gives
	// up on the TCP connection. EnforcementPolicy.MinTime must be
	// <= the client's keepalive Time so the client's pings aren't
	// rejected as too frequent — keep these in sync with capsulectl's
	// dial() params.
	srv := grpc.NewServer(
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	)
	capsulev1.RegisterCapsuleServiceServer(srv, cfg.Capsule)
	if cfg.Workload != nil {
		capsulev1.RegisterWorkloadServiceServer(srv, cfg.Workload)
	}
	if cfg.Volume != nil {
		capsulev1.RegisterVolumeServiceServer(srv, cfg.Volume)
	}

	// Reflection is handy for phase 0 grpcurl exploration; keep it until
	// mTLS lands in phase 5 and gate behind a flag there.
	reflection.Register(srv)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("gRPC server listening", "addr", cfg.Addr)
		errCh <- srv.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		slog.Info("gRPC server stopping")
		srv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}
