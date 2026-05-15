package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/geekgonecrazy/capsule/auth"
	"github.com/geekgonecrazy/capsule/store"
)

// FirstBootPath is where the installer drops the bundle. Lives at the
// root of /perm so the boot path can find it before any other state
// has been written.
const FirstBootPath = "/perm/firstboot.json"

// IngestFirstBoot consumes /perm/firstboot.json if present: seeds the
// identity store, enrolls the operator key, materializes the TLS
// keypair onto /perm/tls/, and unlinks the bundle so a second boot
// doesn't try to re-ingest it.
//
// Idempotent on absence — returns nil with no side effects if there's
// no bundle. Idempotent on partial completion: each step is checked
// against the current store state so a crash during ingest doesn't
// wedge the disk on next boot (worst case: claim window opens
// instead).
//
// permDir is normally "/perm"; tests can override.
func IngestFirstBoot(ctx context.Context, st store.Store, permDir string) error {
	path := filepath.Join(permDir, "firstboot.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var b FirstBootBundle
	if err := json.Unmarshal(data, &b); err != nil {
		// Corrupt bundle: rename it aside so the operator can
		// inspect, and fall through to the normal first-boot path.
		// Better than wedging the box.
		broken := path + ".broken"
		_ = os.Rename(path, broken)
		slog.Error("firstboot.json corrupt, renamed for inspection",
			"err", err, "moved_to", broken)
		return nil
	}

	if err := applyIdentity(ctx, st, &b); err != nil {
		return err
	}
	if err := applyAuthorizedKey(ctx, st, &b); err != nil {
		return err
	}
	if err := writeTLSMaterial(permDir, &b); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		// Failure here means we'll re-ingest on next boot. The Put /
		// Add paths are idempotent (Add returns ErrConflict on
		// duplicate kid, which we swallow), so that's recoverable.
		slog.Warn("could not remove firstboot.json after ingest", "err", err)
	}
	slog.Info("firstboot bundle ingested",
		"capsule_id", b.CapsuleID,
		"short_id", b.ShortID,
		"operator", b.OperatorName)
	return nil
}

func applyIdentity(ctx context.Context, st store.Store, b *FirstBootBundle) error {
	id, err := st.Identity().Get(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("load identity: %w", err)
	}
	if id == nil {
		id = &store.CapsuleIdentity{}
	}
	// Preserve any existing CreatedAtUnix; otherwise stamp now so the
	// row reflects the install moment rather than the disk's first
	// boot wall clock.
	if id.CreatedAtUnix == 0 {
		id.CreatedAtUnix = time.Now().Unix()
	}
	id.CapsuleID = b.CapsuleID
	id.ShortID = b.ShortID
	return st.Identity().Put(ctx, id)
}

func applyAuthorizedKey(ctx context.Context, st store.Store, b *FirstBootBundle) error {
	if b.OperatorPubkey == "" {
		return fmt.Errorf("firstboot bundle missing operator_pubkey")
	}
	pub, err := base64.StdEncoding.DecodeString(b.OperatorPubkey)
	if err != nil {
		return fmt.Errorf("decode operator_pubkey: %w", err)
	}
	if _, err := auth.ParsePubkey(pub); err != nil {
		return fmt.Errorf("invalid operator_pubkey: %w", err)
	}
	kid := auth.KidFromPubkey(pub)
	name := b.OperatorName
	if name == "" {
		name = "operator"
	}
	if err := st.AuthorizedKeys().Add(ctx, &store.AuthorizedKey{
		Kid:         kid,
		Pubkey:      pub,
		Name:        name,
		AddedAtUnix: time.Now().Unix(),
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Already enrolled — second-boot re-ingest path. Treat as
			// success so the unlink at the end can proceed.
			return nil
		}
		return fmt.Errorf("enroll key: %w", err)
	}
	return nil
}

// writeTLSMaterial drops the cert + key PEMs into /perm/tls/ so the
// disk-booted capsule's auth.LoadOrGenerate finds them on first
// startup. Won't overwrite existing material — re-ingest is a no-op
// if the cert is already there.
func writeTLSMaterial(permDir string, b *FirstBootBundle) error {
	if b.TLSCertPEM == "" || b.TLSKeyPEM == "" {
		return fmt.Errorf("firstboot bundle missing TLS material")
	}
	tlsDir := filepath.Join(permDir, "tls")
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return fmt.Errorf("mkdir tls: %w", err)
	}
	certPath := filepath.Join(tlsDir, "server.crt")
	keyPath := filepath.Join(tlsDir, "server.key")
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			// Already present — skip. Most likely a re-ingest after
			// a crash between bundle unlink and SQLite commit.
			return nil
		}
	}
	if err := os.WriteFile(certPath, []byte(b.TLSCertPEM), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", certPath, err)
	}
	if err := os.WriteFile(keyPath, []byte(b.TLSKeyPEM), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	return nil
}
