# Plan

Where Capsule is, where it's going, and the gotchas that need cleaning up before it's not a toy.

## What's working today

Walk this checklist top-to-bottom on a running capsule and every item is a green light:

- **Boot** — `make image && make qemu`; `capsuled` runs as PID 1, mounts /perm, brings up eth0, supervises containerd, listens on :50000.
- **Containers** — `capsulectl apply -f` → `containerd` pulls + runs. Host networking works. Bridge networking via CNI works with port mappings (container port ↔ host port via CNI portmap plugin).
- **MicroVMs** — Firecracker-backed, smolvm-style: shared rootfs + per-VM OCI payload ext4 + vsock agent. `capsulectl exec -t alpine-vm -- /bin/sh` gives you a real interactive shell inside runc inside the VM. `--serial` streams kernel+firecracker output for debugging.
- **MicroVM port mapping** — iptables DNAT tagged with `capsule-vm:<workload>` comment; teardown finds + removes. Tested with nginx on :8080 reachable via curl.
- **MicroVM NAT / DNS** — MASQUERADE rule for `172.20.0.0/16`; capsule-guest injects a default `/etc/resolv.conf` (`1.1.1.1`, `8.8.8.8`) into the OCI payload if absent.
- **Volumes, unified** — raw ext4 at `/perm/volumes/<name>.ext4`. Containers loop-mount; VMs attach as virtio-blk. Same backing file, data moves between them.
- **Lifecycle** — `workload stop / start / restart`; `desired_state=STOPPED` persists across capsule reboots.
- **Logs + Exec** — container workloads via containerd; MicroVM workloads via the vsock agent (`capsule.v1.GuestAgent`). Same `capsulectl` commands, kind-transparent.
- **Persistence** — SQLite at `/perm/state.db`. Workloads + volumes survive capsule reboots.

## Next (in rough priority order)

### 1. Phase 3 — A/B OS updates

The big missing milestone before Capsule is usable without physical access.

- Two rootfs slots (`SLOT_A`, `SLOT_B`) — currently only `SLOT_A` (partition 2). Add SLOT_B as partition 3, bump PERM to partition 4.
- Bootloader selector — a file on the EFI partition (or a syslinux default) says which slot to boot.
- `CapsuleService.UpdateOS` streaming RPC — receives a new rootfs squashfs/ext4, writes to inactive slot, sets `next_boot=inactive` + `tentative=1`, reboots.
- Tentative-flag rollback — on first successful boot in a new slot, `capsuled` clears `tentative`. If capsule fails to come up healthy before the bootloader retry countdown, it falls back to the known-good slot.
- `capsulectl capsule update push <rootfs.sqsh>` round-trip.

Probably 1-2 days of implementation + partition-layout surgery in `pack.sh`.

### 2. Resource limits on MicroVMs

Expose `resources: { cpuMillis, memoryMib, pidsMax }` in `MicroVMSpec`. Translate to `linux.resources` in the OCI config `capsule-guest` writes. runc already enforces. Kubernetes-style limits without the indirection.

### 3. mTLS bootstrap

Today `:50000` is plaintext gRPC. For anything on a real network:
- `capsuled` on first boot generates a cert at `/perm/tls/`, prints its SHA-256 fingerprint on the serial console.
- `capsulectl trust add <fingerprint>` → stored in `~/.config/capsule/trusted-hosts.yaml`.
- gRPC dialer pins the fingerprint; rotates if the server presents a different one.

Matches Talos's model. ~half-day.

### 4. CLI polish

- `capsulectl workload list` should show declared port mappings (today they're only in `workload get`).
- `capsulectl workload get` could print a human-friendly summary on top of the JSON.
- `capsulectl volume mount <name> <path>` — loop-mount a volume on the capsule shell for inspection without a workload.
- `capsulectl volume resize <name> <size>` — grow an ext4 file + resize2fs.
- `--wait` flag on `apply` / `start` / `restart` — block until phase=Running.

### 5. Fleet / multi-capsule CLI (Phase 5)

`capsulectl --capsule a.example.com,b.example.com workload list` — sequential fan-out, tabled output. `~/.config/capsule/config.yaml` with named capsules. Not a cluster yet, just fleet-of-identicals.

### 6. Alternate MicroVM backends

Reserved slots in `MicroVMBackend`: `SMOLVM`, `QEMU`. Adding a QEMU driver unlocks **virtiofs** for volume sharing (if we ever want it) and **PCI passthrough** (for GPU/NIC workloads on real hardware).

## Gotchas to clean up

Things that currently work but are brittle, hacky, or "good enough for now."

### Networking

- **Capsule hostname** is hardcoded to `capsule` in `image/etc/hostname`. Needs to be set per-capsule at first boot (read a MAC-derived default, or fetch from a `/perm/capsule/hostname` if present).
- **VM IP allocation** is hash-of-workload-name → `172.20.254.X`, X = `(hash % 252) + 2`. Collisions possible above ~20 VMs. Swap for a real IPAM that tracks allocations in sqlite.
- **MASQUERADE is a blanket rule** on all traffic leaving the capsule. Fine for homelab; needs narrowing if Capsule ever lives on a shared L2.
- **Port mapping doesn't consider conflicts.** Two VMs both declaring `hostPort: 8080` will both install DNAT rules; whichever got there first wins. Validate at Apply time.

### MicroVM lifecycle

- **TAP teardown race** was hit and fixed: Firecracker's `m.StopVMM` sends SIGTERM and returns; the TAP device was still open when `ip link del` ran. Now we `m.Wait()` for the process to exit before teardownTAP, and `setupTAP` has a nuke-and-retry path for stale busy TAPs. If this bites again in a new way, look at the same area.
- **vsock.uds file lingers** on Firecracker crash. Driver pre-unlinks `vsock.uds` and `api.sock` on every Start. Same for the payload disk dir.
- **Exec -t over vsock has no window-resize** — we get the ExecResize message from the client but runc's CLI has no way to forward it mid-session. Needs a console-socket protocol. Low priority — most interactive use is `-t` for a short command; real sessions via `capsulectl exec` work with 80x24.
- **Guest ready timeout is 60s**, generous to cover contended cloud VMs. Could be smarter (e.g., adapt based on kernel boot time).

### Volumes

- **Fixed 512 MiB size at create.** No resize RPC yet.
- **No concurrent-mount protection beyond what the kernel gives you.** `volume list` shows `MOUNTED_BY` but nothing prevents a user from declaring the same volume on two containers; the second mount fails at runtime (ext4 refuses). Enforce at Apply time.
- **`volume delete` after a crashed workload** may leave `/run/capsule/mounts/<workload>/` dirs. `unmountContainerVolumes` best-effort cleans these, but a stale loop device could linger. `losetup -a` will show any.

### Build / dev loop

- **Buildkit + `mknod`** — overlayfs doesn't allow character-device mknod in Docker RUN, so `vm-shared.ext4` dev nodes are injected via `debugfs` after `mkfs.ext4 -d`. Requires `e2fsprogs-extra` in the rootfs. Works but unusual enough to be surprising.
- **SCP with `-C` (compression) corrupts 2.7 GB disk.raw uploads** on at least one macOS→Linux path we hit. Use `scp` (no -C) for disk.raw. Needs investigation — may be an ssh client bug.
- **pack.sh preserves /perm across rebuilds** by extracting the existing disk.raw's partition 3 before repacking. If someone runs `make clean` they wipe all capsule state. Make sure that's intended before ripping it out.
- **Firecracker is apt-less.** We pull a static upstream release tarball (`v1.10.1`) in the Dockerfile. Bump pins carefully — `firecracker-go-sdk v1.0.0` was verified against this.
- **Firecracker CI kernel 6.1.128** is required for `CONFIG_CGROUP_BPF=y` (runc 1.2+). The old 4.14 quickstart kernel doesn't have BPF cgroup support and `runc run` fails with `bpf_prog_query(BPF_CGROUP_DEVICE): invalid argument`. Don't swap back.

### Daemon / reconciler

- **Reconciler is serial** — one Tick runs `reconcileOne` for every workload sequentially. A slow EnsureRunning (15-20s for a VM that cold-starts) blocks the rest. Acceptable at homelab scale; doesn't scale to 50+ workloads. Parallelize with a worker pool + per-workload mutex eventually.
- **`boot.ExecMu` is a coarse mutex** for *all* exec.Command calls vs the reap loop. Correct but pessimistic. Long-running `runc exec` sessions (interactive shells) hold it the whole time, so orphans pile up until the exec returns. PR-level fix: waitid(WNOWAIT) peek + skip tracked PIDs.
- **Go's `exec.Cmd.Wait` vs Wait4(-1)** race was the original source of pain. The mutex is the right fix at our scale. Revisit if/when we do a PID-tracking reaper.

### Observability

- **No metrics.** No Prometheus endpoint, no `capsulectl capsule stats`. Worth adding once the fleet CLI exists.
- **`capsulectl workload events`** — the reconciler could write a rolling event log (`applied → pulling → starting → running`) visible to operators.
- **Structured logs exist on the capsule serial console only.** A `CapsuleService.StreamLogs` RPC would let `capsulectl capsule logs -f` tail without SSH.

### Security

- **No authN/Z at all.** The capsule is open on :50000. See Next #3 for the mTLS plan.
- **Workloads run as root inside the container by default.** User namespaces are doable via runc config but not wired. Acceptable for homelab; revisit if multi-tenant.
- **`/perm/tls/` doesn't exist yet.** When mTLS lands, this path is where keys live.

## Anti-goals

Things people ask for that are intentionally out of scope for Capsule:

- **Kubernetes compatibility.** Capsule is a smaller shape on purpose. If you want k8s, run k3s inside a capsule.
- **Live migration.** VMs run where they run.
- **Autoscaling.** That's the future orchestrator's job, not the capsule's.
- **Web UI.** Capsule is CLI / API first; a UI on top is welcome as a separate project.
- **Cluster gossip in v1.** Each capsule is its own island. The operator's CLI fans out; no peer-to-peer discovery.
