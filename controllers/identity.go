package controllers

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/geekgonecrazy/capsule/auth"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
)

// IdentityController implements capsule.v1.IdentityServiceServer. It is
// a thin RPC adapter — claim-window enforcement and JWT verification
// live in the auth interceptor; this file is the only place that
// actually mutates the authorized-keys table.
type IdentityController struct {
	capsulev1.UnimplementedIdentityServiceServer

	// Identity is the singleton CapsuleIdentity store. Read on every
	// WhoAmI to surface the capsule_id; written by Adopt to record the
	// adoption timestamp + bootstrap kid.
	Identity store.IdentityStore
	// Keys is the authorized_keys store.
	Keys store.AuthorizedKeyStore
	// Claim is the boot-time adoption gate. The interceptor also checks
	// this before letting Adopt through; the controller calls Close on
	// success so a second Adopt in the same window race loses cleanly.
	Claim *auth.ClaimWindow
	// TLSFingerprint is the SHA-256 hex digest of the server's TLS leaf
	// cert. Echoed back in AdoptResponse so the client can cross-check
	// against the cert it saw on the wire.
	TLSFingerprint string
}

// Adopt enrolls the caller's pubkey as the first authorized key.
// Authentication is unnecessary (the interceptor confirmed the claim
// window is open); we still re-check Claim.Open() to lose cleanly if
// two Adopt calls race.
func (c *IdentityController) Adopt(ctx context.Context, req *capsulev1.AdoptRequest) (*capsulev1.AdoptResponse, error) {
	if !c.Claim.Open() {
		return nil, status.Error(codes.FailedPrecondition, "adoption window closed")
	}
	pub, err := auth.ParsePubkey(req.GetPubkey())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	name := req.GetName()
	if name == "" {
		name = "operator"
	}
	kid := auth.KidFromPubkey(pub)

	id, err := c.Identity.Get(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load identity: %v", err)
	}

	if err := c.Keys.Add(ctx, &store.AuthorizedKey{
		Kid:         kid,
		Pubkey:      pub,
		Name:        name,
		AddedByKid:  "", // bootstrap key
		AddedAtUnix: time.Now().Unix(),
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Re-running adopt with the same key is a no-op success.
			slog.Info("adopt: key already enrolled", "kid", kid)
		} else {
			return nil, status.Errorf(codes.Internal, "store key: %v", err)
		}
	}
	id.AdoptedAtUnix = time.Now().Unix()
	id.AdoptedByKid = kid
	if err := c.Identity.Put(ctx, id); err != nil {
		return nil, status.Errorf(codes.Internal, "save identity: %v", err)
	}
	c.Claim.Close()
	slog.Info("capsule adopted", "kid", kid, "name", name, "capsule_id", id.CapsuleID)
	return &capsulev1.AdoptResponse{
		CapsuleId:            id.CapsuleID,
		Kid:                  kid,
		TlsFingerprintSha256: c.TLSFingerprint,
	}, nil
}

// WhoAmI returns the caller's enrolled identity.
func (c *IdentityController) WhoAmI(ctx context.Context, _ *capsulev1.WhoAmIRequest) (*capsulev1.WhoAmIResponse, error) {
	kid := auth.KidFromContext(ctx)
	if kid == "" {
		return nil, status.Error(codes.Unauthenticated, "no authenticated identity")
	}
	id, err := c.Identity.Get(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load identity: %v", err)
	}
	k, err := c.Keys.Get(ctx, kid)
	if err != nil {
		// The interceptor verified the kid moments ago — a Get failure
		// means the key was just removed concurrently. Degrade to "kid
		// only" rather than 500.
		return &capsulev1.WhoAmIResponse{CapsuleId: id.CapsuleID, Kid: kid}, nil
	}
	return &capsulev1.WhoAmIResponse{
		CapsuleId: id.CapsuleID,
		Kid:       kid,
		Name:      k.Name,
	}, nil
}

// KeyAdd enrolls another operator's pubkey. Available to any
// already-authenticated caller (flat trust model).
func (c *IdentityController) KeyAdd(ctx context.Context, req *capsulev1.KeyAddRequest) (*capsulev1.KeyAddResponse, error) {
	caller := auth.KidFromContext(ctx)
	if caller == "" {
		return nil, status.Error(codes.Unauthenticated, "no authenticated identity")
	}
	pub, err := auth.ParsePubkey(req.GetPubkey())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	kid := auth.KidFromPubkey(pub)
	now := time.Now().Unix()
	if err := c.Keys.Add(ctx, &store.AuthorizedKey{
		Kid:         kid,
		Pubkey:      pub,
		Name:        name,
		AddedByKid:  caller,
		AddedAtUnix: now,
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, status.Error(codes.AlreadyExists, "key already enrolled")
		}
		return nil, status.Errorf(codes.Internal, "store key: %v", err)
	}
	slog.Info("key added", "kid", kid, "name", name, "added_by", caller)
	return &capsulev1.KeyAddResponse{
		Key: &capsulev1.AuthorizedKey{
			Kid:         kid,
			Name:        name,
			AddedByKid:  caller,
			AddedAtUnix: now,
		},
	}, nil
}

// KeyList returns every enrolled key (kid + metadata only — pubkey
// bytes intentionally omitted from the wire).
func (c *IdentityController) KeyList(ctx context.Context, _ *capsulev1.KeyListRequest) (*capsulev1.KeyListResponse, error) {
	keys, err := c.Keys.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list keys: %v", err)
	}
	out := make([]*capsulev1.AuthorizedKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, &capsulev1.AuthorizedKey{
			Kid:         k.Kid,
			Name:        k.Name,
			AddedByKid:  k.AddedByKid,
			AddedAtUnix: k.AddedAtUnix,
		})
	}
	return &capsulev1.KeyListResponse{Keys: out}, nil
}

// KeyRemove removes an enrolled key. Rejects a removal that would leave
// zero keys — the operator must add a second key first or use the
// console-side RESET_AUTH procedure.
func (c *IdentityController) KeyRemove(ctx context.Context, req *capsulev1.KeyRemoveRequest) (*capsulev1.KeyRemoveResponse, error) {
	caller := auth.KidFromContext(ctx)
	if caller == "" {
		return nil, status.Error(codes.Unauthenticated, "no authenticated identity")
	}
	kid := req.GetKid()
	if kid == "" {
		return nil, status.Error(codes.InvalidArgument, "kid required")
	}
	count, err := c.Keys.Count(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count keys: %v", err)
	}
	if count <= 1 {
		return nil, status.Error(codes.FailedPrecondition,
			"cannot remove the last enrolled key; add another first or use the console RESET_AUTH procedure")
	}
	if err := c.Keys.Delete(ctx, kid); err != nil {
		return nil, status.Errorf(codes.Internal, "delete key: %v", err)
	}
	slog.Info("key removed", "kid", kid, "removed_by", caller)
	return &capsulev1.KeyRemoveResponse{}, nil
}
