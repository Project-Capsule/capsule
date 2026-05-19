# Sealed capsules: a fixed-payload appliance you can ship (proposal)

> **Status:** Proposal. Not implemented.
>
> **Reuses mechanism from [install.md](install.md).** Sealed capsules borrow
> install's first-boot seed/ingest path (`firstboot.json`-style: skip the claim
> window, pre-enroll an operator) but move the seed from a *network installer*
> to *build time*. **Extends [updates.md](updates.md):** the atomic A/B bundle
> grows a signed payload and learns to confirm itself on payload health when no
> operator is in the field. **Touches [discovery.md](discovery.md):** a sealed
> device can be told not to announce. Independent of fabric/edge/web-ui.
>
> This addresses the "ship the box" gap: today every capsule is a *managed*
> capsule. You adopt it, you `apply` workloads to it over gRPC, images are pulled
> from a registry or side-loaded by hand. There is no notion of a capsule whose
> job is fixed at build time, that ships inside a product, runs offline, and is
> updated as one signed artifact.

## Summary

Capsule today assumes a live operator on the LAN: adopt the machine, `apply`
manifests over gRPC, let containerd pull images from a registry (or
`image push` them by hand). That is exactly right for a homelab fleet. It is
the wrong shape for a capsule that is **a component inside a shipped product**:

- A projector with an onboard control computer. It is installed in a ceiling,
  has no operator, may have no network, and runs exactly one application that
  must come up every time the projector is powered on.
- Compute on a drone, paired with a flight microcontroller over a serial link.
  It runs a fixed vision/telemetry stack, is updated between flights over an
  intermittent link, and there is nobody on a laptop to `apply` anything.

What these have in common: the **payload is decided at build time, not in the
field**; the device must run that payload **offline, with zero operator
interaction**; and an update must ship the **OS *and* the workloads *and* the
images** as one atomic, reversible, verifiable artifact — exactly the property
[updates.md](updates.md) already gives the OS, extended to the workloads.

A **sealed capsule** is a build mode, not a new daemon. The same `capsuled`,
the same gRPC API, the same A/B update machinery. Three things change:

1. **The payload travels in the rootfs.** `make sealed-image` resolves a
   payload directory (workload + volume manifests) and the OCI images those
   manifests reference, pulls and bakes the images into the squashfs, and
   stamps the manifest set into the immutable rootfs. The payload is therefore
   part of the A/B-versioned, atomically-updated, rollback-covered slot — "one
   version, one rollback, one *what's installed* answer" now includes the
   workloads. No registry, no `apply`, ever, in the field.
2. **The desired state is the baked payload, and it is locked.** On a sealed
   capsule the reconciler's source of truth is the baked manifest set, not
   accumulated `apply` calls. `WorkloadService.Apply/Delete/Stop/Start` against
   sealed workloads is refused. Observe/diagnose verbs (`info`, `list`, `get`,
   `logs`, `exec`) still work — you can still operate the box, you just can't
   redefine its job from the field.
3. **Updates are signed, and confirm themselves.** The update bundle gains a
   detached signature verified against a key baked at build time, and (because
   there is no operator in the field to type `confirm`) the capsule
   self-confirms an update once its own payload comes up healthy, or
   auto-rolls-back if it doesn't.

Everything is **strictly additive**: a non-sealed build behaves exactly as
today; a sealed build is the same binary reading one extra config file.

## Why this shape

- **The payload belongs in the immutable artifact.** Capsule's whole thesis is
  immutable, A/B, atomic, reversible. A shipped device's *application* has the
  same need the *OS* does: one version, one rollback, no drift, no "what's
  actually running here?" ambiguity after a field power-cycle. Baking the
  payload into the squashfs makes the slot the single unit of truth and gives
  payload rollback for free — flip the slot, you flipped the app.
- **Offline-first, operator-optional — not operator-hostile.** A sealed capsule
  must run with no network and no human. But the gRPC surface is the same one
  `capsulectl` already speaks, so a technician who *does* plug into the
  projector's service port, or the ground station that *does* reach the drone,
  gets every read/diagnose verb with zero new client code.
- **Sealing is a build input, not a fork.** `SEAL_*` build variables select the
  mode; `capsuled` reads one `sealed.json` early in boot. No second binary, no
  second proto package, no behavioural divergence to maintain.
- **It reuses install's seed path.** install.md already designed "seed identity
  onto the disk so first boot skips the claim window." Sealed pre-registration
  is the same ingest, with the seed produced by the build instead of a network
  installer. We are not inventing an enrollment path; we are relocating one.

## Goals

- **`make sealed-image PAYLOAD=<dir>` produces a shippable artifact**:
  `build/sealed-disk.raw` (flash it into the product) and
  `build/sealed-update.tar` (signed OS+payload bundle for OTA), with every
  referenced image baked in — no registry reachable required, ever.
- **A sealed capsule boots its payload with zero network and zero operator.**
  First boot projects the baked manifests into desired state, loads the baked
  images into the containerd cache, and the reconciler runs them. Power-cycle
  safe by the existing crash-recovery=steady-state property.
- **The payload is locked in the field.** Mutating a sealed workload over gRPC
  is refused with a clear error; the only way to change the app is a new signed
  bundle (or an explicit, audited, reboot-scoped maintenance unseal).
- **Updates are atomic, reversible, *and* authenticated.** Signature verified
  against a build-time key; downgrade refused; the unattended-confirm gap
  closed by health-gated self-confirm.
- **A clear, documented adoption story** with a recommended default for shipped
  hardware (see [Adoption](#adoption-the-evaluation)).
- **Strictly additive.** Non-sealed builds and existing flows are byte-for-byte
  unaffected.

## Non-goals (v1)

- **Encryption at rest / anti-tamper of a stolen device.** Sealed locks the
  *control plane and payload definition*, not the bytes on disk. Physical-theft
  confidentiality is [encrypted-volumes.md](encrypted-volumes.md) /
  [secrets.md](secrets.md); the seam is noted, not solved here.
- **Partial / per-workload payload updates.** v1 ships the whole OS+payload as
  one bundle (preserves the one-version thesis). Delta updates for
  bandwidth-constrained links (drone LTE) are an open question.
- **Readiness probes.** "Healthy" for self-confirm is "all sealed workloads
  Running for N seconds" in v1. Declarative readiness checks are future work.
- **Remote key rotation without a re-flash.** Rotating a leaked seal key is a
  physical re-flash in v1; per-batch keys limit blast radius. Networked
  rotation is an open question.
- **Fleet OTA orchestration.** This proposal makes *one* device updatable
  unattended and safely. Pushing to a thousand of them is an orchestration
  layer above this (and out of scope).
- **A second binary or a stripped "appliance" image.** Same `capsuled`. Sealing
  is configuration, not a build target split.

## Adoption: the evaluation

The brief explicitly asks how a shipped device should enroll. There are three
coherent modes; the build picks one via `SEAL_MODE`.

| Mode | Operator key | Claim window | gRPC mutation | mDNS | Ship when… |
|------|--------------|--------------|---------------|------|------------|
| `adoptable` | none baked | **opens** on first boot, as today | allowed until sealed-payload lock applies | on | the **recipient is the operator** and will adopt it on their own LAN |
| `pre-registered` | baked at build (`SEAL_OPERATOR_PUBKEY`) | **never opens** — already enrolled | refused for sealed workloads; observe/update OK | configurable (default off) | **you** retain management of the shipped device (the common case) |
| `locked` | none — no network operator at all | never opens | all mutation refused; updates only via signed bundle | **off** | a true appliance: projector in a ceiling, no one will ever `capsulectl` it |

**Recommendation: `pre-registered` is the default for shipped hardware.** It is
the install.md seal path moved to build time: the build emits a
`firstboot.json`-equivalent seed (the device's own TLS keypair + `capsule_id` +
the operator's *public* key) baked into the rootfs; first boot ingests it,
inserts the `authorized_keys` row, and skips the claim window — the device comes
up already yours, reachable with a context entry you generated at build time,
with no claim window an attacker on the device's local link could race.
`adoptable` is correct only when you are handing the box to someone who *is* the
operator. `locked` is the strongest posture for an unattended appliance that
genuinely has no operator — it still takes signed updates, but exposes no
enrollment or mutation surface at all.

The pubkey baked into a `pre-registered` device is not a secret (same argument
as install.md's threat model). What must be protected is the **update signing
key**, addressed in [Signed updates](#signed-updates).

## Architecture

```
 build host                                        shipped device (offline)
┌─────────────────────────────────────┐           ┌──────────────────────────────┐
│ make sealed-image PAYLOAD=projector/ │           │ capsuled (PID 1)             │
│  1. read payload/*.yaml manifests    │           │  reads /usr/lib/capsule/     │
│  2. resolve image refs → docker pull │  flash    │        sealed.json (mode)    │
│  3. docker save → payload/images.tar │ ───────▶  │  first boot:                 │
│  4. bake manifests + images.tar      │ disk.raw  │   ├─ ingest baked operator   │
│     INTO the squashfs rootfs         │           │   │   pubkey → authorized_   │
│  5. partition disk (4-part, as today)│           │   │   keys, skip claim win   │
│  6. sign bundle with SEAL_UPDATE_KEY │           │   ├─ load images.tar → cache │
│     → build/sealed-update.tar (+sig) │   OTA     │   └─ project manifests →     │
└──────────────────────────────────────┘ ───────▶  │       workloads desired set  │
                                       sealed-     │  reconciler runs the payload │
                                       update.tar  │  (no registry, no apply)     │
                                                   └──────────────────────────────┘
```

### Where the payload physically lives

**In the squashfs rootfs**, read-only, at a fixed path
(`/usr/lib/capsule/payload/`):

```
/usr/lib/capsule/payload/
  manifests/            # the sealed workload + volume YAMLs
    projector-app.yaml
  images.tar            # docker-save multi-image archive of every referenced ref
  PAYLOAD               # sha256 of (manifests/ + images.tar), for change detection
/usr/lib/capsule/sealed.json   # mode, baked keys, mDNS policy, unseal policy
```

This is deliberately the same artifact the OS already ships in: per-slot,
immutable, A/B, atomically replaced by an update, and **rolled back for free**
when the slot is rolled back — no extra `os_state` bookkeeping, because the
payload *is* the slot. It reuses the existing constraint guard: `pack.sh`
already refuses to build if the squashfs would overflow the slot
([architecture.md](architecture.md#disk-layout)); an over-large payload trips
the same guard, with the same `SLOT_SIZE_MIB=` install-time override as the
escape hatch.

Alternatives considered and rejected for v1:

- **A dedicated payload partition pair.** The 4-partition MBR layout is frozen
  at install and PARTUUID-anchored ([install.md](install.md)); adding p5/p6
  breaks the disklayout contract and the install proposal. Rejected.
- **Payload staged on `/perm`, versioned per slot.** Survives A/B (it's `/perm`)
  and dodges the 2 GiB cap, but requires new `os_state` columns binding a
  payload version to a slot and explicit rollback bookkeeping, duplicating what
  the squashfs gives for free. Kept as the **large-payload open question**, not
  v1.

### First boot (sealed)

After `mountPerm` + SQLite open, before the reconciler starts:

```
1. read /usr/lib/capsule/sealed.json   → mode, keys, policy
2. if mode == pre-registered and authorized_keys is empty:
     INSERT authorized_keys(kid,pubkey,name) from baked operator pubkey
     mark identity sealed; do NOT open the claim window
3. if /usr/lib/capsule/payload/PAYLOAD != os_state.payload_applied:
     for each image ref in payload/manifests not already in the content store:
        load it from payload/images.tar into containerd  (same code path as
        `capsulectl image push`; content-addressed ⇒ idempotent, cheap after 1st)
     UPSERT the manifest set into the `workloads` table, marked origin=sealed
     UPDATE os_state SET payload_applied = <PAYLOAD sha256>
4. reconciler starts; desired state for origin=sealed rows = the baked set
```

`payload_applied` is the only new persisted field. Steady state and crash
recovery are unchanged: the reconciler already treats startup like any other
tick, so an unexpected power-cycle on a drone just re-converges to the baked
payload. Offline by construction — every image is local, nothing dials out.

### Sealed-mode enforcement

`capsuled` gates the mutating RPCs when `sealed.json` mode is
`pre-registered` or `locked`:

| RPC | Sealed behaviour |
|-----|------------------|
| `WorkloadService.Apply/Delete/Stop/Start` on `origin=sealed` | `FailedPrecondition: capsule is sealed; payload ships via signed update` |
| `WorkloadService.List/Get`, `Logs`, `Exec`, `CapsuleService.GetInfo`, `StreamLogs` | unchanged — full observe/diagnose |
| `VolumeService.*` on sealed-referenced volumes | create/grow at boot allowed; `delete` refused while sealed (data, not spec — see [the data boundary](#what-sealed-does-not-seal)) |
| `CapsuleService.Adopt` | refused if already enrolled (pre-registered); claim window never opened |
| `UpdateOS` / `ReceiveBundle` | requires a valid `bundle.sig`; see below |
| `CapsuleService.Unseal` (new, operator-authed) | only if `sealed.json.allow_unseal`; drops to maintenance mode until reboot; audited |

`locked` mode additionally refuses *all* workload mutation (there is no operator
identity at all) and forces mDNS off.

### Signed updates

Today `ReceiveBundle` trusts the authenticated gRPC channel (operator JWT). A
shipped device may take updates over a link you do not fully control, or be
`locked` with no operator JWT at all. So the sealed bundle gains a fifth member:

```
sealed-update.tar
  VERSION        # one version string — OS *and* payload (one-version thesis)
  vmlinuz
  initramfs
  rootfs.sqsh    # now contains /usr/lib/capsule/payload/*  baked in
  bundle.sig     # Ed25519 detached sig over sha256(the other four members)
```

`capsuled` verifies `bundle.sig` against the `update_pubkey` baked in
`sealed.json` **before** writing anything to the inactive slot.
`pre-registered`/`locked` builds *require* a valid signature; `adoptable` builds
treat it as optional (back-compat with today's unsigned bundles). A
**downgrade** (`VERSION` sorting older than `os_state.last_version`) is refused
unless the bundle carries an explicit allow-downgrade marker, which `locked`
mode ignores entirely.

The signing key never ships on the device — only its public half is baked. The
private key is a build/release-infra secret. Recommended practice (documented,
not enforced): **per-batch** signing keys so a leak is contained to a production
run, not the whole fleet.

### Self-confirm on health (the unattended-confirm gap)

[updates.md](updates.md)'s safety model needs an operator (or an operator-side
poller via `--auto-confirm`) to `confirm` before the deadline, else
auto-rollback. A projector in a ceiling has neither. Sealed bundles therefore
carry a confirm policy, baked, default `health`:

```
sealed update lands on slot_b, capsuled reboots into slot_b:
  1. OnStartup sees pending_slot == slot_b, arms the existing rollback timer
  2. NEW: also arm a self-confirm watcher:
       wait until every origin=sealed workload has been Running for
       sealed.json.confirm_healthy_seconds (default 60)
  3. healthy in time   → capsuled calls its own confirm path; slot_b committed
     not healthy in time → existing deadline fires → reboot → GRUB default is
                            still slot_a → device is back on the known-good
                            OS *and* the known-good payload, untouched
```

No operator, no laptop, no network needed for a safe field update: a bad
OS-or-payload update on an unattended drone rolls itself back to the last bundle
that actually ran. This is the single most important capability sealed adds on
top of updates.md, and it falls out of the existing pending-slot machinery plus
one watcher.

### What sealed does *not* seal

Sealing fixes the **payload definition** — the workload specs and the image
bytes. It does **not** freeze runtime **data**. A sealed projector app may write
calibration to a persistent volume; a drone may log telemetry. Those volumes are
field state: they roll **forward** across updates (they are `/perm`-backed LVM
LVs, untouched by a slot flip), they are *not* rolled back with the OS, and they
are *not* part of the signed artifact. The boundary is: **sealed = code + spec;
mutable = data.** This is called out because "sealed" otherwise invites the
wrong mental model ("nothing can change") — the app can't change, its data still
must.

## Operating a sealed capsule

"Still benefits from most of our commands" — concretely:

| `capsulectl` verb | Sealed (`pre-registered`) | Sealed (`locked`) |
|-------------------|---------------------------|-------------------|
| `capsule info`, `seal-info` (new) | ✅ | ✅ (seal-info; info needs no auth change) |
| `workload list` / `get` | ✅ | ✅ |
| `workload logs` / `logs --serial` | ✅ | ✅ |
| `workload exec` | ✅ (audited) | ✅ (audited) |
| `cp` (pull diagnostics off the box) | ✅ | ✅ |
| `capsule update push` (signed bundle) | ✅ | ✅ |
| `capsule reboot` | ✅ | ✅ |
| `apply` / `workload delete|stop|start` (sealed wl) | ❌ `FailedPrecondition` | ❌ |
| `adopt` | ❌ already enrolled | ❌ no operator model |
| `capsule unseal` / `reseal` (new) | ✅ if `allow_unseal` (audited, reboot-scoped) | ❌ |

New verbs:

- `capsulectl capsule seal-info` — mode, payload version (the `PAYLOAD`
  sha256), baked operator-key + update-key fingerprints, mDNS state, whether
  unseal is permitted. The "what is this shipped box?" answer.
- `capsulectl capsule unseal` / `reseal` — maintenance toggle for a field
  technician (only if the build allowed it). `unseal` drops to mutation-allowed
  for live debugging, is audited, and is **reset on reboot** so a forgotten
  unseal doesn't permanently un-seal a shipped device.
- `capsulectl sealed verify build/sealed-update.tar` — *build-side* dry run:
  signature checks against the intended key, every manifest-referenced image is
  present in `images.tar`, no dangling refs, `VERSION` not a downgrade. Run it
  before you ship a bundle.

Build:

```sh
make sealed-image \
  PAYLOAD=examples/sealed/projector/ \
  SEAL_MODE=pre-registered \
  SEAL_OPERATOR_PUBKEY=~/.config/capsule/keys/projector-fleet.pub \
  SEAL_UPDATE_KEY=release-keys/batch-2026Q2.ed25519
# → build/sealed-disk.raw        (flash into the product)
# → build/sealed-update.tar      (signed OS+payload, OTA)
# → build/projector-001.ctx      (a ready-to-use capsulectl context entry)
```

## Threat model

| Threat | Protected? |
|--------|------------|
| Attacker on the device's local link pushes a tampered OS/payload | **Yes (sealed).** `bundle.sig` verified against the baked `update_pubkey` before any write; `pre-registered`/`locked` require it. Unsigned/altered ⇒ rejected, nothing written. |
| Attacker pushes an *old* signed bundle (known-vulnerable version) | **Yes.** Downgrade refused unless an explicit marker is present; `locked` ignores the marker. |
| Attacker on the local link races the claim window to adopt the device | **Yes (pre-registered/locked).** The claim window never opens — the device is enrolled (or has no operator model) before it ships. |
| Update signing key leaks | **Partial / by design.** Per-batch keys cap blast radius to one production run; rotation is a physical re-flash in v1 (open question for networked rotation). The private key never ships on the device. |
| Physical theft of the device (read data off disk) | **No — out of scope.** Sealed locks the control plane, not data confidentiality. Cross-ref [encrypted-volumes.md](encrypted-volumes.md)/[secrets.md](secrets.md); the seam is explicit. |
| Bad field update bricks an unattended device | **Yes.** Health-gated self-confirm: not-healthy-by-deadline ⇒ existing auto-rollback ⇒ back on the last good OS *and* payload, no operator needed. |
| mDNS reveals a shipped appliance on a customer network | **Yes (locked default-off; pre-registered configurable).** A locked device announces nothing. |
| Forgotten maintenance `unseal` leaves a shipped box mutable forever | **Yes.** `unseal` is reboot-scoped and audited; it cannot outlive a power-cycle. |
| Operator-key compromise on a `pre-registered` device | The baked key is a *public* key; possessing it grants nothing. Compromise of the *operator's private* key is the operator's problem (same as any adopted capsule) — mitigation is per-batch operator keys + revocation via a signed bundle that rewrites `authorized_keys`. |
| Malicious workload forges a sealed manifest to escape the lock | Sealed rows carry `origin=sealed` set only by the boot-time projector from the read-only squashfs; the gRPC path cannot create `origin=sealed` rows. The squashfs is immutable; a workload cannot rewrite it. |

## Failure scenarios

| Failure | Symptom | Recovery |
|---------|---------|----------|
| Payload bigger than the slot at build | `pack.sh` refuses (existing squashfs-overflow guard) | Trim payload, or build with `SLOT_SIZE_MIB=` and flash; large-payload `/perm` variant is an open question |
| Baked image missing for a referenced ref | `sealed verify` fails at build (caught before shipping) | Fix the payload dir; never ships |
| Power loss mid-OTA on a drone | Staging file deleted next boot (existing behaviour) | No half-applied update; device stays on current slot+payload |
| Sealed update boots but payload crash-loops | Self-confirm watcher never satisfied | Deadline fires → auto-rollback → last-good OS+payload, unattended |
| `firstboot` operator-seed corrupted (pre-registered) | First boot can't ingest the baked authorized key | Logs the error; falls back to claim window (degraded — device is adoptable, not pre-enrolled). Documented; recommend `sealed verify` of `disk.raw` before flashing a batch |
| Field tech `unseal`s and forgets | Box mutable until… | …next reboot, by design — `unseal` is reboot-scoped |
| Operator pushes an *unsigned* bundle to a `locked` device | `ReceiveBundle` rejects `InvalidArgument: signature required` | Re-issue signed via `make sealed-image`/release infra |
| `payload_applied` matches but containerd cache GC'd an image | Reconciler can't start a sealed workload | Boot-time projector re-loads from the still-present read-only `images.tar` in the squashfs (idempotent); pin sealed images from GC |
| Wrong-batch signed bundle pushed (valid sig, wrong device family) | Sig verifies but payload is for another product | `VERSION` namespacing + per-batch keys; mismatch documented as an operator/release-process error, not a `capsuled` guarantee |

## Schema

One column, mirroring discovery/install's "small additions on existing
surfaces" approach. No new tables.

```sql
ALTER TABLE os_state ADD COLUMN payload_applied TEXT;
-- sha256 of the currently-projected baked payload (the rootfs PAYLOAD file).
-- Lets first boot skip re-projection when the slot's payload is unchanged.
```

`workloads` gains a logical `origin` discriminator (`sealed` vs `operator`) —
implementable as a column or a reserved name prefix; it gates the
mutation-refusal and is set *only* by the boot-time projector reading the
immutable squashfs, never by the gRPC path.

`sealed.json` and `/usr/lib/capsule/payload/` are **read-only rootfs files**,
not SQLite — they are part of the immutable, A/B-versioned artifact by
construction, which is the entire point.

## Implementation phases

### Phase 1 — payload baked, projected, offline, locked (no signing yet)

- `make sealed-image PAYLOAD=<dir>`: resolve manifests → pull/`docker save`
  images → bake `payload/` + `sealed.json` into the squashfs. `pack.sh`'s
  overflow guard already covers size.
- Boot-time projector: load `images.tar` into the cache, UPSERT manifests as
  `origin=sealed`, set `payload_applied`. Reconciler runs them offline.
- Sealed-mode mutation refusal; `capsule seal-info`.
- `adoptable` mode only (trust the build channel for now).

### Phase 2 — signed, downgrade-safe bundles

- `bundle.sig` (Ed25519) + verification against baked `update_pubkey` in
  `ReceiveBundle`, before any slot write. Downgrade refusal.
- `capsulectl sealed verify` (build-side preflight).
- `SEAL_UPDATE_KEY` build input + per-batch key guidance in docs.

### Phase 3 — pre-registration + locked + maintenance unseal

- Build-time operator-seed (install.md `firstboot.json` path, build-produced):
  first boot enrolls the baked operator pubkey, skips the claim window. Emit a
  ready-made context entry from `make sealed-image`.
- `locked` mode (no operator model, all mutation refused, mDNS forced off).
- `CapsuleService.Unseal`/`reseal`, reboot-scoped, audited; `allow_unseal`
  build switch. mDNS suppression honoring `sealed.json`.

### Phase 4 — unattended self-confirm

- Health-gated self-confirm watcher on the pending-slot path (`confirm_healthy_
  seconds`, "all `origin=sealed` Running for N s"). Field-update failure matrix
  validated in QEMU (the existing in-place reboot loop already exercises A/B).

## Open questions

- **Large payloads past the 2 GiB slot cap.** The rejected `/perm`-staged,
  versioned-per-slot variant becomes necessary for image-heavy stacks (CUDA,
  big models). It costs new `os_state` slot↔payload-version bookkeeping and
  explicit rollback logic. Defer until a real payload needs it; design sketch
  belongs in a follow-up if so.
- **Delta / per-workload updates** for bandwidth-constrained links (drone LTE,
  cellular). Whole-bundle preserves the one-version thesis but is heavy. A
  signed *payload-only* bundle (no kernel/rootfs) is the obvious middle ground
  and probably the first thing real users ask for — but it forks "one version,
  one rollback." Needs its own analysis.
- **Networked seal-key rotation.** v1 rotates by re-flash. Can a signed bundle
  carry a new `update_pubkey` to install (signed by the *old* key, one-time
  hop)? Plausible and powerful; also a foot-gun. Out of v1.
- **Readiness for self-confirm.** "All Running for N s" is crude; a payload that
  starts then wedges 90 s later confirms anyway. Declarative readiness probes
  (Capsule has none today) would tighten this — future work, cross-cutting
  beyond sealed.
- **Sealed installer USB.** install.md's installer could itself be sealed: a
  field tech plugs in a USB that flashes the appliance with the sealed,
  pre-registered payload and walks away. Strong synergy with install.md;
  in-scope to *note*, out of scope to *build* until both land.
- **Encrypted sealed devices.** The physical-theft gap wants
  [encrypted-volumes.md](encrypted-volumes.md): a sealed *and* encrypted device
  is the real "ship it into a hostile environment" story. Orthogonal
  mechanisms; the composition (where does the unseal key come from with no
  operator and no TPM interaction?) needs its own treatment.
- **PCI/USB-serial passthrough for the drone case.** The drone↔MCU link is a
  passed-through serial device — that is [pci-devices.md](pci-devices.md). A
  sealed manifest referencing a passed-through device is the natural combined
  example; flag the dependency, don't solve it here.
- **`origin` as a column vs. a name namespace.** A column is cleaner; a
  reserved-prefix is zero-migration. Decide at Phase 1; it does not affect the
  external contract.

## Recommendation

Build sealed capsules as a **build mode over the existing daemon**, in the
phase order above:

1. **Bake the payload into the squashfs and project it at boot** (Phase 1) —
   this alone delivers "ship a box that runs a fixed app offline," reusing the
   immutable/A-B/atomic machinery wholesale, with no security surface yet and
   zero daemon forking.
2. **Sign the bundle** (Phase 2) before any device ships that can take a field
   update — an unauthenticated OTA into a shipped product is the one thing that
   must not exist.
3. **Pre-register at build time and offer `locked`** (Phase 3) — the adoption
   story, recommending `pre-registered` as the shipped-hardware default and
   `locked` for true no-operator appliances.
4. **Close the unattended-confirm gap** (Phase 4) — health-gated self-confirm,
   the capability that makes an OTA to a ceiling-mounted projector or an
   in-field drone *safe* rather than merely *possible*.

The result: the same `capsuled`, the same gRPC surface, the same A/B update —
extended so the *application* enjoys the exact guarantees the *OS* already does
(one version, one rollback, one truth), authenticated for an untrusted ship
channel, and able to recover itself with nobody in the field. **Strictly
additive**: a non-sealed build is unchanged, a sealed build is the same binary
reading one config file and one read-only payload directory.
