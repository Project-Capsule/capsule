package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// LoadOrGenerate returns the TLS keypair at certPath/keyPath, creating
// a fresh self-signed Ed25519 cert on first call. The cert is intended
// to be pinned by SHA-256 of its DER form (fingerprint pinning), so
// validity is set to 100 years — it never needs to rotate, and the
// pinning model doesn't care.
//
// Both files are written with mode 0600. The directory is expected to
// exist (capsuled creates /perm/tls during ensurePermDirs).
func LoadOrGenerate(certPath, keyPath, capsuleID string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		return cert, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		// Distinguish "file exists but unreadable / corrupt" from "fresh
		// install" so we don't silently overwrite valid material.
		if _, statErr := os.Stat(certPath); statErr == nil {
			return tls.Certificate{}, fmt.Errorf("auth: load cert: %w", err)
		}
	}
	return generate(certPath, keyPath, capsuleID)
}

func generate(certPath, keyPath, capsuleID string) (tls.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("auth: gen ed25519: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("auth: gen serial: %w", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: capsuleID},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(100 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"capsule", "localhost", capsuleID},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("auth: x509 create: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("auth: marshal key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := writeAtomic(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeAtomic(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("auth: parse fresh keypair: %w", err)
	}
	return cert, nil
}

// GenerateCertPEM mints a fresh self-signed Ed25519 cert and returns
// it as PEM-encoded cert + key blobs along with the leaf fingerprint.
// Used by the installer to pre-generate the disk-booted capsule's TLS
// material so the operator's context entry can pin a known fingerprint
// before the disk ever boots. Nothing is written to disk here.
func GenerateCertPEM(capsuleID string) (certPEM, keyPEM []byte, fingerprint string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", fmt.Errorf("auth: gen ed25519: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, "", fmt.Errorf("auth: gen serial: %w", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: capsuleID},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(100 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"capsule", "localhost", capsuleID},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, pub, priv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("auth: x509 create: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, "", fmt.Errorf("auth: marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	fingerprint = FingerprintRaw(der)
	return certPEM, keyPEM, fingerprint, nil
}

// LeafFingerprint extracts the SHA-256 hex fingerprint of the first
// certificate in a tls.Certificate. Caller-friendly wrapper around
// FingerprintRaw.
func LeafFingerprint(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", errors.New("auth: empty certificate chain")
	}
	return FingerprintRaw(cert.Certificate[0]), nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tls-*.tmp")
	if err != nil {
		return fmt.Errorf("auth: temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("auth: write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("auth: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("auth: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("auth: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
