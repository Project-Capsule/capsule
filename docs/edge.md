# Edge: exposing workloads to the public internet (proposal)

> **Status:** Proposal. Not implemented. Builds on [fabric.md](fabric.md) — read that first. The edge is the publicly-reachable doorway into the fabric; without the fabric there is no private backend network to route into.

## Summary

The fabric proposal gives workloads cross-capsule reachability inside a private WireGuard mesh. This proposal adds the other half: a way to **expose specific fabric workloads to the public internet**. The shape is the one the linked article describes — a small public-facing edge that terminates TLS and routes into the private fabric — but Capsule-native: the edge is **just a Capsule** that happens to have a public IP and a route table.

There is **no new daemon, no new protocol, no third-party tunnel**. The edge capsule runs the same `capsuled` as every other node, joins the fabric the same way, and exposes workloads by running a Capsule-managed Caddy container whose config capsuled regenerates from an `edge_routes` table. The operator points `blog.example.com` at the edge capsule's public IP (or at a cloud LB in front of it) and declares one route:

```
capsulectl --capsule capsule-edge edge route add my-blog \
    --hostname blog.example.com \
    --backend web@capsule-storage:8080
```

That's the operator surface. Capsuled handles ACME, the Caddyfile, fabric policy for the proxy, health checks, and cleanup. Backend workloads on `capsule-storage` keep their existing spec — they need a `fabric:` block (so the proxy can reach them) but do not need to know they are publicly exposed.

The non-negotiable invariant: **a workload is only reachable from the public internet if an `edge_route` references it.** No magic, no default-public, no opt-out-required.

## Goals

- **Public reachability for any fabric workload**, opt-in per route on an edge capsule.
- **TLS termination at the edge.** Let's Encrypt by default, manual certs as an option. Backends receive plaintext over the fabric — the fabric itself is already encrypted, so the per-segment story is clean (public ↔ edge encrypted by TLS; edge ↔ backend encrypted by WireGuard).
- **HTTP and HTTPS in v1.** TCP/UDP exposures deferred to v2 (Caddy `layer4` plugin or a small TCP forwarder; out of scope here).
- **Multiple deployment models without surface-area changes**: direct DNS-to-edge, edge behind a cloud LB, edge behind a CDN (Cloudflare-style). The edge capsule does not know or care which.
- **HA via multiple edge capsules.** Same route declared on two edges; operator uses DNS round-robin or an upstream LB to balance.
- **No automatic public exposure.** A workload becomes public if and only if an explicit `edge_route` row names it. Removing the route removes the public reachability immediately.
- **Real client IP visible to backends.** When an upstream LB or CDN is configured, the edge strips and rebuilds `X-Forwarded-For` / `Forwarded` correctly — backends see the actual originator without trusting spoofed headers.
- **Certificate state survives reboot, capsule update, and edge-capsule replacement.** ACME accounts and issued certs persist on `/perm`; replacing the edge capsule re-uses the existing account.

## Non-goals (v1)

- **Capsule-managed public DNS.** Pointing `blog.example.com` at the edge is the operator's job, via whatever DNS provider they use. Capsule does not write to Route53 / Cloudflare DNS / etc. (DNS-01 ACME challenges *do* talk to a DNS provider, but only to set a temporary `_acme-challenge` record — see *TLS / ACME*.)
- **WAF / DDoS / rate-limiting beyond what Caddy ships.** Operators who want serious WAF put a CDN in front of the edge. Caddy's built-in rate-limit module is available but Capsule does not configure it past a sensible default.
- **TCP and UDP exposure.** v1 is HTTP/HTTPS only. The shape for raw L4 exposures is sketched in *Open questions*.
- **Gateway-level auth.** Putting Tailscale/Authelia/oauth2-proxy in front of a backend is a workload-composition pattern, not an edge feature. Out of scope here.
- **Multi-region routing or geo-DNS.** Capsule does not pretend to be Cloudflare. Operators run multiple edges in different regions if they want and front them with whatever cloud LB they prefer.
- **Cert sharing across edges.** Each edge capsule does its own ACME for the routes it serves. Two edges serving the same hostname will each issue their own cert — LE's rate limits permit this. Centralized cert storage is *Open questions*.
- **Reverse tunnels from behind CGNAT** (Cloudflare-Tunnel-style, no public IP on any capsule). Discussed and deferred — the v1 model assumes at least one capsule has an inbound-reachable port.

## Threat model

The fabric proposal's threats apply unchanged for backend traffic. New threats specific to the edge:

| Threat                                                                   | Protected? |
|--------------------------------------------------------------------------|------------|
| Attacker on the public internet reaches a workload with no `edge_route`  | **Yes** — default deny: no route, no reachability. The edge proxy has fabric `allow_to` only for workloads named in its routes |
| Attacker reaches an unmapped hostname on the edge IP                     | **Yes** — Caddy's default for unknown SNI / Host is `400`; no backend is touched |
| Attacker downgrades TLS to HTTP                                          | **Yes** — Caddy auto-redirects HTTP→HTTPS by default; HSTS is on for `auto` mode |
| Attacker forges `X-Forwarded-For` headers                                | **Yes** — only `trusted_proxies` listed in the route can supply those headers; everything else is stripped |
| Attacker compromises an upstream CDN account and reaches the edge directly | Partial — if the operator configured `trusted_proxies` for the CDN, an attacker bypassing the CDN with the right IP-range trick could spoof headers. Mitigation: cert-pin from the CDN to the edge (mTLS), discussed in *Open questions* |
| Attacker compromises the edge capsule                                    | The edge has fabric `allow_to` only for workloads named in routes; the blast radius is "every publicly exposed backend." The fabric still drops everything else |
| Attacker compromises a backend workload and pivots to the edge           | The edge listens on `:443` and accepts public connections; that does not change because the connecting peer is a fabric workload. Existing fabric policy controls the reverse direction |
| Certificate private key exfiltrated                                      | All Caddy ACME keys live in `/perm/edge/<edge-name>/` on the edge capsule — encrypt that directory via [encrypted-volumes.md](encrypted-volumes.md) for hardware-loss protection. Process-memory exfiltration is the same threat as anywhere else |
| ACME account compromised at the registry                                 | Out of scope. Operator rotates the ACME account: `capsulectl edge rotate-acme-account` |

The edge does not soften the fabric's posture. Every backend workload remains behind default-deny; the edge is one more fabric peer with a narrow `allow_to`.

## Concept: edge capsules and routes

An **edge** is a regular capsule with three additional facts:

1. It has at least one inbound-reachable public IP (or an upstream LB that does).
2. It has a `capsuled`-managed Caddy workload running with host-port mappings for `:80` and `:443`.
3. It has an `edge_routes` table whose rows compile to that Caddy's config.

Every capsule *can* be an edge. Whether one *is* an edge is decided by `capsulectl edge init`, which records the public IP, deploys the Caddy workload, and creates the routes table. Most fleets will have one or two edges; the rest stay backend-only.

### Two deployment models

The proxy listens on `:80` and `:443` of the edge's public IP. What sits in front of those ports is the operator's choice; the edge capsule does not care:

**Direct DNS-to-edge** — `blog.example.com` resolves to the edge capsule's public IP. Used when the homelab has a static public IP or a low-churn dynamic one. Caddy does HTTP-01 ACME and serves traffic directly.

```
internet → DNS A blog.example.com → edge capsule public IP:443
       → Caddy (workload on edge capsule) → fabric → backend
```

**Behind a cloud LB / CDN** — `blog.example.com` resolves to a managed LB (Hetzner, AWS, Cloudflare, etc.) whose origin is the edge capsule's public IP. The edge capsule does DNS-01 ACME for the cert (HTTP-01 won't survive a CDN that terminates TLS), and `trusted_proxies` lets the LB pass through real client IPs.

```
internet → DNS A blog.example.com → cloud LB → edge capsule public IP:443
       → Caddy → fabric → backend
```

Both models reuse the same `edge_routes` schema and the same Caddy workload. The operator picks at route-create time which TLS challenge style and which `trusted_proxies` set applies.

### SQLite schema

New tables on every edge capsule:

```sql
CREATE TABLE edge_self (
  id              INTEGER PRIMARY KEY CHECK (id = 1),  -- singleton
  public_v4       TEXT,                                -- '203.0.113.10' or NULL
  public_v6       TEXT,                                -- '2001:db8::1' or NULL
  proxy_workload  TEXT NOT NULL,                       -- 'edge-caddy' (the managed proxy workload name)
  acme_account_blob BLOB,                              -- wrapped under the node master key
  default_acme_email TEXT,
  created_at      INTEGER NOT NULL
);

CREATE TABLE edge_routes (
  name              TEXT PRIMARY KEY,            -- 'my-blog'
  hostname          TEXT NOT NULL,               -- 'blog.example.com'
  listen_port       INTEGER NOT NULL DEFAULT 443,
  protocol          TEXT NOT NULL,               -- 'http' | 'https' | 'tcp' (v2) | 'udp' (v2)
  backend_workload  TEXT NOT NULL,               -- 'web@capsule-storage'
  backend_port      INTEGER NOT NULL,
  tls_mode          TEXT NOT NULL,               -- 'auto' (LE) | 'manual' | 'none'
  acme_challenge    TEXT,                        -- 'http-01' | 'dns-01:<provider>'; NULL for 'none'/'manual'
  cert_blob         BLOB,                        -- present iff tls_mode='manual'
  key_blob          BLOB,                        -- present iff tls_mode='manual'; wrapped under master
  trusted_proxies   TEXT,                        -- CSV of CIDRs that may set X-Forwarded-For; default empty
  health_path       TEXT,                        -- e.g. '/healthz'; NULL = no active health check
  health_interval_s INTEGER DEFAULT 30,
  created_at        INTEGER NOT NULL
);
```

`edge_routes.backend_workload` is a fabric workload reference (`name@capsule`). The proxy workload's `fabric.allow_to` is compiled from the union of every route's backend — capsuled regenerates it on every route add / remove.

`acme_account_blob` is wrapped under the node master key from [encrypted-volumes.md](encrypted-volumes.md) when that proposal lands. Until then, it lives plaintext on `/perm`; operators who want stronger at-rest protection enable per-volume encryption on the edge's `/perm` mount.

## Route lifecycle

### Adding a route

```
capsulectl --capsule capsule-edge edge route add my-blog \
    --hostname blog.example.com \
    --backend web@capsule-storage:8080 \
    --tls auto \
    --acme-challenge http-01
```

Capsuled:

1. **Validate**: hostname is a real DNS name, no other route shares `(hostname, listen_port)`, the backend reference is well-formed.
2. **Resolve the backend in the fabric**. The backend workload must exist on the named capsule and have a `fabric:` block. If not, the route still gets created but is marked `pending_backend`; capsuled retries every 30 s.
3. **Insert** the `edge_routes` row.
4. **Update the proxy's fabric policy**: add `web@capsule-storage:8080/tcp` to its `allow_to`. Push the updated workload spec, which re-runs the fabric publish loop and informs `capsule-storage` to install the ingress allow.
5. **Regenerate the Caddyfile** from `edge_routes`. Write to `/perm/edge/Caddyfile.next`, validate with `caddy validate`, atomically rename to `/perm/edge/Caddyfile`, send the proxy workload a `SIGUSR1` (Caddy reload).
6. **Wait for the first ACME issuance** (auto mode) and report status. Errors at this step are non-fatal to the route — the row stays, status flags `cert_pending`, capsuled polls Caddy's admin API for completion.

### Removing a route

```
capsulectl --capsule capsule-edge edge route remove my-blog
```

1. Delete the SQLite row.
2. Regenerate Caddyfile, validate, reload.
3. Update the proxy's `allow_to` — remove any backend whose only reference was this route. The change propagates to backend capsules' ingress policy on the next fabric publish tick (≤ 5 s).
4. Caddy's stored cert for `blog.example.com` is *retained* for 30 days under `/perm/edge/certs/`. Re-adding the route in that window reuses the cert. After 30 days a janitor sweeps it.

### Replacing the edge capsule

The most operationally interesting flow. To swap out the public-facing box:

1. Bring up the new edge capsule. `capsulectl edge init` records the new public IP.
2. `capsulectl edge route export` on the old edge → JSON blob → `capsulectl edge route import` on the new edge.
3. Copy `/perm/edge/` from old to new (out of band — `capsulectl cp`, manual scp from a debug container, etc.). The ACME account and existing certs come along, so the new edge does not re-issue.
4. Re-point public DNS at the new edge's IP. As DNS propagates, traffic shifts.
5. Old edge: `capsulectl edge disable` — Caddy workload removed, fabric `allow_to` cleared, routes archived.

The "copy `/perm/edge/`" step is the manual seam. A v2 `capsulectl edge migrate <old> <new>` verb that automates it is *Open questions*.

## The proxy: Caddy as a managed workload

The proxy is a regular Capsule workload, declared by capsuled at `edge init` time:

```yaml
kind: Container
name: edge-caddy
spec:
  image: caddy:2-alpine
  command: [caddy, run, --config, /etc/caddy/Caddyfile, --adapter, caddyfile]
  network_mode: BRIDGE
  ports:
    - {host_port: 80, container_port: 80, protocol: tcp}
    - {host_port: 443, container_port: 443, protocol: tcp}
    - {host_port: 443, container_port: 443, protocol: udp}   # HTTP/3
  mounts:
    - {volume_name: edge-state, mount_path: /data}        # ACME account + certs
    - {volume_name: edge-config, mount_path: /etc/caddy}  # capsuled writes Caddyfile here
  fabric:
    # allow_to populated by capsuled from edge_routes; one entry per distinct backend
    allow_to:
      - web@capsule-storage:8080/tcp
      - api@capsule-storage:9000/tcp
```

The host port mappings put the proxy on the edge capsule's public IP. The `fabric:` block puts it on the fabric so it can reach backends. Capsuled owns this workload's spec lifecycle — operators do not `apply -f` it; it is materialized from `edge_self` and `edge_routes`.

### Why Caddy

The alternative is to build a tiny proxy inside capsuled. Rejected:

- **TLS + ACME is a large surface.** HTTP/1.1, HTTP/2, HTTP/3, SNI routing, OCSP stapling, on-line cert renewal, retry logic for ACME failures, the whole IETF ACME state machine. Caddy already does this, well, in production.
- **Reload semantics.** Caddy's `SIGUSR1` reload is graceful: in-flight connections drain on the old config, new connections take the new. Recreating this in capsuled is achievable but adds non-trivial code.
- **Plugin ecosystem.** DNS-01 providers, OAuth2-Proxy integration, the `layer4` plugin for v2 raw-TCP routes — all available without re-implementation.
- **Memory footprint.** Caddy 2 idles at ~30 MiB. Acceptable on a homelab edge.

The cost is one extra container in the workload list and an external dependency that has to be image-pinned. Worth it.

### Caddyfile generation

For each row in `edge_routes`, capsuled emits a Caddyfile block:

```
# auto-generated by capsuled. Do not edit.
# source: edge_routes table (5 rows)

{
    email ops@example.com
    auto_https on
}

blog.example.com {
    tls {
        # 'auto' with http-01: nothing extra; Caddy default
        # 'auto' with dns-01:cloudflare: dns cloudflare {env.CLOUDFLARE_API_TOKEN}
    }
    @real_client {
        remote_ip 173.245.48.0/20 103.21.244.0/22  # trusted_proxies, if any
    }
    handle @real_client {
        # rebuild X-Forwarded-For from RFC 7239 'Forwarded' or the CDN's header
        request_header X-Real-IP {http.request.header.cf-connecting-ip}
    }
    reverse_proxy 100.64.16.10:8080 {
        health_uri /healthz
        health_interval 30s
        # ↑ from edge_routes.backend_workload, resolved via fabric to the workload's fabric IP
        header_up Host {host}
        header_up X-Forwarded-Proto {scheme}
    }
}
```

Backend addresses are resolved at Caddyfile-generation time, not at request time. When a backend's fabric IP changes (workload renamed, capsule reinstalled), the fabric reconciler tick notifies the edge service, which regenerates the Caddyfile and signals a reload. Same name-resolution model as fabric policy: stable names, IPs resolved at compile time.

## TLS / ACME

Three modes:

### `auto` with HTTP-01

Default when the edge capsule is directly DNS-pointed. Caddy listens on `:80`, the ACME server hits `http://blog.example.com/.well-known/acme-challenge/...`, Caddy responds, cert issued. Renewal automatic at 30 days remaining.

Fails if a CDN/LB in front terminates the HTTP-01 challenge — the request never reaches the edge. Use DNS-01 in that case.

### `auto` with DNS-01

Caddy sets a `_acme-challenge` TXT record at the operator's DNS provider via a Caddy DNS plugin (Cloudflare, Route53, etc.), waits for the ACME server to verify, removes the record. Works regardless of what is upstream. Requires a DNS provider API token stored as a secret on the edge.

```
capsulectl --capsule capsule-edge edge secret set CLOUDFLARE_API_TOKEN @/path/to/token
capsulectl --capsule capsule-edge edge route add my-blog \
    --hostname blog.example.com \
    --backend web@capsule-storage:8080 \
    --acme-challenge dns-01:cloudflare
```

The secret is stored in SQLite, wrapped under the node master key (per [encrypted-volumes.md](encrypted-volumes.md)). The Caddy workload receives it as an env var via the existing `env:` spec mechanism. Rotation = re-set; Caddy picks it up on the next reload.

### `manual`

Operator provides cert + key directly. For cases where the cert comes from somewhere else (corporate CA, a wildcard cert managed elsewhere). Stored in `cert_blob` / `key_blob` (key wrapped under master), written by capsuled to Caddy's data volume on every regeneration.

### `none`

HTTP-only route. Useful for back-of-house health checks behind a CDN that does TLS itself, or for `:80` redirects that don't need a cert. Capsuled refuses to set this on a route whose `listen_port` is 443.

### Cert storage

Caddy stores its ACME account and issued certs under `/data` (mounted from the `edge-state` volume). The volume is encryptable per [encrypted-volumes.md](encrypted-volumes.md). Backup story: `/data` is one Capsule volume; the existing `capsulectl cp` flow can pull it.

### ACME failure modes

| Failure                                              | Symptom                                | Recovery                                                                                       |
|------------------------------------------------------|----------------------------------------|------------------------------------------------------------------------------------------------|
| LE rate limit (5 certs / hostname / week)            | Cert issuance returns 429              | Caddy backs off automatically. Route status: `cert_throttled`. Operator waits or uses `manual` |
| DNS-01 token missing or wrong                        | DNS plugin error                       | Route status: `acme_failed`. Fix the secret, run `edge route renew <name>`                     |
| HTTP-01 challenge unreachable (CDN in front)         | LE returns "connection refused"        | Switch to DNS-01 or upstream-bypass the challenge path                                          |
| Expired cert, ACME server unreachable                | Caddy serves the expired cert ~30 days past expiry with a warning header | Caddy retries on a schedule. Route status: `cert_renewal_overdue`. Alerting hook for `capsulectl edge status --watch` |

## Multiple edges (HA)

For an HA story without a single point of failure:

```
                          ┌── capsule-edge-a (public IP A)
DNS round-robin or LB ────┤
                          └── capsule-edge-b (public IP B)
                                  ↓
                          fabric (100.64.0.0/10)
                                  ↓
                          backend workloads
```

- Same route declared on both edges (`edge route add my-blog --hostname blog.example.com ...` on each).
- Each edge does its own ACME issuance — LE rate limits permit it.
- DNS round-robin returns both IPs; clients fail over via their resolver's retry. Or an upstream LB health-checks the edges and balances.
- Backend's fabric ingress allow lists both edges' proxy workloads.

There is **no cross-edge state sync** in v1. Routes are declared per-edge; if they diverge, they diverge. Operator-driven consistency is fine at homelab scale. A `capsulectl edge route sync <a> <b>` convenience verb is *Open questions*.

## Routing details

Public packet path, for `https://blog.example.com → web@capsule-storage:8080`:

1. Client resolves `blog.example.com` to `203.0.113.10` (edge capsule public IP).
2. TCP+TLS handshake to `203.0.113.10:443`, terminated by the Caddy container (host port 443 → container port 443 via the existing port-mapping DNAT).
3. Caddy looks up the SNI/Host, finds the `blog.example.com` block, calls its `reverse_proxy` upstream `100.64.16.10:8080`.
4. Caddy's container has a fabric IP (e.g. `100.64.0.10`). The TCP connection from `100.64.0.10 → 100.64.16.10:8080` follows the fabric path documented in [fabric.md](fabric.md): out via `wg-fabric`, encrypted, into `capsule-storage`'s `wg-fabric`, ingress-policy match, in to `veth-web`, into the backend workload.
5. Backend responds, packet returns the same way reversed.

The two policy gates are both load-bearing:

- **Public ingress on the edge**: only declared `hostname:port` combos are accepted. Caddy `400`s the rest.
- **Fabric ingress on the backend**: only the proxy workload is in `allow_from`. A direct fabric-to-backend connection from any other workload is dropped.

Removing either gate widens the blast radius. Both belong.

### Real client IP propagation

When an upstream LB / CDN is configured, the edge's Caddy block has `trusted_proxies <cidrs>` and a `request_header X-Real-IP {http.request.header.cf-connecting-ip}` (or equivalent) so backends always see the original client IP in a known header. Without this, `X-Forwarded-For` from arbitrary clients is honored and an attacker spoofs identity. Capsuled enforces: a route with `trusted_proxies = ""` strips any inbound `X-Forwarded-For` header; a route with `trusted_proxies = "1.2.3.4/32"` accepts it only from that CIDR.

## Failure scenarios

### Edge-specific

| Failure                                              | Symptom                                | Recovery                                                                                       |
|------------------------------------------------------|----------------------------------------|------------------------------------------------------------------------------------------------|
| Caddy workload crashes                               | All edge routes return TCP RST         | capsuled's existing workload restart-policy brings it back. RTO ~5 s. Routes resume |
| Edge capsule loses uplink                            | DNS still points at the dead IP        | If multi-edge, DNS round-robin partial outage. If single edge, full outage. Operator points DNS at backup edge or upstream LB peels the dead origin |
| Backend's fabric IP changes                          | Caddy upstream returns "no route"      | Fabric publish updates → capsuled regenerates Caddyfile → reload. ≤ 30 s convergence            |
| Backend deleted but route remains                    | Route status: `backend_missing`; Caddy serves 502 | Route stays in SQLite (operator-declared state of the world). Remove the route or re-create the backend |
| Cert expired, ACME unreachable                       | Caddy serves expired cert with warning | See ACME failure modes above. Backend reachability does not depend on cert validity |
| Public IP changes (DHCP from ISP)                    | DNS now stale                          | Operator updates DNS. v1 has no dynamic DNS integration; *Open questions* covers a hook |
| Edge capsule disk lost                               | All ACME state + manual certs gone     | If `/perm/edge/` is on an encrypted volume backed up off-box, restore. Otherwise re-issue all certs from scratch on a new edge (subject to LE rate limits) |

### Configuration mistakes

| Failure                                              | Symptom                                | Recovery                                                                                       |
|------------------------------------------------------|----------------------------------------|------------------------------------------------------------------------------------------------|
| Two routes claim the same `(hostname, listen_port)`  | `edge route add` refuses               | Pick a different hostname or remove the conflicting route                                       |
| Route references a backend without `fabric:`         | Route status: `pending_backend`        | Add `fabric:` to the backend spec, re-apply. Capsuled retries automatically                     |
| `trusted_proxies` typo (CIDR malformed)              | `edge route add` refuses               | Caddyfile is validated before reload — bad config never goes live                              |
| Operator points DNS at an edge that doesn't have the route | Caddy 400 / TLS SNI rejection         | Add the route on the right edge, or fix DNS                                                    |
| Manual cert provided in `manual` mode, expired       | Caddy serves expired cert              | Operator re-uploads via `edge route update --cert/--key`. No auto-renewal in manual mode       |

### Capsuled / proxy interaction crashes

| Crash point                                          | State left behind                                | Cleanup                              |
|------------------------------------------------------|--------------------------------------------------|--------------------------------------|
| Mid `route add`, after SQLite commit but before Caddy reload | Route in DB, Caddy still on old config | Reconciler tick: detect Caddyfile drift from `edge_routes`, regenerate, reload |
| Mid `route add`, after Caddy reload but before fabric policy push | Caddy has the route, backend's ingress still denies | Fabric publish loop retries on every tick; convergence ≤ 30 s |
| Mid `route remove`, after Caddyfile reload but before allow_to update | Caddy 502s (no upstream), fabric ingress still allows | Reconciler: walk `edge_routes`, recompute proxy `allow_to` union, push |
| capsuled OOM / restarted                             | Caddy keeps serving on the last loaded config; ACME runs inside Caddy and is unaffected | On start, capsuled regenerates Caddyfile from `edge_routes` and reloads. No public outage |

The pattern matches the other proposals: **the SQLite write is the commit point**, and reconciliation is idempotent. A reload of capsuled rebuilds the proxy config from the table without any operator action.

## What this does NOT protect against

- **Application-layer attacks on the backend.** The edge gates *who connects*. What the connecting client says after the TLS handshake is the backend's responsibility. Run a WAF in front (CDN, mod_security as a sidecar, etc.) if the workload demands it.
- **DDoS at the IP layer.** A flood of SYNs to `203.0.113.10:443` saturates the edge's uplink long before Caddy gets involved. Put a CDN in front for serious public traffic.
- **A compromised edge capsule reading backend traffic.** TLS is terminated at the edge; the edge sees plaintext. If the threat model requires end-to-end encryption past the edge, the backend has to terminate its own TLS and the edge passes through (TLS passthrough is supported by Caddy via SNI routing without termination — possible v2 feature).
- **An attacker who controls public DNS.** They can repoint `blog.example.com` at their own server. Capsule cannot defend against that — it is upstream of every gate the edge owns.
- **Cert misissuance by a compromised CA.** Out of scope; mitigation lives in upstream CAA records and CT monitoring, neither of which Capsule manages.

## Open questions

- **TCP / UDP routes (L4).** Caddy has a `layer4` community plugin but it is not in the official build. Options: (a) build a Caddy image with `layer4` baked in, (b) ship a tiny standalone TCP forwarder workload, (c) defer. Lean (c) for v1; revisit when someone needs an exposed Postgres or game server.
- **Reverse tunnels for CGNAT homelabs.** No-public-IP fleets need a different model — a small public VPS that the homelab dials *out* to, and the public VPS proxies back. Could be Cloudflare Tunnel (third-party), `frp`, or a custom WireGuard-over-public-relay. The shape that fits Capsule best is probably "the public VPS is a Capsule too, but its 'fabric peer' role is reversed — it accepts dial-out from the homelab edge over TCP/TLS, and tunnels HTTP back." Significant scope; deserves its own proposal.
- **Cert sharing across edges.** Today each edge issues its own LE cert. A `cert-store` capsule that holds the LE account and pushes issued certs to subscribing edges would avoid duplicate issuance and rate-limit pressure. Probably overkill for homelab scale, but documenting the model so we can build it later.
- **Dynamic DNS for residential IPs.** Edge capsules on ISPs with rotating IPs need an automated way to update DNS. Possible: `capsulectl edge dns-update-hook <script>` that runs whenever the public IP changes (capsuled monitors the IP on the public interface). Operator-script handles the actual provider call. v1: document the manual flow.
- **mTLS between CDN and edge.** If `trusted_proxies` is the only thing keeping spoofed-XFF attacks out, an attacker with the CDN's source-IP range can bypass it. Optional mTLS where the CDN presents a cert capsuled pins per-route closes that gap. Caddy supports `client_auth`. Wire it up in v1.5 if anyone asks.
- **Should `edge init` be reversible?** `edge disable` removes the Caddy workload and clears `edge_self`, but leaves cert material on disk for restoration. A nuclear `edge reset` that scrubs `/perm/edge/` is needed; how loud should the confirmation be? Probably match `capsule update push`'s confirmation pattern.
- **Per-route rate limits and bot filtering.** Caddy has modules; Capsule could surface a small subset (`--rate-limit 100/m`). Useful for hobby blogs that get scraped. Probably yes in v1.5.

## Implementation pointers

- Proto: new `models/capsule/v1/edge.proto` with `EdgeService` (`Init`, `Disable`, `RouteAdd`, `RouteUpdate`, `RouteRemove`, `RouteList`, `RouteGet`, `Status`, `SecretSet`, `SecretRemove`, `RotateACMEAccount`).
- Schema: `edge_self`, `edge_routes` (above), plus an `edge_secrets` table (name → wrapped value) used for ACME provider tokens.
- Logic: new `core/edge/` package — owns the Caddyfile renderer, the ACME-status poller (talks to Caddy's admin API at `127.0.0.1:2019` over a loopback-only bind), the secrets store, the reconciler tick that diffs `edge_routes` against the live Caddyfile.
- Runtime: no driver changes. The Caddy workload is a normal container with normal port mappings and a `fabric:` block.
- Image: no host-side packages needed. The Caddy image is pulled like any other.
- Boot: capsuled, at startup, checks for `edge_self`; if present and the proxy workload is missing from the reconciler, re-creates it. Routes are loaded into the renderer; Caddyfile regenerated; Caddy started.
- CLI: `capsulectl edge {init, disable, status, route-add, route-update, route-remove, route-list, route-get, secret-set, secret-remove, rotate-acme-account}`.

The edge is **strictly additive** to today's networking and to the fabric: a capsule with no `edge_self` row is not an edge, and a workload that no route references is not public. The two gates compose — fabric isolates, edge exposes — and removing either falls back to the model it sits on top of.
