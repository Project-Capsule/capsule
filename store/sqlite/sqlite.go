// Package sqlite is the production store implementation, backed by
// modernc.org/sqlite (pure-Go, no CGO — keeps the capsuled build static).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
	"github.com/geekgonecrazy/capsule/store"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed implementation of store.Store.
type Store struct {
	db        *sql.DB
	workloads *workloadStore
	volumes   *volumeStore
	osState   *osStateStore
}

// Open opens (and creates if necessary) the SQLite database at path, runs
// migrations, and returns a Store.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	s.workloads = &workloadStore{db: db}
	s.volumes = &volumeStore{db: db}
	s.osState = &osStateStore{db: db}
	return s, nil
}

// Workloads returns the WorkloadStore. See store.Store.
func (s *Store) Workloads() store.WorkloadStore { return s.workloads }

// Volumes returns the VolumeStore. See store.Store.
func (s *Store) Volumes() store.VolumeStore { return s.volumes }

// OSState returns the OSStateStore. See store.Store.
func (s *Store) OSState() store.OSStateStore { return s.osState }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS workloads (
			name TEXT PRIMARY KEY,
			spec BLOB NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
		`CREATE TABLE IF NOT EXISTS volumes (
			name TEXT PRIMARY KEY,
			host_path TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
		// Singleton row of A/B update bookkeeping. CHECK enforces at most
		// one row; capsuled inserts the seed record on first boot.
		`CREATE TABLE IF NOT EXISTS os_state (
			singleton             INTEGER PRIMARY KEY CHECK (singleton = 1),
			active_slot           TEXT NOT NULL,
			pending_slot          TEXT,
			pending_deadline_unix INTEGER,
			last_good_slot        TEXT NOT NULL,
			last_version          TEXT,
			updated_at_unix       INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("sqlite migrate: %w", err)
		}
	}
	return nil
}

type osStateStore struct{ db *sql.DB }

func (s *osStateStore) Get(ctx context.Context) (*store.OSState, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT active_slot, pending_slot, pending_deadline_unix,
       last_good_slot, last_version, updated_at_unix
FROM os_state WHERE singleton = 1;`)
	var (
		st       store.OSState
		pending  sql.NullString
		deadline sql.NullInt64
		ver      sql.NullString
	)
	if err := row.Scan(&st.ActiveSlot, &pending, &deadline, &st.LastGoodSlot, &ver, &st.UpdatedAtUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if pending.Valid {
		st.PendingSlot = pending.String
	}
	if deadline.Valid {
		st.PendingDeadlineUnix = deadline.Int64
	}
	if ver.Valid {
		st.LastVersion = ver.String
	}
	return &st, nil
}

func (s *osStateStore) Put(ctx context.Context, st *store.OSState) error {
	var pending, ver any
	var deadline any
	if st.PendingSlot != "" {
		pending = st.PendingSlot
	}
	if st.PendingDeadlineUnix != 0 {
		deadline = st.PendingDeadlineUnix
	}
	if st.LastVersion != "" {
		ver = st.LastVersion
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO os_state(singleton, active_slot, pending_slot, pending_deadline_unix,
                    last_good_slot, last_version, updated_at_unix)
VALUES(1, ?, ?, ?, ?, ?, strftime('%s','now'))
ON CONFLICT(singleton) DO UPDATE SET
    active_slot           = excluded.active_slot,
    pending_slot          = excluded.pending_slot,
    pending_deadline_unix = excluded.pending_deadline_unix,
    last_good_slot        = excluded.last_good_slot,
    last_version          = excluded.last_version,
    updated_at_unix       = excluded.updated_at_unix;
`, st.ActiveSlot, pending, deadline, st.LastGoodSlot, ver)
	return err
}

type volumeStore struct{ db *sql.DB }

func (s *volumeStore) Put(ctx context.Context, v *capsulev1.Volume) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO volumes(name, host_path, created_at)
VALUES(?, ?, COALESCE(NULLIF(?, 0), strftime('%s','now')))
ON CONFLICT(name) DO UPDATE
SET host_path = excluded.host_path;
`, v.GetName(), v.GetHostPath(), v.GetCreatedAtUnix())
	return err
}

func (s *volumeStore) Get(ctx context.Context, name string) (*capsulev1.Volume, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, host_path, created_at FROM volumes WHERE name = ?;`, name)
	v := &capsulev1.Volume{}
	if err := row.Scan(&v.Name, &v.HostPath, &v.CreatedAtUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return v, nil
}

func (s *volumeStore) List(ctx context.Context) ([]*capsulev1.Volume, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, host_path, created_at FROM volumes ORDER BY name;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*capsulev1.Volume
	for rows.Next() {
		v := &capsulev1.Volume{}
		if err := rows.Scan(&v.Name, &v.HostPath, &v.CreatedAtUnix); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *volumeStore) Delete(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM volumes WHERE name = ?;`, name)
	return err
}

type workloadStore struct {
	db *sql.DB
}

func (s *workloadStore) Put(ctx context.Context, w *capsulev1.Workload) error {
	blob, err := proto.Marshal(w)
	if err != nil {
		return fmt.Errorf("marshal workload: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO workloads(name, spec, updated_at)
VALUES(?, ?, strftime('%s','now'))
ON CONFLICT(name) DO UPDATE
SET spec = excluded.spec, updated_at = excluded.updated_at;
`, w.GetName(), blob)
	return err
}

func (s *workloadStore) Get(ctx context.Context, name string) (*capsulev1.Workload, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM workloads WHERE name = ?;`, name).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w := &capsulev1.Workload{}
	if err := proto.Unmarshal(blob, w); err != nil {
		return nil, fmt.Errorf("unmarshal workload %q: %w", name, err)
	}
	return w, nil
}

func (s *workloadStore) List(ctx context.Context) ([]*capsulev1.Workload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM workloads ORDER BY name;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*capsulev1.Workload
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		w := &capsulev1.Workload{}
		if err := proto.Unmarshal(blob, w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *workloadStore) Delete(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM workloads WHERE name = ?;`, name)
	return err
}
