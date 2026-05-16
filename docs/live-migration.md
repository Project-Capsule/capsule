# Live migration between capsules (proposal)

> **Status:** Proposal. Not implemented. This proposes a practical migration path for both containers and microVM-backed workloads between machines, while keeping Capsule's "no extra smart control plane" direction.

## Summary

Capsule today is single-node by design: workloads, memory state, and local thin-LV-backed volumes live on one machine. This proposal adds **operator-driven cross-machine migration** with two workload paths:

1. **Containers:** host-level CRIU checkpoint/restore.
2. **MicroVMs:** two-tier strategy
   - **Portable baseline:** run CRIU *inside the guest* against the payload container (`capsule-guest` + runc), then restore on destination.
   - **Backend-specific fast path:** Firecracker snapshot/restore where available for near-zero downtime VM-state transfer.

For storage, use the current LVM thin substrate: snapshot, pre-seed destination volume, copy dirty delta at cutover, then switch execution.

The key design choice is orchestration scope: **capsules do not become globally aware by default**. A trusted operator-side workflow (or external scheduler) picks source + destination and invokes migration RPCs against both.

## Why this shape

- It matches Capsule's current architecture: one capsule is autonomous, and multi-node logic is additive.
- It keeps migration explicit and debuggable: no hidden distributed controller.
- It supports both current Firecracker microVMs and future runtimes.
- It takes advantage of existing building blocks already used by Capsule:
  - thin-LV volumes,
  - `capsule-guest` in microVMs,
  - declarative workload specs,
  - operator-mediated control via `capsulectl`.

## Goals

- Migrate a `Container` workload between capsules with minimal downtime.
- Migrate a `MicroVM` workload between capsules, including a runtime-agnostic path.
- Handle attached volumes using LVM thin snapshots and incremental transfer.
- Preserve Capsule's explicit operator control: source and destination are chosen externally.
- Keep migration additive: existing non-migration flows continue unchanged.

## Non-goals (v1)

- Fully automatic placement/scheduling across a fleet.
- Concurrent multi-writer volume semantics across source and destination.
- Transparent migration over arbitrary WAN links without operator-provided reachability.
- Perfectly lossless migration for every workload type (some apps may require short quiesce windows).

## Current constraints (from today's architecture)

- Containers run via containerd/runc.
- MicroVMs run via Firecracker and execute payload containers inside the guest via `capsule-guest`.
- Volumes are ext4 on LVM thin LVs and are single-mounter at runtime.
- Cross-capsule networking/policy is still proposal-stage (`fabric.md`), so migration control traffic must assume explicit routable endpoints.

These constraints are compatible with migration, but they require an explicit cutover protocol.

## Migration model

Migration is a **directed operation**:

- source capsule: current running workload.
- destination capsule: pre-created target workload shell + target volume LVs.
- operator (or external orchestrator): coordinates both via API/CLI.

### High-level phases

1. **Preflight**
   - Validate destination compatibility (runtime backend, CPU features, kernel capabilities, volume capacity, image availability).
2. **Prepare destination**
   - Create workload placeholder in `STOPPED/PREPARING` state.
   - Materialize image/layers and volume targets.
3. **State replication**
   - Memory/process checkpoint transfer (CRIU or VM snapshot path).
   - Volume snapshot streaming and incremental catch-up.
4. **Cutover**
   - Brief source freeze.
   - Final delta transfer.
   - Restore/start on destination.
   - Switch networking exposure.
5. **Finalize**
   - Mark destination authoritative.
   - Stop and optionally garbage-collect source artifacts.

## Containers: host-level CRIU path

For `kind: Container`, use CRIU in the host namespace around the container process tree.

### Flow

1. Source performs iterative `pre-dump` rounds while container runs.
2. Destination receives checkpoint artifacts and prepares restore environment.
3. Source performs final freeze + dump.
4. Destination restores container and resumes.
5. Source is torn down after health confirmation.

### Notes

- Requires kernel + CRIU feature checks on both hosts.
- Works best when runtime and kernel capabilities are closely matched.
- Networking continuity depends on addressing strategy (see Networking section).

## MicroVMs: two-tier approach

MicroVM migration needs both portability and performance. Use two modes.

### Mode A (portable baseline): CRIU inside guest

Because Capsule microVMs run a payload container inside the VM, we can checkpoint **that payload** from within the guest via `capsule-guest`.

Flow:

1. Source host starts target microVM shell on destination (same guest base image contract).
2. Source asks guest agent to CRIU checkpoint payload container.
3. Checkpoint is transferred to destination guest.
4. Destination guest restores payload container.
5. Source guest payload is stopped.

This gives migration portability across microVM backends as long as the guest contract (`capsule-guest` ABI + base OS tooling) is preserved.

Tradeoff: this migrates application/container state, not every detail of VM kernel/device runtime state.

### Mode B (optimized fast path): runtime snapshot/restore

When source and destination share a compatible backend (e.g., Firecracker) and feature set, use VMM snapshot/restore for lower downtime.

Flow:

1. Source pauses VM and captures memory/device snapshot.
2. Snapshot and block deltas transfer.
3. Destination restores VM from snapshot and resumes.

This mode is backend-specific but faster. It should be optional and capability-gated.

### Recommendation

- Ship **Mode A first** for portability and architecture stability.
- Add **Mode B** as an accelerator for supported backend pairs.

## Volumes and LVM thin

Volumes are the hardest part, but current LVM thin choices make them tractable.

### Proposed volume strategy

1. Create destination thin LV with target size.
2. Source creates read-only snapshot of source LV.
3. Stream snapshot blocks to destination (initial seed).
4. During pre-copy, track changed extents and send incremental deltas.
5. At cutover, briefly quiesce/freeze workload, take final delta snapshot, apply final delta.
6. Attach destination volume and start restored workload.

This follows the same shape already outlined in `PLAN.md` for advanced migration (`dm-clone` style) and can evolve from simpler snapshot-send first to more live-like block hydration later.

### Consistency rules

- ext4 journal replay is expected and acceptable at destination.
- One active writer invariant remains: source writer is stopped before destination writer is declared active.

## Networking and identity at cutover

Migration UX depends on preserving how clients find the workload.

v1 options:

- **Operator-managed endpoint flip:** update DNS / reverse proxy / edge mapping during cutover.
- **Fabric-aware stable workload identity (future):** if `fabric.md` lands, migrate by moving workload identity to destination while keeping policy and address stable.

Without stable identity, migration still works but may require a short reconnect window.

## API and CLI shape (proposal)

Operator-facing (illustrative):

- `capsulectl workload migrate <name> --from <src> --to <dst> [--mode auto|criu|snapshot]`
- `capsulectl workload migrate status <migration-id>`
- `capsulectl workload migrate abort <migration-id>`

Daemon-side concepts:

- Migration session ID
- Source/destination capability negotiation
- Explicit phase state machine persisted in SQLite
- Idempotent resume after daemon restart

## Security model

- Migration is authenticated with existing capsule API auth.
- Session-scoped, short-lived migration tokens authorize source↔destination transfer.
- Checkpoint artifacts and volume streams must be encrypted in transit.
- Sensitive checkpoint data is deleted from source/destination staging after finalize/abort.

## Failure handling

- **Preflight failure:** no source interruption; migration not started.
- **Replication failure:** source stays authoritative and running.
- **Cutover failure before destination ready:** source resumes.
- **Destination unhealthy after cutover:** rollback policy configurable:
  - immediate failback if source checkpoint still resumable,
  - or operator-mediated recovery if not.

Every phase must report clear, user-visible status.

## Compatibility contract

For predictable outcomes, migration should require/validate:

- compatible Capsule version range,
- compatible kernel/CRIU capability set for CRIU modes,
- compatible microVM backend + CPU feature set for snapshot mode,
- compatible volume filesystem expectations.

If checks fail, CLI reports a concrete unsupported reason before any freeze.

## Implementation phases

### Phase 1: Container migration (CRIU) + offline volume handoff

- Add migration state machine and RPC scaffolding.
- Implement container CRIU checkpoint/restore with explicit cutover.
- Implement snapshot-based volume seed + final sync with source freeze.
- Keep networking cutover operator-managed.

### Phase 2: MicroVM portable migration (guest CRIU)

- Extend `capsule-guest` with checkpoint/restore verbs for payload container.
- Standardize guest toolchain contract (e.g., Alpine baseline + CRIU availability where needed).
- Reuse host migration orchestration and volume path from Phase 1.

### Phase 3: MicroVM fast path (backend snapshot)

- Add Firecracker snapshot/restore migration mode with capability gate.
- Keep guest-CRIU path as fallback.

### Phase 4: UX and policy integration

- Integrate with fabric identity once fabric exists.
- Add guardrails, progress telemetry, retries, and resumability improvements.

## Open questions

- Should v1 require a standardized microVM guest base (e.g., Alpine guest root) to guarantee guest-CRIU behavior?
- Do we expose migration policy in workload spec, CLI-only, or both?
- For large volumes, when do we switch from snapshot-send to dm-clone-style lazy block hydration?
- What is the minimal supported rollback guarantee after final cutover?

## Recommendation

Proceed with an **operator-orchestrated migration feature** that keeps capsules simple and independent:

1. Start with container CRIU + LVM-thin snapshot transfer.
2. Add microVM migration using guest-side CRIU for runtime portability.
3. Layer backend-native VM snapshots as an optimization, not as the only path.

This keeps Capsule aligned with its current philosophy (explicit, minimal control plane), while making cross-machine workload movement practical for both containers and microVMs.
