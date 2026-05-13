package auth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// KidFromPubkey returns the canonical key identifier for an Ed25519
// public key: base64url(sha256(pubkey)) without padding. Stable across
// machines, short enough to print, and what JWTs carry in `sub`/`kid`.
func KidFromPubkey(pubkey ed25519.PublicKey) string {
	sum := sha256.Sum256(pubkey)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// FingerprintCert returns the SHA-256 of the leaf certificate's DER
// bytes, lowercase hex (no separator). This is the value pinned by
// capsulectl after adoption.
func FingerprintCert(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// FingerprintRaw returns the SHA-256 hex digest of an arbitrary byte
// slice — the wire form of a leaf cert seen during a TLS handshake.
func FingerprintRaw(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// FormatFingerprint groups a hex fingerprint into colon-separated bytes
// in 8-byte rows for human inspection on the HDMI banner / adoption
// prompt. Returns the input unchanged on any odd-length input.
func FormatFingerprint(hexFingerprint string) string {
	if len(hexFingerprint)%2 != 0 {
		return hexFingerprint
	}
	var (
		out  []byte
		col  int
		rows int
	)
	for i := 0; i < len(hexFingerprint); i += 2 {
		if col == 8 {
			out = append(out, '\n')
			col = 0
			rows++
		} else if col > 0 {
			out = append(out, ':')
		}
		out = append(out, hexFingerprint[i], hexFingerprint[i+1])
		col++
	}
	_ = rows
	return string(out)
}

// ParsePubkey validates a 32-byte Ed25519 public key and returns it
// typed. Returns an error message suitable for surfacing to the operator.
func ParsePubkey(b []byte) (ed25519.PublicKey, error) {
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid ed25519 public key: expected %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(append([]byte(nil), b...)), nil
}
