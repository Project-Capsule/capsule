package auth

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func newAuthForTest(t *testing.T, capsuleID string, pub ed25519.PublicKey, claimOpen bool) *Authenticator {
	t.Helper()
	enrolled := 1
	if claimOpen {
		enrolled = 0
	}
	claim := NewClaimWindow(enrolled, time.Minute)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	return NewAuthenticator(capsuleID,
		func(_ context.Context, kid string) (ed25519.PublicKey, bool) {
			if kid != KidFromPubkey(pub) {
				return nil, false
			}
			return pub, true
		}, claim, stop)
}

func TestInterceptorAdoptOpenWindow(t *testing.T) {
	pub, _ := mustKeypair(t)
	a := newAuthForTest(t, "cap", pub, true)
	called := false
	handler := func(_ context.Context, _ any) (any, error) { called = true; return "ok", nil }
	_, err := a.Unary()(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: AdoptMethod}, handler)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !called {
		t.Fatal("handler not invoked")
	}
}

func TestInterceptorAdoptClosedWindow(t *testing.T) {
	pub, _ := mustKeypair(t)
	a := newAuthForTest(t, "cap", pub, false)
	_, err := a.Unary()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: AdoptMethod},
		func(context.Context, any) (any, error) { return "ok", nil },
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
}

func TestInterceptorRequiresToken(t *testing.T) {
	pub, _ := mustKeypair(t)
	a := newAuthForTest(t, "cap", pub, false)
	_, err := a.Unary()(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/capsule.v1.WorkloadService/List"},
		func(context.Context, any) (any, error) { return "ok", nil },
	)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestInterceptorAcceptsValidToken(t *testing.T) {
	pub, priv := mustKeypair(t)
	a := newAuthForTest(t, "cap", pub, false)
	tok, err := Mint(priv, "cap", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	md := metadata.Pairs("authorization", "Bearer "+tok)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	gotKid := ""
	handler := func(c context.Context, _ any) (any, error) { gotKid = KidFromContext(c); return "ok", nil }
	_, err = a.Unary()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/capsule.v1.WorkloadService/List"}, handler)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if gotKid != KidFromPubkey(pub) {
		t.Errorf("kid in context = %q want %q", gotKid, KidFromPubkey(pub))
	}
}

func TestInterceptorRejectsReplay(t *testing.T) {
	pub, priv := mustKeypair(t)
	a := newAuthForTest(t, "cap", pub, false)
	tok, _ := Mint(priv, "cap", time.Minute)
	md := metadata.Pairs("authorization", "Bearer "+tok)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	handler := func(context.Context, any) (any, error) { return "ok", nil }
	if _, err := a.Unary()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/capsule.v1.WorkloadService/List"}, handler); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := a.Unary()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/capsule.v1.WorkloadService/List"}, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("second call (replay) want Unauthenticated, got %v", err)
	}
}

func TestInterceptorRejectsWrongAudience(t *testing.T) {
	pub, priv := mustKeypair(t)
	a := newAuthForTest(t, "cap-A", pub, false)
	tok, _ := Mint(priv, "cap-B", time.Minute)
	md := metadata.Pairs("authorization", "Bearer "+tok)
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := a.Unary()(ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/capsule.v1.WorkloadService/List"},
		func(context.Context, any) (any, error) { return "ok", nil },
	)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}
