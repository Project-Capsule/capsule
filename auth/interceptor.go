package auth

import (
	"context"
	"crypto/ed25519"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// AdoptMethod is the only fully-qualified gRPC method that may be
// called without a valid bearer token on a runtime capsule. Even
// Adopt is gated by the ClaimWindow — no key, no token, no
// authentication of any kind is possible against any other method.
const AdoptMethod = "/capsule.v1.IdentityService/Adopt"

// InstallerMethods are exposed by capsuled when it boots in installer
// mode (USB on a machine with a viable internal target). Pre-adoption,
// physical access to the USB is the trust boundary — the same posture
// as the claim window. The operator validates the installer's TLS
// fingerprint via the HDMI banner before sealing a key.
var InstallerMethods = map[string]struct{}{
	"/capsule.v1.InstallService/Status":  {},
	"/capsule.v1.InstallService/Install": {},
}

// KeyLookup resolves an enrolled kid to its raw Ed25519 public key, or
// returns false if no such key is enrolled. The interceptor calls this
// on the hot path; implementations should keep it cheap (e.g. a SQLite
// SELECT by primary key is fine, no extra joins).
type KeyLookup func(ctx context.Context, kid string) (ed25519.PublicKey, bool)

// Authenticator owns the per-request authorization decision. It is
// constructed once at capsuled startup and shared by both the unary
// and stream interceptors.
type Authenticator struct {
	// CapsuleID is the JWT audience this server will accept. A token
	// minted for a different capsule is rejected.
	CapsuleID string
	// Lookup resolves enrolled kids to public keys.
	Lookup KeyLookup
	// Claim is the boot-time adoption gate. Open() == true is the only
	// state in which AdoptMethod is callable.
	Claim *ClaimWindow
	// InstallerMode, when true, lets the methods in InstallerMethods
	// through without authentication. Runtime capsules MUST leave this
	// false — otherwise an adopted box would accept unauthenticated
	// Install RPCs that re-flash its own disk.
	InstallerMode bool

	jti *jtiCache
}

// NewAuthenticator constructs an Authenticator and starts the jti
// sweeper goroutine. Stop the sweeper by closing stopCh on shutdown.
func NewAuthenticator(capsuleID string, lookup KeyLookup, claim *ClaimWindow, stopCh <-chan struct{}) *Authenticator {
	a := &Authenticator{
		CapsuleID: capsuleID,
		Lookup:    lookup,
		Claim:     claim,
		jti:       newJTICache(),
	}
	go a.jti.Run(stopCh, 30*time.Second)
	return a
}

type kidCtxKey struct{}

// KidFromContext returns the kid of the authenticated caller, or "" if
// the context is unauthenticated (e.g. inside the Adopt RPC).
func KidFromContext(ctx context.Context) string {
	v, _ := ctx.Value(kidCtxKey{}).(string)
	return v
}

func (a *Authenticator) authorize(ctx context.Context, fullMethod string) (context.Context, error) {
	if fullMethod == AdoptMethod {
		// Pass through unconditionally — the controller handles the claim
		// window check and the pre-enrolled key path (key pre-added via KeyAdd).
		return ctx, nil
	}
	if a.InstallerMode {
		if _, ok := InstallerMethods[fullMethod]; ok {
			return ctx, nil
		}
	}
	md, _ := metadata.FromIncomingContext(ctx)
	tok := bearer(md.Get("authorization"))
	if tok == "" {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token (run: capsulectl adopt)")
	}
	claims, err := Verify(tok, a.CapsuleID, func(kid string) (ed25519.PublicKey, bool) {
		return a.Lookup(ctx, kid)
	})
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	if a.jti.SeenOrAdd(claims.Jti, claims.Exp) {
		return nil, status.Error(codes.Unauthenticated, "auth: token replay")
	}
	return context.WithValue(ctx, kidCtxKey{}, claims.Sub), nil
}

// Unary returns the grpc.UnaryServerInterceptor to install on the server.
func (a *Authenticator) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		newCtx, err := a.authorize(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(newCtx, req)
	}
}

// Stream returns the grpc.StreamServerInterceptor. Authorization is
// checked once at stream start; mid-stream revocation is intentionally
// not a primitive at this layer (kill the connection if you need to
// revoke an active session).
func (a *Authenticator) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		newCtx, err := a.authorize(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: newCtx})
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// bearer extracts the token from a single "authorization" header value
// of the form "Bearer <token>". Case-insensitive on the scheme; trims
// whitespace. Returns "" if the slice is empty or the header is malformed.
func bearer(values []string) string {
	if len(values) == 0 {
		return ""
	}
	v := strings.TrimSpace(values[0])
	if len(v) < 7 || !strings.EqualFold(v[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(v[7:])
}
