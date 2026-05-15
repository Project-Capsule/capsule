package main

import (
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/geekgonecrazy/capsule/auth"
	"github.com/geekgonecrazy/capsule/core/mdns"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// runInstall implements `capsulectl install`. Drives a fresh install
// against an installer-mode capsule discovered by short_id (via mDNS)
// or by --addr (direct dial).
//
//   capsulectl install <short-id|capsule-XXXX> [--target /dev/X]
//                       [--name nuc-1] [--no-seal] [--addr host:port]
//                       [--yes] [--timeout 5s]
//
// On success, writes a context entry pointing at the future disk
// identity (capsule_id + fingerprint) and tells the operator how to
// finish the install (pull USB, power-cycle).
func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	target := fs.String("target", "", "target disk override (must be one of the installer's candidates)")
	name := fs.String("name", "", "context name for the installed capsule (defaults to short_id)")
	noSeal := fs.Bool("no-seal", false, "do not seal operator pubkey into the new disk")
	addrFlag := fs.String("addr", "", "installer host:port (bypass mDNS resolution)")
	autoYes := fs.Bool("yes", false, "skip the interactive fingerprint confirmation")
	browseTimeout := fs.Duration("timeout", 5*time.Second, "how long to wait for the installer to appear on mDNS")
	_ = fs.Parse(args)

	posArgs := fs.Args()
	if *addrFlag == "" && len(posArgs) < 1 {
		return fmt.Errorf("install requires <short-id> or --addr host:port")
	}

	// --- 1. resolve installer address ---
	installerAddr, shortHint, err := resolveInstaller(posArgs, *addrFlag, *browseTimeout)
	if err != nil {
		return err
	}

	// --- 2. capture fingerprint via raw TLS handshake ---
	wireFingerprint, err := captureLeafFingerprint(installerAddr)
	if err != nil {
		return fmt.Errorf("capture fingerprint from %s: %w", installerAddr, err)
	}

	// --- 3. confirm with operator (HDMI comparison) ---
	if !*autoYes {
		fmt.Println("\n  Installer TLS fingerprint (sha256):")
		fmt.Println()
		fmt.Println(indent("    ", auth.FormatFingerprint(wireFingerprint)))
		fmt.Println()
		fmt.Println("  Verify this matches the HDMI banner on the target machine.")
		fmt.Println("  Type \"yes\" to continue; anything else aborts.")
		fmt.Print("  > ")
		var got string
		if _, err := fmt.Scanln(&got); err != nil {
			return fmt.Errorf("confirmation cancelled")
		}
		if strings.ToLower(strings.TrimSpace(got)) != "yes" {
			return fmt.Errorf("confirmation declined")
		}
	}

	// --- 4. dial installer pinned ---
	conn, err := dialPinnedForAdopt(installerAddr, wireFingerprint)
	if err != nil {
		return fmt.Errorf("dial %s: %w", installerAddr, err)
	}
	defer conn.Close()
	client := capsulev1.NewInstallServiceClient(conn)

	// --- 5. confirm Status (avoid wiping a runtime capsule by accident) ---
	statusCtx, statusCancel := context.WithTimeout(context.Background(), 15*time.Second)
	statusResp, err := client.Status(statusCtx, &capsulev1.InstallStatusRequest{})
	statusCancel()
	if err != nil {
		return fmt.Errorf("install Status: %w (is this an installer-mode capsule?)", err)
	}
	if *target == "" {
		*target = statusResp.GetTargetDisk()
	} else {
		ok := false
		for _, t := range statusResp.GetTargets() {
			if t == *target {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("target %s not in installer candidates: %s", *target, strings.Join(statusResp.GetTargets(), ", "))
		}
	}

	// --- 6. generate operator keypair ---
	contextName := *name
	if contextName == "" {
		if statusResp.GetShortId() != "" {
			contextName = statusResp.GetShortId()
		} else if shortHint != "" {
			contextName = shortHint
		} else {
			contextName = defaultContextName(installerAddr)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if existing, ok := cfg.Contexts[contextName]; ok {
		return fmt.Errorf("context %q already exists (addr=%s); pick another --name or `capsulectl context rm %s`",
			contextName, existing.Addr, contextName)
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "keys", contextName+".ed25519")
	priv, err := generatePrivateKey(keyPath)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)

	// --- 7. stream Install ---
	installCtx, installCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer installCancel()
	stream, err := client.Install(installCtx, &capsulev1.InstallRequest{
		TargetDisk:     *target,
		OperatorPubkey: pub,
		OperatorName:   contextName,
		Seal:           !*noSeal,
	})
	if err != nil {
		_ = os.Remove(keyPath)
		return fmt.Errorf("install rpc: %w", err)
	}
	fmt.Printf("\nInstalling to %s on %s …\n\n", *target, installerAddr)
	var result *capsulev1.InstallResult
	currentPhase := ""
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = os.Remove(keyPath)
			return fmt.Errorf("install stream: %w", err)
		}
		if msg.GetPhase() != currentPhase {
			currentPhase = msg.GetPhase()
			fmt.Printf("\n  [%s] ", currentPhase)
		}
		// One-line moving progress within a phase.
		fmt.Printf("\r  [%s] %3d%%  %s", currentPhase, msg.GetPercent(), msg.GetMessage())
		if msg.GetResult() != nil {
			result = msg.GetResult()
			fmt.Println()
		}
	}
	if result == nil {
		_ = os.Remove(keyPath)
		return fmt.Errorf("install completed without an InstallResult")
	}

	// --- 8. save context entry pointing at the future disk identity ---
	// DHCP lease on the same MAC typically keeps the same IP after the
	// disk boots, so reusing installerAddr is the right default. The
	// operator can correct it via `capsulectl discover` after reboot.
	cfg.Contexts[contextName] = Context{
		Addr:           installerAddr,
		CapsuleID:      result.GetCapsuleId(),
		TLSFingerprint: result.GetTlsFingerprintSha256(),
		KeyPath:        keyPath,
	}
	cfg.Current = contextName
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	// --- 9. next steps ---
	fmt.Println()
	fmt.Printf("  ✓ install complete — context %q saved\n", contextName)
	fmt.Printf("    capsule_id  : %s\n", result.GetCapsuleId())
	fmt.Printf("    short_id    : %s\n", result.GetShortId())
	fmt.Printf("    fingerprint : %s\n", result.GetTlsFingerprintSha256())
	fmt.Println()
	fmt.Println("  Next:")
	fmt.Println("    1. Remove the USB from the target machine.")
	fmt.Println("    2. Power-cycle.")
	fmt.Printf("    3. Verify: capsulectl --capsule %s capsule info\n", contextName)
	fmt.Println("       (if the IP changed, run `capsulectl discover` to see the new one.)")
	return nil
}

// resolveInstaller takes the CLI's positional args + --addr and returns
// the addr:port to dial and a best-effort short_id hint (used to seed
// the default context name when the operator didn't pass --name).
//
//   capsulectl install capsule-a3f2        → mDNS browse, match on short_id
//   capsulectl install --addr 192.168.1.1:50000   → direct
//   capsulectl install capsule-a3f2 --addr X:50000  → direct, but uses hint
func resolveInstaller(posArgs []string, addrFlag string, browseTimeout time.Duration) (addr, shortHint string, err error) {
	if addrFlag != "" {
		if len(posArgs) > 0 {
			shortHint = posArgs[0]
		}
		return addrFlag, shortHint, nil
	}
	hint := posArgs[0]
	ctx, cancel := context.WithTimeout(context.Background(), browseTimeout+5*time.Second)
	defer cancel()
	entries, err := mdns.Browse(ctx, browseTimeout)
	if err != nil {
		return "", hint, fmt.Errorf("mdns browse: %w (try --addr <ip:port>)", err)
	}
	for _, e := range entries {
		if !e.IsInstaller() {
			continue
		}
		if e.ShortID == hint || e.Hostname == hint || e.Instance == hint {
			if e.Addr == "" {
				return "", hint, fmt.Errorf("found installer %q but it has no advertised address", hint)
			}
			return e.Addr, e.ShortID, nil
		}
	}
	return "", hint, fmt.Errorf("no installer named %q on the LAN (try --addr <ip:port>, or check `capsulectl discover --installers`)", hint)
}
