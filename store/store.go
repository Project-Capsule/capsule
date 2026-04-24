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
	// Close releases any underlying resources (database handles etc).
	Close() error
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
