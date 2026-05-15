// Package mdns publishes a Capsule's identity over multicast DNS so
// operators can discover it from `capsulectl discover` without prior
// knowledge of IP addresses.
//
// Two announcement shapes share the same service name (`_capsule._tcp`):
//
//   - Runtime (mode=runtime, or no mode TXT): a normal Capsule node.
//     Carries capsule_id, short_id, adopted flag, and version.
//   - Installer (mode=installer): a Capsule booted from removable media
//     with a viable internal install target. Carries the same identity
//     fields plus the candidate target disk(s) so `capsulectl install`
//     can drive the flash without the operator having to look up the
//     target manually.
//
// The announcer is best-effort. mDNS-blocked LANs degrade silently to
// the address-based fallback (`capsulectl adopt --capsule <ip>`, or
// `capsulectl install --addr <ip>`).
package mdns

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/grandcat/zeroconf"
)

// service is the DNS-SD service type for Capsule. Browsers query
// `_capsule._tcp.local.` to find every capsule on the LAN.
const service = "_capsule._tcp"

// Announcement is the dynamic state surfaced over mDNS. Pass a value to
// Start() to register the service; call Update() to change TXT records
// (e.g., adopted=true after the first key is enrolled).
type Announcement struct {
	// CapsuleID is the stable UUID used as JWT audience and the unique
	// identifier in `discover` output. Always set.
	CapsuleID string
	// ShortID is the human-memorable handle ("capsule-a3f2"). Used as
	// the mDNS instance name unless Hostname is non-empty.
	ShortID string
	// Hostname is the operator-set friendly name. Empty until the
	// operator runs `capsulectl capsule set-hostname` (proposed; not
	// yet wired). When set, becomes the mDNS instance name.
	Hostname string
	// Adopted is true once at least one operator pubkey is enrolled.
	// Drives the ADOPTED column in `capsulectl discover`.
	Adopted bool
	// Version is the build identifier baked into the capsuled binary.
	Version string
	// Mode is "runtime" for an installed capsule, "installer" for one
	// running from removable media with an internal install target.
	// Empty is treated as runtime by discover.
	Mode string
	// TargetDisk is the auto-selected internal target ("/dev/nvme0n1").
	// Installer mode only; ignored for runtime.
	TargetDisk string
	// TargetSizeBytes is the byte size of TargetDisk. Used by discover
	// to render a SIZE column without re-probing the machine.
	TargetSizeBytes uint64
	// Targets is the full set of candidate install disks (e.g.
	// "/dev/nvme0n1", "/dev/sda"). Installer mode only.
	Targets []string
}

// instanceName picks the DNS-SD instance name. Operator-set hostname
// wins; otherwise the short_id. The instance name is what the operator
// sees in `discover` output before any other label.
func (a Announcement) instanceName() string {
	if a.Hostname != "" {
		return a.Hostname
	}
	return a.ShortID
}

// text builds the TXT record set in DNS-SD `key=value` form. The order
// is deterministic so re-publishing the same Announcement produces the
// same bytes.
func (a Announcement) text() []string {
	mode := a.Mode
	if mode == "" {
		mode = "runtime"
	}
	out := []string{
		"capsule_id=" + a.CapsuleID,
		"short_id=" + a.ShortID,
		"adopted=" + boolStr(a.Adopted),
		"version=" + a.Version,
		"mode=" + mode,
	}
	if a.Hostname != "" {
		out = append(out, "hostname="+a.Hostname)
	}
	if mode == "installer" {
		if a.TargetDisk != "" {
			out = append(out, "target_disk="+a.TargetDisk)
		}
		if a.TargetSizeBytes > 0 {
			out = append(out, fmt.Sprintf("target_size_bytes=%d", a.TargetSizeBytes))
		}
		if len(a.Targets) > 0 {
			ts := append([]string(nil), a.Targets...)
			sort.Strings(ts)
			out = append(out, "targets="+strings.Join(ts, ","))
		}
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// Announcer publishes a Capsule's identity over mDNS for the lifetime
// of capsuled. It re-publishes when the announcement changes; the
// underlying mDNS library handles TXT-record updates without going off
// the air.
type Announcer struct {
	port int

	mu      sync.Mutex
	server  *zeroconf.Server
	current Announcement
}

// New returns an Announcer for the given gRPC listen port. Call Start
// once the network is up and the identity has been resolved.
func New(port int) *Announcer {
	return &Announcer{port: port}
}

// Start registers the service with the announcement. Subsequent
// changes go through Update.
//
// ifaces == nil means "announce on every interface zeroconf can find,"
// which is what we want today — capsules only have one uplink. If/when
// fabric brings up an extra interface we'll want to scope this to the
// uplink only so the fabric never accidentally advertises capsules to
// the wider mesh.
func (a *Announcer) Start(ctx context.Context, ann Announcement) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return fmt.Errorf("mdns: already started")
	}
	srv, err := zeroconf.Register(ann.instanceName(), service, "local.", a.port, ann.text(), nil)
	if err != nil {
		return fmt.Errorf("mdns register: %w", err)
	}
	a.server = srv
	a.current = ann
	slog.Info("mDNS announcement registered",
		"instance", ann.instanceName(),
		"service", service,
		"port", a.port,
		"mode", ann.Mode,
		"adopted", ann.Adopted)

	// On ctx cancel, tear the server down. capsuled never returns from
	// Serve so the announcer effectively runs forever; this is the
	// belt-and-suspenders cleanup path.
	go func() {
		<-ctx.Done()
		a.Stop()
	}()
	return nil
}

// Update changes the announcement. If only the TXT records changed,
// SetText is used (cheap, no re-probe). If the instance name (i.e.
// the operator's hostname) changed, the server is torn down and
// re-registered so the new label takes effect.
func (a *Announcer) Update(ctx context.Context, ann Announcement) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server == nil {
		// Not yet started. Lazy-start so callers don't have to track
		// "started or not" themselves.
		srv, err := zeroconf.Register(ann.instanceName(), service, "local.", a.port, ann.text(), nil)
		if err != nil {
			return fmt.Errorf("mdns register: %w", err)
		}
		a.server = srv
		a.current = ann
		return nil
	}
	if ann.instanceName() == a.current.instanceName() {
		a.server.SetText(ann.text())
		a.current = ann
		return nil
	}
	// Instance name changed — must re-register.
	a.server.Shutdown()
	srv, err := zeroconf.Register(ann.instanceName(), service, "local.", a.port, ann.text(), nil)
	if err != nil {
		a.server = nil
		return fmt.Errorf("mdns re-register: %w", err)
	}
	a.server = srv
	a.current = ann
	slog.Info("mDNS announcement re-registered",
		"instance", ann.instanceName(),
		"mode", ann.Mode)
	return nil
}

// MarkAdopted flips the adopted TXT key to "true" and re-publishes.
// Idempotent if already adopted. Called by IdentityController after a
// successful Adopt RPC so `capsulectl discover` reflects the change
// without waiting for a poll.
func (a *Announcer) MarkAdopted(ctx context.Context) error {
	a.mu.Lock()
	if a.server == nil {
		a.mu.Unlock()
		return nil
	}
	if a.current.Adopted {
		a.mu.Unlock()
		return nil
	}
	next := a.current
	next.Adopted = true
	a.mu.Unlock()
	return a.Update(ctx, next)
}

// Stop unregisters the service. Safe to call from any goroutine; safe
// to call when the announcer was never started.
func (a *Announcer) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server == nil {
		return
	}
	a.server.Shutdown()
	a.server = nil
}
