# Fabric: a WireGuard mesh for workloads (proposal)

> **Status:** Proposal. Not implemented. This is the cross-host networking story for Capsule — today every capsule is an island and `br0` traffic does not leave the box. The companion proposals [encrypted-volumes.md](encrypted-volumes.md) and [external-disks.md](external-disks.md) are independent of this one.

## Summary

Capsule today is a *single-node* control plane: each capsule has its own private `br0` at `172.20.254.0/24`, containers and microVMs get addresses on that bridge, and there is no cross-capsule reachability at the workload layer. Operators wanting "container A on capsule-1 should reach container B on capsule-2" fall back to host port mappings and the underlying LAN — which means there is no isolation, no portable addressing, and no policy that capsule itself enforces.

This proposal adds a **fabric**: a WireGuard mesh that runs between capsules, gives every fabric-enrolled workload a stable address in a private `100.64.0.0/10` space, and lets the operator declare allow-list policy on a per-workload basis (`workload A may reach workload B on tcp/5432`, default deny everything else). The fabric is one bridge wider than today's `br0`: each capsule keeps `br0` as the local L2, and a new `wg-fabric` interface routes the cross-capsule slice. Workloads opt in by setting `fabric: {}` on their spec — workloads with no `fabric` block keep today's behavior unchanged.

The shape closely matches Tailscale's "subnet router" pattern and Talos's KubeSpan, with one big simplification: there is no relay, no DERP, no STUN. v1 requires that any two capsules expected to talk have at least one direction of UDP reachability between their host endpoints. That is the right default for a homelab fleet.

## Goals

- **A WireGuard mesh between capsules.** One `wg-fabric` interface per capsule, one peer entry per other capsule in the fleet. Encrypted, point-to-point, no shared key.
- **Stable cross-host workload addresses.** Each fabric workload gets an IP that does not change when it is restarted, moved between capsules, or briefly absent. Addresses live in `100.64.0.0/10` (CGNAT range — chosen so it never collides with home LANs).
- **Declarative, per-workload allow-list policy.** `allow_from` / `allow_to` on a workload spec name other workloads (or `cidr:` chunks). Default is deny.
- **Policy enforced on both ends.** Same rule expressed as egress on the sender and ingress on the receiver. Defense in depth, and an operator-readable answer to "where did this packet die."
- **Operator-driven enrollment.** Capsuleers do not auto-discover each other. `capsulectl fabric enroll <capsule>` from a workstation that already talks to both nodes does the key exchange. No central registry.
- **Survives capsule reboot and `capsule update push`.** WireGuard keys and the fabric DB rows persist on `/perm` and are reloaded at boot.
- **Workloads stay portable.** A manifest references peer workloads by *name*, not by fabric IP. Capsuled resolves names → addresses locally.

## Non-goals (v1)

- **Relayed traffic between capsules behind separate NATs.** If both endpoints are unreachable from each other, the fabric does not connect them. A future relay capsule (think Tailscale DERP) is sketched in *Open questions*.
- **Automatic endpoint roaming.** WireGuard endpoints (host:port) are recorded once at enrollment and updated by an operator command. UDP source-port roaming inside an established peering works; rediscovering a peer whose public IP changed does not.
- **Workload-side WireGuard.** A workload does *not* get its own keypair. Encryption stops at the host's `wg-fabric` interface, and traffic between the host and the workload is plaintext over `br0` — same trust boundary the host already has for that workload. Designs that put `wg` inside every container are evaluated in *Why not per-workload WireGuard* and rejected for v1.
- **L7 policy (HTTP paths, hostnames, identities).** Policy is L3/L4 only. Application-layer routing is the service-gateway pattern from the homelab article and lives in a workload (Caddy/Traefik), not in capsuled.
- **A control plane outside the capsule fleet.** No Headscale, no coordination server. Each capsule holds the slice of the topology it needs; the operator's `capsulectl` is the only thing that sees the whole graph.
- **NAT64, multicast, broadcast.** The fabric is unicast IPv4 + IPv6 ULA. mDNS will not cross it.
- **DNS for fabric names.** Discussed under *Open questions*. v1 resolves names to addresses inside capsuled's policy compiler; workloads see the resolved IP and use it. A `<workload>.fabric` DNS responder is plausible later.

## Threat model

| Threat                                                                   | Protected? |
|--------------------------------------------------------------------------|------------|
| Passive snooping on the LAN between two capsules                         | **Yes** — WireGuard ChaCha20-Poly1305 |
| Active MITM with a forged WireGuard public key                           | **Yes** — peers are pinned by public key at enrollment |
| A workload reaching a peer workload it has no policy allowance for       | **Yes** — nftables drop at egress on sender, ingress on receiver |
| A workload spoofing another fabric IP from its own veth                  | **Yes** — `rp_filter` + per-veth source-address check; capsuled installs an anti-spoof rule that drops packets entering `br0` with a source IP that does not belong to that veth |
| A compromised capsule reaching workloads on a peer capsule it has no allowance for | **Partial** — the peer capsule's ingress nftables still drops. A compromised capsule *can* freely talk on the fabric as itself, but cannot impersonate a specific workload from another capsule |
| A compromised capsule with `capsulectl` credentials adding new peers     | No — same as today's `capsulectl` trust boundary. mTLS + Ed25519 JWT is the gate |
| A workload exhausting another workload's bandwidth                       | No — no QoS in v1. `tc` is in scope for a future proposal |

The fabric inherits Capsule's existing trust boundary (mTLS + operator JWT for the API plane). It does not try to defend a workload from its own host: a workload's egress is rewritten by the host before it touches `wg-fabric`, and a host that wants to lie about which workload a packet came from can do so.

## Concept: the fabric

A **fabric** is a single named WireGuard network that any number of capsules belong to. v1 has exactly one fabric per fleet — there is no support for multiple parallel fabrics, and the name is informational. The fabric has:

- **A `/10` IP range.** `100.64.0.0/10` by default, configurable at fabric init. Each enrolled capsule gets a `/20` slice (4096 addresses) carved deterministically from its node ID, so two capsules cannot accidentally collide on the same subnet.
- **A capsule peer table.** Public key, last-known endpoint, allowed-IPs (= that capsule's `/20`), pubkey fingerprint shown in CLI output.
- **A workload address table.** Workload name → fabric IPv4 + IPv6 ULA. Capsuled allocates from its own `/20` on workload start and reuses the same address when the workload restarts.

```
                         100.64.0.0/10  (the fabric)
                ┌──────────────────────────────┬─────────────────────────────────┐
                │                              │                                 │
       capsule-edge (100.64.0.0/20)   capsule-storage (100.64.16.0/20)   capsule-build (100.64.32.0/20)
       wg-fabric: 100.64.0.1          wg-fabric: 100.64.16.1             wg-fabric: 100.64.32.1
       ├─ web (100.64.0.10)           ├─ postgres (100.64.16.10)          ├─ buildkit (100.64.32.10)
       └─ api (100.64.0.11)           └─ minio (100.64.16.11)             └─ runner-a (100.64.32.11)

       br0 (172.20.254.0/24)          br0 (172.20.254.0/24)               br0 (172.20.254.0/24)
       ├─ veth-web → web              ├─ veth-postgres → postgres          ├─ veth-buildkit → buildkit
       └─ veth-api → api              └─ veth-minio → minio                └─ veth-runner-a → runner-a
```

A workload IP lives logically on the fabric but is physically reachable only through the host's `wg-fabric` (for off-host traffic) or through `br0` (for on-host traffic). Capsuled programs the routing tables and nftables so both paths arrive at the same veth — the workload sees one address and does not care which path was taken.

### The two interfaces, side by side

```
                       capsule-edge
            ┌──────────────────────────────────────┐
            │                                       │
   uplink   │                                       │  to capsule-storage / capsule-build
   eth0 ─── NAT ─┐                                  │      via WireGuard UDP/51820
            │   │                                   │
            │   ├─ wg-fabric (100.64.0.1)  ◀────────┤
            │   │   AllowedIPs:                     │
            │   │     100.64.16.0/20 → storage      │
            │   │     100.64.32.0/20 → build        │
            │   │                                   │
            │   └─ br0 (172.20.254.1)               │
            │       ├─ veth-web   → 172.20.254.10   │
            │       │   fabric IP 100.64.0.10       │
            │       └─ veth-api   → 172.20.254.11   │
            │           fabric IP 100.64.0.11       │
            └───────────────────────────────────────┘
```

`br0` is unchanged. The new wire is `wg-fabric`, and the new state is the mapping from each workload's veth (or TAP) to its fabric address — programmed by capsuled at workload-attach time.

## Identity and enrollment

Each capsule generates a Curve25519 keypair the first time `capsulectl fabric init` is invoked on it. The **private key** lives in `/perm/fabric/wg.key` (mode 0400, owned by root, never logged). The **public key** is what gets shared with peers.

Enrollment is operator-driven and one-shot:

```
capsulectl --capsule capsule-edge fabric init                  # one-time on first capsule
capsulectl --capsule capsule-storage fabric init               # one-time on each subsequent capsule
capsulectl fabric enroll capsule-edge capsule-storage \
    --edge-endpoint 10.0.5.10:51820 \
    --storage-endpoint 10.0.5.20:51820
```

`fabric enroll` is run from a workstation that already authenticates to both capsules. It:

1. Talks to each capsule via the existing `capsulectl` mTLS API.
2. `GET /fabric.pubkey` from each.
3. `POST /fabric.peers` to each, carrying the other's pubkey + endpoint + assigned `/20`.
4. Both capsules write the new peer row to SQLite and apply it to `wg-fabric` (`wg set wg-fabric peer <pubkey> endpoint ... allowed-ips ...`).

The operator never types a private key. The pubkey fingerprint is printed at both ends and the operator visually confirms the pair before committing — same UX shape as `ssh-keygen -lf`.

### Why no central coordinator

The fleet is small (a handful of capsules) and the operator already has a tool (`capsulectl`) that can talk to all of them. Adding a Headscale-style coordination service would mean another stateful component to back up, restore, and key-rotate — and the only thing it would buy at this scale is auto-discovery, which a homelab does not need. The same enrollment loop scales linearly with peers added, and the operator can script it.

If the fleet grows to dozens of capsules, a designated `fleet-controller` capsule (one of the existing nodes, with a `--fleet-controller` flag) that watches `fabric_peers` rows and gossips changes is the natural next step. *Open questions* covers this.

### SQLite schema

New table on every capsule:

```sql
CREATE TABLE fabric_self (
  id              INTEGER PRIMARY KEY CHECK (id = 1),  -- singleton
  pubkey          TEXT NOT NULL,                       -- base64
  private_key_ref TEXT NOT NULL,                       -- path on /perm; not the key itself
  fabric_v4_cidr  TEXT NOT NULL,                       -- this capsule's /20 within the fabric
  fabric_v6_cidr  TEXT NOT NULL,                       -- /64 ULA slice
  listen_port     INTEGER NOT NULL DEFAULT 51820,
  fabric_id       TEXT NOT NULL,                       -- fleet-wide constant, e.g. 'home-prod'
  created_at      INTEGER NOT NULL
);

CREATE TABLE fabric_peers (
  capsule_name  TEXT PRIMARY KEY,         -- 'capsule-storage'
  pubkey        TEXT NOT NULL,            -- base64
  endpoint      TEXT,                     -- 'host:port'; NULL if this peer is the listener side
  allowed_v4    TEXT NOT NULL,            -- the peer's /20
  allowed_v6    TEXT NOT NULL,            -- the peer's /64
  persistent_keepalive INTEGER DEFAULT 25,
  last_handshake INTEGER,                 -- updated by reconciler from wg show
  created_at    INTEGER NOT NULL
);

CREATE TABLE fabric_workloads (
  workload_name TEXT NOT NULL,            -- local workload name
  capsule_name  TEXT NOT NULL,            -- which capsule it lives on (this capsule for local rows)
  fabric_v4     TEXT NOT NULL,
  fabric_v6     TEXT NOT NULL,
  last_seen     INTEGER NOT NULL,
  PRIMARY KEY (workload_name, capsule_name)
);
```

A capsule holds rows for **its own workloads** in `fabric_workloads`, plus rows pushed to it from peers when those workloads are referenced in policy (see *Address resolution* below). The rows are advisory metadata used to compile policy; the routing-correctness of cross-capsule traffic depends only on `wg-fabric` AllowedIPs, not on this table.

## Workload attachment

A workload opts into the fabric by adding a top-level `fabric` block to its spec:

```yaml
kind: Container
name: web
spec:
  image: ghcr.io/example/web:1.2.3
  ports: [...]                       # host port mappings, unchanged
  fabric:
    allow_to:
      - postgres@capsule-storage:5432/tcp
      - minio@capsule-storage:9000/tcp
    allow_from:
      - api@capsule-edge:8080/tcp
```

Same shape under `MicroVMSpec.fabric`. The `name@capsule` form is the human-portable identifier; the `cidr:` prefix is the escape hatch (`cidr:100.64.16.0/24:5432/tcp` for an existing service whose ownership is not modeled in Capsule).

At workload start, capsuled:

1. **Allocates a fabric IP** from its own `/20` (next free slot, sticky per `workload_name`).
2. **Adds a route** on the host: `ip route add <fabric_ip>/32 dev <veth-of-workload>` for containers, `dev <tap>` for microVMs. This makes the host's `wg-fabric` ingress path resolve to the workload's veth without bridging — capsuled keeps `br0` as today and grafts the fabric address on top.
3. **Adds the fabric IP to the workload's interface inside the netns** (containers) or hands it to the guest via `ip=` cmdline / vsock RPC (microVMs running `capsule-guest`).
4. **Compiles policy** for this workload (see below) and installs nftables rules tagged `capsule-fab:<workload>` for O(1) teardown.
5. **Publishes the workload row** to peer capsules named in `allow_from` / `allow_to` via the gRPC `FabricService.PublishWorkload` RPC. Peers store it in their `fabric_workloads` table and re-compile any rule that referenced this workload by name.

A workload without `fabric:` skips every step above — it gets its normal `172.20.254.x` and is invisible on the fabric.

### Why not per-workload WireGuard

Two designs were considered and rejected:

1. **Each workload runs its own `wg` interface inside its netns / guest.** Pros: cryptographic identity at the workload layer; a compromised host *cannot* lie about which workload sent a packet. Cons: every workload image needs `wg-tools` or the kernel `wireguard` module, every key needs a rotation story, microVMs need a key-delivery RPC, and policy still has to be enforced somewhere because allowed-IPs alone is not expressive enough for "from A to B on tcp/5432 only." The complexity multiplier is large, the threat-model gain only matters once you do not trust the host — and a homelab Capsule already runs the workload's image as root on the host kernel.

2. **`wg` interface in the workload, peering with the local host's `wg-fabric`.** A halfway option. Same downsides on imagery / key delivery; saves nothing on policy.

v1 lands on **per-capsule WireGuard, host-mediated workload addresses, nftables for policy.** It is the Tailscale subnet-router model and matches the simplicity invariant the rest of Capsule holds itself to. If the threat model changes later, swapping to per-workload `wg` is additive — the host-mediated path can stay for workloads that do not care.

## Address resolution

Policy is written by name; the kernel works in IPs. The compile step is:

```
For each rule on local workload W:
  For each remote target R (name@capsule:port/proto):
    Look up R in fabric_workloads where workload_name = name AND capsule_name = capsule.
    If missing or stale (last_seen > 5 min ago):
      RPC FabricService.GetWorkload to <capsule> for <name>.
      If 404 → workload not yet enrolled. Compile rule with a tombstone target;
              reconciler retries every 30 s and replaces the tombstone when the row arrives.
      If RPC fails → use the cached row if any; mark rule "stale" in status.
  Emit nftables rule with the resolved IPs.
```

Names are resolved at compile time, not at packet time. The performance is good (nftables sees concrete addresses) and the failure mode is observable (a workload status line says `fabric: 1 stale rule (api@capsule-edge)`).

When a referenced workload's IP changes (rare — IPs are sticky per name), the source capsule re-resolves on the next 5-minute tick or when the destination capsule pushes an updated row. Stale rules eventually self-heal; a workload restart on either side accelerates the convergence by emitting an explicit publish.

## Policy compilation

Each fabric-enrolled workload gets an nftables chain owned by capsuled. For workload `web` on `capsule-edge`:

```
table inet capsule {
  chain fab-egress-web {
    type filter hook output priority 0;
    # match packets originating from web's fabric IP
    ip saddr 100.64.0.10 ip daddr 100.64.16.10 tcp dport 5432 accept comment "capsule-fab:web → postgres@capsule-storage"
    ip saddr 100.64.0.10 ip daddr 100.64.16.11 tcp dport 9000 accept comment "capsule-fab:web → minio@capsule-storage"
    ip saddr 100.64.0.10 counter drop comment "capsule-fab:web default-deny egress"
  }
  chain fab-ingress-web {
    type filter hook input priority 0;
    ip daddr 100.64.0.10 ip saddr 100.64.0.11 tcp dport 8080 accept comment "capsule-fab:web ← api@capsule-edge"
    ip daddr 100.64.0.10 counter drop comment "capsule-fab:web default-deny ingress"
  }
}
```

Tagging every rule with `capsule-fab:<workload>` lets teardown match the existing `iptables`-based pattern: delete by comment, no rule-table walks. The `counter drop` is intentional — operators reading `nft list ruleset` see exactly which workloads are silently dropping packets and how many.

Ingress and egress are symmetric: a rule on `web` to reach `postgres` shows up as egress on `capsule-edge` and as ingress on `capsule-storage`. The publish RPC at workload start carries the rule set so each end installs the matching half. If the two ends disagree (one side has a newer spec), the *deny* wins — both rules must allow the flow. Failure-closed beats failure-open here.

## Lifecycle

### Fabric init on the first capsule

```
capsulectl --capsule capsule-edge fabric init --range 100.64.0.0/10
```

Capsuled:

1. Generates the keypair, writes `/perm/fabric/wg.key` (0400) and records the pubkey.
2. Hashes the capsule name into the `/10` to deterministically pick a `/20` slot. Records it.
3. Creates `wg-fabric`, assigns `<self>/20` and `fdcd:....::/64` (ULA), brings it up, listens on `:51820/udp`.
4. Inserts the singleton `fabric_self` row, fingerprint printed to stdout once.

No peer entries yet — the fabric exists with one node.

### Enrolling a second capsule

```
capsulectl --capsule capsule-storage fabric init
capsulectl fabric enroll capsule-edge capsule-storage \
    --edge-endpoint 10.0.5.10:51820 \
    --storage-endpoint 10.0.5.20:51820
```

Both capsules end the call with:
- a row in `fabric_peers` for the other end,
- a `wg set wg-fabric peer ... allowed-ips ... endpoint ... persistent-keepalive 25` applied,
- a fingerprint check the operator confirms.

### Adding a fabric workload

The operator edits the manifest and applies it normally:

```
capsulectl --capsule capsule-edge workload apply -f web.yaml
```

Capsuled detects the new `fabric:` block on a workload that previously did not have one (or on a new workload) and runs the attachment flow. Existing flows on `br0` are untouched.

### Removing a peer

```
capsulectl fabric peer remove capsule-storage  # from capsule-edge
```

`wg set wg-fabric peer <pubkey> remove`, delete the SQLite row, recompile every local workload's rule set (rules that named workloads on the removed capsule become tombstones, which compile to deny). Workloads stay running; their cross-capsule connectivity to the removed peer is gone.

The reverse symmetric operation must be run on the removed peer too — `fabric peer remove` does not reach across the wire. This is deliberate: a unilateral removal from one side is the right semantic when capsules are decommissioned or compromised.

### Rotating a key

`capsulectl fabric rotate-key` on a capsule generates a fresh keypair, then re-enrolls with every peer (replacing the old peer entry with the new pubkey). The operator drives this — capsuled does not auto-rotate, same posture as SSH host keys.

## Routing details

The kernel side is straightforward once the topology table is built. Per capsule:

```
# wg-fabric carries the cross-host slice
wg-fabric inet 100.64.0.1/20    fdcd:1234::1/64

# per-peer routes are managed by wg AllowedIPs, not by hand:
#   peer capsule-storage AllowedIPs 100.64.16.0/20, fdcd:1234:0:10::/64

# per-workload /32 routes within this capsule's own /20 point at the workload's veth/tap:
ip route add 100.64.0.10/32 dev veth-web
ip route add 100.64.0.11/32 dev veth-api

# anti-spoof on br0: drop packets whose source IP doesn't match the veth's assigned addrs
# (one rule per veth, installed at workload attach, tagged for teardown)

# MASQUERADE policy for fabric egress: NONE.
# Fabric traffic is end-to-end private; we never NAT it to the uplink.
# The existing br0 MASQUERADE rule for plain-bridge traffic is unchanged.
```

Packet flow, web → postgres:

1. `web` sends from `100.64.0.10` to `100.64.16.10:5432` on its netns interface.
2. Kernel routes via the `100.64.0.10/32 dev veth-web` route to the host. Source NAT does *not* apply (no MASQUERADE for fabric).
3. Egress hits chain `fab-egress-web`, matches the postgres rule, accepted.
4. Routing table on the host: `100.64.16.0/20` is the AllowedIPs for peer `capsule-storage` → kernel hands the packet to `wg-fabric`, which encrypts and sends UDP to `10.0.5.20:51820`.
5. `capsule-storage` `wg-fabric` decrypts; AllowedIPs check passes (source `100.64.0.10` is in `capsule-edge`'s `/20`).
6. Routing on `capsule-storage`: `100.64.16.10/32 dev veth-postgres` → ingress chain on postgres → accepted (matches the symmetric rule).
7. Postgres receives a connection from `100.64.0.10`. No source rewriting.

The fabric source IP is preserved end to end. This is the property that makes per-workload policy meaningful — the destination capsule's nftables can match on `ip saddr 100.64.0.10`, not on a NAT'd host address.

## Failure scenarios

Every failure has a documented path that does not corrupt local workload state. Cross-capsule connectivity may be temporarily lost; persistent state on either capsule is unaffected.

### Reachability

| Failure                                              | Symptom                                | Recovery                                                                                       |
|------------------------------------------------------|----------------------------------------|------------------------------------------------------------------------------------------------|
| Peer endpoint changes IP (DHCP lease rotation)       | Handshake fails after ~3 min           | Operator runs `capsulectl fabric peer set-endpoint <capsule> <new>` on each side. Auto-discovery is *Open questions* |
| Both peers behind separate NAT                       | No handshake ever completes            | v1 documents this as unsupported; relay-via-third-capsule is *Open questions* |
| Uplink is up but UDP/51820 is firewalled             | Same as above                          | Operator fixes firewall; `wg show` makes the symptom obvious                                   |
| Peer powered off                                     | `last_handshake` ages past 5 min       | Workloads keep running; cross-capsule rules to that peer drop traffic until handshake recovers. Status flags rules `stale`. No automatic re-routing — fabric does not have alternative paths in v1 |

### Configuration drift

| Failure                                              | Symptom                                | Recovery                                                                                       |
|------------------------------------------------------|----------------------------------------|------------------------------------------------------------------------------------------------|
| One side has a `fabric.allow_to` to a peer that does not have the symmetric `allow_from` | Traffic dropped at the receiver's ingress chain | Status line on the receiving workload: `fabric: ingress denied from web@capsule-edge`. Operator adds the missing rule |
| Workload renamed                                     | Old name's address tombstoned on peers | Renaming = delete+create as far as fabric is concerned; the new name allocates a new fabric IP. Operator updates references on peers |
| Two capsules accidentally claim the same `/20` slot  | `fabric init` refuses if a peer already advertises the slot at enrollment | Operator picks a different name (slot is a hash of the name) or passes `--slot` explicitly |
| Clock drift between capsules                         | WireGuard tolerates large skew         | No effect on fabric. Affects mTLS cert validity on the existing API plane — same as today      |

### Capsuled crash / restart

| Crash point                                          | State left behind                                | Cleanup                              |
|------------------------------------------------------|--------------------------------------------------|--------------------------------------|
| Mid `fabric init` (key generated, SQLite not committed) | Keypair on `/perm`, no `fabric_self` row      | Boot-time reconciler: orphan keypair detected → either rolled into a new init or deleted by `capsulectl fabric reset` |
| Mid `fabric enroll` (peer added on one side only)    | One side has the peer; other does not             | Operator re-runs `enroll`. Idempotent: the second side adds the missing peer; the first side updates timestamps |
| Mid workload attach (route added, nftables not yet installed) | Route present, no policy installed         | Reconciler tick: every fabric workload's policy is recomputed each minute; missing chain is re-added. Until then, traffic is denied by the default-deny on the receiving side |
| capsuled restart                                     | `wg-fabric` is in the host netns, persists across capsuled restart; nftables `capsule` table also persists | On start, capsuled reconciles every fabric workload — re-applies routes, re-installs chains, re-emits publishes. Steady state restored within seconds |

### Key compromise

| Event                                                | Recovery                                                                                       |
|------------------------------------------------------|------------------------------------------------------------------------------------------------|
| Capsule disk stolen with `/perm/fabric/wg.key` readable | Treat the capsule as compromised. From every peer: `capsulectl fabric peer remove <stolen>`. Fabric is restored. Workload data on the stolen disk is a separate problem (see [encrypted-volumes.md](encrypted-volumes.md)) |
| Operator suspects a key leak                         | `capsulectl fabric rotate-key` on the suspected capsule, then re-enroll with every peer. ~30 s of cross-capsule traffic outage during rotation |

## What this does NOT protect against

- **A compromised capsule attacking peers within their allowed policy.** Once a peer has policy allowing `web → postgres`, the host running `web` can send anything-shaped traffic to `postgres:5432`. The fabric is an authenticated channel, not an application firewall.
- **A workload exhausting another workload's CPU or bandwidth.** No `tc` shaping in v1.
- **Side channels via the uplink.** The fabric carries fabric traffic; the host's uplink still carries the existing `br0 → eth0` MASQUERADE for plain workloads. A workload with both `fabric:` and `network_mode: BRIDGE` can reach the public internet through the uplink path while reaching fabric peers through `wg-fabric`. Both paths obey their own policies.
- **L7 attacks against an exposed service.** The fabric controls *who* can connect; what the connecting peer says after the TCP handshake is the workload's problem.

## Open questions

- **Relay path for double-NAT peers.** A designated "relay" role on one capsule that other peers route through when no direct path exists. Mechanically straightforward (third peer with AllowedIPs covering both sides, source-routed via wg-fabric on the relay), but the operator UX is unclear — should every capsule advertise itself as a possible relay, or is the relay a named role on one machine? Lean toward a single named relay capsule, opt-in via `capsulectl fabric set-relay <capsule>`.
- **A fleet controller.** For fleets larger than ~10 capsules, the O(n²) operator enrollment becomes annoying. A designated `fleet-controller` capsule that holds the canonical `fabric_peers` table and pushes diffs to all members would scale better. The DB schema already supports it (the table is keyed by `capsule_name`, not by `local` versus `peer`). Defer; revisit when someone has more than 10 capsules.
- **Workload-name DNS on the fabric.** A capsuled-internal DNS responder that resolves `<workload>.<capsule>.fabric` to its fabric IPv4/IPv6 would let manifests omit hardcoded IPs entirely. Trivial to add (capsuled already holds the table); the question is whether to bind it to `100.64.0.53` on every capsule (one bind, multiple paths to it via the fabric) or run a tiny resolver per workload netns. Lean centralized.
- **IPv6 outer.** WireGuard's outer transport is UDP4 in v1. The article's caution about mobile carriers blocking WireGuard over IPv6 suggests sticking with v4 outer, but a future `--listen-port-v6` is plausible for capsules that want both. Inner IPv6 (ULA) is already in v1.
- **Should fabric workloads also get a routable address on `br0`?** Today a workload on `br0` gets `172.20.254.x` and that is what other on-host workloads reach. Adding a fabric address gives it a second IP. Possible cleanups: (a) collapse `br0` into the fabric for fabric-enrolled workloads (drop the `172.20` address; pure-fabric workloads only have one IP), (b) keep both, with the fabric address as a routable alias. Lean (b) for v1 because it preserves backwards compatibility with `network_mode: BRIDGE` and lets a workload be reached from both an on-host neighbor and a fabric peer without surprises.
- **Multiple fabrics on one capsule.** A "prod fabric" and a "dev fabric" with different membership and different policy. Possible (one `wg-N` interface per fabric), but no real use case yet at homelab scale. Defer.
- **Persistent-keepalive default.** 25 s is the WireGuard convention. Aggressive enough for most NAT idle timeouts, gentle enough not to wake disks. Document as 25; let operators tune via `capsulectl fabric peer set-keepalive`.

## Implementation pointers

The execution plan lives outside this doc; once accepted, it lands in `PLAN.md` and roughly:

- Proto changes: new `models/capsule/v1/fabric.proto` with `FabricService` (`Init`, `Enroll`, `PeerList`, `PeerRemove`, `PeerSetEndpoint`, `RotateKey`, `Reset`, `PublishWorkload`, `GetWorkload`). Workload proto grows a `FabricSpec fabric = N` on both `ContainerSpec` and `MicroVMSpec`.
- Schema: `fabric_self`, `fabric_peers`, `fabric_workloads` tables (above). `volumes` and `workloads` unaffected.
- Logic: new `core/fabric/` package (key management, peer state, allocator, policy compiler, publish loop). The reconciler grows a fabric-reconcile tick alongside its workload tick.
- Runtime: `runtime/container/driver.go` and `runtime/microvm/firecracker/network.go` learn to ask the fabric package for an address at attach and to install the per-veth route + anti-spoof rule. Existing `br0` setup is unchanged.
- Image: `apk add wireguard-tools nftables`; ensure the kernel `wireguard` module is in `linux-lts` (it is; nothing to compile). `modules-load.d/capsule.conf` adds `wireguard` and `nf_tables`.
- Boot: `boot/boot_linux.go` initializes `wg-fabric` if `fabric_self` is set, before workload reconciliation runs. Idempotent: re-applies all peers and AllowedIPs from SQLite on every boot.
- CLI: `capsulectl fabric {init, enroll, peer-list, peer-remove, peer-set-endpoint, rotate-key, reset, status}`. The `status` verb is the operator's window into stale rules, missing peers, and last-handshake ages.

The fabric is **strictly additive** to today's networking: a capsule with no `fabric_self` row behaves exactly as it does today, and a workload with no `fabric:` block does too. That is the migration story — none required, operators turn it on per-workload when they want it.
