package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/geekgonecrazy/capsule/store"
)

// openOldSchema creates a DB with the schema as it existed BEFORE the
// short_id migration. Simulates what a real adopted capsule had on
// disk before the install/install-proposal update was pushed.
func openOldSchema(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE workloads (
			name TEXT PRIMARY KEY,
			spec BLOB NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
		`CREATE TABLE volumes (
			name TEXT PRIMARY KEY,
			host_path TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
		`CREATE TABLE os_state (
			singleton             INTEGER PRIMARY KEY CHECK (singleton = 1),
			active_slot           TEXT NOT NULL,
			pending_slot          TEXT,
			pending_deadline_unix INTEGER,
			last_good_slot        TEXT NOT NULL,
			last_version          TEXT,
			updated_at_unix       INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
		// Pre-short_id schema.
		`CREATE TABLE capsule_identity (
			singleton          INTEGER PRIMARY KEY CHECK (singleton = 1),
			capsule_id         TEXT NOT NULL,
			created_at_unix    INTEGER NOT NULL DEFAULT (strftime('%s','now')),
			adopted_at_unix    INTEGER,
			adopted_by_kid     TEXT
		);`,
		`CREATE TABLE authorized_keys (
			kid             TEXT PRIMARY KEY,
			pubkey          BLOB NOT NULL,
			name            TEXT NOT NULL,
			added_by_kid    TEXT,
			added_at_unix   INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);`,
		`INSERT INTO capsule_identity (singleton, capsule_id, adopted_at_unix, adopted_by_kid)
		 VALUES (1, 'preexisting-uuid-1234', 1700000000, 'op-kid-1');`,
		`INSERT INTO authorized_keys (kid, pubkey, name, added_at_unix)
		 VALUES ('op-kid-1', x'00', 'laptop', 1700000000);`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Re-open via the production path: this is what Open does.
	return nil
}

// TestMigrationUpgradesOldSchema simulates a real upgrade: an existing
// capsule's state.db gets opened by the new sqlite.Open. The migration
// must add short_id without losing rows.
func TestMigrationUpgradesOldSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	openOldSchema(t, path)

	// Now open it the production way. This is what capsuled does on
	// boot — if Open returns an error, capsuled falls back to memory
	// store and loses all adoption state, which is the bug we're
	// trying to prevent.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open after old-schema seed: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	id, err := s.Identity().Get(ctx)
	if err != nil {
		t.Fatalf("Identity.Get: %v", err)
	}
	if id.CapsuleID != "preexisting-uuid-1234" {
		t.Errorf("capsule_id = %q, want %q (data lost!)", id.CapsuleID, "preexisting-uuid-1234")
	}
	if id.AdoptedByKid != "op-kid-1" {
		t.Errorf("adopted_by_kid = %q, want %q", id.AdoptedByKid, "op-kid-1")
	}
	// short_id is empty until ensureIdentity backfills it.
	if id.ShortID != "" {
		t.Errorf("short_id = %q on upgrade, want \"\" (backfilled later)", id.ShortID)
	}
	count, err := s.AuthorizedKeys().Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Errorf("authorized_keys count = %d, want 1 (adoption lost!)", count)
	}
}

// TestMigrationIdempotent: running Open twice on the same DB should
// not fail (the second migrate() pass sees short_id already exists).
func TestMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Identity().Put(context.Background(), &store.CapsuleIdentity{
		CapsuleID: "test-uuid",
		ShortID:   "capsule-test",
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer s2.Close()
	id, err := s2.Identity().Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.ShortID != "capsule-test" {
		t.Errorf("short_id lost across re-open: got %q, want %q", id.ShortID, "capsule-test")
	}
}
