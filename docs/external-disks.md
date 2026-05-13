# External and secondary disks (proposal)

> **Status:** Proposal. Not implemented. Companion to [encrypted-volumes.md](encrypted-volumes.md) — read that first; the encryption sections here build on its key hierarchy.

## Summary

Capsule today has one disk: the OS disk holds the ESP, both A/B rootfs slots, and a `PERM` partition that the LVM `capsule` VG sits on. All user volumes share that one thin pool. This proposal introduces **pools**: each additional disk becomes its own VG + thinpool with a user-facing name, and volumes get a `pool` field that selects which one hosts them. Pools can be **fresh** (capsuled formats the disk on attach), **adopted** (capsuled recognises an existing capsule VG when a disk from another machine is plugged in), and either **plain**, **pool-encrypted** (one LUKS container under the whole VG), or **per-volume-encrypted** (LUKS per LV inside an unencrypted VG — what the encrypted-volumes proposal already specifies for the bootstrap pool).

The non-negotiable invariants:

1. **Yanking an external disk never corrupts data** beyond what was un-fsync'd at the moment of removal. ext4 journal + dm-crypt are tolerant of clean detach; clean detach is what `capsulectl pool detach` provides.
2. **The OS keeps booting** when an external pool's disk is missing. Workloads that mount volumes from the absent pool stay `PENDING`; everything else runs.
3. **An external disk can be moved to a different Capsule host** and its volumes opened there, given the master recovery code (for encrypted pools) or just the disk itself (for plain pools).

## Goals

- A first-class **pool** concept that names a disk and is referenced by `volume create --pool <name>`.
- **Whole-disk encryption** as a pool-level setting, in addition to the per-volume encryption from the encrypted-volumes proposal.
- **Attach / detach** verbs that make plugging and unplugging external storage a normal operator action, not a breakglass procedure.
- **Adopt** existing Capsule-formatted disks moved between hosts.
- **Stable across reboot and device-name shuffle** — LVM PV UUIDs are the source of truth, not `/dev/sdb`.
- **Never auto-import** an unrecognised disk. Attaching storage is always operator-initiated.

## Non-goals (v1)

- Multi-disk pools (RAID, striping, mirroring). One disk per pool. LVM supports the multi-device case; failure semantics are intricate enough to defer.
- Cross-pool volume migration verbs (e.g. `volume move bulk fast`). Operators `cp`-and-recreate.
- Automatic tiering / hot-data promotion between pools.
- iSCSI, NFS, or any network-backed pool. Local block devices only.
- Live re-encryption of a pool (changing a plain pool to encrypted in place). Operators rebuild.

## Concept: pools

A **pool** is a one-to-one mapping from a user-facing name to a Capsule-managed VG on a specific disk. The bootstrap pool is the one that already exists today.

| User-facing name | VG name on disk        | Backing                                           | Encryption       |
|------------------|-----------------------|---------------------------------------------------|------------------|
| `bootstrap`      | `capsule`             | Partition 4 of the OS disk (the PERM partition)   | per-volume only  |
| `<custom>`       | `capsule-<custom>`    | A whole secondary disk (or one partition of it)   | none / per-volume / pool-level |

`bootstrap` is special: `/perm` lives in it, capsuled boots from it, and SQLite (every pool's metadata) lives in it. Everything else is **secondary**. Secondary pools can be **internal** (a second NVMe / SATA SSD that's always present) or **external** (USB, eSATA, dock) — Capsule treats them the same; the *external* distinction is about operator workflow (hotplug, detach-before-unplug) not about the on-disk format.

The default pool for `volume create` (no `--pool` flag) is `bootstrap`. Operators can change the default with `capsulectl pool set-default <name>`.

### SQLite schema

New table:

```
CREATE TABLE pools (
  name              TEXT PRIMARY KEY,
  vg_name           TEXT NOT NULL UNIQUE,        -- LVM VG name on the disk
  pv_uuid           TEXT NOT NULL,               -- stable across device-name changes
  disk_serial       TEXT,                        -- /dev/disk/by-id snapshot, informational
  encryption_mode   TEXT NOT NULL,               -- 'none' | 'pool' | 'per_volume'
  pool_key_blob     BLOB,                        -- present iff encryption_mode='pool'
  state             TEXT NOT NULL,               -- 'active' | 'unavailable' | 'detached'
  is_default        INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
```

Existing `volumes` table grows a `pool TEXT NOT NULL DEFAULT 'bootstrap'` column. Migration: add column with default, fill in `bootstrap` for every existing row.

## Disk lifecycle

### Attach (fresh)

```
capsulectl pool attach /dev/sdb --name bulk [--encrypt]
```

capsuled:

1. **Safety check**: refuse if `wipefs --probe` shows any existing filesystem or partition signature. Operator must pass `--force-wipe` to clobber.
2. **`pvcreate /dev/sdb`** — initialise as LVM PV. Record PV UUID.
3. **`vgcreate capsule-bulk /dev/sdb`** — VG named after the pool with a `capsule-` prefix to make ownership obvious.
4. **`lvcreate --thinpool thinpool --extents 95%FREE capsule-bulk`** — thin pool consuming the disk (reserve 5% for LVM metadata + growth).
5. If `--encrypt`: see *Pool-level encryption* below — wraps the whole VG behind a single LUKS container before step 2.
6. Insert row into `pools` with `state='active'`.
7. Print summary: pool name, capacity, encryption mode, PV UUID.

### Attach (adopt)

```
capsulectl pool attach /dev/sdb --adopt [--recovery-key <code>]
```

For a disk that already has a `capsule-<name>` VG on it from another Capsule host. capsuled:

1. **`pvscan` + `vgimport`** — discover existing VGs on the disk.
2. Filter to those with the `capsule-` prefix.
3. For each: read VG metadata, look for the encryption marker (a known LUKS header signature in slot 0 of the disk, or a `capsule-meta` LV inside the VG that we drop at create time).
4. If encrypted at the pool level → operator must supply the recovery key (or, if the source host's master is reachable, the original wrapped key). For per-volume-encrypted pools → the per-volume recovery keys are still needed but the VG opens fine.
5. Activate the VG, run thin-pool repair if needed (`thin_check`).
6. Insert `pools` row with `state='active'`, fresh `is_default=0`.

The new host learns about each volume on the pool by reading capsule's per-volume metadata LV (a tiny LV in each capsule pool holding a JSON manifest of `{name, size, encryption_spec, ...}` — written by `volume create`, kept in sync with the host SQLite, and the source of truth on adoption). Each adopted volume's record gets inserted into the new host's `volumes` table with `pool='<this pool>'`.

### Detach (clean)

```
capsulectl pool detach bulk
```

capsuled:

1. Refuse if any workload on the host is currently using a volume from this pool (same check as `volume delete`).
2. Refuse if `bootstrap` (special-cased — can't detach the OS).
3. `luksClose` any open mappers from this pool.
4. `vgchange -an capsule-bulk` — deactivate the VG.
5. If pool-encrypted: `cryptsetup luksClose` the pool-level mapper.
6. `vgexport capsule-bulk` — mark the VG as exported so other LVM hosts know it's available for import.
7. Update `pools.state='detached'`.
8. Print: "Safe to physically remove /dev/sdb (PV UUID: ...)."

### Detach (forced — emergency)

```
capsulectl pool detach bulk --force
```

For when the disk has *already* been yanked and the pool is wedged. Skips all the orderly steps, just deletes the SQLite rows. Volumes from the gone pool become orphans in `volumes`; reconciler marks them `MISSING`. Workloads using them stay `PENDING` forever (until operator deletes or reattaches the disk).

## Encryption modes

A pool's `encryption_mode` is set at create / adopt time and never changes. Three options:

### `none` — plain

Today's behavior for the bootstrap pool. PV directly on the disk/partition. LVs are bare block devices, ext4 lives on them. Anyone with the disk reads everything.

### `pool` — full-disk LUKS under the VG

```
/dev/sdb                                ← physical disk
  └─ /dev/mapper/capsule-pool-bulk      ← LUKS2 container, opened at pool-attach
       └─ capsule-bulk (VG)             ← LVM PV here
            └─ thinpool / vol-X / vol-Y
```

Setup at attach:

1. `cryptsetup luksFormat /dev/sdb` with a random 64-byte key.
2. `cryptsetup luksAddKey` with a random 32-byte recovery key — **printed once to operator**, same UX as per-volume recovery in encrypted-volumes.md.
3. `cryptsetup luksOpen` → `/dev/mapper/capsule-pool-bulk`.
4. `pvcreate /dev/mapper/capsule-pool-bulk`, then `vgcreate` etc on the mapper device.
5. Wrap the pool key under the node master key (same master from encrypted-volumes.md). Store wrapped form in `pools.pool_key_blob`.
6. `cryptsetup luksHeaderBackup` → `/perm/luks-headers/pool-bulk.hdr`.

At every boot (or `pool attach <name>` for a previously-detached pool), capsuled:

1. Unwraps `pool_key_blob` with the master (requires TPM unseal or operator `node unlock`).
2. `luksOpen` the disk with the unwrapped key.
3. Activates the VG.

**This is the right default for external/USB disks** because the on-disk metadata (LVM, free-space patterns, volume names) is itself revealing. One key opens the whole pool. Per-volume LUKS on top would be redundant overhead.

### `per_volume` — LUKS per LV inside an unencrypted VG

Exactly what [encrypted-volumes.md](encrypted-volumes.md) describes. Useful for the `bootstrap` pool (where capsuled must boot and reach SQLite before any unsealing happens) and for mixed-content pools. Pool-level metadata (LV layout, free space) is unencrypted; volume contents are.

### Mode-vs-pool matrix

| Pool                | `none` | `pool` | `per_volume` |
|---------------------|--------|--------|--------------|
| `bootstrap`         | yes (today's default) | **no** (can't decrypt before SQLite is reachable) | yes (planned per encrypted-volumes.md) |
| Internal secondary  | yes    | yes    | yes          |
| External / USB      | yes    | yes (recommended default) | yes |

## With-TPM and without-TPM behavior

Pools inherit the master key from the node-level encryption setup. There is **one node master** regardless of how many pools exist; every pool key (for `pool` mode) and every per-volume key (for `per_volume` mode) is wrapped under that one master.

- **TPM present, auto-unsealed:** At boot, capsuled unseals the master, then for each `active` pool with `encryption_mode != 'none'` it unwraps the pool/volume keys and opens what it needs as workloads start. Operator sees no difference between encrypted and plain pools.
- **TPM absent / sealed unavailable / PCR drift:** capsuled enters `locked`. Volumes in plain pools work; volumes in any encrypted pool wait. `capsulectl node unlock` (passphrase) or `node unlock --recovery <code>` unwraps the master, and from there all encrypted pools open.

External pools attached *while the node is locked* fail the open step and report a clear error. Operator unlocks the node first.

## Identifying disks across reboot and device-name shuffle

`/dev/sdb` is not stable — plug a USB stick in first and your secondary SSD becomes `/dev/sdc`. LVM solves this by writing a PV UUID into the disk's metadata header; `pvscan` finds the VG regardless of `/dev/sdX` name. capsuled stores `pv_uuid` on the `pools` row and **always** resolves a pool to a device via `pvs --noheadings -o pv_name --select "pv_uuid=<uuid>"`, never by remembered `/dev/sdb`.

The `disk_serial` column is informational only — a snapshot of `/dev/disk/by-id/...` at attach time, shown in `pool list` so operators can correlate a pool with a physical disk. capsuled does not trust it for lookup.

## Boot semantics with missing pools

At capsuled startup, after `bootstrap` is activated and SQLite is reachable:

1. For each `pools` row with `state='active'`:
   - `pvscan` for the recorded `pv_uuid`.
   - **Found** → activate VG, open pool-LUKS if encrypted, mark in-memory state `online`.
   - **Not found** → log a warning, mark state `unavailable`. Do not block boot.
2. Workload reconciler proceeds. Any workload that mounts a volume from a pool currently `unavailable` stays `PENDING` with a clear status message (`pool 'bulk' is unavailable: disk not present`).
3. Plugging the disk in later → operator runs `capsulectl pool attach bulk --rescan` → capsuled rescans, finds the PV, activates, opens, marks `online`. Pending workloads start.

The operator-explicit rescan step is deliberate — capsuled does not poll for disks or react to udev events to import volumes. **No automatic import.** This avoids two foot-guns:

- A misplaced USB stick whose VG happens to be named `capsule-bulk` getting auto-imported and shadowing the real pool.
- An external disk in a flaky cradle that disconnects and reconnects causing thinpool corruption when LVM re-finds it half-active.

## Failure scenarios

Builds on encrypted-volumes.md §Failure scenarios; only the *disk-additional* modes are listed here.

### Physical / hotplug

| Failure                                              | Symptom                                | Recovery                                                                                       |
|------------------------------------------------------|----------------------------------------|------------------------------------------------------------------------------------------------|
| External disk unplugged with no live writes          | Pool transitions `online → unavailable`; in-flight reads/writes hit I/O errors at first attempt then dm-multipath-style queueing if configured (we don't configure it; errors propagate immediately) | Replug → `pool attach bulk --rescan`. Workloads resume. ext4 journal handles any partially-written transactions |
| External disk unplugged during heavy writes          | Same as above + un-fsync'd dirty pages dropped | Same recovery. **Data written but not fsync'd is lost** — same guarantee as any consumer USB-with-yank scenario. Workloads should fsync if they care |
| USB cradle flap (disconnects/reconnects rapidly)     | LVM may see the PV come back with a different `/dev/sdX` mid-operation, thinpool can corrupt | Mitigation: capsuled `pool attach --rescan` is required to re-activate; LVM won't auto-reactivate. Worst case: thin_check + `pool repair` from a debug session |
| Disk fails entirely (drive electronics)              | I/O errors on every access             | If pool-encrypted: data unrecoverable (same as any disk failure). Per-volume recovery keys don't help — there's no readable ciphertext to decrypt |
| Two USB disks swapped between ports                  | Both come up; `pvscan` finds both by UUID; capsuled associates each with the correct pool | No action needed — UUID lookup is correct |

### Adoption / move

| Failure                                              | Recovery                                                                                       |
|------------------------------------------------------|------------------------------------------------------------------------------------------------|
| Move pool-encrypted external disk to new Capsule host | `capsulectl pool attach /dev/sdb --adopt --recovery-key <code>`. Pool LUKS opens; per-volume metadata LV is read; volumes are imported into the new host's SQLite |
| Move per-volume-encrypted disk to new Capsule host   | Adopt opens the VG (no encryption at VG level). Each encrypted volume needs its per-volume recovery key to open. Volumes with lost recovery keys are inaccessible but visible (capsulectl marks them `LOCKED`) |
| Move plain external disk to new Capsule host         | Adopt activates VG and imports volumes — no keys involved                                      |
| Move disk to *non*-Capsule Linux host                | LVM tools see the VG normally. Encrypted pools open with `cryptsetup luksOpen --key-file=<recovery>`; encrypted volumes open the same way. Reading the actual files needs only standard ext4 |

### Pool / VG corruption

| Failure                                              | Recovery                                                                                       |
|------------------------------------------------------|------------------------------------------------------------------------------------------------|
| Pool-level LUKS header corrupted                     | `cryptsetup luksHeaderRestore --header-backup-file /perm/luks-headers/pool-bulk.hdr` |
| Thinpool metadata corrupted (power-loss during pool-op) | `thin_repair` from a debug session; documented in operations.md addendum |
| VG metadata corrupted but PVs intact                 | `vgcfgrestore capsule-bulk` from one of LVM's automatic VG metadata backups (in `/etc/lvm/backup` on the host) |
| `/perm` (bootstrap) corrupted, losing SQLite        | Other pools' on-disk metadata LV is the source of truth. After bootstrapping a fresh node, `pool attach <disk> --adopt` re-populates `pools` and `volumes` rows from each disk's metadata LV |

### Capsuled crash / partial operation

| Crash point                                          | State left behind                                       | Cleanup                              |
|------------------------------------------------------|---------------------------------------------------------|--------------------------------------|
| Mid `pool attach`, after `pvcreate`/`vgcreate` but before SQLite commit | Stranded VG on disk                              | Boot-time reconciler: any VG matching `capsule-*` not referenced by `pools` is reported in `capsule info`; operator deletes manually with `pool attach --adopt` then `pool detach` |
| Mid `pool attach` with `--encrypt`, after `luksFormat` but before SQLite commit | Stranded LUKS container with no key recorded     | Same — operator runs `pool attach --adopt` with the recovery key (which *was* printed before SQLite write) |
| Mid `pool detach`, after `vgchange -an` but before SQLite update | VG deactivated on disk, `pools.state` still 'active' | Reconciler: on next boot, find VG inactive but PV present → activate → resume state |

The pattern matches encrypted-volumes.md: **the SQLite write is the commit point**, but we deliberately print the recovery key *before* it so a crash between key-add and commit doesn't strand the operator without a way back in.

## What this does not cover

- **Hot-swap bays with kernel hotplug events.** The proposal requires operator-explicit `pool attach --rescan`. Even if udev sees a disk reconnect, capsuled won't auto-import. This is the right safety default; a future opt-in `pool attach --auto-rescan` flag could relax it for trusted bays.
- **A failed *bootstrap* disk.** If the OS disk dies, the node is gone. External pools survive — adopt them from a fresh Capsule install on new hardware.
- **Pooled redundancy.** No mirroring, no RAID at the Capsule layer. Operators who want this layer it underneath (mdadm, bcachefs, dm-raid) and present the array as a single block device to `pool attach`. Capsule treats it as one disk.
- **Network-attached storage.** Out of scope; iSCSI / NBD would work mechanically (they're block devices) but timeouts, multipathing, and credential management are a whole separate proposal.

## Open questions

- **Per-pool metadata LV format.** Proposed: a 16 MiB LV named `capsule-meta` inside each capsule VG, holding a length-prefixed JSON blob with the pool's volume manifest. Updated atomically (write to a temp LV, `lvrename` swap, remove old). Alternative: store everything only in host SQLite, accept that adoption needs a sidecar manifest file — simpler but breaks the "disk is self-describing" property.
- **Default encryption mode for external pools.** Proposal leans toward `pool` (full LUKS). Counter-argument: per-volume gives finer-grained recovery (one bad volume doesn't take the whole disk). Probably let operator pick, with `pool` as the default we recommend in docs.
- **Pool resizing.** Adding a second disk to an existing pool (multi-disk pool) is explicitly non-goal v1, but growing the *underlying* disk (e.g. a virtualized environment where the disk got expanded) is reasonable. `pool grow <name>` = `pvresize` + `lvextend thinpool 100%FREE`. Worth including in v1; trivial after the rest lands.
- **Should `pool detach` require a confirmation prompt?** Yes, probably — destructive-ish (workloads pending) and physical-action-coupled. Match `capsule update push` confirmation pattern.

## Implementation pointers

- Proto: `models/capsule/v1/pool.proto` (new) — `PoolService` with `Attach`, `Adopt`, `Detach`, `Get`, `List`, `Rescan`, `SetDefault`, `Grow`. `Volume` proto grows `string pool = N`.
- Schema: new `pools` table; `volumes.pool` column with default `'bootstrap'` for migration.
- Logic: new `core/pool/` package. `core/volume/service.go` learns to resolve `pool` → VG name and call `lvcreate -T capsule-<pool>/thinpool` instead of the hardcoded `capsule/thinpool`.
- Key manager (from encrypted-volumes.md) grows methods for wrapping/unwrapping pool keys — same primitive as volume keys, different table.
- Runtime drivers don't change — they still see `/dev/mapper/...` or `/dev/<vg>/...` paths; resolving which VG happens earlier.
- Boot: extend `boot/boot_linux.go:initializeCapsuleVG` to *only* set up the bootstrap pool. Secondary pool activation is a `pool.Service.ActivateAll(ctx)` call after SQLite is open.
- Image: no new packages (LVM and cryptsetup already in scope from encrypted-volumes.md). Possibly add `thin_check` / `thin_repair` to documented breakglass tools in operations.md.
- CLI: `capsulectl pool {attach,detach,list,get,rescan,grow,set-default}`. `volume create --pool` flag.
