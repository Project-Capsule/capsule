package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/geekgonecrazy/capsule/auth"
)

// dialAuth opens an authenticated gRPC connection to a previously
// adopted capsule: TLS with strict fingerprint pinning, plus client
// interceptors that mint a fresh EdDSA JWT per call.
func dialAuth(c Context) (*grpc.ClientConn, error) {
	if c.Addr == "" {
		return nil, errors.New("context missing addr")
	}
	if c.TLSFingerprint == "" {
		return nil, errors.New("context missing tls fingerprint")
	}
	if c.KeyPath == "" {
		return nil, errors.New("context missing key path")
	}
	if c.CapsuleID == "" {
		return nil, errors.New("context missing capsule_id")
	}
	priv, err := loadPrivateKey(c.KeyPath)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(pinningTLSConfig(c.TLSFingerprint))
	mint := func() (string, error) {
		return auth.Mint(priv, c.CapsuleID, auth.DefaultLifetime)
	}
	return grpc.NewClient(c.Addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithUnaryInterceptor(jwtUnary(mint)),
		grpc.WithStreamInterceptor(jwtStream(mint)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                15 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

// captureLeafFingerprint does a synchronous TLS handshake against addr,
// returns the SHA-256 hex digest of the server's leaf cert. Called by
// `capsulectl adopt` before prompting the operator — gRPC's NewClient is
// lazy (no handshake until first RPC), so we can't rely on it to populate
// a fingerprint into the prompt.
func captureLeafFingerprint(addr string) (string, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // fingerprint shown to operator for confirmation
		MinVersion:         tls.VersionTLS13,
	})
	if err != nil {
		return "", fmt.Errorf("tls dial %s: %w", addr, err)
	}
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return "", errors.New("server presented no certificates")
	}
	return auth.FingerprintCert(state.PeerCertificates[0]), nil
}

// dialPinnedForAdopt opens a gRPC connection to addr that pins the
// already-captured fingerprint. Used by `capsulectl adopt` after the
// operator has confirmed the fingerprint — the Adopt RPC fires over a
// connection that's already MITM-resistant.
func dialPinnedForAdopt(addr, fingerprint string) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(pinningTLSConfig(fingerprint))),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                15 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
}

// pinningTLSConfig returns a TLS config that skips standard CA
// validation (we don't have a CA — the cert is self-signed) and
// instead requires the leaf to match the pinned SHA-256 hex digest.
// MitM-resistant once the fingerprint is captured.
func pinningTLSConfig(fingerprintHex string) *tls.Config {
	want := strings.ToLower(strings.TrimSpace(fingerprintHex))
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // we do strict pinning below
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server presented no certificates")
			}
			got := auth.FingerprintRaw(rawCerts[0])
			if got != want {
				return fmt.Errorf("tls fingerprint mismatch: pinned %s, server presented %s", want, got)
			}
			return nil
		},
	}
}

func jwtUnary(mint func() (string, error)) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		tok, err := mint()
		if err != nil {
			return err
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func jwtStream(mint func() (string, error)) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		tok, err := mint()
		if err != nil {
			return nil, err
		}
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// generatePrivateKey creates a fresh Ed25519 keypair and writes the
// private half to path in PKCS#8 PEM (mode 0600). Returns the private
// key for immediate use. The directory is mkdir'd at 0700.
func generatePrivateKey(path string) (ed25519.PrivateKey, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("refusing to overwrite existing key at %s", path)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	tmp, err := os.CreateTemp(dir, ".key-*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(pemBytes); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	return priv, nil
}

// loadPrivateKey reads a PKCS#8 PEM Ed25519 private key from disk.
func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("key %s: no PEM block", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key %s is not Ed25519", path)
	}
	return priv, nil
}
