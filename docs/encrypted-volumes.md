# Encrypted volumes (proposal)

> **Status:** Proposal. Not implemented. This document captures the design we want to commit to *before* writing code, so the failure modes and recovery story are settled up front. The implementation plan tracking this work lives in `PLAN.md` once accepted.

## Summary

Capsule volumes today are bare ext4 on thin LVs — a stolen disk is fully readable. This proposal adds opt-in **per-volume LUKS2 encryption**, decrypted host-side by `capsuled`. Per-volume LUKS keys are wrapped by a **node master key** that capsuled either unseals from the TPM at boot (preferred) or derives from an operator passphrase (TPM-less fallback). Every encrypted volume also carries a **recovery key** printed once at create time, and the node master itself carries a **master recovery code** printed once at init — both are LUKS-native escape hatches that work even if the TPM dies, the motherboard is replaced, or the disk is moved to a different host.

The non-negotiable invariant: **no single hardware failure, configuration change, or capsuled crash should make data unrecoverable**, provided the operator kept the recovery codes that capsuled emitted at create/init time. If they didn't keep the codes, that is the only way to lose data.

## Goals

- **Encryption at rest** for per-volume user data on a Capsule node.
- **Auto-unlock** on boot when a TPM 2.0 is present and the boot chain hasn't been tampered with.
- **Explicit, documented operation** on hardware without a TPM.
- **Per-volume opt-in.** Existing plain volumes keep working unchanged; encryption is a flag on `volume create`.
- **A second keyslot** for every encrypted volume, printed once at create time, as the recovery path.
- **Bulk recovery** for the operator who loses the host TPM (replacement motherboard, disk moved between machines) without needing every per-volume key separately.
- **Survives `capsule update push`.** Kernel / userspace updates must not require operator intervention to re-unlock volumes.

## Non-goals (v1)

- Re-encrypting an existing plain volume in place. Operators migrate by `cp`/recreate.
- Per-volume key rotation (`cryptsetup luksAddKey`/`luksRemoveKey` is available manually).
- Master-key rotation. Master is set at `node init-encryption` and lives forever.
- Network-attached unseal (Tang/Clevis), KMS escrow, Shamir-split shares.
- Guest-side decryption (key never visible on host). Host-side is enough for the stolen-disk threat model; see *What this does not protect against* below.

## Threat model

| Threat                                       | Protected? |
|----------------------------------------------|------------|
| Disk pulled from a powered-off Capsule       | **Yes**    |
| Disk imaged via USB-boot on the same machine | **Yes** (different PCRs → TPM won't unseal; no operator passphrase → no master) |
| Capsule running, attacker with `capsulectl` credentials | No — same as today; mTLS + operator-JWT is the trust boundary |
| Capsule running, attacker with root on the host | No — master is in `capsuled` memory; host-side decryption means a memory dump exposes it |
| Cold-boot / DRAM remanence attack             | No (out of scope for homelab; guest-side decryption would help if we ever care) |
| Compromised firmware update changing PCRs    | **Yes, with operator notice** — TPM unseal fails, capsule enters `locked` state, operator must supply recovery code; data is not lost |

## Design

### Storage layout

Per encrypted volume:

```
/dev/capsule/vol-<name>            (thin LV)  ← already exists today
/dev/mapper/capsule-vol-<name>     (dm-crypt) ← new; ext4 lives here
```

`capsuled` opens the LUKS device when a workload needs the volume, mounts the resulting mapper node for containers, and hands the mapper path to Firecracker as the virtio-blk backing for microVMs. The guest sees plain ext4 — decryption is invisible to it.

### Key hierarchy

```
            ┌─────────────────────────────────────────────────┐
            │   Node master key  (32 bytes, random)           │
            │   - Wrapped at rest in node_keys.master_blob    │
            │   - Unwrapped only in capsuled memory           │
            └────────────────────┬────────────────────────────┘
                                 │ AES-256-GCM
              ┌──────────────────┼─────────────────────────────┐
              │                  │                             │
      ┌───────▼────────┐ ┌───────▼────────┐           ┌────────▼────────┐
      │ vol-A LUKS key │ │ vol-B LUKS key │   ...     │ vol-N LUKS key  │
      │ (in volumes    │ │ (in volumes    │           │ (in volumes     │
      │ row, wrapped)  │ │ row, wrapped)  │           │ row, wrapped)   │
      └───────┬────────┘ └───────┬────────┘           └────────┬────────┘
              │ LUKS slot 0      │ slot 0                      │ slot 0
              │                  │                             │
     Slot 1 = per-volume recovery key, printed-once at create. Independent of master.
```

The master is wrapped under one of two mechanisms (chosen at `node init-encryption` and recorded in `node_keys.source`):

| Source              | `node_keys.master_blob` is...                           | Auto-unlock on boot?       |
|---------------------|---------------------------------------------------------|----------------------------|
| `TPM2_SEALED`       | TPM2 sealed object bound to PCR 7 (secure-boot policy)  | Yes                        |
| `PASSPHRASE`        | Argon2id parameters + salt; master = Argon2id(pass, salt) | No — operator unlocks each boot |

A **master recovery code** is generated alongside the master in either mode and printed once. It is a separate Argon2id-derived alternative path to the same master key — equivalent in power to the passphrase, but distinct so it can be filed offline as a break-glass.

### Why PCR 7 (and only PCR 7)

The TPM seal binds the master blob to a PCR policy. Capsule binds to **PCR 7 only**:

- PCR 7 captures the **secure-boot policy state** — which keys are trusted, whether secure boot is on. It changes only when the operator deliberately reconfigures secure boot in firmware.
- PCR 4, 5, 8, 9 capture *measurements* of the kernel and initramfs. Binding to these would cause **every `capsule update push` to invalidate the seal**, forcing the operator to recovery-key the node after every routine update. That's a bad UX.
- The trade: an attacker who can sign a kernel with the operator's secure-boot keys can boot a custom kernel that the TPM will still unseal for. If they can do that, they don't need our disk in the first place.

systemd-cryptenroll defaults to PCR 7 for the same reason. This is the right default. Operators who want stricter binding can opt into more PCRs later via a config knob (out of scope for v1).

### Per-volume create flow

```
capsulectl volume create foo --encrypt
  │
  ▼
capsuled.VolumeService.Create:
  1. lvcreate -V <size>M -T capsule/thinpool -n vol-foo
  2. generate vol_key (64 bytes random)
  3. generate recovery_key (32 bytes random)
  4. cryptsetup luksFormat --type luks2 --cipher aes-xts-plain64 \
       --key-size 512 --key-file=<vol_key> /dev/capsule/vol-foo
  5. cryptsetup luksAddKey --key-file=<vol_key> \
       /dev/capsule/vol-foo <recovery_key>
  6. cryptsetup luksHeaderBackup /dev/capsule/vol-foo \
       --header-backup-file /perm/luks-headers/vol-foo.hdr    (see Failure §)
  7. cryptsetup luksOpen --key-file=<vol_key> ... vol-foo
  8. mkfs.ext4 /dev/mapper/capsule-vol-foo
  9. cryptsetup luksClose capsule-vol-foo
 10. wrapped = AES-GCM(master, vol_key)
 11. INSERT INTO volumes (..., encrypted=1, key_blob=wrapped) VALUES (...)
 12. PRINT recovery_key to operator stdout ONCE; never logged.
```

Every step is idempotent or rolled back on the next step's failure (the LV is removed if any of steps 4–11 fail). The order matters: the recovery key is added *before* mkfs, so even if step 8 crashes mid-format, the recovery key still opens the (empty) LV.

### Attach flow (workload start)

```
capsuled.VolumeService.Attach("foo"):
  if v.encrypted:
    if /dev/mapper/capsule-vol-foo exists: return that
    wrapped = SELECT key_blob FROM volumes WHERE name='foo'
    vol_key = AES-GCM-Open(master_in_memory, wrapped)
       └── master must be unwrapped (TPM unsealed or passphrase supplied)
       └── if locked → return ErrNodeLocked; workload stays pending
    cryptsetup luksOpen --key-file=<vol_key> /dev/capsule/vol-foo capsule-vol-foo
    return /dev/mapper/capsule-vol-foo
  else:
    return /dev/capsule/vol-foo   (existing plain path)
```

Container driver bind-mounts the returned path; Firecracker driver attaches it as virtio-blk. Neither cares which kind it got.

## With-TPM operation

1. **Adopt the node**, push the encryption-capable image.
2. `capsulectl node init-encryption` (one time). capsuled:
   - probes `/sys/class/tpm/tpm0/tpm_version_major`,
   - generates the master, seals to PCR 7 via `tpm2_create` + `tpm2_load`,
   - generates and prints the master recovery code **once**,
   - stores the sealed blob in `node_keys`.
3. `capsulectl volume create foo --encrypt`. Recovery key printed once.
4. Reboot. capsuled at startup: `tpm2_unseal` → master in memory → encrypted volumes attachable. Operator does nothing.

## Without-TPM operation

This is the path on hardware that doesn't expose a usable TPM 2.0 (older boards, virtualized installs without vTPM, BIOS with TPM disabled and operator can't / won't change it). It is fully supported.

1. `capsulectl node init-encryption --passphrase`. capsuled:
   - generates the master (random),
   - generates Argon2id parameters (`time=4, memory=256 MiB, parallelism=1`, per-node random salt),
   - derives a wrapper key from the operator passphrase + salt,
   - stores `AES-GCM(wrapper, master)` in `node_keys.master_blob`,
   - prints master recovery code **once**.
2. `capsulectl volume create foo --encrypt` — same as with-TPM.
3. **Reboot**. capsuled has no usable TPM and a `PASSPHRASE` `node_keys.source`, so it enters the `locked` state at startup. Workloads with encrypted-volume mounts stay `PENDING` (status message: `waiting for node unlock`). Workloads using only plain volumes start normally.
4. Operator runs `capsulectl node unlock` → prompted for passphrase → capsuled derives wrapper, unwraps master, drops to `unlocked` state. Pending workloads attach their volumes and start.

The master recovery code works in either mode: `capsulectl node unlock --recovery <code>` bypasses both the TPM seal and the passphrase wrapper, deriving the master from the recovery code directly.

## Recovery keys: what they unlock, when to use them

There are **two recovery secrets** an operator must store off-box. They are different:

### Per-volume recovery key

- Printed once, at `volume create --encrypt`.
- Equivalent in power to the master-derived LUKS slot 0, but for *that volume only*.
- Used when the operator moves the LV to a *different host* (where the master is not available at all), or when capsuled / the master is broken but a specific volume needs to be opened from a debug session: `cryptsetup luksOpen --key-file=<key> /dev/capsule/vol-foo`.

### Master recovery code

- Printed once, at `node init-encryption`.
- Equivalent in power to the TPM unseal or the passphrase wrapper. Unwraps the master, which then unwraps every encrypted volume's wrapped key.
- Used when the TPM dies, when PCR 7 has drifted, or when the operator just doesn't remember the passphrase.

The mental model: **per-volume recovery moves one volume. Master recovery saves the whole node.** Operators should keep both, but the master recovery is the one that's needed 99% of the time.

## Failure scenarios

Every failure below has a documented path to **zero data loss**, provided the operator kept the recovery codes printed at init/create time.

### TPM-related

| Failure                                              | Symptom                                | Recovery                                                                                          |
|------------------------------------------------------|----------------------------------------|---------------------------------------------------------------------------------------------------|
| TPM disabled in BIOS / cleared                       | `tpm2_unseal` fails → node `locked`    | `capsulectl node unlock --recovery <code>` → capsuled re-seals to new TPM state on next boot      |
| Motherboard replaced (new TPM)                       | `tpm2_unseal` fails on new hardware    | Same as above                                                                                     |
| Secure-boot config changed → PCR 7 drift             | `tpm2_unseal` fails → node `locked`    | Same as above. capsuled re-seals to current PCR 7 after unlock                                    |
| TPM hardware fault / takes too long                  | `tpm2_unseal` times out                | Same as above. capsuled falls back to recovery-code mode automatically after one boot of timeouts |

### Disk-level

| Failure                                              | Symptom                                | Recovery                                                                                          |
|------------------------------------------------------|----------------------------------------|---------------------------------------------------------------------------------------------------|
| Disk moved to a new Capsule host                     | New host has no master in SQLite       | `capsulectl volume import` (new verb) with per-volume recovery key — or reinit encryption on the new host and replay recovery code |
| LUKS header corrupted on a specific volume           | `luksOpen` errors with "no valid keyslots" | `cryptsetup luksHeaderRestore --header-backup-file /perm/luks-headers/vol-foo.hdr` (we always back up the header at create — see step 6 above) |
| `/perm` corrupted, SQLite lost                       | No `volumes` rows, no `node_keys` row  | Volumes still exist as LUKS LVs; the per-volume recovery keys still open them. Operator reinitializes encryption on a fresh node and imports volumes one at a time with their recovery keys |
| Thin pool exhausted mid-write                        | Workload sees I/O errors               | Unchanged from today — ext4 surfaces the error; thin-pool autoextend / `volume resize` is the fix. Encryption does not amplify this |

### Power loss

| Failure                                              | Symptom                                | Recovery                                                                                          |
|------------------------------------------------------|----------------------------------------|---------------------------------------------------------------------------------------------------|
| Power loss during `luksFormat` (step 4)              | LV exists, no LUKS header              | `volumes` row was not yet written (step 11). Reconciler removes the orphan LV on next boot        |
| Power loss between `luksFormat` and `luksAddKey`     | LV has LUKS header with slot 0 only    | `volumes` row not yet written → orphan LV → cleanup. *No volume has been promised to the operator yet* |
| Power loss between `luksAddKey` and the SQLite write | LV is a complete encrypted volume      | `volumes` row not yet written → orphan LV → cleanup. The recovery key was never printed because step 12 hadn't run |
| Power loss during workload write                     | Same as today: ext4 journal handles it | Unchanged. dm-crypt is transparent to the FS layer here |

The pattern is: **the SQLite write is the commit point.** Before it, the operator has not been told the volume exists, so removing the half-built artifact is correct. After it, every artifact (LV + LUKS header + ext4) is durable.

### Operator error

| Failure                                              | Recovery                                                                                          |
|------------------------------------------------------|---------------------------------------------------------------------------------------------------|
| Operator lost per-volume recovery key, TPM still alive | Master still unwraps slot 0; no action needed unless the volume is being moved off-host. Optionally regenerate via `volume rotate-recovery <name>` (v2)  |
| Operator lost master recovery code, TPM still alive  | Same — the master is unsealable from TPM, so day-to-day operation is fine. The risk is *future* TPM failure. Operator should `node rotate-recovery` (v2) to print a fresh code |
| Operator lost master recovery code AND TPM died      | **Data is unrecoverable** unless every encrypted volume has its per-volume recovery key stored separately. This is the failure mode the recovery codes exist to prevent. There is no backdoor |
| `capsulectl node reset-encryption` when not intended | Refused if any `volumes.encrypted=1` row exists. Only runs on a node that has *never* created an encrypted volume |

### Update / boot-chain interactions

| Event                                                | Effect on encryption                                                                              |
|------------------------------------------------------|---------------------------------------------------------------------------------------------------|
| `capsule update push` (kernel + initramfs)           | **No effect.** PCR 7 unchanged by kernel content; only secure-boot policy moves it                |
| A/B slot flip                                        | **No effect** (same reason)                                                                       |
| Failed update → auto-rollback to other slot          | **No effect** — the alternate slot's measurements don't matter to PCR 7                           |
| Operator enables/disables secure boot in firmware    | PCR 7 changes → next boot fails to unseal → `locked` state → recovery code → re-seal              |
| Firmware (BIOS) update that changes vendor keys      | PCR 7 may change → same recovery path                                                             |

### capsuled bugs / crash mid-operation

| Crash point                                          | State left behind                                       | Cleanup                              |
|------------------------------------------------------|---------------------------------------------------------|--------------------------------------|
| Mid `Create` (before SQLite commit)                  | Orphan LV, possibly with LUKS header                    | Boot-time reconciler removes orphan LVs not in `volumes` |
| Mid `Delete` (LV removed, SQLite row not yet deleted) | `volumes` row points at missing LV                      | Boot-time reconciler removes orphan SQLite rows |
| Mid `Attach` (luksOpen succeeded, mount failed)      | Open mapper, no mount                                   | Reconciler luksCloses unused mappers at workload-reconcile time |
| capsuled OOM-killed                                  | All wrapped keys lost from memory                       | Restart → re-unseal (TPM) or wait for `node unlock` (passphrase). Nothing on disk changed |

## What this does NOT protect against

- **A running, root-compromised Capsule.** The master is in capsuled's memory. Anyone with `/proc/<pid>/mem` on a compromised host extracts it. Mitigation in scope: jailer for Firecracker (PLAN §0); not in scope: locking down the host itself further.
- **An attacker with valid operator credentials.** Same as today. mTLS + Ed25519 JWT (`auth/doc.go`) is the trust boundary, not the disk encryption.
- **Cold-boot DRAM attacks.** Out of scope for homelab. Guest-side decryption (key never on host) would help, at the cost of adding `cryptsetup` and `dm_crypt` to the microVM rootfs and a key-delivery RPC over vsock. Future work if the threat model changes.
- **A motherboard with a malicious TPM.** Out of scope; if you don't trust your hardware vendor, you cannot use a TPM at all.

## Open questions

- **Recovery-key format.** 32-hex-chars in 4-char groups (`a3f2-1c9d-...`) is the boring choice; BIP39 24-words is more transcribable but pulls in a wordlist dependency. Lean toward hex.
- **`pool` interaction (multi-disk).** When the multi-pool storage proposal lands, each pool gets its own master? Or one node master wraps keys across all pools? Leaning toward *one master, many wrapped keys* — simpler, and "lose a disk" doesn't lose the master.
- **Argon2id parameters for `PASSPHRASE` mode.** `t=4, m=256MiB, p=1` is solid for 2026 desktop CPUs but takes ~1 s on a Beelink N100. Acceptable for boot-time unlock; document the tradeoff.
- **Where the LUKS header backup lives.** `/perm/luks-headers/<volume>.hdr` is the obvious place but means `/perm` corruption now correlates with header loss. Alternative: stash a copy inside the SQLite row (~16 KiB per volume — acceptable). Probably do both.

## Implementation pointers

The execution plan lives outside this doc; once accepted, it lands in `PLAN.md` and roughly:

- Proto changes: `models/capsule/v1/volume.proto`, `models/capsule/v1/node.proto`
- Schema: `store/sqlite/sqlite.go` (`volumes.encrypted`, `volumes.key_blob`, new `node_keys` table, new `luks_header_backup` column or sidecar table)
- Logic: new `core/keymanager/` package; encryption branches in `core/volume/service.go`
- Runtime: `Attach`/`Detach` indirection in `runtime/container/driver.go` and `runtime/microvm/firecracker/volumes.go`
- Image: `apk add cryptsetup tpm2-tools` in `image/Dockerfile`; auto-load `tpm_crb`/`tpm_tis`/`dm_crypt` from `image/etc/modules-load.d/capsule.conf` plus a `boot.LoadModules()` call early in `cmd/capsuled/main.go`
- CLI: `node init-encryption`, `node unlock`, `node reset-encryption`, `volume create --encrypt`, `volume rotate-recovery` (v2)

The kernel modules (`dm-crypt`, `tpm_crb`, `tpm_tis`) already ship in the `linux-lts` module tree on the existing image — no kernel or initramfs work is needed.
