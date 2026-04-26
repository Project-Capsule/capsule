//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// guestVsockPort is the port capsule-guest listens on inside every VM.
// Must match cmd/capsule-guest/main.go.
const guestVsockPort = 52

// dialGuest dials the capsule-guest gRPC agent inside the VM via Firecracker's
// Unix-socket-proxied vsock. The host writes "CONNECT <port>\n" on the main
// UDS and Firecracker tunnels it to the guest's AF_VSOCK listener on that
// port.
func dialGuest(ctx context.Context, udsPath string, port uint32) (*grpc.ClientConn, error) {
	dialer := func(dctx context.Context, _ string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(dctx, "unix", udsPath)
		if err != nil {
			return nil, err
		}
		if _, err := c.Write([]byte(fmt.Sprintf("CONNECT %d\n", port))); err != nil {
			c.Close()
			return nil, err
		}
		br := bufio.NewReader(c)
		line, err := br.ReadString('\n')
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("vsock read reply: %w", err)
		}
		if !strings.HasPrefix(line, "OK ") {
			c.Close()
			return nil, fmt.Errorf("vsock unexpected reply %q", strings.TrimSpace(line))
		}
		// If Firecracker had buffered any bytes past the OK line we'd lose
		// them — but the handshake reply ends with the newline we just read,
		// and gRPC over the returned conn writes before reading, so this is
		// safe in practice.
		return c, nil
	}

	return grpc.DialContext(ctx, "unused",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithBlock(),
	)
}

// waitForGuestReady dials the guest agent and sends Ping until it
// succeeds or timeout elapses. Returns the live gRPC connection on
// success. Polled tightly (50ms) because this sits in the apply→exec
// critical path — the VM is usually ready within a few hundred ms of
// Firecracker spawn, and a 250ms inter-attempt sleep was costing us
// ~125ms of avg latency on every cold VM start. Per-attempt dial/ping
// timeouts stay generous (2s) so a single hiccup doesn't fail the
// whole wait.
func waitForGuestReady(ctx context.Context, udsPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := dialGuest(dctx, udsPath, guestVsockPort)
		cancel()
		if err == nil {
			pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
			_, perr := capsulev1.NewGuestAgentClient(conn).Ping(pctx, &capsulev1.PingRequest{})
			pcancel()
			if perr == nil {
				return conn, nil
			}
			_ = conn.Close()
			lastErr = perr
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("guest agent not ready within %s: %w", timeout, lastErr)
}
