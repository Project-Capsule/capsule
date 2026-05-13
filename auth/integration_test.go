package auth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/geekgonecrazy/capsule/auth"
	"github.com/geekgonecrazy/capsule/controllers"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
	"github.com/geekgonecrazy/capsule/store/memory"
)

// TestAdoptFlow exercises the entire end-to-end auth flow against an
// in-process gRPC server backed by bufconn — no TLS, no real network.
// We isolate the test from TLS plumbing on purpose; the auth logic
// (interceptor + JWT + claim window + last-key guard) is what we want
// to validate. Server TLS gets its own LoadOrGenerate test in tls_test.go.
func TestAdoptFlow(t *testing.T) {
	st := memory.New()

	ctx := context.Background()
	if err := st.Identity().Put(ctx, &store.CapsuleIdentity{
		CapsuleID:     "cap-test-1",
		CreatedAtUnix: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	claim := auth.NewClaimWindow(0, time.Minute)
	stop := make(chan struct{})
	defer close(stop)
	authn := auth.NewAuthenticator("cap-test-1",
		func(c context.Context, kid string) (ed25519.PublicKey, bool) {
			k, err := st.AuthorizedKeys().Get(c, kid)
			if err != nil {
				return nil, false
			}
			return ed25519.PublicKey(k.Pubkey), true
		}, claim, stop)

	identity := &controllers.IdentityController{
		Identity:       st.Identity(),
		Keys:           st.AuthorizedKeys(),
		Claim:          claim,
		TLSFingerprint: "test-fp",
	}

	srv := grpc.NewServer(
		grpc.UnaryInterceptor(authn.Unary()),
		grpc.StreamInterceptor(authn.Stream()),
	)
	capsulev1.RegisterIdentityServiceServer(srv, identity)

	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	dial := func(t *testing.T, priv ed25519.PrivateKey) capsulev1.IdentityServiceClient {
		t.Helper()
		opts := []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		}
		if priv != nil {
			mint := func() (string, error) { return auth.Mint(priv, "cap-test-1", time.Minute) }
			opts = append(opts,
				grpc.WithUnaryInterceptor(func(ctx context.Context, m string, req, rep any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, co ...grpc.CallOption) error {
					tok, err := mint()
					if err != nil {
						return err
					}
					ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
					return inv(ctx, m, req, rep, cc, co...)
				}),
			)
		}
		conn, err := grpc.NewClient("passthrough://bufnet", opts...)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		return capsulev1.NewIdentityServiceClient(conn)
	}

	// Operator 1 generates a keypair and adopts.
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	c1 := dial(t, nil) // no JWT — Adopt is unauthenticated
	resp, err := c1.Adopt(ctx, &capsulev1.AdoptRequest{Pubkey: pub1, Name: "lab1"})
	if err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	if resp.GetCapsuleId() != "cap-test-1" {
		t.Errorf("capsule_id = %q", resp.GetCapsuleId())
	}
	if resp.GetTlsFingerprintSha256() != "test-fp" {
		t.Errorf("fingerprint = %q", resp.GetTlsFingerprintSha256())
	}

	// Now the window is closed: a second Adopt fails.
	pub2, priv2, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := c1.Adopt(ctx, &capsulev1.AdoptRequest{Pubkey: pub2, Name: "lab2"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second adopt should be FailedPrecondition, got %v", err)
	}

	// Operator 1 with their JWT can call WhoAmI.
	c1Auth := dial(t, priv1)
	if _, err := c1Auth.WhoAmI(ctx, &capsulev1.WhoAmIRequest{}); err != nil {
		t.Fatalf("op1 whoami: %v", err)
	}

	// Operator 2 without enrollment is rejected.
	c2Auth := dial(t, priv2)
	if _, err := c2Auth.WhoAmI(ctx, &capsulev1.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("op2 whoami should be Unauthenticated, got %v", err)
	}

	// Operator 1 enrolls operator 2's pubkey.
	if _, err := c1Auth.KeyAdd(ctx, &capsulev1.KeyAddRequest{Pubkey: pub2, Name: "lab2"}); err != nil {
		t.Fatalf("key add: %v", err)
	}
	if _, err := c2Auth.WhoAmI(ctx, &capsulev1.WhoAmIRequest{}); err != nil {
		t.Fatalf("op2 whoami after enroll: %v", err)
	}

	// List shows both keys.
	listResp, err := c2Auth.KeyList(ctx, &capsulev1.KeyListRequest{})
	if err != nil {
		t.Fatalf("key list: %v", err)
	}
	if len(listResp.GetKeys()) != 2 {
		t.Errorf("expected 2 keys enrolled, got %d", len(listResp.GetKeys()))
	}

	// Operator 2 removes operator 1's key — should succeed (count==2 > 1).
	kid1 := auth.KidFromPubkey(pub1)
	if _, err := c2Auth.KeyRemove(ctx, &capsulev1.KeyRemoveRequest{Kid: kid1}); err != nil {
		t.Fatalf("remove op1 key: %v", err)
	}

	// Operator 2 cannot remove their own key — count would drop to 0.
	kid2 := auth.KidFromPubkey(pub2)
	if _, err := c2Auth.KeyRemove(ctx, &capsulev1.KeyRemoveRequest{Kid: kid2}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("removing last key should be FailedPrecondition, got %v", err)
	}
}
