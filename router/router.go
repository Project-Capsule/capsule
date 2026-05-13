// Package router bootstraps the capsule gRPC server: it constructs the
// *grpc.Server, installs interceptors (auth, logging, metrics), and mounts
// controllers. It does not know what any service does.
package router

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/geekgonecrazy/capsule/auth"
	"github.com/geekgonecrazy/capsule/controllers"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// Config wires the server's dependencies.
type Config struct {
	// Listen address, e.g. ":50000".
	Addr string

	// TLSCert is the server's self-signed leaf cert + private key. The
	// client pins SHA-256(cert.Certificate[0]) so this only needs to
	// stay byte-stable across reboots, not chain to any CA.
	TLSCert tls.Certificate

	// Auth gates every RPC. The interceptor whitelists exactly one
	// method (IdentityService.Adopt) and only while the claim window
	// is open; everything else requires a valid bearer token.
	Auth *auth.Authenticator

	// EnableReflection turns on grpc.reflection.v1.ServerReflection. Off
	// by default once auth lands — reflection over an unauthenticated
	// path would leak the entire wire schema. Useful for `grpcurl` in dev.
	EnableReflection bool

	// Controllers. Only the ones wired here are exposed.
	Capsule  *controllers.CapsuleController
	Workload *controllers.WorkloadController
	Volume   *controllers.VolumeController
	Image    *controllers.ImageController
	Identity *controllers.IdentityController
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
	if cfg.Auth == nil {
		return fmt.Errorf("router: missing Authenticator")
	}
	if cfg.Identity == nil {
		return fmt.Errorf("router: missing Identity controller")
	}
	if len(cfg.TLSCert.Certificate) == 0 {
		return fmt.Errorf("router: missing TLSCert")
	}

	lis, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("router: listen %s: %w", cfg.Addr, err)
	}

	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cfg.TLSCert},
		MinVersion:   tls.VersionTLS13,
	})

	// Keepalive: long-running streams (logs -f, exec) need to fail
	// fast when the underlying connection breaks (capsule reboot,
	// network blip) instead of stalling in Recv() until the OS gives
	// up on the TCP connection. EnforcementPolicy.MinTime must be
	// <= the client's keepalive Time so the client's pings aren't
	// rejected as too frequent — keep these in sync with capsulectl's
	// dial() params.
	srv := grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(cfg.Auth.Unary()),
		grpc.StreamInterceptor(cfg.Auth.Stream()),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	)
	capsulev1.RegisterIdentityServiceServer(srv, cfg.Identity)
	capsulev1.RegisterCapsuleServiceServer(srv, cfg.Capsule)
	if cfg.Workload != nil {
		capsulev1.RegisterWorkloadServiceServer(srv, cfg.Workload)
	}
	if cfg.Volume != nil {
		capsulev1.RegisterVolumeServiceServer(srv, cfg.Volume)
	}
	if cfg.Image != nil {
		capsulev1.RegisterImageServiceServer(srv, cfg.Image)
	}

	// Reflection leaks the entire wire schema; gate it behind a dev flag
	// now that auth lands. It still lives behind the auth interceptor
	// (default-deny would block it anyway), so a forgotten flag isn't
	// catastrophic — but turning it off by default keeps the surface tight.
	if cfg.EnableReflection {
		reflection.Register(srv)
	}

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
