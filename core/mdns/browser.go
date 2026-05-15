package mdns

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

// Entry is one capsule found by Browse. Fields mirror the
// Announcement TXT keys plus the resolved network address.
type Entry struct {
	Instance        string // mDNS instance name (operator hostname or short_id)
	Addr            string // "ip:port" — first usable IPv4 from the SRV/A pair
	CapsuleID       string
	ShortID         string
	Hostname        string
	Adopted         bool
	Version         string
	Mode            string   // "runtime" or "installer"
	TargetDisk      string   // installer mode only
	TargetSizeBytes uint64   // installer mode only
	Targets         []string // installer mode only
}

// IsInstaller reports whether this capsule is in installer mode (mode TXT
// == "installer"). Treated as "false" when the mode key is absent or
// "runtime" — keeps the browse loop tolerant of older capsules.
func (e Entry) IsInstaller() bool { return e.Mode == "installer" }

// Browse runs an mDNS browse for `_capsule._tcp.local.` for the given
// timeout and returns every entry that responded. Order is by Instance
// name for deterministic display.
//
// This is the only function in the package that requires network access
// on the caller side; runs from `capsulectl discover` and from
// `capsulectl install` when resolving a short_id to an address.
func Browse(ctx context.Context, timeout time.Duration) ([]Entry, error) {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, fmt.Errorf("mdns resolver: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make(chan *zeroconf.ServiceEntry, 16)
	if err := resolver.Browse(ctx, service, "local.", results); err != nil {
		return nil, fmt.Errorf("mdns browse: %w", err)
	}

	out := []Entry{}
	for entry := range results {
		e := fromServiceEntry(entry)
		if e.CapsuleID == "" {
			// Service responded but didn't include our TXT records —
			// not a capsule we recognize. Skip.
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func fromServiceEntry(s *zeroconf.ServiceEntry) Entry {
	e := Entry{
		Instance: s.Instance,
	}
	// Prefer IPv4. macOS sometimes returns the IPv6 only; we accept it
	// as a fallback but the discover output works better with v4.
	ip := pickIPv4(s.AddrIPv4)
	if ip == nil {
		ip = pickIPv6(s.AddrIPv6)
	}
	if ip != nil {
		e.Addr = net.JoinHostPort(ip.String(), strconv.Itoa(s.Port))
	}
	for _, kv := range s.Text {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "capsule_id":
			e.CapsuleID = v
		case "short_id":
			e.ShortID = v
		case "hostname":
			e.Hostname = v
		case "adopted":
			e.Adopted = v == "true"
		case "version":
			e.Version = v
		case "mode":
			e.Mode = v
		case "target_disk":
			e.TargetDisk = v
		case "target_size_bytes":
			if n, err := strconv.ParseUint(v, 10, 64); err == nil {
				e.TargetSizeBytes = n
			}
		case "targets":
			if v != "" {
				e.Targets = strings.Split(v, ",")
			}
		}
	}
	return e
}

func pickIPv4(addrs []net.IP) net.IP {
	for _, a := range addrs {
		if a4 := a.To4(); a4 != nil {
			return a4
		}
	}
	return nil
}

func pickIPv6(addrs []net.IP) net.IP {
	for _, a := range addrs {
		if a.To4() == nil && !a.IsLinkLocalUnicast() {
			return a
		}
	}
	if len(addrs) > 0 {
		return addrs[0]
	}
	return nil
}
