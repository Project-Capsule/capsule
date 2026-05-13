package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/geekgonecrazy/capsule/auth"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// runAdopt drives the one-time enrollment ceremony against a fresh
// capsule:
//  1. Generate (or reuse) an Ed25519 keypair under ~/.config/capsule/keys/.
//  2. Dial with InsecureSkipVerify and capture the leaf cert fingerprint
//     seen on the wire.
//  3. Call IdentityService.Adopt with our pubkey.
//  4. Cross-check the fingerprint the server claims (in AdoptResponse)
//     against the one we saw on the wire — a MITM has to lie consistently
//     in two places, and the operator can spot the lie via the HDMI banner.
//  5. Prompt the operator to type the last 6 hex chars of the fingerprint
//     unless --yes was passed.
//  6. Save the new context and set it as current.
func runAdopt(addr string, args []string) error {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	capsuleAddr := fs.String("capsule", "", "host:port of the capsule (overrides global --capsule)")
	name := fs.String("name", "", "context name (defaults to host portion of the capsule address)")
	autoYes := fs.Bool("yes", false, "skip the interactive fingerprint confirmation (NOT recommended)")
	_ = fs.Parse(args)

	target := *capsuleAddr
	if target == "" {
		target = addr
	}
	if target == "" {
		return fmt.Errorf("adopt requires --capsule host:port")
	}

	ctxName := *name
	if ctxName == "" {
		ctxName = defaultContextName(target)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if existing, ok := cfg.Contexts[ctxName]; ok {
		return fmt.Errorf("context %q already exists (addr=%s); pick another --name or `capsulectl context rm %s` first",
			ctxName, existing.Addr, ctxName)
	}

	// Capture the server's TLS leaf fingerprint synchronously *before*
	// any local material is created or any Adopt RPC fires. gRPC's
	// NewClient is lazy (no handshake until first RPC), so a separate
	// raw tls.Dial is the only way to have a real fingerprint to show
	// the operator at confirmation time.
	wireFingerprint, err := captureLeafFingerprint(target)
	if err != nil {
		return fmt.Errorf("capture fingerprint from %s: %w", target, err)
	}

	if !*autoYes {
		fmt.Println("\n  This capsule's TLS fingerprint (sha256):")
		fmt.Println()
		fmt.Println(indent("    ", auth.FormatFingerprint(wireFingerprint)))
		fmt.Println()
		fmt.Println("  If you trust this fingerprint, type \"yes\" to enroll.")
		fmt.Println("  Anything else aborts (claim window untouched).")
		fmt.Print("  > ")
		var got string
		if _, err := fmt.Scanln(&got); err != nil {
			return fmt.Errorf("confirmation cancelled (claim window untouched)")
		}
		if strings.ToLower(strings.TrimSpace(got)) != "yes" {
			return fmt.Errorf("confirmation declined (claim window untouched)")
		}
	}

	dir, err := configDir()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "keys", ctxName+".ed25519")
	priv, err := generatePrivateKey(keyPath)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)

	// Dial again — this time pinning the fingerprint we just captured.
	// MITM with a different cert can't even establish the connection.
	conn, err := dialPinnedForAdopt(target, wireFingerprint)
	if err != nil {
		_ = os.Remove(keyPath)
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer conn.Close()

	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := capsulev1.NewIdentityServiceClient(conn).Adopt(cctx, &capsulev1.AdoptRequest{
		Pubkey: pub,
		Name:   ctxName,
	})
	if err != nil {
		_ = os.Remove(keyPath)
		return fmt.Errorf("adopt rpc: %w", err)
	}

	// Belt-and-suspenders: the server echoes its TLS fingerprint in the
	// response. With pinning above this is redundant, but a mismatch
	// here would mean something *very* wrong (e.g., server bug).
	if !strings.EqualFold(wireFingerprint, resp.GetTlsFingerprintSha256()) {
		_ = os.Remove(keyPath)
		return fmt.Errorf("tls fingerprint mismatch: wire=%s response=%s — refusing to save context",
			wireFingerprint, resp.GetTlsFingerprintSha256())
	}

	cfg.Contexts[ctxName] = Context{
		Addr:           target,
		CapsuleID:      resp.GetCapsuleId(),
		TLSFingerprint: wireFingerprint,
		KeyPath:        keyPath,
	}
	cfg.Current = ctxName
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("\n  ✓ adopted as context %q\n", ctxName)
	fmt.Printf("    capsule_id : %s\n", resp.GetCapsuleId())
	fmt.Printf("    kid        : %s\n", resp.GetKid())
	fmt.Printf("    addr       : %s\n", target)
	fmt.Printf("    key path   : %s\n\n", keyPath)
	return nil
}

func runWhoAmI(addr string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := capsulev1.NewIdentityServiceClient(conn).WhoAmI(cctx, &capsulev1.WhoAmIRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("capsule_id : %s\n", resp.GetCapsuleId())
	fmt.Printf("kid        : %s\n", resp.GetKid())
	if resp.GetName() != "" {
		fmt.Printf("name       : %s\n", resp.GetName())
	}
	return nil
}

// runKeyAdd reads a public key file (PEM, output of `capsulectl key
// show >my.pub` from the other operator) and enrolls it.
func runKeyAdd(addr, pubPath, name string) error {
	pub, err := readPublicKey(pubPath)
	if err != nil {
		return err
	}
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := capsulev1.NewIdentityServiceClient(conn).KeyAdd(cctx, &capsulev1.KeyAddRequest{
		Pubkey: pub,
		Name:   name,
	})
	if err != nil {
		return err
	}
	fmt.Printf("enrolled: %s (%s)\n", resp.GetKey().GetKid(), resp.GetKey().GetName())
	return nil
}

func runKeyList(addr string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := capsulev1.NewIdentityServiceClient(conn).KeyList(cctx, &capsulev1.KeyListRequest{})
	if err != nil {
		return err
	}
	if len(resp.GetKeys()) == 0 {
		fmt.Println("(no keys enrolled)")
		return nil
	}
	for _, k := range resp.GetKeys() {
		added := time.Unix(k.GetAddedAtUnix(), 0).Format(time.RFC3339)
		fmt.Printf("%s  %s  added=%s by=%s\n", k.GetKid(), k.GetName(), added, k.GetAddedByKid())
	}
	return nil
}

func runKeyRemove(addr, kid string) error {
	conn, err := dial(addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := capsulev1.NewIdentityServiceClient(conn).KeyRemove(cctx, &capsulev1.KeyRemoveRequest{Kid: kid}); err != nil {
		return err
	}
	fmt.Printf("removed: %s\n", kid)
	return nil
}

// runKeyShow prints the local context's pubkey + kid in PEM and raw
// hex form so the operator can hand them to someone else for enrollment.
func runKeyShow(addr string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cctx, err := cfg.Resolve(addr)
	if err != nil {
		return fmt.Errorf("no context found for %q (run: capsulectl adopt --capsule <addr>)", addr)
	}
	priv, err := loadPrivateKey(cctx.KeyPath)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	fmt.Printf("# kid: %s\n", auth.KidFromPubkey(pub))
	fmt.Print(string(pemBytes))
	return nil
}

func runContextList() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Contexts) == 0 {
		fmt.Println("(no contexts — run: capsulectl adopt --capsule <addr>)")
		return nil
	}
	for name, c := range cfg.Contexts {
		marker := " "
		if name == cfg.Current {
			marker = "*"
		}
		fmt.Printf("%s %s  addr=%s  capsule_id=%s\n", marker, name, c.Addr, c.CapsuleID)
	}
	return nil
}

func runContextUse(name string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return fmt.Errorf("no such context: %s", name)
	}
	cfg.Current = name
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("current context: %s\n", name)
	return nil
}

func runContextRemove(name string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		return fmt.Errorf("no such context: %s", name)
	}
	delete(cfg.Contexts, name)
	if cfg.Current == name {
		cfg.Current = ""
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	// Best-effort key removal: the operator may want to keep the key.
	// We leave it on disk and just point them at it.
	fmt.Printf("removed context %q (key file %s left on disk)\n", name, c.KeyPath)
	return nil
}

// readPublicKey accepts a PEM-encoded SubjectPublicKeyInfo (output of
// `capsulectl key show`) and returns the raw 32-byte Ed25519 pubkey.
func readPublicKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: parse: %w", path, err)
	}
	pub, ok := pubAny.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 public key", path)
	}
	return pub, nil
}

// defaultContextName turns a "192.168.1.50:50000" into "192.168.1.50".
// Used when --name is not specified at adopt time.
func defaultContextName(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func indent(prefix, s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
