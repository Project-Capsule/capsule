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

## Vocabulary

- **Capsule** — the OS. A machine running Capsule is also called *a capsule* (same noun for OS + instance).
- **`capsuled`** — the daemon that runs as PID 1 on every capsule. Owns early mounts, process supervision, networking bring-up, the gRPC API on `:50000`, and reconciliation of desired → actual workload state.
- **`capsulectl`** — the operator CLI. Set `CAPSULE_HOST=host:port` in your environment to skip `--capsule` on every command.
- **`capsule-guest`** — the tiny PID 1 that runs *inside* every microVM. Speaks gRPC over vsock so the host can `Exec` / `Logs` / `Stop` into the VM.
- **Workload** — a `Container` or `MicroVM` declared by a YAML manifest. Same spec shape (`image`, `command`, `args`, `env`, `ports`, `mounts`); flip `kind` to switch.
- **Volume** — a thin-provisioned LVM logical volume formatted ext4. Mountable into containers (bind) or microVMs (virtio-blk).
- **Slot** — `slot_a` / `slot_b`. The two rootfs partitions an A/B update toggles between.
