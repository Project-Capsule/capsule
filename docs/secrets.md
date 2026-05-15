# Secrets and credentials (proposal)

> **Status:** Proposal. Not implemented. The edge proposal references `edge secret set` and
> an `edge_secrets` table as if secrets were a solved problem — they are not. That verb and
> table are subsumed by this proposal. The encrypted-volumes proposal's node master key and
> key-wrapping hierarchy is the cryptographic foundation this proposal builds on; read that
> first.

## The problem is actually two problems

Every secrets system conflates two things that have different threat models, different
lifecycles, and different right answers:

**Class A — startup credentials.** A database password. A TLS client cert. An API key that
an application needs before it can answer its first request. The application genuinely needs
the plaintext at start time; there is no way to avoid it. The threat is at rest — the
credential sitting unencrypted on disk or in a config file, readable to anyone who can
mount the disk or read the filesystem. The right answer here is encrypted storage with
automatic unsealing.

**Class B — ambient API credentials.** A Stripe key an AI agent uses when it calls a payment
tool. A Grafana API token used to query metrics. A GitHub token used to open a PR. The
workload needs to *make a call* using these credentials, but it does not need to *hold*
the credential — a broker can hold it and proxy the call. In fact, handing the credential
to the workload is the wrong default: a compromised workload can now exfiltrate the key
and use it forever, far outside the scope of the original task. The right answer here is
a credential broker that authenticates the caller, applies policy, and either proxies
the call or issues a scoped token with a short expiry.

This proposal addresses both, with intentionally different mechanisms.

---

## Goals

- **Class A: sealed secrets.** Operator-defined secrets encrypted under the node master
  key (from [encrypted-volumes.md](encrypted-volumes.md)). Injected into workloads as
  environment variables or file mounts at start time. Plaintext never on disk; never
  in SQLite except wrapped.
- **Class B: credential broker.** A managed workload that holds real credentials (as sealed
  secrets), authenticates callers via their fabric identity, applies per-workload policy, and
  either proxies API calls (workload never sees the credential) or issues time-limited tokens
  scoped to a single operation or time window.
- **Just-in-time issuance with optional human approval.** A workload can request a credential
  for a specific operation. Policy decides: auto-approve, require manual operator sign-off, or
  deny. Once approved, the credential is granted for a declared TTL and then revoked.
- **Privilege downgrade.** A workload can start with broader credentials, complete
  initialization, then explicitly downgrade its own access. Or it can start with no
  credentials at all and request them per-operation for the duration of each request.
- **Credential rotation without workload restart.** For Class A, a `secret rotate` command
  updates the sealed value; workloads that mount it as a file see the update without restarting.
  Env-var-based secrets require a restart — documented as a known limitation.
- **The edge proposal's `edge secret set` is subsumed.** The Cloudflare DNS token and any
  other per-capsule secret the edge needs are stored as sealed secrets, not in an
  `edge_secrets` table. The edge service references them by name.

## Non-goals (v1)

- **Secret sharing across capsules.** Each capsule has its own sealed-secret store. A secret
  defined on `nuc-1` is not automatically available on `nuc-2`. Cross-capsule secret sharing
  (a "fleet secret store") is an open question.
- **Dynamic secrets for databases** (Vault-style: create a short-lived DB user on demand,
  revoke it when the workload stops). This is powerful but requires capsuled or the broker to
  speak the database's wire protocol. Out of scope v1 — the Class A sealed-secret path covers
  static DB passwords; dynamic DB credentials are a future layer.
- **Secret versioning / audit log in v1.** A full audit trail of every credential access is
  desirable but not v1. The broker logs issuances to capsuled's slog output; a proper audit
  store is open questions.
- **Guest-side decryption.** Secrets are decrypted host-side by capsuled and injected into
  the workload. A compromised host can read them from the workload's environment. This is the
  same posture as the encrypted-volumes proposal — host-side is the right boundary for a
  homelab threat model.
- **Multi-tenant policy.** Policy is per-workload on a single capsule. The broker is not a
  multi-tenant system; it does not enforce isolation between capsules or operators.

---

## Threat model

| Threat | Protected? |
|--------|------------|
| Disk stolen from a powered-off node | **Yes** — secrets are wrapped under the node master key, which is TPM-sealed. The plaintext is not on disk. |
| Memory dump of a running capsuled process | No — the master key and decrypted secrets live in capsuled's memory during unsealing and injection. Same posture as encrypted-volumes. |
| Compromised workload exfiltrating a Class A secret from its own env | Partial — the workload has the plaintext in its env; there is nothing capsuled can do once injected. Mitigation: use the Class B broker for secrets the workload doesn't strictly need at startup. |
| Compromised workload calling the broker for credentials it shouldn't have | **Yes** — policy gates every broker request. A workload can only request credentials it has been granted policy for. |
| Compromised workload calling the broker repeatedly to stockpile tokens | Partial — tokens have TTLs, and the broker enforces one active token per (workload, secret) by default. A burst-issuance policy limit is open questions. |
| Operator approves a malicious broker request | No — same as today's `capsulectl` trust boundary. Operator credentials are the root of trust. |
| Network attacker intercepting the broker connection | **Yes** — broker is a fabric workload, reachable only over WireGuard. The broker's fabric `allow_from` is the policy gate. |
| Fabric identity spoofing (fake source IP) | **Yes** — fabric anti-spoof rules in nftables prevent a workload from lying about its fabric IP. The broker's identity inference (fabric IP → workload name) is backed by this. |
| Secret leaked via `capsulectl secret get` output | Partial — `secret get` shows metadata only, not the plaintext value. A separate `secret reveal` verb exists and requires explicit confirmation; logged. |

---

## Layer 1 — Sealed secrets

### Concept

A **sealed secret** is a named string or file value stored encrypted in capsuled's SQLite
database. The encryption uses the same AES-256-GCM + node master key from the
encrypted-volumes proposal. The plaintext only ever exists in two places: in the operator's
terminal at define time, and in capsuled's memory during injection at workload-start time.

Secrets are referenced by name in workload specs. capsuled resolves them at workload start —
after unsealing the node master — and injects them either as environment variables or as
files in a tmpfs mount. The workload sees plaintext; the host disk and the SQLite database
see only the ciphertext wrapped under the master key.

### CLI

```sh
# Define a secret (value from stdin to avoid shell history)
capsulectl secret create db-password
> Enter value: ████████
> Confirm:     ████████
✓ Secret 'db-password' created.

# Or pass value directly (appears in shell history — use with care)
capsulectl secret create stripe-key --value "sk_live_..."

# Or from a file
capsulectl secret create tls-cert --file ./server.crt

# List (shows names and metadata; never plaintext)
capsulectl secret list
# NAME          TYPE    SIZE   CREATED              REFERENCED BY
# db-password   string  24 B   2026-05-13T10:00:00  app, worker
# stripe-key    string  51 B   2026-05-13T10:01:00  (none)
# tls-cert      file    2.1 KB 2026-05-13T10:02:00  proxy

# Rotate (update the value; wrapped under same master)
capsulectl secret rotate db-password
> Enter new value: ████████

# Reveal the plaintext (requires confirmation; logged)
capsulectl secret reveal db-password
WARNING: This prints a plaintext secret. Continue? [y/N]: y
hunter2

# Delete (fails if any workload spec references it)
capsulectl secret delete stripe-key
```

### Referencing secrets in workload specs

```yaml
name: app
kind: Container
container:
  image: myapp:latest
  secrets:
    # Injected as an environment variable
    - name: db-password
      envVar: DATABASE_PASSWORD

    # Injected as a file into a tmpfs directory
    - name: tls-cert
      file: /run/secrets/tls.crt

    # File mount: capsuled creates /run/secrets/ as a tmpfs,
    # writes the secret file into it, and bind-mounts it read-only
    # into the container. The file disappears when the container stops.
```

capsuled validates secret references at `apply` time — unknown secret names are rejected
before the workload is ever started. This prevents a typo from causing a silent startup
failure.

### Injection flow

```
workload start:
  1. capsuled reads spec: sees secrets: [{name: db-password, envVar: DATABASE_PASSWORD}]
  2. For each secret:
       ciphertext = SELECT value_blob FROM secrets WHERE name = 'db-password'
       plaintext  = AES-GCM-Open(master_in_memory, ciphertext)
       └── master must be unsealed (TPM or node-unlock passphrase)
       └── if master not unsealed → workload stays Pending, status: "waiting for node unlock"
  3. OCI runtime: append DATABASE_PASSWORD=hunter2 to the container's env
     (for file secrets: create tmpfs, write file, bind-mount read-only)
  4. plaintext is zeroed from capsuled's memory after hand-off to the OCI bundle
```

The OCI bundle for the container includes the env vars. From that point on, the secret
lives in the container's environment — capsuled no longer holds it. A `docker inspect`
equivalent (`capsulectl workload get`) does NOT show secret values, only that a secret
was injected.

### File-based rotation

For secrets injected as files, capsuled can update the file in the tmpfs mount without
restarting the workload:

```sh
capsulectl secret rotate tls-cert --file ./new-server.crt
# capsuled rewrites /run/secrets/tls.crt in every workload that mounts it.
# Applications that watch the file (most TLS libraries) pick it up automatically.
```

Env-var-based secrets require a workload restart to take effect. This is a known limitation.
For secrets that rotate frequently, prefer the file mount form.

### SQLite schema

```sql
CREATE TABLE secrets (
  name        TEXT PRIMARY KEY,
  type        TEXT NOT NULL,          -- 'string' | 'file'
  value_blob  BLOB NOT NULL,          -- AES-GCM(master, plaintext)
  size_bytes  INTEGER NOT NULL,       -- plaintext size (for display; not a security concern)
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL           -- operator key ID (kid) that created the secret
);

-- Workload → secret reference index (for "delete" validation and "list referenced-by")
CREATE TABLE secret_refs (
  secret_name   TEXT NOT NULL REFERENCES secrets(name),
  workload_name TEXT NOT NULL,
  inject_as     TEXT NOT NULL,        -- 'env:DATABASE_PASSWORD' | 'file:/run/secrets/tls.crt'
  PRIMARY KEY (secret_name, workload_name)
);
```

The node master key that wraps `value_blob` is the same key established by
`capsulectl node init-encryption`. If no encryption is initialized, secrets still work but
`value_blob` is stored with a null-key AES-GCM (functionally plaintext). capsuled warns
loudly on secret creation if encryption is not initialized.

---

## Layer 2 — Credential broker

### Concept

The credential broker is a **managed workload** that capsuled deploys on a capsule (similar
to how the edge proposal deploys a managed Caddy container). It holds real credentials as
sealed secrets injected at start time, authenticates callers by their fabric IP, applies
per-workload policy, and serves credentials via two modes:

**Proxy mode:** The calling workload sends its API request to the broker instead of the real
API. The broker injects the real credential and forwards. The workload never sees the key.

**Token mode:** The calling workload asks the broker for a time-limited token. The broker
issues the real credential (or a scoped derivative) with a TTL. The workload uses it
directly and the token expires automatically.

In both cases, the calling workload authenticates by virtue of its fabric IP — the broker
looks up the source IP in the fabric workload table (`fabric_workloads`) to resolve it to
a `workload@capsule` name, then checks that name against the policy table.

### Why fabric identity is the right auth primitive

Most credential brokers require the caller to present a token or certificate. That token
itself needs to be managed — a circular problem. Fabric identity sidesteps this: a workload's
fabric IP is assigned by capsuled, anti-spoofed by nftables, and maps to a unique workload
name in SQLite. The broker can trust the source IP as a verified identity because capsuled
guarantees it. No credentials needed to get credentials.

This only works because:
1. The broker runs as a fabric workload with `allow_from` entries for the workloads it serves.
2. nftables anti-spoof rules prevent source IP forgery at the host level.
3. The broker's `allow_from` policy is the only way to reach it — workloads not explicitly
   allowed in the broker's fabric spec cannot even establish a TCP connection to it.

### Deploying the broker

The broker is a capsuled-managed workload, declared with `kind: CredentialBroker` (or a
regular container with `credentialBroker: true` — TBD). capsuled deploys it the same way
it deploys the edge's Caddy workload: operator-initiated, capsuled-owned lifecycle.

```sh
capsulectl --capsule nuc-1 broker init
```

This deploys the broker container, creates necessary volumes, and sets up the gRPC or
HTTP endpoint on the fabric. The broker's fabric policy is managed by capsuled based on
the declared broker policies (see below).

### Defining policy

Policy declares which workloads can request which secrets, under what conditions, and with
what issuance mode.

```sh
# Auto-approved proxy: picoclaw can use stripe-key via the proxy, no TTL needed (proxy holds it)
capsulectl --capsule nuc-1 broker policy add \
    --workload picoclaw \
    --secret stripe-key \
    --mode proxy \
    --allow-paths "/v1/charges,/v1/customers" \
    --approval auto

# Auto-approved token: picoclaw can get a 60-second grafana token for read operations
capsulectl --capsule nuc-1 broker policy add \
    --workload picoclaw \
    --secret grafana-token \
    --mode token \
    --ttl 60s \
    --approval auto

# Manual approval: anything touching refunds requires a human
capsulectl --capsule nuc-1 broker policy add \
    --workload picoclaw \
    --secret stripe-key \
    --mode proxy \
    --allow-paths "/v1/refunds" \
    --approval manual \
    --approval-timeout 10m

# Deny: picoclaw cannot use the admin token at all
capsulectl --capsule nuc-1 broker policy add \
    --workload picoclaw \
    --secret admin-token \
    --mode deny
```

```sh
capsulectl --capsule nuc-1 broker policy list
# WORKLOAD   SECRET        MODE   PATHS              APPROVAL  TTL
# picoclaw   stripe-key    proxy  /v1/charges,…      auto      —
# picoclaw   stripe-key    proxy  /v1/refunds        manual    10m
# picoclaw   grafana-token token  *                  auto      60s
# picoclaw   admin-token   deny   *                  —         —
```

### Proxy mode flow

The calling workload is configured to send API requests to the broker's fabric address
instead of the real API endpoint. The broker holds the real credential as a sealed secret
injected at start time.

```
picoclaw wants to charge a card:

  picoclaw.py:
    requests.post(
      "http://broker.nuc-1.fabric/stripe/v1/charges",   ← broker's address, not stripe
      json={"amount": 999, "currency": "usd", ...}
      # no Authorization header needed
    )

broker receives the request:
  1. source IP = 100.64.0.10 → look up in fabric_workloads → picoclaw@nuc-1
  2. path = /stripe/v1/charges → look up policy for (picoclaw, stripe-key, /v1/charges)
  3. policy: mode=proxy, approval=auto → proceed
  4. inject real credential: add "Authorization: Bearer sk_live_..." to request headers
  5. forward to https://api.stripe.com/v1/charges
  6. return response to picoclaw

picoclaw never sees "sk_live_...". It cannot exfiltrate it.
The broker logs: {time, workload: picoclaw@nuc-1, secret: stripe-key, path: /v1/charges, status: 200}
```

### Token mode flow

For cases where a proxy is impractical (streaming connections, binary protocols, SDKs that
can't be pointed at a custom endpoint):

```
picoclaw wants to query Grafana:

  picoclaw calls broker gRPC: RequestToken(secret="grafana-token", ttl=60s)
  broker:
    1. authenticates caller via fabric IP → picoclaw@nuc-1
    2. checks policy: mode=token, approval=auto, ttl=60s
    3. unseals grafana-token from master key
    4. issues a token record: {id, workload, secret, issued_at, expires_at}
    5. returns the plaintext grafana token with 60s TTL

  picoclaw uses the token directly to call Grafana
  after 60 seconds: broker revokes the token (marks it expired in its token table)

  picoclaw can call RequestToken again for the next query.
```

For services that support scoped tokens (GitHub, Google, etc.), the broker can create a
real scoped token via the service's API and return that, then revoke it at TTL. For services
that don't (most), the broker returns the full credential with a TTL that it enforces itself
(it won't re-issue to the same workload while a token is outstanding, unless policy allows).

### Manual approval flow

```
picoclaw calls broker: RequestProxy(secret=stripe-key, path=/v1/refunds, body=...)
broker:
  1. authenticates → picoclaw@nuc-1
  2. policy: mode=proxy, path=/v1/refunds, approval=manual, timeout=10m
  3. creates a pending request record with full request details
  4. sends approval notification (webhook to operator / Slack / PagerDuty — configured at broker init)
  5. blocks the picoclaw call, waiting up to 10m

operator receives notification:
  "picoclaw@nuc-1 is requesting to POST /v1/refunds to Stripe. Approve?"
  capsulectl --capsule nuc-1 broker request list
  # ID      WORKLOAD   SECRET      PATH         CREATED   STATUS
  # req-01  picoclaw   stripe-key  /v1/refunds  10s ago   pending

  capsulectl --capsule nuc-1 broker request approve req-01
  # or deny:
  capsulectl --capsule nuc-1 broker request deny req-01 --reason "not authorized"

broker receives approval:
  6. injects credential, forwards request to Stripe
  7. returns response to picoclaw (which has been blocking)

if operator does nothing within 10m:
  8. broker returns 408 to picoclaw; request logged as expired
```

### Privilege downgrade

A workload that starts with broad access can explicitly narrow its own permissions after
initialization:

```python
# picoclaw startup: do one-time setup with a broad token
setup_token = broker.request_token("admin-token", ttl="30s")
run_initial_migration(setup_token)

# After setup, downgrade — picoclaw no longer has any admin access
broker.downgrade(revoke="admin-token")

# From here on, picoclaw can only request the narrower per-op credentials
```

The broker's `downgrade` call marks the workload's policy entry for `admin-token` as
`suspended` for the lifetime of this workload instance. The suspension is tied to the
workload's current run — if the workload restarts, it starts fresh with its full policy.
This prevents a compromised workload from requesting the admin token after it's been
downgraded.

The "start with nothing and request per operation" pattern is the strongest model: the
workload spec declares no `secrets:` at all, and every credential is acquired from the
broker for the duration of exactly one call.

---

## How the two layers compose

The sealed secrets layer (Layer 1) is the foundation. The broker (Layer 2) uses it:

```
operator defines:
  capsulectl secret create stripe-key --value "sk_live_..."
  capsulectl secret create grafana-token --value "glsa_..."

broker workload spec (managed by capsuled):
  kind: CredentialBroker
  secrets:
    - name: stripe-key     # capsuled injects at broker start
    - name: grafana-token  # capsuled injects at broker start

calling workload spec:
  kind: Container
  name: picoclaw
  secrets: []              # no direct secrets — everything goes through the broker
  fabric:
    allow_to:
      - broker@nuc-1:8080/tcp
```

The broker holds the real credentials in its own memory (injected as env vars at start by
capsuled). Calling workloads have no secrets at all in their specs — they hold nothing at
rest and receive credentials only for the duration of a proxied call or a short-TTL token.

---

## The TPM connection

The sealed secrets layer sits directly on top of the encrypted-volumes key hierarchy:

```
Node master key (TPM-sealed to PCR 7 or passphrase-derived)
   │
   ├─ vol-A LUKS key  (encrypted volume)
   ├─ vol-B LUKS key
   │
   ├─ secret: db-password  ← AES-GCM(master, "hunter2")
   ├─ secret: stripe-key   ← AES-GCM(master, "sk_live_...")
   └─ secret: grafana-token ← AES-GCM(master, "glsa_...")
```

Same master key, same wrapping primitive (`AES-256-GCM`), same unsealing path:

- **TPM present:** capsuled unseals the master at boot → all secrets available immediately
  → workloads start with their secrets injected → no operator action.
- **TPM absent:** node stays `locked` until `capsulectl node unlock` → operator supplies
  passphrase or recovery code → master unsealed → all secrets become available → pending
  workloads start.

A node without `node init-encryption` can still define secrets; they are stored wrapped
under a null key (functionally plaintext). capsuled emits a startup warning:
`"secrets defined but encryption not initialized — secrets are stored unencrypted"`. This
ensures the system works in development/QEMU environments without blocking on TPM setup.

---

## Relationship to the edge proposal

The edge proposal's `edge secret set CLOUDFLARE_API_TOKEN` and `edge_secrets` table are
replaced. After this proposal lands:

```sh
# Old (edge-specific, edge proposal draft):
capsulectl --capsule edge edge secret set CLOUDFLARE_API_TOKEN @~/.cloudflare-token

# New (general sealed secrets):
capsulectl --capsule edge secret create CLOUDFLARE_API_TOKEN --file ~/.cloudflare-token
```

The edge's Caddyfile renderer references the secret by name. capsuled injects it into
the Caddy workload as an env var at start time. The `edge_secrets` table is removed from
the edge schema; the `secrets` + `secret_refs` tables serve that role. The edge proposal's
`edge secret set` and `edge secret remove` CLI verbs are replaced by the general
`capsulectl secret` verbs.

---

## Failure scenarios

### Sealed secrets (Layer 1)

| Failure | Symptom | Recovery |
|---------|---------|----------|
| Node not unlocked at workload start | Workload stays `Pending`, status: `waiting for node unlock` | `capsulectl node unlock` (passphrase or recovery code). Workloads start automatically once unlocked. |
| Secret deleted while a workload references it | `secret delete` refuses with "referenced by: app, worker" | Delete the references first (`capsulectl workload delete app`) or use `--force` to delete the secret and leave references dangling (workload restarts will fail with "unknown secret") |
| Master key lost (TPM dead, no recovery code, no passphrase) | Node stays `locked` forever; secrets inaccessible | Data loss. Same outcome as encrypted volumes with no recovery code — documented as the one unrecoverable failure mode. |
| `secret rotate` during workload run (env var form) | Old value still in the running workload's env | Restart the workload. For file-form secrets, the file is updated in place without restart. |
| capsuled crash mid-injection | Workload not started; no partial state | Standard workload restart path. OCI bundle creation is the atomic boundary. |

### Credential broker (Layer 2)

| Failure | Symptom | Recovery |
|---------|---------|----------|
| Broker workload crashes | All in-flight proxy requests return 502; pending token requests fail | capsuled's restart policy brings the broker back (RTO ~5 s). Outstanding tokens were in-memory; re-request after restart. |
| Calling workload's fabric IP is not in broker's `allow_from` | TCP connection refused before any request is made | Add the workload to the broker's fabric policy |
| Policy misconfiguration: workload has no policy for a secret | Broker returns 403 with a clear error | Add a policy entry with `broker policy add` |
| Manual approval timeout | Broker returns 408 to the calling workload | Workload handles the error. Operator can re-submit or adjust the timeout. |
| Approval notification fails to deliver (webhook down) | Request sits pending until timeout | Check `broker request list` manually. Future: dead-letter queue for failed notifications. |
| Broker's sealed secret value rotated while broker is running | Broker still holds the old plaintext in memory | Restart the broker to pick up the new value. Future: file-form injection + inotify allows hot-reload. |
| Calling workload restarts and re-requests before old token expires | By default broker refuses (one active token per workload+secret) | Configurable per policy: `--allow-concurrent` for workloads that scale horizontally. |

---

## Open questions

- **Cross-capsule secrets.** A secret defined on `nuc-1` is not available on `nuc-2`. For
  a fleet where many capsules run the same workload (e.g., all NUCs run a monitoring agent
  that needs the same Grafana token), operators must define the secret on each capsule
  separately. A `--replicate nuc-1,nuc-2,...` flag on `secret create` that pushes the
  wrapped value to each named capsule over mTLS is the natural v2 feature. The per-node
  wrapping means each capsule re-wraps the secret under its own master key — the plaintext
  crosses the wire briefly in capsuled's memory during push. Alternative: a "fleet secret"
  lives on a designated secret-store capsule and is fetched by other capsules at need, never
  stored locally.

- **Broker notification channel.** The manual approval flow requires notifying the operator.
  The right answer for a homelab is probably a webhook (POST to an operator-defined URL) or
  a native integration with the CLI (`capsulectl broker watch` streams pending requests as
  they arrive). Define the notification target at `broker init`:
  `capsulectl broker init --notify-webhook https://hooks.slack.com/...` or
  `capsulectl broker init --notify cli` (requires an operator to have `capsulectl broker
  watch` running — not reliable). Lean toward webhook; CLI watch as a fallback.

- **Should the broker be a first-class capsuled concept or a workload?** The proposal treats
  it as a managed workload (like edge-Caddy), meaning its binary is not in the Capsule rootfs
  — it's pulled as a container image. This keeps capsuled's surface area small but adds an
  image dependency. The alternative (a mini broker built into capsuled) would work for the
  simple auto-approve / token-issue case but would make the manual approval UX and the policy
  engine awkward to build and evolve. Lean toward managed workload.

- **Token format for services that don't support token scoping.** For Grafana, the "token"
  the broker issues is the real API key with a broker-enforced TTL (broker won't re-issue
  while it's live, and the key itself doesn't expire from Grafana's perspective). If the
  workload is compromised and caches the token, the TTL only matters to the broker's policy —
  the real key is valid indefinitely. Mitigation: use Grafana service accounts with short
  expiry set in Grafana itself; the broker issues a call to create a short-lived Grafana
  service account token and passes *that*. This requires the broker to know Grafana's API for
  token creation. A `--token-endpoint` field on `broker policy add` could express this:
  `--token-endpoint "POST https://grafana/api/serviceaccounts/{id}/tokens ttl={ttl}"`.

- **Policy storage.** The proposal has `broker policy add` as CLI — where does it store the
  policy? Options: (a) in the broker workload's own SQLite database on a volume, (b) in
  capsuled's SQLite as a `broker_policies` table, (c) in a policy file that the broker reads
  at startup. Option (b) feels most consistent with how the rest of capsule works (capsuled
  owns state), but requires a gRPC surface between capsuled and the broker for policy CRUD.
  Option (a) is simpler but means policy is lost if the broker volume is lost. Lean (b).

- **Audit log.** Every credential issuance (both proxy and token) should be logged durably.
  `capsulectl broker log` streaming the issuance records is the minimum. Long-term the log
  should be append-only and signed. Out of scope v1.

- **`capsulectl secret reveal` access control.** Any operator who can reach the capsule can
  reveal any secret. On a single-operator homelab this is fine. Multi-operator fleets may want
  read-restricted secrets (only the operator who created it can reveal it). Simple version:
  `secrets.reveal_restricted BOOLEAN` — if true, only the `created_by` kid can reveal it.

---

## Implementation pointers

### Layer 1 (sealed secrets — in capsuled)

- Proto: `models/capsule/v1/secret.proto` (new) — `SecretService` with `Create`, `Rotate`,
  `Delete`, `List`, `Get` (metadata only), `Reveal` (plaintext, logged). Workload protos
  (`ContainerSpec`, `MicroVMSpec`) grow `repeated SecretMount secrets`.
- Schema: `secrets` and `secret_refs` tables (above). The `core/keymanager/` package from
  encrypted-volumes provides `Wrap(master, plaintext)` / `Unwrap(master, blob)` — reused as-is.
- Logic: new `core/secret/service.go`. `core/workload/` learns to call `secret.Inject(spec)`
  during workload start, which resolves all secret mounts and passes plaintext to the OCI
  builder or Firecracker launcher.
- File-form rotation: after `secret rotate`, the service walks `secret_refs` for all workloads
  that mount this secret as a file and rewrites the tmpfs file in each running workload's
  mount namespace via a file-write RPC to the container runtime.
- CLI: `capsulectl secret {create, rotate, delete, list, get, reveal}`.
- The edge proposal's `edge_secrets` table and `edge secret {set,remove}` verbs are removed;
  the general `secrets` table serves that role.

### Layer 2 (credential broker — managed workload)

- The broker binary is a separate service (not in capsuled). It exposes an HTTP API for
  proxy mode and a gRPC API for token mode. Its image is versioned and pulled like any
  other workload.
- The broker reads its identity table from capsuled's `fabric_workloads` table via a
  read-only API added to capsuled: `FabricService.LookupWorkload(fabric_ip)` → workload name.
  This is the auth primitive.
- Policy: stored in capsuled SQLite as `broker_policies` table; capsuled exposes a
  `BrokerService` gRPC endpoint that the broker calls to read and watch policies. The CLI
  verbs `broker policy add/list/remove` talk to capsuled, not to the broker directly.
- Pending requests (manual approval): stored in `broker_requests` table in capsuled SQLite.
  `capsulectl broker request {list, approve, deny}` talk to capsuled.
- `capsulectl broker init` deploys the broker workload with the right sealed-secret mounts
  and fabric policy, same pattern as `capsulectl edge init`.
- The broker workload's own sealed secrets (the real API keys) are defined by the operator
  with `capsulectl secret create` before `broker init`; the broker's workload spec references
  them in `secrets:`.
