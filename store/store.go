// Package store defines the persistence port for Capsule's business logic
// and sentinel errors. Concrete implementations live in store/sqlite
// (production) and store/memory (tests). The store is the *only*
// persistence surface the core layer knows about.
package store

import (
	"context"
	"errors"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// Sentinel errors. Implementations must return these exactly so callers
// can errors.Is check without depending on a specific backend.
var (
	// ErrNotFound is returned when a requested record does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a Put would violate a uniqueness
	// constraint (e.g. a Create racing with itself).
	ErrConflict = errors.New("store: conflict")
)

// Store is the top-level handle that exposes sub-stores per domain.
// A Store is safe for concurrent use.
type Store interface {
	// Workloads returns the WorkloadStore used for Workload persistence.
	Workloads() WorkloadStore
	// Volumes returns the VolumeStore used for Volume persistence.
	Volumes() VolumeStore
	// OSState returns the OSStateStore used for A/B-update bookkeeping.
	OSState() OSStateStore
	// Identity returns the IdentityStore holding the singleton capsule
	// identity record (UUID + adoption state).
	Identity() IdentityStore
	// AuthorizedKeys returns the AuthorizedKeyStore holding the operator
	// public keys allowed to talk to this capsule.
	AuthorizedKeys() AuthorizedKeyStore
	// Close releases any underlying resources (database handles etc).
	Close() error
}

// OSState is the singleton record describing the capsule's A/B update
// state. There is only ever one instance per capsule.
type OSState struct {
	// ActiveSlot is the slot we booted into ("slot_a" / "slot_b").
	ActiveSlot string
	// PendingSlot is set between an UpdateOS push and the matching
	// UpdateConfirm (or auto-rollback). Empty when no update is pending.
	PendingSlot string
	// PendingDeadlineUnix is the wall-clock time at which capsuled will
	// auto-reboot to roll back if no UpdateConfirm arrives. Zero when no
	// update is pending.
	PendingDeadlineUnix int64
	// LastGoodSlot is the slot that was most recently successfully committed.
	// Seeded on first boot to ActiveSlot.
	LastGoodSlot string
	// LastVersion is the VERSION string of the most recently committed (or
	// pending) bundle. Empty if the slot was never updated via UpdateOS.
	LastVersion string
	// UpdatedAtUnix is when the row was last modified — diagnostics only.
	UpdatedAtUnix int64
}

// OSStateStore persists the single OSState record. The store is keyed by
// nothing — there's only ever one row per capsule.
type OSStateStore interface {
	// Get returns the persisted OSState. Returns ErrNotFound on a fresh
	// capsule (the caller seeds the first record).
	Get(ctx context.Context) (*OSState, error)
	// Put inserts or replaces the singleton row.
	Put(ctx context.Context, state *OSState) error
}

// VolumeStore persists Volume records. Unlike workloads, volumes are
// small and have few fields, so each field is its own column.
type VolumeStore interface {
	// Put inserts or replaces the volume keyed by volume.Name.
	Put(ctx context.Context, volume *capsulev1.Volume) error
	// Get returns the volume with the given name, or ErrNotFound.
	Get(ctx context.Context, name string) (*capsulev1.Volume, error)
	// List returns every persisted volume, sorted by name.
	List(ctx context.Context) ([]*capsulev1.Volume, error)
	// Delete removes the named volume. Missing records are not an error.
	Delete(ctx context.Context, name string) error
}

// WorkloadStore persists Workload records. Implementations store the
// entire Workload proto as an opaque blob; the core layer is the only
// thing that interprets its fields.
type WorkloadStore interface {
	// Put inserts or replaces the workload keyed by workload.Name.
	Put(ctx context.Context, workload *capsulev1.Workload) error
	// Get returns the workload with the given name, or ErrNotFound.
	Get(ctx context.Context, name string) (*capsulev1.Workload, error)
	// List returns every workload currently stored, sorted by name.
	List(ctx context.Context) ([]*capsulev1.Workload, error)
	// Delete removes the named workload. Missing records are not an error.
	Delete(ctx context.Context, name string) error
}

// CapsuleIdentity is the singleton record describing this capsule's stable
// identity and adoption state. Generated on first boot.
type CapsuleIdentity struct {
	// CapsuleID is a UUIDv4 generated once on first boot. Used as the
	// JWT audience so a token minted for capsule A can't be replayed at
	// capsule B even if the operator is enrolled on both.
	CapsuleID string
	// CreatedAtUnix is when the identity row was first written.
	CreatedAtUnix int64
	// AdoptedAtUnix is when the first authorized key was enrolled. Zero
	// until the capsule has been adopted; flipping non-zero is the signal
	// that the claim window should be closed.
	AdoptedAtUnix int64
	// AdoptedByKid is the kid of the first enrolled key, recorded for
	// audit. Empty until adoption.
	AdoptedByKid string
}

// AuthorizedKey is one operator's public key that's allowed to mint JWTs
// for this capsule. The pubkey bytes are the raw 32-byte Ed25519 form.
type AuthorizedKey struct {
	// Kid is the key fingerprint: base64url(sha256(pubkey)) without
	// padding. Stable identifier surfaced in `key list` and JWT `sub`.
	Kid string
	// Pubkey is the 32-byte raw Ed25519 public key.
	Pubkey []byte
	// Name is an operator-friendly label ("laptop", "ci", etc).
	Name string
	// AddedByKid is the kid of the operator who enrolled this key, or
	// empty for the bootstrap key (added via the unauthenticated Adopt
	// RPC during the claim window).
	AddedByKid string
	// AddedAtUnix is the Unix-seconds timestamp of enrollment.
	AddedAtUnix int64
}

// IdentityStore persists the singleton CapsuleIdentity record. There's
// only ever one row per capsule.
type IdentityStore interface {
	// Get returns the persisted CapsuleIdentity. Returns ErrNotFound on
	// a fresh capsule (the caller seeds the first record).
	Get(ctx context.Context) (*CapsuleIdentity, error)
	// Put inserts or replaces the singleton row.
	Put(ctx context.Context, id *CapsuleIdentity) error
}

// AuthorizedKeyStore persists the set of operator public keys allowed to
// authenticate to this capsule.
type AuthorizedKeyStore interface {
	// Add enrolls a new authorized key. Returns ErrConflict if the kid
	// already exists.
	Add(ctx context.Context, k *AuthorizedKey) error
	// Get returns the AuthorizedKey with the given kid, or ErrNotFound.
	Get(ctx context.Context, kid string) (*AuthorizedKey, error)
	// List returns every enrolled key, sorted by added_at then kid.
	List(ctx context.Context) ([]*AuthorizedKey, error)
	// Delete removes the named key. Missing kids are not an error;
	// callers (the controller) enforce the "can't remove last key" rule.
	Delete(ctx context.Context, kid string) error
	// Count returns the number of enrolled keys. Hot path: the auth
	// interceptor uses this to decide whether the claim window opens.
	Count(ctx context.Context) (int, error)
	// DeleteAll wipes the keystore. Used by the console-triggered
	// RESET_AUTH recovery path on capsuled startup.
	DeleteAll(ctx context.Context) error
}
