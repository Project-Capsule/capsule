# Plan

Where Capsule is, where it's going, and the gotchas that need cleaning up before it's not a toy.

## What's working today

Walk this checklist top-to-bottom on a running capsule and every item is a green light:

- **Boot** — `make image && make qemu`; `capsuled` runs as PID 1, mounts /perm, brings up eth0, supervises containerd, listens on :50000.
- **Containers** — `capsulectl apply -f` → `containerd` pulls + runs. Host networking works. Bridge networking via CNI works with port mappings (container port ↔ host port via CNI portmap plugin).
- **MicroVMs** — Firecracker-backed, smolvm-style: shared rootfs + per-VM OCI payload ext4 + vsock agent. `capsulectl exec -t alpine-vm -- /bin/sh` gives you a real interactive shell inside runc inside the VM. `--serial` streams kernel+firecracker output for debugging.
- **MicroVM port mapping** — iptables DNAT tagged with `capsule-vm:<workload>` comment; teardown finds + removes. Tested with nginx on :8080 reachable via curl.
- **MicroVM NAT / DNS** — MASQUERADE rule for `172.20.0.0/16`; capsule-guest injects a default `/etc/resolv.conf` (`1.1.1.1`, `8.8.8.8`) into the OCI payload if absent.
- **Volumes, unified** — thin LVs in VG `capsule` at `/dev/capsule/vol-<name>`, ext4 formatted. Containers mount directly; VMs attach as virtio-blk. Same block device, one mounter at a time.
- **Storage substrate** — `/perm` is a plain LV in the capsule VG; user volumes are thin LVs in the sibling `thinpool`. VG is initialized on first boot if the PERM partition is blank. (VM payload disks are still flatten-to-ext4 today; unifying them into the thin pool is tracked in PLAN §4.)
- **Lifecycle** — `workload stop / start / restart`; `desired_state=STOPPED` persists across capsule reboots.
- **Logs + Exec** — container workloads via containerd; MicroVM workloads via the vsock agent (`capsule.v1.GuestAgent`). Same `capsulectl` commands, kind-transparent.
- **`workload cp`** — scp-style file/directory copy in and out of containers and MicroVMs (kubectl-style tar-pipe through Exec; see "Gotchas" below for the rewrite tracked).
- **Persistence** — SQLite at `/perm/state.db`. Workloads + volumes survive capsule reboots.
- **A/B OS updates** — `capsulectl capsule update push <bundle.tar>` streams a new rootfs+kernel+initramfs to the inactive slot, GRUB flips active, capsule reboots; tentative-commit on first successful boot, auto-rollback to the known-good slot on health failure. `update confirm` locks it in.
- **Breakglass / debug** — `capsulectl capsule debug` deploys a privileged container with host PID + bind-mounted `/dev` + `/sys` + `/perm` + `/sbin`/`/usr/sbin`/`/usr/bin`, drops you into an interactive shell, cleans up on exit (or `--keep`).
- **System introspection** — `capsulectl capsule info` (hostname, kernel, uptime, CPU, memory, boot disk, thinpool fill %), `capsulectl capsule logs -f` (tails `capsuled`'s own slog over gRPC, no SSH needed).
- **Real hardware** — boots end-to-end on Beelink (Intel Jasper Lake) with UEFI, AHCI/SATA M.2 SSDs, and the linux-lts 6.18 kernel. Same image boots under QEMU.

## Next (in rough priority order)

### 0. Jailer hardening for Firecracker (HIGH PRIORITY)

Today `runtime/microvm/firecracker/driver.go` launches the `firecracker` binary directly as a subprocess of capsuled (which runs as PID 1, root, all caps). We get KVM + Firecracker's built-in seccomp for free, and that's genuinely strong — but Firecracker ships a companion `jailer` binary (already installed in the image at `/usr/bin/jailer`) that we're not using. Wiring it up gives defense-in-depth if anyone ever finds a KVM escape:

- chroot per VM
- network / PID / mount namespaces around each VMM
- cap drops (VMM stops being root)
- cgroup memory/CPU limits on the VMM process
- extra seccomp policy layer

For single-tenant homelab the current setup is fine. This matters most if/when we run untrusted workloads, multi-tenant, or compliance-adjacent workloads. Not a rewrite — swap `fc.VMCommandBuilder{}.WithBin(...)` for the jailer path and configure `fc.Config.JailerCfg`. ~1 day of work.

### 1. Resource limits on MicroVMs

Expose `resources: { cpuMillis, memoryMib, pidsMax }` in `MicroVMSpec`. Translate to `linux.resources` in the OCI config `capsule-guest` writes. runc already enforces. Kubernetes-style limits without the indirection.

### 2. mTLS bootstrap

Today `:50000` is plaintext gRPC. For anything on a real network:
- `capsuled` on first boot generates a cert at `/perm/tls/`, prints its SHA-256 fingerprint on the serial console.
- `capsulectl trust add <fingerprint>` → stored in `~/.config/capsule/trusted-hosts.yaml`.
- gRPC dialer pins the fingerprint; rotates if the server presents a different one.

Matches Talos's model. ~half-day.

### 3. Unify VM payload disks with the LVM thin pool

**Natural follow-up to the LVM thin migration** — the user-volume half landed and was verified end-to-end on the VPS; the VM-payload half was explicitly deferred because of a containerd/LVM ownership clash documented below. Pick this up when you're next in the storage code; ~2 days of work on top of the groundwork already in place.

Today user volumes live in the capsule VG's thin pool, but VM payload disks still go through the pre-LVM flatten-to-ext4 path (~30 s on alpine; no block-level CoW between identical VMs). The right architecture (fly.io pattern) is to put VM payloads in the same thin pool via containerd's devmapper snapshotter so 10 identical VMs share image blocks until they write.

Blocker: containerd's devmapper snapshotter wants to own a thin pool's device-id allocation. LVM-managed pools don't expose the internal `-tpool` dm device usefully, and the LVM-visible pool LV rejects the thin-pool messages containerd sends. Fix is to create the thin pool via `dmsetup` directly (data + metadata LVs backing it, still LVM-managed, but the pool target itself is dmsetup-constructed). Then `pool_name` in containerd config matches a dm device containerd fully owns.

Scope: rework `boot/boot_linux.go:initializeCapsuleVG` to create raw `thinmeta`/`thindata` LVs and then `dmsetup create capsule-thinpool` over them. Re-enable devmapper as default snapshotter in `image/etc/containerd-config.toml`. Revert `runtime/microvm/firecracker/image.go:preparePayloadDisk` to the snapshot-prepare path (git history has the version — the commit that this note first appeared in also contains the snapshot-based implementation that was reverted). Re-verify end-to-end on the VPS with two identical alpine VMs: the second should boot under 5 s and `lvs` should show the thin pool dedup'ing extents.

### 4. Volume data lifecycle (builds on LVM thin)

Now that `/perm` is an LVM thin pool and every volume is a thin LV, snapshot/backup/migration is mostly plumbing over existing LVM primitives. Rough order:

- **Phase B — Local snapshots.** `capsulectl volume snapshot <vol> [--name v1]` → `lvcreate -s` (instant, thin, shares extents with source). `capsulectl volume snapshots list <vol>` and `volume restore <vol> <snap>` (creates a new LV from the snapshot). Retention rotation as a scheduled job. Same semantics as fly.io's default daily snapshots + 5-day retention.
- **Phase C — Offline export/import.** `capsulectl volume export <vol> [--snapshot]` → snapshot then stream `dd | zstd` to stdout or an image file. Matching `volume import <name> <file>`. Covers the 90% "back this up somewhere" case — no new daemons, just shell-out pipelines.
- **Phase D — Cross-capsule live migration (dm-clone + iSCSI).** Source exports the snapshot as an iSCSI LUN; destination stacks `dm-clone` over an empty LV with the iSCSI export as the remote source. VM boots immediately on the destination; blocks hydrate in the background. DISCARD pass-through on the guest short-circuits empty space. This is fly.io's machine-migration mechanism. ~2-3 weeks of real work, wait until there's actually a second capsule to migrate between.

### 5. Low-priority cleanup

Accumulated lint/dead code spotted in passing. Batch into a single cleanup PR when someone's in the area:
- `runtime/container/driver.go` — unused `errNotFound` sentinel near EOF; unused `_ = strings.ToLower` import-suppression hack above it.
- `boot/boot_linux.go:38` — `initPlatform(ctx)` takes a `context.Context` that's no longer used; either use it or drop it.
- `boot/boot_linux.go:279` — switch `strings.Split` → `strings.SplitSeq` per analyzer (Go 1.25+ perf nit).

### 6. CLI polish

- `capsulectl workload list` should show declared port mappings (today they're only in `workload get`).
- `capsulectl workload get` could print a human-friendly summary on top of the JSON.
- `capsulectl volume mount <name> <path>` — mount a volume LV on the capsule shell for inspection without a workload.
- `--wait` flag on `apply` / `start` / `restart` — block until phase=Running.

### 7. Fleet / multi-capsule CLI

`capsulectl --capsule a.example.com,b.example.com workload list` — sequential fan-out, tabled output. `~/.config/capsule/config.yaml` with named capsules. Not a cluster yet, just fleet-of-identicals.

### 8. Alternate MicroVM backends

Reserved slots in `MicroVMBackend`: `SMOLVM`, `QEMU`. Adding a QEMU driver unlocks **virtiofs** for volume sharing (if we ever want it) and **PCI passthrough** (for GPU/NIC workloads on real hardware).

### 9. Prebuilt capsule-debug image

`capsulectl capsule debug` currently uses `alpine:3.20` and tells the operator to `apk add lvm2 e2fsprogs iptables iproute2` once they're inside. That works (lvm2 etc. happily talk to the host's LVM via the bind-mounted /dev + /sys + /perm) but adds 5–10 seconds to the first session and needs network access to dl-cdn.alpinelinux.org. Replace with a small purpose-built image we publish to a registry — `ghcr.io/<org>/capsule-debug:<version>` — with the toolchain baked in: `lvm2`, `e2fsprogs`, `iptables`, `iproute2`, `util-linux`, `blkid`, `strace`, `tcpdump`, `lsof`, plus host bin compatibility (the bind-mounted host /sbin/lvs etc. work directly when the image's libdir matches Alpine's). Make the default image override-able with `--image` so operators can use their own. Keep the alpine fallback documented for air-gapped environments.

## Gotchas to clean up

Things that currently work but are brittle, hacky, or "good enough for now."

### Networking

- **VM IP allocation** is hash-of-workload-name → `172.20.254.X`, X = `(hash % 252) + 2`. Collisions possible above ~20 VMs. Swap for a real IPAM that tracks allocations in sqlite.
- **MASQUERADE is a blanket rule** on all traffic leaving the capsule. Fine for homelab; needs narrowing if Capsule ever lives on a shared L2.
- **Port mapping doesn't consider conflicts.** Two VMs both declaring `hostPort: 8080` will both install DNAT rules; whichever got there first wins. Validate at Apply time.

### MicroVM lifecycle

- **TAP teardown race** was hit and fixed: Firecracker's `m.StopVMM` sends SIGTERM and returns; the TAP device was still open when `ip link del` ran. Now we `m.Wait()` for the process to exit before teardownTAP, and `setupTAP` has a nuke-and-retry path for stale busy TAPs. If this bites again in a new way, look at the same area.
- **vsock.uds file lingers** on Firecracker crash. Driver pre-unlinks `vsock.uds` and `api.sock` on every Start. Same for the payload disk dir.
- **Exec -t over vsock has no window-resize** — we get the ExecResize message from the client but runc's CLI has no way to forward it mid-session. Needs a console-socket protocol. Low priority — most interactive use is `-t` for a short command; real sessions via `capsulectl exec` work with 80x24.
- **Guest ready timeout is 60s**, generous to cover contended cloud VMs. Could be smarter (e.g., adapt based on kernel boot time).
- **Volume-flush on Stop depends on in-memory `d.vms[name]`.** `agent.Stop` now unmounts every user volume before returning so ext4 commits its journal before Firecracker dies — but it only fires when `Driver.Remove` dials the guest agent over the existing in-memory `guestConn`. If capsuled was restarted (zombie VM still running, in-memory `d.vms` map empty), `Remove` falls through to `Shutdown`/`StopVMM` directly and the umount never happens → silent data loss recurs. Fix shape: have `Remove` re-dial the guest agent fresh from `<vmDir>/vsock.uds` when the in-memory entry is missing. Tied to the broader "reconnect to live VMs after capsuled restart" gap (today the reconciler will try to *create* a second VM with the same name and fail at TAP/socket conflicts).

### Volumes

- **No concurrent-mount protection beyond what the kernel gives you.** `volume list` shows `MOUNTED_BY` but nothing prevents a user from declaring the same volume on two containers; the second mount fails at runtime (ext4 refuses). Enforce at Apply time.
- **`volume delete` after a crashed workload** may leave `/run/capsule/mounts/<workload>/` dirs. `unmountContainerVolumes` best-effort cleans these.
- **Thin pool exhaustion is fatal.** Overcommitted volumes + a guest that fills one → pool ENOSPC → writes to *every* thin LV in the pool start failing. Need to configure `thin_pool_autoextend_threshold` / `thin_pool_autoextend_percent` in `/etc/lvm/lvm.conf` and expose pool fill % as a capsule metric. Capsule doesn't yet.
- **Volume resize is grow-only.** `resize2fs` can shrink ext4 but requires `e2fsck -f` first and is dangerous; not exposed. Must be detached — the `refsTo` check enforces this.

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

### Security

- **No authN/Z at all.** The capsule is open on :50000. See Next #3 for the mTLS plan.
- **Workloads run as root inside the container by default.** User namespaces are doable via runc config but not wired. Acceptable for homelab; revisit if multi-tenant.
- **`/perm/tls/` doesn't exist yet.** When mTLS lands, this path is where keys live.

## Anti-goals

Things people ask for that are intentionally out of scope for Capsule:

- **Kubernetes compatibility.** Capsule is a smaller shape on purpose. If you want k8s, run k3s inside a capsule.
- **Autoscaling.** That's the future orchestrator's job, not the capsule's.
- **Web UI.** Capsule is CLI / API first; a UI on top is welcome as a separate project.
- **Cluster gossip in v1.** Each capsule is its own island. The operator's CLI fans out; no peer-to-peer discovery.
