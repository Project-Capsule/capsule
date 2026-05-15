package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/geekgonecrazy/capsule/core/mdns"
)

// discoverRow is one capsule found by `discover`, enriched with the
// fingerprint we fetched directly via TLS. mDNS provides identity TXT
// records; the fingerprint comes from a real handshake against the
// announced address.
type discoverRow struct {
	mdns.Entry
	Fingerprint string
	Unreachable bool
}

// runDiscover browses `_capsule._tcp.local.` and prints a table of every
// capsule that responded, separated into PENDING INSTALL (installer-mode
// nodes waiting to be flashed) and CAPSULES (runtime nodes — adopted or
// claim-window-open).
//
// Fingerprints are fetched directly via a raw tls.Dial to each
// discovered address, not taken from mDNS, so they're trustworthy under
// the same assumptions as `capsulectl adopt` today.
func runDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	timeout := fs.Duration("timeout", 3*time.Second, "how long to listen for mDNS responses")
	jsonOut := fs.Bool("json", false, "emit a JSON array instead of a table")
	installers := fs.Bool("installers", false, "show only installer-mode capsules")
	unadopted := fs.Bool("unadopted", false, "show only runtime capsules with no enrolled keys")
	_ = fs.Parse(args)

	if *installers && *unadopted {
		return fmt.Errorf("--installers and --unadopted are mutually exclusive")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
	defer cancel()

	entries, err := mdns.Browse(ctx, *timeout)
	if err != nil {
		return fmt.Errorf("browse: %w", err)
	}

	rows := enrichEntries(entries)

	// Filter.
	filtered := rows[:0]
	for _, r := range rows {
		switch {
		case *installers && !r.IsInstaller():
			continue
		case *unadopted && (r.IsInstaller() || r.Adopted):
			continue
		}
		filtered = append(filtered, r)
	}

	// Sort: installers first (so PENDING INSTALL is at the top of the
	// table), then by short_id within each group for deterministic
	// output.
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].IsInstaller() != filtered[j].IsInstaller() {
			return filtered[i].IsInstaller()
		}
		return filtered[i].ShortID < filtered[j].ShortID
	})

	if *jsonOut {
		return emitJSON(filtered)
	}
	return emitTable(filtered)
}

// enrichEntries fetches a TLS fingerprint for each discovered entry
// in parallel. Entries with an unreachable address get marked rather
// than dropped so the operator sees they exist.
func enrichEntries(entries []mdns.Entry) []discoverRow {
	rows := make([]discoverRow, len(entries))
	type result struct {
		idx int
		fp  string
		ok  bool
	}
	results := make(chan result, len(entries))
	pending := 0
	for i, e := range entries {
		rows[i] = discoverRow{Entry: e}
		if e.Addr == "" {
			rows[i].Unreachable = true
			continue
		}
		pending++
		go func(idx int, addr string) {
			fp, err := captureLeafFingerprint(addr)
			results <- result{idx: idx, fp: fp, ok: err == nil}
		}(i, e.Addr)
	}
	for ; pending > 0; pending-- {
		r := <-results
		if r.ok {
			rows[r.idx].Fingerprint = r.fp
		} else {
			rows[r.idx].Unreachable = true
		}
	}
	return rows
}

func emitJSON(rows []discoverRow) error {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		mode := r.Mode
		if mode == "" {
			mode = "runtime"
		}
		m := map[string]any{
			"instance":    r.Instance,
			"addr":        r.Addr,
			"capsule_id":  r.CapsuleID,
			"short_id":    r.ShortID,
			"hostname":    r.Hostname,
			"adopted":     r.Adopted,
			"version":     r.Version,
			"mode":        mode,
			"fingerprint": r.Fingerprint,
			"unreachable": r.Unreachable,
		}
		if r.IsInstaller() {
			m["target_disk"] = r.TargetDisk
			m["target_size_bytes"] = r.TargetSizeBytes
			m["targets"] = r.Targets
		}
		out = append(out, m)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func emitTable(rows []discoverRow) error {
	if len(rows) == 0 {
		fmt.Println("(no capsules found — try --timeout 10s if the LAN is slow, or check that mDNS isn't blocked)")
		return nil
	}

	var installerRows, runtimeRows []discoverRow
	for _, r := range rows {
		if r.IsInstaller() {
			installerRows = append(installerRows, r)
		} else {
			runtimeRows = append(runtimeRows, r)
		}
	}

	cfg, _ := loadConfig() // best-effort context lookup
	knownByCapsuleID := map[string]string{}
	if cfg != nil {
		for name, c := range cfg.Contexts {
			if c.CapsuleID != "" {
				knownByCapsuleID[c.CapsuleID] = name
			}
		}
	}

	if len(installerRows) > 0 {
		fmt.Println("PENDING INSTALL")
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tADDRESS\tTARGET\tSIZE\tFINGERPRINT")
		for _, r := range installerRows {
			target := r.TargetDisk
			if len(r.Targets) > 1 {
				target += fmt.Sprintf(" (+%d more)", len(r.Targets)-1)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				r.label()+" (installer)",
				r.addrOrMark(),
				target,
				humanBytes(r.TargetSizeBytes),
				shortFingerprint(r.Fingerprint),
			)
		}
		_ = tw.Flush()
		if len(runtimeRows) > 0 {
			fmt.Println()
		}
	}

	if len(runtimeRows) > 0 {
		if len(installerRows) > 0 {
			fmt.Println("CAPSULES")
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tADDRESS\tFINGERPRINT\tADOPTED")
		for _, r := range runtimeRows {
			adopted := "no"
			if r.Adopted {
				if ctxName, ok := knownByCapsuleID[r.CapsuleID]; ok {
					adopted = "yes (context: " + ctxName + ")"
				} else {
					adopted = "yes"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				r.label(),
				r.addrOrMark(),
				shortFingerprint(r.Fingerprint),
				adopted,
			)
		}
		_ = tw.Flush()
	}
	return nil
}

func (r discoverRow) label() string {
	if r.Hostname != "" {
		return r.Hostname
	}
	if r.ShortID != "" {
		return r.ShortID
	}
	return r.Instance
}

func (r discoverRow) addrOrMark() string {
	if r.Addr == "" {
		return "(no address)"
	}
	if r.Unreachable {
		return r.Addr + " (unreachable)"
	}
	return r.Addr
}

// shortFingerprint trims the SHA-256 hex digest to the first 8 bytes
// (16 hex chars) with colons. Operators verify the full string in
// `capsulectl install` / `adopt`; the table view just needs to be
// distinguishable at a glance.
func shortFingerprint(fp string) string {
	if fp == "" {
		return "-"
	}
	const wantHex = 16
	if len(fp) <= wantHex {
		return fp
	}
	out := make([]byte, 0, wantHex+wantHex/2+1)
	for i := 0; i < wantHex; i += 2 {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, fp[i], fp[i+1])
	}
	out = append(out, '.', '.', '.')
	return string(out)
}
