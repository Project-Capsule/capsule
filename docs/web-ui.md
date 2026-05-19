# Web UI: a browser console for a fleet of capsules (proposal)

> **Status:** Proposal. Not implemented. This proposes a browser-based console for
> observing and operating **multiple** capsules at once, **without** adding any HTTP/REST
> surface to `capsuled`. The daemon stays gRPC-only on `:50000`; a separate
> `capsule-console` component carries the gRPC↔JSON gateway, the multi-capsule fan-out,
> its own user identity/RBAC, and custody of the per-capsule operator keys.

## Summary

Today the only operator interface is `capsulectl`: one binary, one capsule per
invocation, driven from a terminal. That is the right primitive and it stays. But a
homelab grows to a handful of machines (the discovery proposal already assumes seven),
and "what is every capsule doing right now, and let me poke the one that's unhappy"
is a poor fit for a CLI loop. There is no fleet-wide view, no point-and-click `apply`,
no shared read-only dashboard for someone who isn't fluent in the CLI.

This proposal adds **`capsule-console`**: a standalone process that serves a
single-page web app plus a [grpc-gateway][ggw]-style JSON transcoder, dials each
adopted capsule over the **existing** authenticated gRPC API, and presents a
fleet-wide UI. It is explicitly **not** part of `capsuled` and adds **no** REST to it.

Two properties shape everything below:

1. **`capsuled` is never modified.** The console is a client. It speaks the same
   pinned-TLS + EdDSA-JWT gRPC that `capsulectl` speaks today. The grpc-gateway
   transcoder is compiled into `capsule-console` only — never into the daemon.
2. **The console is a multi-capsule, multi-user service.** It holds a context
   registry (the same shape as `~/.config/capsule/config.yaml`), custodies the
   operator signing keys, has its own user accounts + RBAC, and acts as a
   deliberate confused deputy: a browser user authenticates to the *console*, the
   *console* authenticates to each *capsule*.

## Why this shape

- **Keeps `capsuled` minimal.** The daemon's whole thesis is one binary, one small
  declarative gRPC surface. A web server, TLS-for-browsers, session cookies, CSRF,
  static assets, and a JSON API do not belong in PID 1. Pushing all of that into a
  separate, restartable, independently-deployed process preserves the thesis.
- **REST stays out of the daemon, by construction.** grpc-gateway generates a
  reverse-proxy from proto annotations. The annotations are inert proto options;
  the generated proxy code links into `capsule-console`, not `capsuled`. Nothing
  about the daemon's build or runtime changes. This directly answers "I don't want
  capsuled to include REST."
- **Multi-capsule is the point, not an afterthought.** The console owns the fleet
  registry and fans out. `capsulectl`'s one-capsule-per-invocation model is great
  for scripting and bad for "show me everything"; the console fills exactly that gap
  and reuses the discovery proposal's mDNS browse for bringup.
- **It reuses every existing building block.** The auth path (`auth.Mint`, fingerprint
  pinning), the context file format, the proto services, the discovery announcer —
  the console is glue, not new protocol.
- **Deployable either place.** The same binary runs on the operator's laptop
  (loopback, one admin) or as a shared fleet service (a workload on a capsule, or a
  dedicated host). Same code, different bind + user table size.

## Goals

- A **fleet dashboard**: every adopted capsule, its reachability, slot/version,
  workload health, resource pressure — one screen, auto-refreshing.
- **Drill into one capsule**: workloads, volumes, images, A/B update state, host logs.
- **Operate** from the browser: `apply` a manifest, start/stop/restart/delete a
  workload, stream workload + host logs, an interactive `exec` terminal, push an
  image, drive an A/B update with the confirm/rollback window visible.
- **The console has its own identity model**: named users, login, per-user RBAC
  (at minimum: viewer / operator / admin), and a per-user audit log of every
  mutating action and the capsule it targeted.
- **`capsuled` is byte-for-byte unchanged.** No new RPC required for v1; no REST.
- **One binary, two deployment modes** (laptop loopback ↔ shared service) with the
  same code path.

## Non-goals (v1)

- **No console in the data path.** The console proxies control-plane RPCs; it is not
  an ingress for workload traffic. Exposing workloads to users is `edge.md`/`fabric.md`.
- **No multi-tenant capsule isolation.** RBAC scopes *who can do what to which
  capsule via the console*; it does not partition a single capsule between tenants.
- **No config drift / GitOps reconciliation.** The console applies what you submit;
  it is not a desired-state controller sitting above the per-capsule reconciler.
- **No write path that bypasses the gRPC API.** The console cannot do anything
  `capsulectl` couldn't; it has no privileged side channel into a capsule.
- **No bidi-streaming features beyond `exec`.** `exec` (TTY) is the one interactive
  stream; everything else is request/response or server-stream.
- **Mobile-first / offline.** Desktop browser, online, same-LAN-or-fabric assumption.

## Current constraints (from today's architecture)

- `capsuled` serves **gRPC only** on `:50000`. Browsers cannot speak raw gRPC (HTTP/2
  trailers, framing). Something must transcode.
- Auth is **pinned server TLS + a per-call EdDSA JWT** minted from the operator's
  Ed25519 key, `aud = capsule_id` (`cmd/capsulectl/auth.go`, `auth.Mint`). There is
  **no CA** — TLS is self-signed and pinned by SHA-256 fingerprint. There is no
  client-cert mTLS; the JWT *is* the client credential.
- The set of reachable capsules is a **context registry** today
  (`~/.config/capsule/config.yaml`: `addr`, `capsule_id`, `tls_fingerprint_sha256`,
  `key_path`). The console needs the same data, server-side.
- Several core operations are **streaming**: `WorkloadService.Logs`,
  `CapsuleService.StreamLogs` (server-stream); `Exec` (bidi, TTY); `UpdateOS`,
  `ImageService.Push`, `CopyTo/From` (client-stream / large upload). A naive
  JSON gateway handles unary cleanly and streaming poorly — this needs design.
- mDNS discovery (`discovery.md`) is the bringup path. It is proposal-stage; the
  console should consume it if present but not require it.

These are compatible with a browser console, but they force an explicit transcoding
+ key-custody design rather than "just point a UI at the API."

## Architecture

```
┌─ browser ─────────┐         ┌─ capsule-console (one process) ───────────────────┐
│  SPA (static)     │  HTTPS  │  ┌──────────┐  ┌───────────────┐  ┌────────────┐  │
│  fleet dashboard ─┼────────▶│  │ user/RBAC│  │ grpc-gateway  │  │ key vault  │  │
│  capsule views    │  cookie │  │  + audit │─▶│  JSON↔gRPC    │─▶│ ed25519 ×N │  │
│  exec (WebSocket) │◀───────▶│  └──────────┘  │  + WS bridge  │  └─────┬──────┘  │
└───────────────────┘   SSE   │                └───────┬───────┘        │         │
                              │   context registry ────┴────────────────┘         │
                              └──────────────┬───────────────┬────────────────────┘
                                  pinned TLS │   per-call     │ pinned TLS
                                   EdDSA JWT  ▼   EdDSA JWT     ▼  EdDSA JWT
                              ┌─ capsule A ─┐ ┌─ capsule B ─┐ ┌─ capsule C ─┐
                              │ capsuled    │ │ capsuled    │ │ capsuled    │
                              │ :50000 gRPC │ │ :50000 gRPC │ │ :50000 gRPC │
                              └─────────────┘ └─────────────┘ └─────────────┘
```

Four pieces inside the one `capsule-console` binary:

1. **Static SPA — Svelte.** The frontend is **Svelte** (SvelteKit with the
   static adapter, or plain Svelte + Vite), built to static assets and embedded in
   the Go binary via `embed.FS`. No CDN, no SSR, no Node runtime at serve time, no
   external fetches — the build produces plain HTML/JS/CSS that the console serves
   itself, consistent with "single binary, no host daemons." Svelte is chosen for
   the small shipped bundle (compiled, no virtual-DOM runtime) and because the
   compile-to-static-assets model fits `embed.FS` cleanly. Node is a build-time-only
   dependency, never present on a capsule or in the runtime image.
2. **User/RBAC + audit layer.** Owns console accounts, sessions (HttpOnly cookie),
   and a role per `(user, capsule)` or `(user, *)`. Every mutating request is
   written to an append-only audit log *before* it is proxied: who, when, which
   capsule, which RPC, the request summary, the outcome.
3. **gRPC-gateway transcoder + WebSocket bridge.** Unary and server-streaming RPCs
   go through generated grpc-gateway handlers (JSON in, JSON / SSE out). `Exec`
   (bidi TTY) and the large client-streams (`UpdateOS`, `ImagePush`, `cp`) go
   through hand-written WebSocket / chunked-upload bridges, because grpc-gateway's
   bidi support is weak.
4. **Per-capsule key vault + dialer.** Holds one Ed25519 key + pinned fingerprint +
   `capsule_id` per capsule (the context registry, server-side). For each proxied
   call it mints a fresh short-lived JWT (`auth.Mint`) for the *target* capsule and
   dials with the existing pinned-TLS config. **Capsule keys never reach the
   browser.**

### Why grpc-gateway and not gRPC-Web

gRPC-Web still needs a proxy (Envoy or the Go grpc-web wrapper) and ships protobuf to
the browser. grpc-gateway gives a plain JSON/HTTP API the SPA can consume with `fetch`,
an OpenAPI document for free, and keeps the browser contract decoupled from protobuf
wire details. The cost is proto annotations — addressed next.

### Keeping REST out of `capsuled`

grpc-gateway needs `google.api.http` annotations on the RPCs it transcodes. Two ways
to supply them without touching the daemon:

- **Annotate via a separate gateway proto** that imports `capsule/v1` and re-declares
  the HTTP bindings, compiled only in the console's `buf` target; or
- **buf managed-mode / a gateway-only `buf.gen.yaml`** that emits
  `protoc-gen-grpc-gateway` + `protoc-gen-openapiv2` outputs into a
  `console/`-scoped package.

Either way the generated reverse-proxy is linked into `capsule-console`. `capsuled`'s
build target gains nothing and serves nothing over HTTP. This is the crux of the
"no REST in capsuled" requirement and it holds by construction, not by convention.

### Streaming: the three hard RPCs

| RPC(s) | gRPC shape | Browser transport |
|--------|-----------|-------------------|
| `Workload.Logs`, `Capsule.StreamLogs` | server-stream | **SSE** (`text/event-stream`), one event per log line; `follow`/`tail` map to query params |
| `Workload.Exec` (`-t`) | bidi | **WebSocket**: stdin frames up, stdout/stderr + exit down; terminal resize as a control frame |
| `UpdateOS`, `Image.Push`, `CopyTo` | client-stream (large) | **chunked upload** over a WebSocket or `Transfer-Encoding: chunked` POST, bridged to the client-stream; progress events back down |
| everything else | unary | grpc-gateway JSON |

`exec` is the only place the console maintains a long-lived bidi bridge. It is also
the highest-risk feature (interactive root shell into any workload) — see RBAC and
threat model.

## Multi-capsule model

The console's context registry is the fleet. It is the existing config shape, owned
by the console process (file or its own SQLite, mode 0600), seeded three ways:

- **Import** an operator's existing `~/.config/capsule/config.yaml`.
- **Discover + adopt** in-browser, reusing `discovery.md`'s mDNS browse: the console
  runs the browse, lists unadopted machines with directly-fetched fingerprints, the
  admin verifies against HDMI and adopts — the same TOFU ceremony as
  `capsulectl discover --adopt`, just with a confirm button.
- **Manual add**: addr + fingerprint + key.

Fan-out for the dashboard is N parallel unary calls (`CapsuleService.GetInfo`,
`WorkloadService.List`) with a short per-capsule timeout; an unreachable capsule
renders as a degraded tile, never blocks the page. This mirrors discovery's
"`unreachable` row instead of silent drop" rule.

## Identity, RBAC, and the confused-deputy boundary

The console is intentionally a confused deputy: it holds keys to the whole fleet and
acts on behalf of browser users. That power must be gated by the console's own
identity model, not by the capsules (capsules only see "a valid operator key").

- **Console users.** Local accounts (argon2id) in v1; OIDC/SSO is an open question.
  First-run bootstrap creates one `admin`.
- **Roles**, scoped per `(user → capsule)` or `(user → *)`:
  - `viewer` — read-only: dashboard, workload/volume/image list, logs.
  - `operator` — viewer + `apply`, lifecycle, `cp`, image push, **exec**.
  - `admin` — operator + manage the context registry, the key vault, console users,
    and drive A/B updates / confirm.
- **Audit log.** Append-only, per user, every mutating proxied RPC + target capsule
  + outcome. This is the accountability layer the capsules can't provide (to a
  capsule, all console actions look like the same operator key).
- **Key custody.** Capsule signing keys live only in the console's vault, never sent
  to the browser, never logged. Loopback/laptop mode may keep them as on-disk files
  (like `capsulectl`); shared-service mode should support an encrypted-at-rest vault
  (ties into `secrets.md` / `encrypted-volumes.md` if present).

## Deployment modes

Same binary, two postures:

- **Laptop / loopback.** `capsule-console` binds `127.0.0.1`, single bootstrap admin,
  reuses the operator's existing on-disk contexts and keys. Effectively a richer,
  multi-capsule `capsulectl`. Lowest blast radius.
- **Shared fleet service.** Runs as a `Container` workload on a capsule (or a
  dedicated host), reachable by the team over the LAN/fabric. Real user table, RBAC,
  encrypted key vault, TLS for the browser side (its own cert; the SPA pins nothing —
  standard web PKI or an internal CA is the operator's call). Highest convenience,
  highest-value target — hardened per the threat model.

The console must refuse to start in shared mode bound to a non-loopback address
without browser-side TLS configured.

## Wireframes

ASCII mockups — layout intent, not final visual design.

### Login (shared-service mode; skipped on loopback single-admin)

```
┌────────────────────────────────────────────────────────────┐
│                        ▣  capsule                           │
│                                                              │
│            user  [ aaron___________________ ]               │
│            pass  [ •••••••••••••••••••••••• ]               │
│                                                              │
│                      [   sign in   ]                         │
│                                                              │
│   first run? the bootstrap admin was printed to the console  │
└────────────────────────────────────────────────────────────┘
```

### Fleet dashboard (landing screen)

```
┌ capsule ─────────────────────────────  aaron (admin) ▾  [+ add capsule] ┐
│                                                                          │
│  FLEET   7 capsules · 5 healthy · 1 degraded · 1 unreachable             │
│  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌──────────────┐    │
│  │ nuc-1     ●  │ │ nuc-2     ●  │ │ nuc-3     ●  │ │ nuc-4     ◐  │    │
│  │ slot_a 0513  │ │ slot_a 0513  │ │ slot_b 0513  │ │ slot_a 0510  │    │
│  │ wl 6/6 ✓     │ │ wl 4/4 ✓     │ │ wl 9/9 ✓     │ │ wl 3/4 ⚠     │    │
│  │ cpu ▓▓░ 41%  │ │ cpu ▓░░ 22%  │ │ cpu ▓▓▓ 78%  │ │ cpu ▓▓░ 55%  │    │
│  │ mem ▓▓░ 60%  │ │ mem ▓░░ 31%  │ │ mem ▓▓░ 64%  │ │ mem ▓▓▓ 88%  │    │
│  └──────────────┘ └──────────────┘ └──────────────┘ └──────────────┘    │
│  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐                     │
│  │ nuc-5     ●  │ │ gpu       ●  │ │ edge      ✕  │   ◐ update pending  │
│  │ slot_a 0513  │ │ slot_a 0513  │ │  unreachable │   ✕ no TLS / down   │
│  │ wl 2/2 ✓     │ │ wl 5/5 ✓     │ │  last seen   │   ● healthy         │
│  │ cpu ▓░░ 12%  │ │ cpu ▓▓▓ 91%  │ │  3m ago      │   ⚠ degraded        │
│  └──────────────┘ └──────────────┘ └──────────────┘                     │
└──────────────────────────────────────────────────────────────────────────┘
```

### Capsule detail

```
┌ ← fleet · nuc-4 ────────────────────────  192.168.10.104:50000  ●─slot_a ┐
│ [Workloads] Volumes  Images  Updates  Host logs                          │
│                                                                          │
│ NAME          KIND       PHASE      IMAGE                  CPU   MEM      │
│ ───────────────────────────────────────────────────────────────────     │
│ api           Container  ● Running  myhost/api:dev         0.4   210Mi   │
│ web           Container  ● Running  caddy:2                 0.1    44Mi   │
│ db            Container  ● Running  postgres:16             0.6   512Mi   │
│ batch         MicroVM    ✕ Failed   myhost/batch:v3         —      —      │
│   └ exited 1 · CrashLoopBackOff · [logs] [restart] [get yaml]            │
│                                                                          │
│ [+ apply manifest]   selected: batch  →  [logs] [exec] [stop] [delete]   │
└──────────────────────────────────────────────────────────────────────────┘
```

### Workload logs (SSE)

```
┌ nuc-4 · batch · logs ───────────  [✓ follow]  tail [200▾]  [⬇ download] ┐
│ 12:01:03 starting batch worker v3                                        │
│ 12:01:03 connecting to db postgres://db:5432                             │
│ 12:01:04 FATAL: dial tcp: connection refused                            │
│ 12:01:04 exit status 1                                                   │
│ ── reconciler: restarting (attempt 7) ───────────────────────────────    │
│ ▌ live                                                                   │
└──────────────────────────────────────────────────────────────────────────┘
```

### Exec terminal (WebSocket, `operator`+ only)

```
┌ nuc-4 · api · exec  /bin/sh ─────────────────────────  [80×24]  [✕ close]┐
│ / # ps                                                                   │
│ PID   USER  COMMAND                                                      │
│ 1     root  /api --config /etc/app/config.toml                           │
│ 24    root  /bin/sh                                                      │
│ / # ▌                                                                    │
│                                                                          │
│ ⚠ audited as aaron@nuc-4 · interactive shell · started 12:04:11          │
└──────────────────────────────────────────────────────────────────────────┘
```

### Apply manifest

```
┌ nuc-4 · apply ───────────────────────────────────────────  [validate] ┐
│ name: batch                                                            │
│ kind: MicroVM                                                          │
│ microvm:                                                               │
│   image: myhost/batch:v3                                               │
│   command: ["/batch"]                                                  │
│   env: { DB_HOST: db }                                                 │
│                                                                        │
│ ───────────────────────────────────────────────────────────────────   │
│ ✓ schema ok   ⚠ references workload "db" — present on nuc-4            │
│                              [cancel]  [apply to nuc-4]                 │
└────────────────────────────────────────────────────────────────────────┘
```

### A/B update (`admin`)

```
┌ nuc-4 · updates ───────────────────────────────────────────────────────┐
│ active  slot_a · 20260510-141000      last good  slot_a · 20260510      │
│ bundle  [ choose update.tar … ]  20260513-120000  (412 MiB)             │
│                                                  [push to nuc-4]        │
│ ───────────────────────────────────────────────────────────────────    │
│ ⏳ tentative on slot_b — auto-rollback in 08:41 if not confirmed        │
│    health: gRPC ✓  workloads 4/4 ✓                                      │
│                               [confirm slot_b]   [rollback now]         │
└─────────────────────────────────────────────────────────────────────────┘
```

### Discover & adopt (reuses `discovery.md` mDNS)

```
┌ add capsule · discover ──────────────────────────  [rescan]  [manual +] ┐
│ NAME          ADDRESS              FINGERPRINT          ADOPTED          │
│ capsule-f1b2  192.168.10.110:50000 f1:b2:7c:3a:9e…      no   [adopt ▸]  │
│ capsule-9e3c  192.168.10.120:50000 9e:3c:4f:8d:2a…      no   [adopt ▸]  │
│ nuc-1         192.168.10.101:50000 a3:f2:1c:9d:4e…      yes (in fleet)  │
│ ──────────────────────────────────────────────────────────────────     │
│ verify the fingerprint against the machine's HDMI before adopting.      │
└──────────────────────────────────────────────────────────────────────────┘
```

### Users & RBAC (`admin`)

```
┌ settings · users ──────────────────────────────────────────  [+ user] ┐
│ USER     ROLE                       SCOPE                                │
│ aaron    admin                      *                                   │
│ sam      operator                   nuc-1, nuc-2, nuc-3                  │
│ guest    viewer                     *                                   │
│ ───────────────────────────────────────────────────────────────────    │
│ audit: 142 actions today · [view audit log]                             │
└─────────────────────────────────────────────────────────────────────────┘
```

## Threat model

| Threat | Protected? |
|--------|------------|
| Console compromise = whole-fleet compromise (it holds every key) | **Partial / by design.** The console is the highest-value target precisely because it custodies fleet keys. Mitigations: encrypted-at-rest vault in shared mode, loopback-only default, refuse non-loopback bind without browser TLS, mandatory append-only audit, short JWT lifetimes (no long-lived bearer leaves the vault). It is not eliminated — it is the cost of a fleet console and is called out explicitly. |
| Browser user escalates past their role | Console enforces RBAC *before* proxying; the capsule cannot help (every console call uses the same operator key). RBAC + audit are the only boundary — they must be server-side and fail-closed. |
| XSS / CSRF stealing a session into fleet-wide power | HttpOnly + SameSite cookies, CSRF tokens on mutations, strict CSP, no third-party origins (assets embedded). Exec/upload WebSockets re-check session + role per connection. |
| Capsule keys exfiltrated via the browser | Keys never serialize to any browser-bound response; the vault is server-only; audit never logs key material. |
| MITM between console and a capsule | Unchanged from `capsulectl`: pinned self-signed TLS + per-call EdDSA JWT (`aud=capsule_id`). The console pins the same fingerprint the context stores; mismatch fails the dial. |
| MITM between browser and shared console | Standard web PKI / internal CA on the browser side (operator-provided). Loopback mode sidesteps this. |
| A spoofed mDNS entry during in-browser adopt | Same posture as `discovery.md`: fingerprint is fetched by direct TLS, not trusted from mDNS, and the admin verifies against HDMI before the adopt button does anything. |
| `exec` abused for lateral movement | `operator`+ only, every session audited with user + capsule + workload + start time, visible banner in the terminal pane. Consider an admin-only kill-switch for active exec sessions. |

## Implementation phases

### Phase 1: read-only fleet console (loopback)

- `capsule-console` binary: embedded **Svelte** SPA, context registry import from
  `~/.config/capsule/config.yaml`, per-capsule key vault + dialer reusing
  `auth.Mint` / `pinningTLSConfig`. Establish the Svelte build → `embed.FS` →
  `make`-target pipeline here so every later phase just adds views.
- grpc-gateway transcoder for the **unary read** RPCs only: `GetInfo`,
  `Workload.List/Get`, `Volume.List/Get`, `Image.List`.
- Fleet dashboard + capsule detail + workload/volume/image tables.
- Single bootstrap admin, loopback bind, no write path.

### Phase 2: operate

- Mutating unary RPCs: `Workload.Apply/Delete/Restart/Stop/Start`, `Volume.*`.
- SSE bridge for `Workload.Logs` and `Capsule.StreamLogs`.
- WebSocket bridge for `Workload.Exec` (TTY, resize).
- Chunked-upload bridges for `Image.Push`, `CopyTo/From`.
- The console-side **audit log** lands here, before the first write ships.

### Phase 3: multi-user + shared deployment

- Console user accounts (argon2id), sessions, RBAC scoped per `(user, capsule)`.
- Shared-service posture: non-loopback bind + browser TLS, encrypted key vault,
  ship as a `Container` workload manifest in `examples/`.
- Users & RBAC + audit-log UI.

### Phase 4: A/B updates + discovery integration

- Updates pane: `UpdateOS` upload, tentative window countdown, `UpdateConfirm`,
  rollback — admin-gated, with the safety rails the CLI flow has.
- In-browser discover + adopt, consuming `discovery.md`'s mDNS browse and the TOFU
  fingerprint ceremony.

## Open questions

- **Vault at rest in shared mode.** Roll our own age/argon2 envelope, or wait for
  `secrets.md` / `encrypted-volumes.md` and reuse the node master key? Leaning:
  ship a self-contained envelope in Phase 3, migrate onto `secrets.md` if it lands.
- **SSO/OIDC vs. local accounts.** Local argon2id is enough for v1. OIDC is the
  obvious homelab ask (one SSO for everything). Phase 3+ if demanded.
- **SvelteKit vs. plain Svelte + Vite.** SvelteKit's static adapter
  (`adapter-static`, fully prerendered, no Node server) embeds just as cleanly and
  gives routing/layouts for free; plain Svelte + Vite is leaner if SvelteKit's
  conventions feel heavy for a single embedded SPA. Either keeps Node build-only.
  Decide at Phase 1 scaffolding; does not affect the backend contract.
- **Where do gateway annotations live** — a separate gateway proto vs. buf
  managed-mode — and does that warrant a `console/` proto package? Decide before
  Phase 1 codegen; both keep `capsuled` REST-free, this is ergonomics.
- **Context address auto-update.** Same DHCP-lease problem `discovery.md` flags: a
  capsule's IP rotates and the registry is stale. Probably a "rediscover & refresh"
  action in the fleet view; shares discovery's open question.
- **One console fanning out vs. console-per-segment.** For multi-VLAN fleets (mDNS
  is link-local), is it one console reaching all segments by routed gRPC, or one per
  segment? Out of scope for v1 (single-LAN assumption) but worth flagging.
- **Exec session governance.** Should an admin be able to see and kill another
  user's live `exec`? Probably yes; sketch a `console exec sessions` view in Phase 3.
- **Does the console ever need a new RPC?** v1 deliberately needs none. A future
  cheap win would be a single `Capsule.Health` summarizing reconciler state so the
  dashboard isn't composing it from `GetInfo` + `Workload.List` per tile.

## Recommendation

Build **`capsule-console`** as a standalone, never-in-`capsuled` component:

1. Start **read-only and loopback** (Phase 1) — proves the gateway + fan-out + key
   custody with zero write risk and zero daemon changes.
2. Add the **write path with the audit log shipped alongside it**, never after
   (Phase 2). The audit log is not a nice-to-have; it is the only accountability the
   capsules structurally cannot provide.
3. Layer **multi-user RBAC and the shared-service posture** (Phase 3), then
   **updates + in-browser discovery** (Phase 4).

This gives a fleet-wide browser console that materially improves multi-capsule
operations, while keeping `capsuled` exactly as it is: one binary, one small
gRPC surface, **no REST** — the console carries every gram of the new complexity,
by construction.

[ggw]: https://github.com/grpc-ecosystem/grpc-gateway
