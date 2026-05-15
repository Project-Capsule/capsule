package install

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/geekgonecrazy/capsule/auth"
	"github.com/geekgonecrazy/capsule/store"
	"github.com/geekgonecrazy/capsule/store/memory"
)

func TestIngestFirstBoot_Absent(t *testing.T) {
	st := memory.New()
	dir := t.TempDir()
	if err := IngestFirstBoot(context.Background(), st, dir); err != nil {
		t.Fatalf("ingest with no bundle should be nil: %v", err)
	}
	// Identity must remain unset.
	if _, err := st.Identity().Get(context.Background()); err == nil {
		t.Errorf("identity unexpectedly set; expected ErrNotFound")
	}
}

func TestIngestFirstBoot_Success(t *testing.T) {
	dir := t.TempDir()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	capsuleID := uuid.NewString()
	certPEM, keyPEM, _, err := auth.GenerateCertPEM(capsuleID)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &FirstBootBundle{
		CapsuleID:      capsuleID,
		ShortID:        "capsule-test",
		TLSCertPEM:     string(certPEM),
		TLSKeyPEM:      string(keyPEM),
		OperatorPubkey: base64.StdEncoding.EncodeToString(pub),
		OperatorName:   "nuc-1",
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "firstboot.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	ctx := context.Background()
	if err := IngestFirstBoot(ctx, st, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Identity row populated.
	id, err := st.Identity().Get(ctx)
	if err != nil {
		t.Fatalf("identity get: %v", err)
	}
	if id.CapsuleID != capsuleID {
		t.Errorf("capsule_id = %q, want %q", id.CapsuleID, capsuleID)
	}
	if id.ShortID != "capsule-test" {
		t.Errorf("short_id = %q, want %q", id.ShortID, "capsule-test")
	}
	// Operator key enrolled.
	count, _ := st.AuthorizedKeys().Count(ctx)
	if count != 1 {
		t.Fatalf("expected 1 enrolled key, got %d", count)
	}
	wantKid := auth.KidFromPubkey(pub)
	got, err := st.AuthorizedKeys().Get(ctx, wantKid)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if got.Name != "nuc-1" {
		t.Errorf("key name = %q, want %q", got.Name, "nuc-1")
	}
	// TLS material on disk.
	if _, err := os.Stat(filepath.Join(dir, "tls", "server.crt")); err != nil {
		t.Errorf("missing server.crt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tls", "server.key")); err != nil {
		t.Errorf("missing server.key: %v", err)
	}
	// Bundle unlinked.
	if _, err := os.Stat(filepath.Join(dir, "firstboot.json")); !os.IsNotExist(err) {
		t.Errorf("firstboot.json should be removed after ingest, got err=%v", err)
	}
}

func TestIngestFirstBoot_Idempotent(t *testing.T) {
	dir := t.TempDir()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	capsuleID := uuid.NewString()
	certPEM, keyPEM, _, _ := auth.GenerateCertPEM(capsuleID)
	bundle := &FirstBootBundle{
		CapsuleID:      capsuleID,
		ShortID:        "capsule-idem",
		TLSCertPEM:     string(certPEM),
		TLSKeyPEM:      string(keyPEM),
		OperatorPubkey: base64.StdEncoding.EncodeToString(pub),
		OperatorName:   "nuc-1",
	}
	st := memory.New()
	ctx := context.Background()

	// Simulate a partial-completion scenario: pre-populate identity
	// and key but leave the bundle on disk. A second ingest should
	// not double-enroll the key.
	st.Identity().Put(ctx, &store.CapsuleIdentity{
		CapsuleID: capsuleID,
		ShortID:   "capsule-idem",
	})
	st.AuthorizedKeys().Add(ctx, &store.AuthorizedKey{
		Kid:    auth.KidFromPubkey(pub),
		Pubkey: pub,
		Name:   "nuc-1",
	})

	data, _ := json.Marshal(bundle)
	os.WriteFile(filepath.Join(dir, "firstboot.json"), data, 0o600)
	if err := IngestFirstBoot(ctx, st, dir); err != nil {
		t.Fatalf("second ingest should be idempotent: %v", err)
	}
	count, _ := st.AuthorizedKeys().Count(ctx)
	if count != 1 {
		t.Errorf("expected 1 key (idempotent), got %d", count)
	}
}

func TestIngestFirstBoot_Corrupt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "firstboot.json"), []byte("{not json"), 0o600)
	st := memory.New()
	if err := IngestFirstBoot(context.Background(), st, dir); err != nil {
		t.Fatalf("corrupt bundle should not propagate error: %v", err)
	}
	// Bundle moved aside for inspection, not consumed.
	if _, err := os.Stat(filepath.Join(dir, "firstboot.json.broken")); err != nil {
		t.Errorf("expected firstboot.json.broken, got: %v", err)
	}
	// Identity unset (claim window will open).
	if _, err := st.Identity().Get(context.Background()); err == nil {
		t.Errorf("identity unexpectedly set after corrupt bundle")
	}
}
