package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ClockSkew is the allowed deviation between the verifier's clock and
// the iat/exp claims. Real capsules can boot with a 1970 wall clock
// before NTP catches up; this padding (plus the jti replay cache) makes
// short-lived JWTs survive the gap without losing replay protection.
const ClockSkew = 5 * time.Minute

// DefaultLifetime is how long a freshly minted JWT is valid for. Kept
// short so that a token captured off the wire is useless quickly even
// if the jti cache is bypassed (e.g. across a server restart).
const DefaultLifetime = 60 * time.Second

// Claims is capsule's JWT body. We use a closed schema (not a generic
// map) so changes show up as compile errors rather than silent drift.
type Claims struct {
	// Sub is the kid of the signing key — same value as the JWT header's
	// kid. Carried in both places so verifiers don't have to choose.
	Sub string `json:"sub"`
	// Aud is the capsule_id this token is valid against. A token minted
	// for capsule A must not work on capsule B.
	Aud string `json:"aud"`
	// Iat / Exp are Unix seconds. Verify enforces both with ClockSkew.
	Iat int64 `json:"iat"`
	Exp int64 `json:"exp"`
	// Jti is 16 random bytes (base64url, no padding). Used by the
	// server-side seen-cache to collapse the replay window to "first
	// acceptance wins".
	Jti string `json:"jti"`
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Mint signs a JWT for the given audience using priv. Returns the
// compact serialization (header.payload.signature, all base64url, no
// padding). A fresh jti is generated; iat/exp are derived from now.
func Mint(priv ed25519.PrivateKey, audience string, lifetime time.Duration) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("auth: invalid ed25519 private key length")
	}
	if lifetime <= 0 {
		lifetime = DefaultLifetime
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return "", errors.New("auth: failed to derive public key from private")
	}
	kid := KidFromPubkey(pub)
	now := time.Now().Unix()
	jti, err := randomJTI()
	if err != nil {
		return "", err
	}
	header := jwtHeader{Alg: "EdDSA", Typ: "JWT", Kid: kid}
	claims := Claims{
		Sub: kid,
		Aud: audience,
		Iat: now,
		Exp: now + int64(lifetime/time.Second),
		Jti: jti,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// LookupKey resolves a kid to its enrolled public key. Returns false if
// the kid is unknown — verification then fails with ErrUnknownKey.
type LookupKey func(kid string) (ed25519.PublicKey, bool)

// Verify parses and validates a JWT against the given audience and key
// lookup. Returns the claims on success. Errors are intentionally
// generic (no leakage of which check failed) — interceptors map all of
// them to gRPC codes.Unauthenticated.
func Verify(token string, audience string, lookup LookupKey) (*Claims, error) {
	parts := splitN(token, '.', 3)
	if len(parts) != 3 {
		return nil, errors.New("auth: malformed token")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("auth: bad header encoding")
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, errors.New("auth: bad header json")
	}
	if hdr.Alg != "EdDSA" || hdr.Typ != "JWT" {
		return nil, errors.New("auth: unsupported algorithm")
	}
	if hdr.Kid == "" {
		return nil, errors.New("auth: missing kid")
	}
	pub, ok := lookup(hdr.Kid)
	if !ok {
		return nil, errors.New("auth: unknown key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("auth: bad signature encoding")
	}
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(pub, []byte(signingInput), sig) {
		return nil, errors.New("auth: bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("auth: bad payload encoding")
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, errors.New("auth: bad payload json")
	}
	if claims.Sub != hdr.Kid {
		return nil, errors.New("auth: sub/kid mismatch")
	}
	if audience != "" && claims.Aud != audience {
		return nil, errors.New("auth: wrong audience")
	}
	now := time.Now().Unix()
	skew := int64(ClockSkew / time.Second)
	if claims.Iat == 0 || claims.Iat > now+skew {
		return nil, errors.New("auth: token not yet valid")
	}
	if claims.Exp == 0 || claims.Exp+skew < now {
		return nil, errors.New("auth: token expired")
	}
	if claims.Jti == "" {
		return nil, errors.New("auth: missing jti")
	}
	return &claims, nil
}

// splitN is a tiny single-pass strings.SplitN that avoids importing
// strings into this file (the file already pulls enough crypto weight).
func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func randomJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: jti rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
