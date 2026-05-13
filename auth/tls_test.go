package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrGenerateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	cert1, err := LoadOrGenerate(certPath, keyPath, "capsule-test")
	if err != nil {
		t.Fatalf("first generate: %v", err)
	}
	fp1, err := LeafFingerprint(cert1)
	if err != nil {
		t.Fatalf("fingerprint1: %v", err)
	}

	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key perms: %v mode=%v", err, info.Mode().Perm())
	}

	cert2, err := LoadOrGenerate(certPath, keyPath, "capsule-test")
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	fp2, _ := LeafFingerprint(cert2)
	if fp1 != fp2 {
		t.Errorf("fingerprint mismatch on reload: %s vs %s", fp1, fp2)
	}
}
