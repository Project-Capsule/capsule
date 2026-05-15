# Install: from USB to adopted capsule in one operator pass (proposal)

> **Status:** Proposal. Not implemented.
>
> **Depends on parts of [discovery.md](discovery.md).** Install reuses
> discovery's mDNS announcer, short IDs, HDMI banner format, and the
> `capsulectl discover` browse command. Shipping install means shipping
> those foundations of discovery in the same pass. The fancier discovery
> features (`discover --adopt`, `capsule set-hostname`,
> `discover --refresh-contexts`, adopted-context cross-referencing) are
> *not* required for install and can land later. See [Dependencies](#dependencies)
> below for the precise required-vs-optional split.
>
> This addresses the install-time UX gap: today, putting Capsule on a piece of
> bare metal requires `dd`-to-USB → boot from USB → adopt the USB-booted
> instance → open a debug container → `dd` the live system onto the internal
> disk → reboot → adopt the disk-booted instance *again*. Two adoptions, the
> debug container abused as an installer, and no clean concept of "this machine
> is sitting in the rack waiting to be installed."

## Summary

Today's first-install flow has more ceremony than it should:

1. `sudo dd if=build/disk.raw of=/dev/<usb> bs=4M conv=fsync`
2. Plug USB into the target machine, boot from it.
3. `capsulectl adopt --capsule <usb-booted-ip>:50000 --name tmp`
4. `capsulectl capsule debug -i alpine:3.23 -- /bin/sh -c 'dd if=/dev/sdX of=/dev/nvme0n1 bs=4M'`
5. Power-cycle. Remove USB.
6. `capsulectl adopt --capsule <disk-booted-ip>:50000 --name nuc-1`

Two adoptions, one of which is throwaway. The debug container is doing
installer work it shouldn't be. The operator has to keep track of which
"capsule" is the USB and which is the disk.

This proposal collapses the flow to:

1. `sudo dd if=build/disk.raw of=/dev/<usb> bs=4M conv=fsync` (unchanged)
2. Plug USB into the target machine, boot from it.
3. `capsulectl install <short-id> --name nuc-1`
4. Power-cycle. Remove USB. Done — the disk boots already adopted as `nuc-1`.

The pieces that make this work:

- **Mode detection.** The same `disk.raw` is both the installer and the
  runtime. On boot, capsuled decides which one it is by looking at where
  it booted from (`/sys/block/<rootdev>/removable`) and whether a viable,
  uninstalled internal target exists.
- **Discoverable installers.** Installer-mode capsules announce over mDNS
  with `mode=installer` and the candidate target disk. They show up in
  `capsulectl discover` as a distinct "pending install" section.
- **A real `capsulectl install` verb.** Replaces the
  debug-container-with-`dd` pattern. Picks the target disk, captures
  fingerprints under TOFU, drives the flash.
- **Seal-during-install.** The installer pre-generates the disk-booted
  capsule's TLS keypair and `capsule_id` and seeds them, along with the
  operator's pubkey, into the new disk's PERM. First boot from the disk
  ingests the pre-seed, skips the claim window, and comes up already
  enrolled with the identity the operator's context entry expects.

## Goals

- **One operator command per machine.** `capsulectl install <short-id> --name X`
  ends with the operator pulling the USB; no second adopt, no debug container.
- **Same `disk.raw`.** No separate installer image artifact. The same bits
  ship to both USB and internal disk. Installer mode is selected by
  environment, not by build target.
- **Installers are discoverable.** `capsulectl discover` shows machines
  pending install in their own section, with target disk and size, so the
  operator can see at a glance "I have three machines waiting."
- **Fingerprint TOFU at install time.** The operator validates the
  installer's TLS fingerprint against the HDMI banner before sealing a
  pubkey, same posture as `capsulectl adopt` today.
- **`dd build/disk.raw of=/dev/sdX` still works.** For OS development,
  laptop-side flashing of pulled disks, and as a recovery escape hatch.
  The new install verb is the recommended path; the direct path is
  documented as advanced.

## Non-goals (v1)

- **Zero-touch / unattended install.** No "boot the USB and walk away."
  The operator is in the loop because (a) target-disk selection on
  weird hardware needs human confirmation and (b) seal-during-install
  is the whole point — the operator must be on the LAN with their key.
- **PXE / netboot.** Out of scope. The fleet sizes Capsule targets don't
  justify the boot-server infrastructure.
- **Re-install onto a populated Capsule disk.** v1 refuses to enter
  installer mode if the target disk already has Capsule's MBR signature
  (`0xb1a570ff`). Force-reinstall (`capsulectl install --reinstall`) is
  documented as an open question.
- **GRUB menu prompt on USB boot.** Considered; rejected. The mode-detection
  approach handles 100% of the cases without a console keyboard, which is
  the constraint that matters for headless hardware.
- **Cross-LAN install.** Same as discovery: installer mDNS is link-local.
  Operators with VLAN-segmented installs use `--addr` and direct IP.

## Dependencies

The install flow described here piggybacks on infrastructure from
[discovery.md](discovery.md). Shipping install pulls some of that
infrastructure with it; the rest is independently scheduleable.

**Required to land with (or before) install:**

| Piece | From discovery | Why install needs it |
|-------|----------------|----------------------|
| mDNS announcer in capsuled | yes | Installer mode adds TXT keys on top of the existing announcement; without an announcer there's nothing to add to |
| `_capsule._tcp` service definition | yes | Same |
| Short IDs (`capsule-a3f2`) in `os_state` | yes | The CLI handle for `capsulectl install <short-id>` |
| HDMI banner with short ID | yes | Operator reads the short ID off the screen to type the install command |
| `capsulectl discover` browse command | yes | How operators see machines pending install (without it, install only works via `--addr <ip:port>`) |

**Not required — can land separately:**

| Piece | From discovery | Why install doesn't need it |
|-------|----------------|------------------------------|
| `capsulectl discover --adopt` interactive walkthrough | optional | Install has its own non-interactive verb |
| `capsulectl capsule set-hostname` + `hostname` TXT field | optional | Hostname-setting is post-adoption polish, orthogonal to install |
| `discover --refresh-contexts` for DHCP drift | optional | Useful for fleet management, but install captures the future address from the operator's command line, not from mDNS |
| Adopted-context cross-referencing in discover output | optional | Affects the runtime `CAPSULES` table, not the `PENDING INSTALL` table |
| IPv6 mDNS | optional | Same posture as discovery — defer until demand exists |

**The `--addr <ip:port>` fallback.** If discovery isn't shipped at all,
install still works as a degraded flow: operator reads the IP off the
installer's HDMI banner, runs `capsulectl install --addr 192.168.10.101:50000`.
The mode-detection + `InstallService` + seal-during-install machinery is
self-contained and doesn't *require* mDNS. But the happy-path UX as written
above (`capsulectl install capsule-a3f2`) does require the discovery
foundations to exist.

The practical recommendation: implement install as one PR series that
includes the discovery foundations listed in the **Required** table.
The discovery polish features can be tracked as a separate follow-up
once those foundations are in place.

## Threat model

| Threat | Protected? |
|--------|------------|
| Attacker on the LAN runs `capsulectl install` against an installer they don't own | Partial — pre-adoption the installer accepts the first valid Install RPC, then closes. The window is open from "installer boots" to "operator runs install." Same posture as today's claim window. Defense: operator is at the rack during install, USB is physically present, the install window is short (minutes, not hours). |
| Attacker spoofs an mDNS installer announcement to lure the operator's `capsulectl install` to a hostile machine | Same posture as the discovery threat model — operator verifies fingerprint against HDMI before sealing the key. Spoofed mDNS gets caught at fingerprint comparison. |
| Pre-seed bundle (`/perm/firstboot.json`) leaks the operator's pubkey | The pubkey is not a secret. The bundle contains only the disk-booted capsule's *new* TLS keypair (created by the installer for the disk's exclusive use) and the operator's public key — no operator private material, no shared secret. Bundle is deleted by capsuled on first ingest. |
| Operator runs `capsulectl install` against the wrong machine and seals their key into hardware they don't own | Same mitigation as today's adopt-by-wrong-IP: revoke the key via `capsulectl key remove <kid>` on the unintended host. The friction is higher than a mis-adopt because the disk got flashed; the proposal accepts that. |
| `mode=installer` mDNS broadcast advertises that a machine is ready to be installed by anyone on the LAN | Yes, intentionally — same posture as `adopted=false` in discovery. The window is short and bounded by physical presence. Operators on untrusted networks use `--addr` with a known IP. |
| Malicious workload running on an *adopted* capsule pretends to be an installer via mDNS | Workloads run on `br0`, not the host namespace; they can't send multicast to the uplink interface. (Same defense as `discovery.md`.) |

## Concept: how install works

### Mode detection (capsuled side)

At boot, after the rootfs is mounted and `/proc` is available but before the
reconciler / scheduler start, capsuled decides whether it's a runtime or an
installer. The decision is mechanical:

```
rootdev   = resolve(root=PARTUUID=... from /proc/cmdline)
removable = read("/sys/block/" + parentOf(rootdev) + "/removable")

candidates = []
for blk in /sys/block/*:
    if blk == parentOf(rootdev): continue
    if read(blk + "/removable") == "1": continue   # skip USBs / SD cards
    if size(blk) < 16 GiB: continue                # too small for /perm
    if hasCapsuleMBR(blk): continue                # already a Capsule install
    candidates.append(blk)

if removable == "1" and len(candidates) >= 1:
    mode = installer
else:
    mode = runtime
```

`hasCapsuleMBR(blk)` reads the first 512 bytes of the block device and checks
the four-byte MBR disk signature at offset `0x1b8` against `0xb1a570ff`
(BLASTOFF) — already a fixed value in `image/pack.sh`, already used as a
stable PARTUUID anchor today.

A USB-booted machine whose internal disk already has a Capsule install is
treated as **runtime** (recovery / live mode), not installer. The operator
can still use the USB for forensics, but `capsulectl install` will refuse
to run — `--reinstall` is the explicit override.

### What installer mode does and doesn't do

In installer mode, capsuled brings up:

- `/proc`, `/sys`, `/dev`, `/run`, `/tmp`, cgroup v2 (existing `initPlatform`).
- Network (`eth0` + `udhcpc` — same as runtime).
- SQLite at `/perm/state.db` on the USB's small ephemeral PERM (so the
  installer has a `capsule_id`, a TLS keypair, and a stable identity for the
  operator to TOFU-pin against).
- mDNS announcer (from `discovery.md`) with installer TXT fields.
- gRPC on `:50000` with **only** `IdentityService` (for fingerprint capture)
  and the new `InstallService`.

It does **not** start:

- The workload reconciler.
- The scheduler.
- containerd.
- The volume manager.
- Any A/B-update machinery.

The point is the installer is a degenerate capsule — enough to host the
install RPC, no more. If the operator opens a `capsulectl capsule debug`
session against an installer, it works (the debug container code path is
independent), but `capsulectl workload list` returns "this capsule is in
installer mode" and refuses.

### mDNS announcement (installer mode)

On top of the discovery proposal's standard TXT records, installer mode adds:

- `mode=installer` (vs `mode=runtime` for ordinary capsules; the absence of
  this key is also treated as runtime for backward compatibility).
- `target_disk=/dev/nvme0n1` — the auto-selected candidate target.
- `target_size_bytes=512110190592` — for display in `discover`.
- `targets=/dev/nvme0n1,/dev/sda` — full candidate list (comma-separated).

Other TXT records (`capsule_id`, `short_id`, `version`, `adopted`) keep their
discovery meanings; in installer mode `adopted` is always `false` and
`capsule_id` is the installer's transient identity (which is *not* the
identity the future disk-booted capsule will have).

### `capsulectl discover` with installers

`discover` output gets split into two sections when both are present:

```
PENDING INSTALL
NAME                  ADDRESS               TARGET             SIZE       VERSION
capsule-a3f2 (inst)   192.168.10.101:50000  /dev/nvme0n1       512 GB     20260513-120000
capsule-9c11 (inst)   192.168.10.105:50000  /dev/sda           1.0 TB     20260513-120000

CAPSULES
NAME    ADDRESS               FINGERPRINT          ADOPTED
nuc-2   192.168.10.102:50000  b7:e1:3f:4a:8c:2d:…  yes (context: nuc-2)
```

`--installers` filters to only the pending-install rows; `--unadopted`
keeps its discovery meaning (adopted=false runtime capsules). The two flags
are mutually exclusive.

When `targets` contains more than one candidate, the display prefixes the
auto-selected one with `*`:

```
TARGET                          SIZE
/dev/nvme0n1*, /dev/sda         512 GB
```

### `capsulectl install`

```
capsulectl install <short-id-or-addr>
                   [--target /dev/nvme0n1]
                   [--name nuc-1]
                   [--no-seal]
                   [--reinstall]
                   [--addr <ip:port>]
```

Flow:

1. **Resolve the installer.** If `<short-id-or-addr>` looks like
   `capsule-XXXX`, mDNS-browse for it. If it's `host:port`, dial directly
   (the `--addr` form is the no-discovery fallback). Confirm `mode=installer`
   from the TXT record (or, when bypassing mDNS, by an `InstallService.Status`
   RPC that the runtime mode doesn't expose).
2. **Capture TLS fingerprint.** Raw `tls.Dial` to the installer; capture
   the server cert SHA-256. Print for HDMI verification. Prompt operator
   for `yes` to continue. Same TOFU UX as `capsulectl adopt` today.
3. **Pick the target disk.** If `--target` is given, use it (after checking
   it's in the installer's `targets` list and isn't already a Capsule).
   Otherwise: if `len(targets) == 1`, auto-pick; if more than one, abort with
   "multiple candidate disks, re-run with --target."
4. **Generate operator keypair.** Reuse the existing
   `~/.config/capsule/keys/<name>.ed25519` path from adopt; create if missing.
5. **Call `InstallService.Install`** over the fingerprint-pinned connection
   (no JWT — the installer is pre-adoption, gated by physical presence).
   Request body:
   ```
   InstallRequest {
     target_disk:       "/dev/nvme0n1"
     operator_pubkey:   <Ed25519 public key bytes>
     name:              "nuc-1"
     seal:              true       # default; --no-seal sets false
     reinstall:         false      # --reinstall sets true (refuses w/o this)
   }
   ```
6. **Installer streams progress.** The RPC is server-streaming:
   `InstallProgress { phase, percent, message }`. Phases:
   `verify`, `partition`, `format_boot`, `write_slot_a`, `write_slot_b`,
   `install_grub`, `seal_firstboot`, `sync`, `done`. The CLI renders a
   single-line progress bar.
7. **Receive future identity.** The final `InstallProgress` of `phase=done`
   carries the disk-booted capsule's identity:
   ```
   InstallResult {
     capsule_id:               <UUID generated for the disk>
     tls_fingerprint_sha256:   <fingerprint of the disk's cert>
     kid:                      <base64url(sha256(operator_pubkey))>
   }
   ```
8. **Write the context entry.** `capsulectl` writes
   `~/.config/capsule/config.yaml` with the disk's future address (left
   blank, to be filled in by `discover --refresh-contexts` later — see
   discovery.md) and the captured `capsule_id` + `tls_fingerprint_sha256`.
9. **Print next-steps.** "Install complete. Remove the USB and power-cycle.
   The disk will come up adopted as `nuc-1` — run
   `capsulectl discover --refresh-contexts` to capture its IP."

### Seal-during-install mechanics

The disk-booted capsule must come up with three things already set:

| What | Why |
|------|-----|
| A TLS keypair | The operator's pinned fingerprint must match the disk's first TLS handshake. |
| A `capsule_id` UUID | The operator's context entry resolves to this; the disk uses it as its stable identity. |
| One row in `authorized_keys` | So JWT-authenticated RPCs work immediately, with no claim window. |

The installer generates all three and writes them to a file on the **target
disk's** PERM partition before unmounting and exiting. The file is plain JSON,
mode `0600`:

```
/perm/firstboot.json
{
  "capsule_id":     "a3f21c9d-...",
  "tls_cert_pem":   "-----BEGIN CERTIFICATE-----\n...",
  "tls_key_pem":    "-----BEGIN PRIVATE KEY-----\n...",
  "operator_pubkey": "<base64 Ed25519>",
  "operator_name":  "nuc-1"
}
```

(The PERM partition is unformatted at install time; the installer formats
the LV-backed `meta` LV early, mounts it, writes `firstboot.json`, unmounts,
and that's it. `meta` does not yet exist before install — see
"Flashing the target disk" below for the partition / LVM step.)

On first boot of the disk-booted capsule, `boot/boot_linux.go` does:

1. Mount PERM as today (`mountPerm`).
2. Check `/perm/firstboot.json`. If present:
   - Open SQLite, run normal migrations.
   - `INSERT INTO os_state(capsule_id, tls_cert, tls_key, …)` from the bundle.
   - `INSERT INTO authorized_keys(kid, pubkey, name)` from the bundle.
   - `unlink /perm/firstboot.json`.
   - Skip claim-window opening (already enrolled).
3. Continue normal startup.

If `firstboot.json` is absent, the existing first-boot path runs unchanged
(generate fresh `capsule_id`, fresh TLS keypair, open claim window for 30
minutes). This is what happens after a `--no-seal` install or a direct
`dd build/disk.raw of=/dev/sdX`.

### Flashing the target disk

The installer owns the squashfs slot it booted from (`slot_a` of the USB).
That same squashfs goes into both slot_a and slot_b of the target disk —
there is no separate "image to install" payload, the running rootfs *is*
the payload.

Sequence inside `InstallService.Install`:

1. **Validate target.** Block device exists, is in `targets`, is not
   currently mounted, doesn't have Capsule MBR (unless `reinstall=true`).
2. **Write MBR + partition table.** Same 4-partition MBR layout as
   `image/pack.sh`:
   - p1: FAT32 CAPSULEBOOT, 256 MiB (0xEF)
   - p2: squashfs slot_a, `SLOT_SIZE_MIB` (default 2 GiB)
   - p3: squashfs slot_b, `SLOT_SIZE_MIB`
   - p4: LVM2 PV "capsule", remainder (0x8E)
   - MBR disk signature: `0xb1a570ff` (BLASTOFF) — fixed.
3. **Format FAT32 partition.** `mkfs.vfat -F32 -n CAPSULEBOOT`.
4. **Install GRUB EFI.** Same `grub-install --target=x86_64-efi`
   invocation as `pack.sh`. Copy `grub.cfg` with both slot menu entries,
   default to slot_a.
5. **Copy kernel + initramfs.** Both to `/boot_a/` and `/boot_b/` on the
   FAT32 partition; same files the installer is itself running from.
6. **Write slot_a squashfs.** `dd` the running squashfs (read from the
   installer's slot via `/proc/mounts` lookup → loopback / source device)
   into target p2.
7. **Write slot_b squashfs.** Same bytes into target p3 (A/B starts
   identical).
8. **Initialize LVM on p4.** `pvcreate`, `vgcreate capsule`, `lvcreate`
   the `meta` LV (sized per the `min(25% of VG, 32 GiB)` rule with the
   1 GiB floor — same as boot.go's first-boot path). `mkfs.ext4 -L PERM`.
9. **Seed firstboot bundle.** Mount `meta` at `/mnt/perm-target`, write
   `firstboot.json`, `umount`.
10. **`sync` and report `phase=done`.**

The installer does not initialize the thinpool — that's left to first
boot from the disk, which already knows how to do it. This keeps the
installer simple and the first-boot path unchanged.

The partition-layout logic — currently a shell pipeline in `image/pack.sh`
— is lifted into a Go package (`image/disklayout` or similar) callable from
both the build (via a small CLI wrapper) and the installer. This is the one
non-trivial refactor in the proposal.

### HDMI banner in installer mode

Runtime banner today:

```
   ____    _    ____  ____  _   _ _     _____
  / ___|  / \  |  _ \/ ___|| | | | |   | ____|
 | |     / _ \ | |_) \___ \| | | | |   |  _|
 | |___ / ___ \|  __/ ___) | |_| | |___| |___
  \____/_/   \_\_|   |____/ \___/|_____|_____|

  192.168.10.101:50000
```

Installer banner:

```
   ____    _    ____  ____  _   _ _     _____
  / ___|  / \  |  _ \/ ___|| | | | |   | ____|
 | |     / _ \ | |_) \___ \| | | | |   |  _|
 | |___ / ___ \|  __/ ___) | |_| | |___| |___
  \____/_/   \_\_|   |____/ \___/|_____|_____|

  INSTALLER  capsule-a3f2  192.168.10.101:50000
  fingerprint: a3:f2:1c:9d:4e:7f:b2:c8:...
  target:      /dev/nvme0n1  (512 GB)

  Run on your laptop:
    capsulectl install capsule-a3f2 --name <name>
```

Operator types the command verbatim from the screen. The fingerprint on
HDMI is the one they verify against `capsulectl install`'s prompt.

## Lifecycle

### USB boot → installer state

```
capsuled startup (installer mode):
  1. initPlatform (mounts, modules, network) — same as runtime
  2. Detect installer mode via /sys/block/<rootdev>/removable + candidate scan
  3. Mount PERM on the USB (small, ephemeral — used only for the installer's
     own SQLite)
  4. Open SQLite, generate capsule_id + TLS keypair if absent
  5. Bring up eth0 / udhcpc
  6. Print installer HDMI banner (with target + fingerprint)
  7. Start mDNS announcer with mode=installer, targets=...
  8. Start gRPC server on :50000 with only IdentityService + InstallService
  9. Wait for InstallRequest
```

### Install → disk first boot

```
capsuled startup (disk runtime, first boot after sealed install):
  1. initPlatform — same as today
  2. mountPerm — same as today; PV already exists, mount /perm
  3. Open SQLite, run migrations
  4. Check /perm/firstboot.json:
     - If present: ingest capsule_id, TLS keypair, authorized_keys row.
       Unlink the file. Skip claim window.
     - If absent: existing path — generate fresh, open claim window.
  5. Bring up eth0 / udhcpc
  6. Print HDMI banner (no installer text — this is a real runtime)
  7. Start mDNS announcer with mode=runtime, adopted=true
  8. Start gRPC on :50000 with all services
  9. Start reconciler, scheduler, etc.
```

From the operator's perspective: they removed the USB, powered the machine
back on, and the disk-booted capsule is immediately reachable with their
existing context entry. No claim window, no second adopt.

### After install — installer USB

The installer USB itself is unchanged after running `Install`. The operator
can pull it and reuse it on the next machine. capsuled stays in installer
mode (mode detection re-runs at each boot — the USB's PERM has no
"already-installed-once" flag). Multiple installs from the same USB are the
expected pattern.

## Failure scenarios

| Failure | Symptom | Recovery |
|---------|---------|----------|
| Multiple candidate disks on the target machine | Installer announces `targets=A,B`; `capsulectl install` refuses without `--target` | Operator re-runs with `--target /dev/X` after deciding which is which |
| Operator runs `install` with a target not in the candidate list | Installer rejects with "target /dev/X not in candidate list" | Operator picks from advertised targets, or uses `--addr` + manual `--target` if mDNS missed a disk |
| Target already has Capsule's MBR | Installer rejects with "target has existing Capsule install — use --reinstall" | `--reinstall` forces; v1 also documents `dd /dev/zero ...` of the first sector as a manual override |
| Install crashes mid-flash (power loss between partition write and slot_b copy) | Disk has a partial install; MBR may or may not be written | Boot the USB again; installer detects "target has no Capsule MBR" (because the crash was before MBR write) or "target has Capsule MBR but no firstboot.json" (post-MBR crash); v1 documents force-reinstall path |
| Operator pulls USB before `phase=done` | Installer wedges (lost write target mid-flash) | Same recovery — boot USB again, force-reinstall |
| Network not up when operator runs `install` | `discover` returns empty; `--addr` works once IP is known | Wait, retry. Installer also prints IP on HDMI |
| Pre-seed bundle corrupted | First-boot disk capsuled fails to parse `firstboot.json`, logs error, falls back to claim-window path | Operator adopts the disk-booted capsule manually as today; their context entry's fingerprint won't match (different TLS key), so they re-create the context |
| `firstboot.json` survives unexpectedly into a later boot | Second-boot capsuled finds it again, tries to ingest again, fails on UNIQUE constraint | The ingest path unlinks the file *before* the second-boot risk window; if it's present, log + unlink, no other action |
| Operator runs `capsulectl install` against a runtime capsule (mistype) | RPC fails with "this capsule is in runtime mode" | Re-run with correct short ID |
| `--no-seal` install, but operator's context was created assuming seal | Disk boots with claim window open; operator's context fingerprint doesn't match | Documented — `--no-seal` callers know they need `discover --adopt` after first disk boot |
| USB stick reported `removable=0` (some rare ones do) | Installer mode not entered; runs as runtime, mDNS shows it as a regular capsule | Use `--reinstall` against the runtime, or `dd disk.raw` escape hatch |
| Internal NVMe reported `removable=1` (some eMMC modules) | Installer mode not entered (no candidates); runs as live | `--addr` + `--target` overrides; documented as a known sharp edge |

## SQLite / on-disk schema

No new SQLite tables. Two small additions on existing surfaces:

- `boot/boot_linux.go` gets a `firstBootIngest()` helper called after
  SQLite is open. Reads `/perm/firstboot.json`, inserts into `os_state` and
  `authorized_keys`, unlinks the file, returns. Idempotent on absence.
- The installer's runtime PERM (on the USB) gets the same schema as a normal
  capsule. There's nothing installer-specific in SQLite; "installer mode"
  is a runtime decision, not a persisted flag.

## Open questions

- **Force-reinstall UX.** `--reinstall` is the obvious flag, but the
  semantics need care: it wipes user data, the thinpool, and any
  authorized_keys on the old install. Probably needs a two-prompt
  confirmation (CLI + installer-side echo of target details).
  Defer to a v2.
- **Heterogeneous disk sizes during reinstall.** If the new target is
  *smaller* than the old, partitioning math changes. The installer should
  refuse and tell the operator to use `dd disk.raw` directly.
- **Multi-disk targets (RAID, second data disk).** Out of scope for v1 —
  the installer always picks one boot/root disk. Secondary disks are
  handled by [external-disks.md](external-disks.md) once installed.
- **Pre-seeding TPM state.** If [encrypted-volumes.md](encrypted-volumes.md)
  lands, the TPM-sealed master key is generated on first boot from the
  disk's TPM, which the installer doesn't have access to. Sealing happens
  post-install, on the disk's own first boot. No conflict, but worth
  noting that `firstboot.json` does **not** carry encryption material.
- **Network-segmented installs.** Operator's laptop and target machine
  on different VLANs → mDNS doesn't cross, `--addr` works if IP is known
  but the operator has to read it off HDMI. Same posture as discovery.
- **`capsulectl install --batch`.** If discovery surfaces N installers,
  could `install --batch --name-prefix nuc-` walk them and install in
  sequence? Useful for fleet bringup. Defer; the single-machine flow has
  to land first.
- **Installer auto-power-off.** After `phase=done`, should the installer
  `poweroff` automatically so the operator just pulls the USB without
  power-cycling? Probably yes — reduces the chance of accidentally
  re-running an install. Open.
- **Removing `dd disk.raw` from docs.** Once `capsulectl install` is
  established, the `dd` path becomes "advanced." But for OS development
  it's still the fastest iteration loop. Leave it in
  [getting-started.md](getting-started.md) under an "Advanced" section.

## Implementation pointers

- **Proto:** new `models/capsule/v1/install.proto` with `InstallService`,
  `InstallRequest`, server-streamed `InstallProgress`, terminal `InstallResult`.
  Add to the same buf module the other services live in.
- **Boot:** `boot/boot_linux.go` grows `detectInstallerMode()` (sysfs probe +
  candidate enumeration + MBR signature check) called before `mountPerm`,
  and `firstBootIngest()` called after SQLite is open. Mode determines
  whether to start the reconciler / scheduler.
- **Image layout module:** lift `image/pack.sh`'s partition-table /
  GRUB-install / slot-write logic into `image/disklayout/` (Go). Both
  `pack.sh`'s CI build and the installer at runtime call into this. Net
  result: one source of truth for the disk layout.
- **mDNS:** the installer-mode TXT keys (`mode`, `target_disk`,
  `target_size_bytes`, `targets`) plug into the announcer from
  [discovery.md](discovery.md). The announcer code reads them from a struct
  populated by `detectInstallerMode()` at startup.
- **Controllers:** new `controllers/install.go` implementing
  `InstallService`. It owns the partition + flash + seed loop and emits
  progress events. Only registered in the gRPC server when capsuled is in
  installer mode; runtime mode doesn't expose the service at all.
- **CLI:** `cmd/capsulectl/install.go` new verb, sharing the
  fingerprint-pinning helper with `adopt`. Streamed-RPC progress rendering
  reuses whatever the update-push command uses today.
- **Strictly additive.** A capsuled binary without this code keeps booting
  as today (the mode detector just always returns "runtime"). A
  capsulectl binary without `install` keeps adopt + the `dd disk.raw`
  flow. The proposal is opt-in at every layer; the existing flows don't
  change.
