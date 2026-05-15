# Discovery: finding capsules on the local network (proposal)

> **Status:** Proposal. Not implemented. Independent of all other proposals — no dependencies.
> This addresses the bootstrap UX gap that exists today: to adopt a capsule you must already
> know its IP address, which means reading the HDMI on each machine or pre-configuring DHCP
> leases. With seven machines that's workable; with more it becomes a real obstacle.

## Summary

Today's first-contact flow requires an out-of-band step: boot a capsule, walk to the machine
(or wait for someone else to), read the IP off the HDMI display, then run
`capsulectl adopt --capsule <ip>:50000 --name <name>`. There is no way to answer "what
capsules are on my network right now?" from the operator's laptop.

This proposal adds **mDNS-based discovery**: each capsule announces itself on the local LAN
as a `_capsule._tcp` service. `capsulectl discover` browses those announcements and returns
a table of every capsule visible on the network — name, address, and fingerprint — in a few
seconds. A `--adopt` mode walks through each unadopted machine interactively, eliminating
the need to look up IPs at all.

Two changes make the output useful rather than just a list of indistinguishable `192.168.x.x`
addresses:

1. **Each capsule generates a stable short ID** (`capsule-a3f2`) on first boot, derived from
   its node UUID. This short ID appears in the HDMI banner alongside the IP, and in discover
   output, giving the operator a human-memorable handle to correlate the physical machine
   with the CLI entry.
2. **Fingerprints in discover output are fetched directly** via a raw TLS connection to each
   machine — the same mechanism as `capsulectl adopt`. They are not taken from the mDNS
   announcement (which is unauthenticated). Operators can verify the fingerprint against the
   HDMI display before committing to adoption.

## Goals

- **`capsulectl discover` lists every capsule on the LAN** in seconds, without any prior
  knowledge of IP addresses or hostnames.
- **Fingerprints are fetched directly, not from mDNS**, so the display value is trustworthy
  under the same assumptions as `capsulectl adopt` today.
- **Each capsule has a stable, unique short ID** visible on HDMI and in discover output,
  so operators can correlate physical machines with CLI entries without byte-by-byte
  fingerprint comparison.
- **`capsulectl discover --adopt` is the new happy path** for enrolling a fresh fleet: boot
  all the machines, run one command on your laptop, name each one interactively as it appears.
- **Already-adopted capsules appear in discover output**, marked as such. `capsulectl
  discover` becomes a fleet inventory view, not just a bootstrapping tool.
- **Operators can set a friendly hostname** with `capsulectl capsule set-hostname <name>`.
  The name persists in SQLite, is advertised via mDNS, and appears in the HDMI banner.
  After setting it, `capsulectl discover` shows the friendly name instead of the auto-generated
  short ID.
- **The short ID is always unique; the friendly hostname is operator-controlled and not
  enforced to be unique.** The short ID remains the stable fallback if two machines end up
  with the same friendly name.

## Non-goals (v1)

- **mDNS across routed subnets.** mDNS is link-local multicast. Machines on different VLANs
  or subnets will not discover each other. Operators with multi-subnet fleets use static
  addressing or a DNS-SD proxy (e.g., Avahi's reflector mode) — that is outside Capsule's
  scope.
- **Auto-adopt without operator confirmation.** Discovery and adoption are always separate
  acts. `capsulectl discover --adopt` prompts the operator for each machine; there is no
  flag to skip all confirmation.
- **DNS resolution of capsule names.** `capsule-a3f2.local` resolving to the machine's IP
  via mDNS is a side effect of the mDNS announcement on supporting resolvers (macOS,
  systemd-resolved). It is not guaranteed and capsulectl does not depend on it.
- **Context address auto-update.** When a capsule's IP changes (DHCP lease rotation), the
  stored context still has the old IP. Updating it is a manual `capsulectl context set-addr`
  operation, or re-running `capsulectl discover` and noting the new address. Auto-update is
  an open question.
- **Discovery across the fabric.** The fabric is a WireGuard overlay for workloads.
  Discovery is a LAN-level mechanism for finding machines before the fabric exists. They do
  not interact.

## Threat model

| Threat | Protected? |
|--------|------------|
| Attacker on the LAN spoofs an mDNS announcement with a fake IP/short-ID | Partial — `capsulectl adopt` still does a direct TLS handshake and fingerprint capture. A spoofed mDNS record causes the discover table to show a wrong entry, but the fingerprint displayed comes from the real TLS connection to that IP, not from mDNS. The operator sees a mismatch if the short ID in mDNS differs from what's on HDMI. |
| Attacker on the LAN intercepts the TLS connection during discover's fingerprint fetch | No — same posture as `capsulectl adopt` today. If an attacker can MITM TLS on your LAN, the TOFU model has already lost. Physical fingerprint verification (HDMI vs. CLI) is the defense. |
| Broadcasting `adopted=false` leaks that a machine is unadopted and accepting keys | Yes, intentionally — the information is useful and the blast radius of knowing a machine is unadopted is low on a trusted homelab LAN. Operators on untrusted networks should use `capsulectl adopt` directly with a known IP and skip mDNS. |
| A malicious workload on an adopted capsule registering a fake mDNS service | No — workloads run on `br0` (not on the host network namespace); they cannot send multicast to the uplink interface. The mDNS announcer runs in capsuled's network namespace. |
| Short ID collision (two machines share the same `capsule-xxxx`) | Probability is ~1/65536 per pair at 4-byte IDs. Detect on announce: if a capsule hears its own short ID announced by a different `capsule_id`, it appends an extra hex byte and re-announces. Documented as a corner case; real-world probability at homelab scale is negligible. |
| mDNS traffic blocked by a managed switch (no multicast forwarding) | Discovery silently returns nothing. Symptom is obvious; recovery is to use `capsulectl adopt --capsule <ip>:50000` directly, as today. |

## Concept: how discovery works

### mDNS announcement (capsuled side)

At startup, after the network interface is up and SQLite is readable, capsuled starts
an mDNS announcer goroutine. It registers a `_capsule._tcp.local.` service with:

- **Instance name:** `<short-id>._capsule._tcp.local.` (e.g., `capsule-a3f2._capsule._tcp.local.`)
- **Port:** 50000
- **A record:** the uplink interface's IPv4 address (the same one shown on HDMI)
- **TXT records:**
  - `capsule_id=<uuid>` — the node's stable UUID
  - `short_id=capsule-a3f2` — abbreviated display name
  - `hostname=nuc-1` — operator-set friendly name; empty string if not set
  - `adopted=false` — true once `authorized_keys` has at least one entry
  - `version=20260513-120000` — build ID baked into the binary

The announcer runs as long as capsuled runs, updating `adopted=true` after the first
`capsulectl adopt` succeeds. The announcement is on the uplink interface only — not on
`br0` or `wg-fabric`.

capsuled implements mDNS directly (using a small Go library, `github.com/grandcat/zeroconf`
or equivalent), consistent with the principle of a single binary with no host daemons.
Avahi is not required or installed.

### `capsulectl discover` (laptop side)

```
capsulectl discover [--timeout 5s] [--unadopted] [--json]
```

The CLI browses `_capsule._tcp.local.` for `--timeout` seconds, collects all responses,
then for each discovered entry opens a raw TLS connection to the announced `ip:50000` and
captures the server certificate's SHA-256 fingerprint. The mDNS TXT records provide
metadata; the fingerprint is always fetched directly.

Output:

```
NAME           ADDRESS              FINGERPRINT                ADOPTED
capsule-a3f2   192.168.10.101:50000  a3:f2:1c:9d:4e:7f:…       no
capsule-b7e1   192.168.10.102:50000  b7:e1:3f:4a:8c:2d:…       no
capsule-c2d5   192.168.10.103:50000  c2:d5:8e:9b:1f:6a:…       no
capsule-d8b3   192.168.10.104:50000  d8:b3:2c:7e:5a:1f:…       no
capsule-e9a4   192.168.10.105:50000  e9:a4:6d:8b:3f:2c:…       no
capsule-f1b2   192.168.10.110:50000  f1:b2:7c:3a:9e:4d:…       no
capsule-9e3c   192.168.10.120:50000  9e:3c:4f:8d:2a:6b:…       no
```

Once machines are adopted and have friendly hostnames set:

```
NAME     ADDRESS              FINGERPRINT                ADOPTED
nuc-1    192.168.10.101:50000  a3:f2:1c:9d:4e:7f:…       yes  (context: nuc-1)
nuc-2    192.168.10.102:50000  b7:e1:3f:4a:8c:2d:…       yes  (context: nuc-2)
nuc-3    192.168.10.103:50000  c2:d5:8e:9b:1f:6a:…       yes  (context: nuc-3)
nuc-4    192.168.10.104:50000  d8:b3:2c:7e:5a:1f:…       yes  (context: nuc-4)
nuc-5    192.168.10.105:50000  e9:a4:6d:8b:3f:2c:…       yes  (context: nuc-5)
gpu      192.168.10.110:50000  f1:b2:7c:3a:9e:4d:…       yes  (context: gpu)
edge     192.168.10.120:50000  9e:3c:4f:8d:2a:6b:…       yes  (context: edge)
```

The ADOPTED column shows `yes (context: <name>)` when the discovered `capsule_id` matches
a context in `~/.config/capsule/config.yaml`. An adopted capsule that is not in the local
context file shows `yes (unknown)` — someone else enrolled it.

`--unadopted` filters to only show machines with `adopted=false`.

`--json` emits a JSON array for scripting:

```json
[
  {
    "name": "capsule-a3f2",
    "addr": "192.168.10.101:50000",
    "capsule_id": "a3f21c9d-...",
    "fingerprint": "a3:f2:1c:9d:...",
    "adopted": false,
    "version": "20260513-120000"
  }
]
```

### `capsulectl discover --adopt` (the new happy path)

```
capsulectl discover --adopt [--timeout 5s]
```

Runs discovery, then walks through each unadopted machine interactively. For each one:

```
[1/5] capsule-a3f2  192.168.10.101
  fingerprint: a3:f2:1c:9d:4e:7f:b2:c8:...
  Verify this fingerprint matches the HDMI display on the machine before continuing.
  Name: nuc-1
  Adopt? [y/N]: y
  ✓ Adopted nuc-1
  Set hostname on machine? [y/N]: y
  ✓ Hostname set to nuc-1

[2/5] capsule-b7e1  192.168.10.102
  fingerprint: b7:e1:3f:4a:8c:2d:...
  Name: nuc-2
  Adopt? [y/N]: y
  ✓ Adopted nuc-2
  Set hostname on machine? [y/N]: y
  ✓ Hostname set to nuc-2

...

Done. 5 capsules adopted.
```

Setting the hostname (`Set hostname on machine?`) triggers `CapsuleService.SetHostname`
on the newly adopted machine, which stores the name in SQLite and updates the mDNS
announcement immediately. After this, future `capsulectl discover` output shows the
friendly name.

The operator can skip the hostname prompt and set it later:

```sh
capsulectl --capsule nuc-1 capsule set-hostname nuc-1
```

### HDMI banner changes

Before (today):

```
  192.168.10.101:50000
```

After:

```
  capsule-a3f2  192.168.10.101:50000
```

After `set-hostname`:

```
  nuc-1 (capsule-a3f2)  192.168.10.101:50000
```

The short ID is always shown in parentheses alongside the friendly name, so the operator
can always find the machine in `capsulectl discover` output even if they forget the context
name. The short ID is immutable — it stays the same across reboots, OS updates, and even
after a re-adopt.

## SQLite schema

Two new columns on `os_state` (the singleton identity row that already tracks active slot,
pending updates, etc.):

```sql
ALTER TABLE os_state ADD COLUMN short_id TEXT;
-- 'capsule-a3f2'. Generated once on first boot from the first 4 bytes of capsule_id.
-- Never changes. Used for mDNS announcement and HDMI display.

ALTER TABLE os_state ADD COLUMN hostname TEXT;
-- Operator-set friendly name, e.g. 'nuc-1'. NULL until set.
-- Written by CapsuleService.SetHostname. Used for mDNS announcement and HDMI display.
```

`short_id` is generated at first boot (the same boot that initializes the LVM VG, sets
up `/perm`, and generates the TLS keypair). It is derived deterministically from the first
4 bytes of the `capsule_id` UUID: `fmt.Sprintf("capsule-%x", capsuleID[:4])`. It is
written to `os_state` once and never recomputed.

No changes to `authorized_keys`. The mDNS `adopted` flag is computed at runtime:
`SELECT COUNT(*) FROM authorized_keys > 0`.

## Lifecycle

### First boot — short ID generation

```
capsuled startup:
  1. Initialize LVM, mount /perm, open SQLite (existing)
  2. If os_state.short_id IS NULL:
       short_id = "capsule-" + hex(capsule_id[:4])
       UPDATE os_state SET short_id = short_id
  3. Bring up network, get DHCP (existing)
  4. Print HDMI banner (updated to include short_id / hostname)
  5. Start mDNS announcer goroutine with short_id, hostname, adopted=false
  6. Continue normal startup...
```

### After adoption — adopted flag updates

The adopt RPC (`CapsuleService.Adopt`) already writes a row to `authorized_keys`.
After the write, the mDNS announcer is signaled to update its TXT record to `adopted=true`.
This is a simple goroutine notification; the mDNS library supports in-place TXT record
updates without re-registering the service.

### `capsulectl capsule set-hostname <name>`

```
capsulectl --capsule nuc-1 capsule set-hostname nuc-1
```

capsuled:
1. Validates: name is non-empty, ≤ 63 characters, valid DNS label characters.
2. `UPDATE os_state SET hostname = 'nuc-1'`.
3. Calls `sethostname("nuc-1")` — takes effect immediately for new processes.
4. Signals the mDNS announcer to re-register with the new instance name
   (`nuc-1._capsule._tcp.local.`) and update the `hostname` TXT record.
5. Returns success.

The `capsule_id` and `short_id` do not change. The mDNS service is now addressable under
both the old instance name (for a grace period of 30 s, then de-registered) and the new one.

### `capsulectl discover`

```
capsulectl discover [--timeout 5s] [--unadopted] [--json]
```

CLI:
1. Open a multicast listener on `224.0.0.251:5353` and `[ff02::fb]:5353` for mDNS browse.
2. Send a PTR query for `_capsule._tcp.local.` and collect responses for `--timeout` seconds.
3. For each unique `(ip, port)` discovered: open a raw TLS connection, capture the server
   certificate SHA-256 fingerprint, then close the connection. No authentication at this step.
4. Load `~/.config/capsule/config.yaml`; for each result, check if `capsule_id` matches a
   known context (to populate the ADOPTED column).
5. Print the table (or JSON).

If the TLS connection to a discovered address fails (machine went down between the mDNS
response and the fingerprint fetch): mark that row `unreachable` in output rather than
silently dropping it.

### `capsulectl discover --adopt`

```
capsulectl discover --adopt [--timeout 5s]
```

CLI:
1. Run `capsulectl discover` internally to get the full list.
2. Filter to `adopted=false`.
3. For each unadopted machine (sorted by IP for deterministic order):
   - Display: short ID, IP, fingerprint (fetched directly).
   - Prompt for a context name.
   - Prompt for confirmation.
   - On confirmation: run the existing `adopt` flow (keypair generate → Adopt RPC → context save).
   - Prompt to set hostname on the machine; if yes, send `SetHostname` RPC.
4. Report a summary.

## Failure scenarios

| Failure | Symptom | Recovery |
|---------|---------|----------|
| mDNS blocked by switch (no multicast forwarding) | `capsulectl discover` times out and returns nothing | Use `capsulectl adopt --capsule <ip>:50000` directly — the existing flow, as today |
| Machine is on a different VLAN/subnet | Not discovered | Same — direct adoption by IP, or configure a DNS-SD proxy |
| Machine booted but network not yet up | mDNS not yet announced; missing from discover output | Run discover again in a few seconds, or use `--timeout 15s` |
| Short ID collision (two machines share `capsule-xxxx`) | Both appear in discover output with the same NAME column | capsuled detects the conflict on the wire (hears its short_id from a different capsule_id) and appends a byte: `capsule-a3f244`. Rare — log a warning in `capsule logs`. |
| Friendly hostname collision (two machines `set-hostname nuc-1`) | Both appear in discover output as `nuc-1`; mDNS conflict resolution renames one to `nuc-1 (2)` | Capsuled logs the rename. Operator should use unique names; the short ID in parentheses distinguishes them. |
| Machine crashes between mDNS response and fingerprint fetch | Discover table row shows `unreachable` | Expected. Re-run discover once the machine is back up. |
| capsuled restarts mid-discover | mDNS re-announces on restart (few seconds gap); fingerprint is the same (TLS cert is on `/perm`) | Transient gap in discover output; re-run or increase `--timeout`. |
| Operator adopts the wrong machine (wrong fingerprint) | Wrong machine's key stored; operator has access to unintended hardware | Revoke the wrong adoption: on the unintended machine, `capsulectl --capsule <it> key remove <kid>`. On the intended machine, run `capsulectl discover --adopt` again. |
| OS update changes the TLS cert fingerprint | It doesn't — the cert lives on `/perm` and survives A/B slot flips | No action needed. |

## Open questions

- **Context address auto-update.** When `capsulectl discover` finds a context's `capsule_id`
  at a different IP than stored (DHCP lease changed), should it offer to update the context
  address automatically? Probably yes — a `discover --refresh-contexts` flag that writes
  updated IPs for any known capsule found at a new address. Keeps contexts usable without
  manual intervention after lease changes.

- **IPv6 mDNS.** The announcer should also advertise an AAAA record if the uplink has
  a usable IPv6 address. The browse side needs to listen on the IPv6 mDNS group. Straightforward
  to add alongside the IPv4 path; defer if no immediate demand.

- **`capsulectl discover` as the default for `adopt`.** Should `capsulectl adopt` with no
  `--capsule` flag run discover automatically and prompt? The current default of `localhost:50000`
  (QEMU) is more useful for development. Keep the explicit flag required; `discover --adopt` is
  the fleet-enrollment entry point.

- **Discover timeout tuning.** 5 seconds is a reasonable default for a quiet LAN. On a busy
  LAN with many mDNS sources, the capsule responses may arrive quickly. On a slow or large
  LAN they may take longer. `--timeout` lets operators tune; consider a `--wait-for N` flag
  that exits as soon as N capsules have been found (useful in scripts: "wait until all 7
  machines are visible, then proceed").

- **Machine ordering in `discover --adopt`.** Currently sorted by IP for determinism.
  Should we sort by short ID? By order discovered? IP is fine for now; revisit if there's
  a reason to prefer another order.

- **mDNS on the fabric (`wg-fabric`).** The fabric proposal explicitly excludes mDNS from
  crossing `wg-fabric`. Discovery is a LAN bootstrap mechanism and does not need to cross
  the fabric. These are orthogonal.

- **The `capsulectl context list` view.** Today contexts are stored in config.yaml and
  `context list` shows them. After discovery, `context list` should probably show the
  short ID and hostname alongside the IP, as a "known fleet" summary. Minor UX polish;
  include in the same implementation pass.

## Implementation pointers

- **mDNS library.** `github.com/grandcat/zeroconf` is a pure-Go mDNS/DNS-SD implementation
  with no CGo dependency and no host daemon requirement. Alternatively `github.com/hashicorp/mdns`.
  Either adds to the capsuled binary and capsulectl binary without system dependencies.
- **Proto changes:** `models/capsule/v1/capsule.proto` — `CapsuleService` grows
  `SetHostname(SetHostnameRequest) → SetHostnameResponse`. `CapsuleInfoResponse` grows
  `string short_id` and `string hostname`. No new proto files needed.
- **Schema:** `store/sqlite/sqlite.go` migration adds `short_id TEXT` and `hostname TEXT`
  to `os_state`. The migration generates `short_id` from `capsule_id` at migration time
  for existing nodes (i.e., a re-adopt is not required after upgrading).
- **Boot:** `cmd/capsuled/main.go` — after SQLite is open, ensure `short_id` is set, then
  start the mDNS announcer goroutine before entering the reconciler loop. The announcer
  watches a channel for `adopted` and `hostname` change events and re-registers the service
  record as needed.
- **HDMI:** `boot/boot_linux.go` already prints the banner. Read `short_id` and `hostname`
  from `os_state` before printing, format accordingly.
- **CLI:** new `discover` subcommand in `cmd/capsulectl/main.go`. Shares the mDNS browse
  code with no server-side component. `capsule set-hostname` is a new verb under the
  existing `capsule` subcommand group. `context list` gets a minor formatting update.
- **Discovery is strictly additive.** A capsule with mDNS disabled at the network layer
  (multicast blocked) behaves exactly as today. A capsulectl binary without `discover` still
  works for direct adoption. Nothing breaks if the mDNS announcer fails to start — it logs
  a warning and the rest of capsuled continues normally.
