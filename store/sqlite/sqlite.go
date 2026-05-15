// Package sqlite is the production store implementation, backed by
// modernc.org/sqlite (pure-Go, no CGO — keeps the capsuled build static).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
	identity  *identityStore
	authKeys  *authKeyStore
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
	s.identity = &identityStore{db: db}
	s.authKeys = &authKeyStore{db: db}
	return s, nil
}

// Workloads returns the WorkloadStore. See store.Store.
func (s *Store) Workloads() store.WorkloadStore { return s.workloads }

// Volumes returns the VolumeStore. See store.Store.
func (s *Store) Volumes() store.VolumeStore { return s.volumes }

// OSState returns the OSStateStore. See store.Store.
func (s *Store) OSState() store.OSStateStore { return s.osState }

// Identity returns the IdentityStore. See store.Store.
func (s *Store) Identity() store.IdentityStore { return s.identity }

// AuthorizedKeys returns the AuthorizedKeyStore. See store.Store.
func (s *Store) AuthorizedKeys() store.AuthorizedKeyStore { return s.authKeys }

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
		// Singleton row of capsule identity (UUIDv4 + adoption state).
		// Seeded by capsuled on first boot if absent.
		`CREATE TABLE IF NOT EXISTS capsule_identity (
			singleton          INTEGER PRIMARY KEY CHECK (singleton = 1),
			capsule_id         TEXT NOT NULL,
			short_id           TEXT,
			created_at_unix    INTEGER NOT NULL DEFAULT (strftime('%s','now')),
			adopted_at_unix    INTEGER,
			adopted_by_kid     TEXT
		);`,
		// Operator public keys allowed to mint JWTs for this capsule.
		// kid is base64url(sha256(pubkey)) — a stable fingerprint.
		`CREATE TABLE IF NOT EXISTS authorized_keys (
			kid             TEXT PRIMARY KEY,
			pubkey          BLOB NOT NULL,
			name            TEXT NOT NULL,
			added_by_kid    TEXT,
			added_at_unix   INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("sqlite migrate: %w", err)
		}
	}
	// Additive column migrations for existing databases. CREATE TABLE IF
	// NOT EXISTS is a no-op once the table exists, so columns added after
	// the initial schema need a separate ALTER TABLE step. Errors that
	// look like "duplicate column" are swallowed so re-runs are safe.
	if err := addColumnIfMissing(db, "capsule_identity", "short_id", "TEXT"); err != nil {
		return fmt.Errorf("sqlite migrate short_id: %w", err)
	}
	return nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN, treating "duplicate
// column" as success. Both modernc.org/sqlite and the SQLite shell return
// the same message, so a substring match is portable here.
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s;`, table, column, decl)
	if _, err := db.Exec(stmt); err != nil {
		if strings.Contains(err.Error(), "duplicate column") {
			return nil
		}
		return err
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

type identityStore struct{ db *sql.DB }

func (s *identityStore) Get(ctx context.Context) (*store.CapsuleIdentity, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT capsule_id, short_id, created_at_unix, adopted_at_unix, adopted_by_kid
FROM capsule_identity WHERE singleton = 1;`)
	var (
		id        store.CapsuleIdentity
		shortID   sql.NullString
		adoptedAt sql.NullInt64
		adoptedBy sql.NullString
	)
	if err := row.Scan(&id.CapsuleID, &shortID, &id.CreatedAtUnix, &adoptedAt, &adoptedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if shortID.Valid {
		id.ShortID = shortID.String
	}
	if adoptedAt.Valid {
		id.AdoptedAtUnix = adoptedAt.Int64
	}
	if adoptedBy.Valid {
		id.AdoptedByKid = adoptedBy.String
	}
	return &id, nil
}

func (s *identityStore) Put(ctx context.Context, id *store.CapsuleIdentity) error {
	var shortID, adoptedAt, adoptedBy any
	if id.ShortID != "" {
		shortID = id.ShortID
	}
	if id.AdoptedAtUnix != 0 {
		adoptedAt = id.AdoptedAtUnix
	}
	if id.AdoptedByKid != "" {
		adoptedBy = id.AdoptedByKid
	}
	createdAt := id.CreatedAtUnix
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO capsule_identity(singleton, capsule_id, short_id, created_at_unix, adopted_at_unix, adopted_by_kid)
VALUES(1, ?, ?, ?, ?, ?)
ON CONFLICT(singleton) DO UPDATE SET
    capsule_id      = excluded.capsule_id,
    short_id        = excluded.short_id,
    created_at_unix = excluded.created_at_unix,
    adopted_at_unix = excluded.adopted_at_unix,
    adopted_by_kid  = excluded.adopted_by_kid;
`, id.CapsuleID, shortID, createdAt, adoptedAt, adoptedBy)
	return err
}

type authKeyStore struct{ db *sql.DB }

func (s *authKeyStore) Add(ctx context.Context, k *store.AuthorizedKey) error {
	var addedBy any
	if k.AddedByKid != "" {
		addedBy = k.AddedByKid
	}
	addedAt := k.AddedAtUnix
	if addedAt == 0 {
		addedAt = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO authorized_keys(kid, pubkey, name, added_by_kid, added_at_unix)
VALUES(?, ?, ?, ?, ?);
`, k.Kid, k.Pubkey, k.Name, addedBy, addedAt)
	if err != nil {
		// modernc.org/sqlite returns the constraint error as a generic
		// error string; the safest portable detection is on the message.
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint") {
			return store.ErrConflict
		}
		return err
	}
	return nil
}

func (s *authKeyStore) Get(ctx context.Context, kid string) (*store.AuthorizedKey, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT kid, pubkey, name, added_by_kid, added_at_unix
FROM authorized_keys WHERE kid = ?;`, kid)
	var (
		k       store.AuthorizedKey
		addedBy sql.NullString
	)
	if err := row.Scan(&k.Kid, &k.Pubkey, &k.Name, &addedBy, &k.AddedAtUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if addedBy.Valid {
		k.AddedByKid = addedBy.String
	}
	return &k, nil
}

func (s *authKeyStore) List(ctx context.Context) ([]*store.AuthorizedKey, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT kid, pubkey, name, added_by_kid, added_at_unix
FROM authorized_keys ORDER BY added_at_unix, kid;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.AuthorizedKey
	for rows.Next() {
		var (
			k       store.AuthorizedKey
			addedBy sql.NullString
		)
		if err := rows.Scan(&k.Kid, &k.Pubkey, &k.Name, &addedBy, &k.AddedAtUnix); err != nil {
			return nil, err
		}
		if addedBy.Valid {
			k.AddedByKid = addedBy.String
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

func (s *authKeyStore) Delete(ctx context.Context, kid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM authorized_keys WHERE kid = ?;`, kid)
	return err
}

func (s *authKeyStore) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM authorized_keys;`).Scan(&n)
	return n, err
}

func (s *authKeyStore) DeleteAll(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM authorized_keys;`)
	return err
}
