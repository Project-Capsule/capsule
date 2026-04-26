# Architecture

## High-level

```
┌─ operator's laptop ─┐                 ┌─ a capsule (one machine) ───────────┐
│                     │   gRPC :50000   │  capsuled (PID 1)                   │
│   capsulectl ───────┼────────────────▶│   ├─ workload reconciler            │
│                     │                 │   ├─ container driver  (containerd) │
└─────────────────────┘                 │   ├─ microvm driver    (Firecracker)│
                                        │   ├─ volume service    (LVM thin)   │
                                        │   └─ update service    (A/B)        │
                                        └─────────────────────────────────────┘
```

`capsuled` is the *only* long-running process you write. Everything else (containerd, Firecracker, runc, the kernel) it supervises or shells out to.

## Disk layout

A bootable Capsule disk is MBR-partitioned with **four** partitions. Disk signature is fixed at `0xb1a570ff` (BLASTOFF) so PARTUUIDs are deterministic across rebuilds.

| # | Type   | Label/format         | Size       | Purpose                              |
|---|--------|----------------------|------------|--------------------------------------|
| 1 | 0xEF   | FAT32 `CAPSULEBOOT`  | ~256 MiB   | EFI System Partition. GRUB EFI binary, `grub.cfg`, both slots' `vmlinuz_*` + `initramfs_*`. |
| 2 | raw    | squashfs (`slot_a`)  | ~200 MiB   | Rootfs A. Compressed, immutable.     |
| 3 | raw    | squashfs (`slot_b`)  | ~200 MiB   | Rootfs B. Compressed, immutable.     |
| 4 | 0x8E   | LVM2 PV (`capsule`)  | remainder  | Thin pool backs `/perm` + every user volume + containerd snapshots. |

Slot partitions hold a raw squashfs image written directly to the partition (no filesystem wrapper). Updates `dd` a new squashfs onto the inactive slot.

The LVM volume group `capsule` contains:

- `lv_perm` — ext4, mounted at `/perm`. Holds `state.db`, update staging, anything that must survive reboot.
- `thinpool` — thin pool. Backs both user volumes (`vol-<name>`) and containerd's snapshotter.

## Boot chain

```
UEFI firmware
   ↓ loads
GRUB EFI (from CAPSULEBOOT/EFI/BOOT/BOOTX64.EFI)
   ↓ reads
/EFI/BOOT/grub.cfg                    ← rewritten on confirm to flip default slot
   ↓ chooses entry (default 0 = slot_a, 1 = slot_b)
linux /vmlinuz_<a|b> root=PARTUUID=b1a570ff-0(2|3) rootfstype=squashfs ro capsule.slot=<a|b> ...
initrd /initramfs_<a|b>
   ↓
custom busybox-static initramfs:
   - loads modules in dependency order (virtio → SCSI core → AHCI → NVMe → USB → squashfs → overlay).
     busybox `insmod` does NOT auto-resolve deps, so each module's deps must
     load earlier in the list — see the "ORDERING RULE" comment in
     `image/initramfs-init`. A misorder fails silently with rc=2 and the
     device never enumerates.
   - retries up to 30 s for /dev/disk/by-partuuid/b1a570ff-0(2|3) to appear (USB boot can be slow)
   - mounts the slot squashfs read-only at /media/root-ro
   - tmpfs at /media/root-rw, overlayfs writable upper at /media/root-rw/upper
   - mounts overlay (lower=root-ro, upper=tmpfs) at /sysroot
   - switch_root /sysroot /sbin/init
   ↓
/sbin/init = capsuled (PID 1)
```

The active slot is detected by parsing `capsule.slot=a|b` from `/proc/cmdline`. No PARTUUID math, no device-path string parsing.

## capsuled startup

In order, on a real capsule (`isPID1 == true`):

1. **Early logging** — open `/dev/tty0` if available, multiplex slog to `stderr + tty0` so kernel-message-style output shows on the HDMI console.
2. **Boot init** (`boot.Init`) — early mounts (`/proc`, `/sys`, `/dev`, `/run`, `/tmp`, cgroup v2), bring up loopback, load NIC + bridge modules, set hostname.
3. **Activate LVM** — `vgchange -ay capsule`. On first boot of a machine where `capsule` VG doesn't exist, create it on partition 4 (looked up by `0x8E` type), then create `lv_perm` (ext4) + `thinpool`.
4. **Mount /perm** — `/dev/capsule/lv_perm` → `/perm`.
5. **Configure containerd** — write `/etc/containerd/config.toml` with the `devmapper` snapshotter pointed at `capsule/thinpool`.
6. **Bring up uplink** — wait up to 15 s for any `eth*` interface, then `udhcpc -i <iface> -q -n -t 4 -A 2`. On success, kick off a one-shot `ntpd -q` against `pool.ntp.org`/Cloudflare/Google in a goroutine.
7. **Print ASCII banner + IP** to `/dev/tty0` and `/dev/console` so the operator can see "we're up, here's the IP".
8. **Update service `OnStartup`** — read `os_state` from SQLite. If we're in tentative mode (booted into a pending slot), arm the auto-rollback `time.AfterFunc`. If the bootloader rolled us back (active slot != pending slot), clear pending state.
9. **Start containerd** as a supervised child process.
10. **Open SQLite** at `/perm/state.db`, run migrations.
11. **Start workload reconciler** — desired-state loop reads `workloads` table, drives container/microvm drivers to match.
12. **Serve gRPC** on `:50000` (default) — registers `CapsuleService`, `WorkloadService`, `VolumeService`.
13. **Reap zombies** in the background (PID 1 duty).

In dev mode (running `capsuled` on a Linux laptop, not as PID 1), most of the boot work is skipped; just the gRPC server starts. Updates and slot logic are no-ops.

## Workloads

A workload is one row in SQLite. The `kind` enum picks which driver runs it:

- `Container` → `runtime/container.Driver` (containerd + runc, devmapper snapshotter on the thin pool).
- `MicroVM` → `runtime/microvm/firecracker.Driver` (Firecracker over KVM).

The reconciler ticks every few seconds: read all workloads, ask each driver "what's the actual state?", compute the diff, drive forward. Crash recovery is the same as steady state — the diff handles it.

### Container path

containerd creates a snapshot in the thin pool for the OCI image, runc starts the process. Network mode picks the namespace plumbing:

- `HOST` — shares the capsule's net namespace. Ports bind directly on the host.
- `BRIDGE` — veth into `br0` via the CNI `bridge` plugin. Port mappings are `iptables` DNAT rules tagged `capsule-ctr:<name>` for O(1) teardown.

Volume mounts on a container are bind mounts: capsuled mounts `/dev/capsule/vol-<name>` at a host path (`/run/capsule/vol-<workload>-<volume>`), then injects an OCI bind mount entry into the runtime spec at the requested `mountPath`.

### MicroVM path (smolvm-style)

Every microVM boots the *same* shared rootfs (`vm-shared.ext4` — busybox + runc + `capsule-guest` as `/sbin/init`). The user's OCI image is flattened into a per-VM payload disk (`payload.ext4`) attached as `/dev/vdb`. Inside the VM:

```
capsule-guest (PID 1, gRPC over vsock CID=3 port 52)
  └─ runc run payload   ← user's container, mounted from /dev/vdb at /oci
```

Volume mounts on a microVM become additional virtio-blk drives (`/dev/vdc`, `/dev/vdd`, …). `capsule-guest` mounts each at the requested `mountPath` inside the VM's namespace before starting the payload.

Networking: TAP device on `br0`, static IP in `172.20.254.0/24`, kernel `ip=` cmdline. Outbound `MASQUERADE`. Port mappings via `iptables` DNAT tagged `capsule-vm:<name>`.

The host control surface (`Exec`, `Logs`, `Stop`) flows over the vsock to `capsule-guest`, which proxies into runc. Same API shape as containers.

### Logs

- **Container** — capsuled tails containerd's task IO files.
- **MicroVM** — two paths:
  - `workload logs <name>` — payload stdout/stderr via the guest agent (vsock).
  - `workload logs --serial <name>` — Firecracker serial console (kernel boot + `capsule-guest` + early failures). Use this when the guest agent isn't reachable.

## Volumes

Volumes are first-class. One verb, two consumers:

- Created via `apply -f volume.yaml` or `volume create`.
- Backed by an LVM thin LV at `/dev/capsule/vol-<name>`, ext4 formatted on first create.
- Mountable into a container *or* a microVM by referencing `volumeName` in the workload manifest. Same data either way; the volume can move between a container and a VM with no conversion.
- One mounter at a time (kernel-enforced — ext4 doesn't allow concurrent mounts).
- Resizable while detached: `volume resize <name> <size>` runs `lvresize` + `resize2fs`. **Grow only.**

`/perm` itself is *not* a volume — it's a fixed LV (`lv_perm`) for capsule state. User data goes in named volumes.

## Networking

Single bridge, `br0`, lives in the capsule's net namespace. The capsule's uplink (e.g. `eth0`) keeps DHCP'd; `br0` is for workloads.

```
                         ┌──────────────────────────┐
   uplink (eth0/dhcp)    │  capsule netns           │
   ────────────────▶ NAT │   ├─ br0 (172.20.254.1)  │
                         │   │   ├─ veth-ctr-foo ──▶ container
                         │   │   ├─ veth-ctr-bar ──▶ container
                         │   │   ├─ tap-vm-baz   ──▶ Firecracker microVM
                         │   │   └─ tap-vm-qux   ──▶ Firecracker microVM
                         │   └─ MASQUERADE on eth0 │
                         └──────────────────────────┘
```

Port mappings are iptables DNAT rules with `--comment` tags. Container rules carry `capsule-ctr:<name>`; VM rules carry `capsule-vm:<name>`. Workload teardown deletes the tagged rules — no rule churn against the rest of the table.

## State

All persistent capsule state is one SQLite database at `/perm/state.db`:

- `workloads` — desired specs. The reconciler's source of truth.
- `volumes` — volume metadata (size).
- `os_state` — singleton row tracking active slot, pending slot + deadline, last good slot, last committed version. The A/B update brain.

Writes are serialized through one connection. The `core/` packages know nothing about SQLite — they take a `Store` interface (`store/store.go`) and there's an in-memory impl for tests (`store/memory/`).

## Code layout

```
cmd/
  capsuled/        — PID-1 daemon main
  capsulectl/      — operator CLI
  capsule-guest/   — microVM PID-1 agent (vsock gRPC server, runs runc)
boot/              — early mounts, zombie reap, banner, supervised child spawn
core/
  workload/        — kind-agnostic workload lifecycle
  volume/          — volume CRUD, lvcreate/resize/remove
  reconciler/      — desired → actual tick loop
  update/          — A/B update receive/stage/confirm/rollback
runtime/
  container/       — containerd-backed driver
  microvm/firecracker/ — Firecracker-backed driver
controllers/       — gRPC handlers (proto ⇄ core)
router/            — gRPC server wiring
store/             — Store interface + sqlite + in-memory impls
models/capsule/v1/ — proto sources + generated Go (flat capsule.v1 package)
image/
  Dockerfile          — capsule rootfs image
  Dockerfile.packer   — packer image (kernel, modules, mksquashfs, grub-mkstandalone)
  build.sh / pack.sh  — orchestration: rootfs → squashfs → bootable disk + update bundle
  initramfs-init      — custom PID 1 init for the initramfs
examples/          — declarative manifests
docs/              — this folder
```

## Design notes

- **No `internal/`.** Layers are `models → core → store + runtime`; concrete adapters are wired in `cmd/capsuled/main.go` only.
- **Flat proto package** (`capsule.v1`) with service-prefixed message names so the three services coexist without collisions.
- **`boot.ExecMu`** serializes `exec.Command` calls against the PID-1 reap loop. Without it, the reaper `wait4`s a subprocess before `cmd.Wait` can, producing spurious `waitid: no child processes` errors on `iptables` / `ip` / `mkfs` / etc.
- **Atomic OS update bundles.** Kernel + initramfs + rootfs ship together. One version, one rollback, no kernel/userland version drift.
- **Squashfs rootfs, overlayfs upper.** The rootfs is genuinely immutable; runtime writes land on a tmpfs that vanishes at reboot. Persistence is `/perm` only.
- **One mounter per volume.** Enforced by ext4. Move data between a container and a VM by deleting one workload before applying the other.
