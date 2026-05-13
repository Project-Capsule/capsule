package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return pub, priv
}

func staticLookup(pub ed25519.PublicKey) LookupKey {
	kid := KidFromPubkey(pub)
	return func(k string) (ed25519.PublicKey, bool) {
		if k != kid {
			return nil, false
		}
		return pub, true
	}
}

func TestMintVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKeypair(t)
	tok, err := Mint(priv, "capsule-id-1", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	claims, err := Verify(tok, "capsule-id-1", staticLookup(pub))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Sub != KidFromPubkey(pub) {
		t.Errorf("sub = %q want kid", claims.Sub)
	}
	if claims.Aud != "capsule-id-1" {
		t.Errorf("aud = %q", claims.Aud)
	}
	if claims.Jti == "" {
		t.Errorf("jti empty")
	}
}

func TestVerifyTamperedSignature(t *testing.T) {
	pub, priv := mustKeypair(t)
	tok, _ := Mint(priv, "cap", time.Minute)
	// Decode the signature, flip the first byte, re-encode. Guarantees
	// the signature differs from the genuine one regardless of which
	// 6-bit groups happen to land at the end.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed minted token: %s", tok)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	sig[0] ^= 0xFF
	bad := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(sig)
	if _, err := Verify(bad, "cap", staticLookup(pub)); err == nil {
		t.Fatal("expected error for tampered signature")
	}
}

func TestVerifyWrongAudience(t *testing.T) {
	pub, priv := mustKeypair(t)
	tok, _ := Mint(priv, "cap-A", time.Minute)
	if _, err := Verify(tok, "cap-B", staticLookup(pub)); err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestVerifyExpired(t *testing.T) {
	pub, priv := mustKeypair(t)
	// Hand-craft a token whose iat/exp are far in the past — beyond
	// ClockSkew tolerance — without sleeping. Same encoding path as
	// Mint(), just with controlled timestamps.
	tok := signClaims(t, priv, Claims{
		Sub: KidFromPubkey(pub),
		Aud: "cap",
		Iat: 100, // unix epoch + 100s
		Exp: 200,
		Jti: "expired-test",
	})
	if _, err := Verify(tok, "cap", staticLookup(pub)); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func signClaims(t *testing.T, priv ed25519.PrivateKey, claims Claims) string {
	t.Helper()
	header := jwtHeader{Alg: "EdDSA", Typ: "JWT", Kid: KidFromPubkey(priv.Public().(ed25519.PublicKey))}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifyUnknownKey(t *testing.T) {
	_, priv := mustKeypair(t)
	otherPub, _ := mustKeypair(t)
	tok, _ := Mint(priv, "cap", time.Minute)
	// Lookup only knows about otherPub, not the signing key.
	if _, err := Verify(tok, "cap", staticLookup(otherPub)); err == nil {
		t.Fatal("expected error for unknown kid")
	}
}

func TestJTICacheReplay(t *testing.T) {
	c := newJTICache()
	if c.SeenOrAdd("a", time.Now().Add(time.Minute).Unix()) {
		t.Fatal("first sighting should not be a replay")
	}
	if !c.SeenOrAdd("a", time.Now().Add(time.Minute).Unix()) {
		t.Fatal("second sighting should be flagged as replay")
	}
	if c.SeenOrAdd("b", time.Now().Add(time.Minute).Unix()) {
		t.Fatal("different jti should not be a replay")
	}
}

func TestJTICacheSweep(t *testing.T) {
	c := newJTICache()
	c.SeenOrAdd("expired", 100)  // way in the past
	c.SeenOrAdd("future", 1<<31) // far future
	c.sweep(time.Now().Unix())   // sweeps anything with exp <= now
	// "expired" was swept → re-adding it should NOT trip the replay
	// check (return false meaning "fresh").
	if c.SeenOrAdd("expired", 1<<31) {
		t.Fatal("expected swept jti to be re-addable")
	}
	// "future" was retained → re-adding it SHOULD trip replay (true).
	if !c.SeenOrAdd("future", 1<<31) {
		t.Fatal("expected retained jti to flag as replay")
	}
}

func TestKidFromPubkeyDeterministic(t *testing.T) {
	pub, _ := mustKeypair(t)
	a := KidFromPubkey(pub)
	b := KidFromPubkey(pub)
	if a != b {
		t.Errorf("kid not deterministic: %s vs %s", a, b)
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("kid not base64url-safe: %s", a)
	}
}

