# Capsule documentation

Capsule is a minimal homelab OS that runs **containers and microVMs** under a single declarative gRPC API. One binary (`capsuled`) is PID 1 on every machine; you talk to it from your laptop with `capsulectl`.

This folder is the operator's manual.

## Start here

- **[getting-started.md](getting-started.md)** — Build the image, boot it (QEMU or real hardware), connect with `capsulectl`, deploy your first workload.
- **[workloads.md](workloads.md)** — Deploy containers and microVMs, with and without volumes. All the manifest shapes.
- **[cli.md](cli.md)** — Full `capsulectl` reference.

## How it works

- **[architecture.md](architecture.md)** — Disk layout, boot chain, runtime drivers, networking, volumes, store. Read this when something breaks at a layer the higher-level guides don't cover.
- **[updates.md](updates.md)** — A/B OS updates: how `capsule update push` ships a new bundle, the tentative-commit window, and automatic rollback.
- **[operations.md](operations.md)** — Breakglass runbooks: growing the PERM partition, debug-container gotchas, and other things that don't (yet) have a first-class verb.

## Proposals

- **[discovery.md](discovery.md)** — mDNS-based discovery of capsules on the LAN: `_capsule._tcp` service announcements, stable short IDs (`capsule-a3f2`) shown on HDMI, `capsulectl discover` and `discover --adopt` for fleet bringup. Status: proposal.
- **[install.md](install.md)** — One-pass bare-metal install: same `disk.raw` boots into installer mode when on USB, mDNS-announces as pending install, `capsulectl install <short-id>` drives the flash and seals the operator's pubkey so the disk comes up already adopted. Replaces today's USB-boot-then-debug-`dd`-then-re-adopt dance. Status: proposal. **Pulls in discovery's mDNS announcer, short IDs, and `discover` browse command as required foundations** (see install.md for the precise required-vs-optional split); discovery's `--adopt` walkthrough and `set-hostname` are not required and can land separately.
- **[encrypted-volumes.md](encrypted-volumes.md)** — Per-volume LUKS2 encryption with TPM-sealed master key, recovery codes, and the full failure-mode matrix. Status: proposal.
- **[secrets.md](secrets.md)** — Two-class secrets model: at-rest startup credentials (sealed via the encrypted-volumes node master key) vs identity-based runtime auth (workload-scoped tokens, no shared blobs). Subsumes the `edge secret set` verb sketched in edge.md. Status: proposal. Builds on encrypted-volumes.
- **[external-disks.md](external-disks.md)** — Pools as a first-class concept: attach / adopt / detach secondary and external (USB) disks, with optional whole-pool or per-volume encryption. Status: proposal.
- **[pci-devices.md](pci-devices.md)** — Operator-registered device passthrough for containers (GPUs, FPGAs, USB-serial). v1 is containers-only; microVM passthrough via smolvm/libkrun is sketched as follow-up. Status: proposal.
- **[fabric.md](fabric.md)** — A WireGuard mesh between capsules: fabric IPs for containers and microVMs in `100.64.0.0/10`, declarative per-workload allow-list policy, default-deny. Operator-driven enrollment, no central control plane. Status: proposal.
- **[edge.md](edge.md)** — Exposing fabric workloads to the public internet: an edge capsule with a public IP runs a managed Caddy, terminates TLS, routes into the fabric. Direct DNS or behind a cloud LB / CDN. ACME auto + DNS-01 + manual cert modes. Status: proposal. Builds on the fabric proposal. 
- **[live-migration.md](live-migration.md)** — Live migration between capsules for containers and microVMs: host CRIU for containers, guest-CRIU baseline for microVM payloads, backend snapshot fast path, and LVM-thin snapshot/delta volume transfer. Status: proposal.
- **[web-ui.md](web-ui.md)** — A browser console for a fleet of capsules: a standalone `capsule-console` (never part of `capsuled`) carrying a grpc-gateway JSON transcoder, multi-capsule fan-out, its own user identity/RBAC + audit, and custody of per-capsule operator keys. Includes wireframes. Status: proposal.
- **[sealed.md](sealed.md)** — Sealed capsules: a build mode for a fixed-payload appliance you can ship (a projector computer, drone compute). The payload (workload manifests + their OCI images) is baked into the immutable A/B squashfs so it rolls back with the slot; updates are signed against a build-time key and self-confirm on payload health when no operator is in the field; pre-registered / adoptable / locked enrollment modes. Reuses install.md's first-boot seed path; extends updates.md. Status: proposal.

## Vocabulary

- **Capsule** — the OS. A machine running Capsule is also called *a capsule* (same noun for OS + instance).
- **`capsuled`** — the daemon that runs as PID 1 on every capsule. Owns early mounts, process supervision, networking bring-up, the gRPC API on `:50000`, and reconciliation of desired → actual workload state.
- **`capsulectl`** — the operator CLI. Set `CAPSULE_HOST=host:port` in your environment to skip `--capsule` on every command.
- **`capsule-guest`** — the tiny PID 1 that runs *inside* every microVM. Speaks gRPC over vsock so the host can `Exec` / `Logs` / `Stop` into the VM.
- **Workload** — a `Container` or `MicroVM` declared by a YAML manifest. Same spec shape (`image`, `command`, `args`, `env`, `ports`, `mounts`); flip `kind` to switch.
- **Volume** — a thin-provisioned LVM logical volume formatted ext4. Mountable into containers (bind) or microVMs (virtio-blk).
- **Image cache** — containerd's local image store on `/perm`. Workloads pull on first use; `capsulectl image push` side-loads images that aren't in any reachable registry.
- **Slot** — `slot_a` / `slot_b`. The two rootfs partitions an A/B update toggles between.
