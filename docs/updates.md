# A/B OS updates

Capsule updates are **whole-image, atomic, and reversible**. You build a new bundle, push it over gRPC, the capsule reboots into the new rootfs, and either you `confirm` it or it auto-rolls-back.

## What's in a bundle

`make update-bundle` produces `build/update.tar`, a tar with exactly four members:

| File          | What                                             |
|---------------|--------------------------------------------------|
| `VERSION`     | One-line version string baked into this build    |
| `vmlinuz`     | Kernel for this build                            |
| `initramfs`   | Initramfs (busybox + squashfs/overlay modules)   |
| `rootfs.sqsh` | The squashfs rootfs image                        |

Atomic bundles avoid kernel/userland version drift — one version, one rollback, one "what's installed?" answer. Same pattern as Flatcar / Talos / CoreOS.

## The flow

```
              ┌────────────── operator ──────────────┐
              │                                      │
              │  capsulectl capsule update push X   │
              │                                      │
              └───────────────┬──────────────────────┘
                              │ stream
                              ▼
┌─────────────── capsule (was on slot_a) ────────────────┐
│  1. write rootfs.sqsh to slot_b partition (raw dd)     │
│  2. copy vmlinuz_b + initramfs_b to /boot              │
│  3. rewrite grub.cfg → one-shot boot slot_b            │
│  4. record pending_slot=slot_b, deadline=now+10m       │
│  5. reboot                                             │
└─────────────────────────────────────────────────────────┘
                              │
                              ▼
                       GRUB picks slot_b
                              │
                              ▼
┌─────────────── capsule (now on slot_b) ────────────────┐
│  6. capsuled sees pending_slot==slot_b, arms timer     │
│  7. operator verifies; runs:                           │
│     capsulectl capsule update confirm                  │
│  8. capsuled rewrites grub.cfg default=1, clears       │
│     pending. Slot_b is now the committed default.      │
└─────────────────────────────────────────────────────────┘
```

If step 7's confirm doesn't arrive before the deadline, capsuled reboots itself. GRUB's default is still slot_a (we never rewrote it), so the next boot lands back on the old slot. The pending row is cleared on next-boot startup so future updates start clean.

## Push an update

```sh
make update-bundle
capsulectl capsule update push build/update.tar
```

The push streams chunks with a sha256 verified end-to-end. On success:

```
slot:        slot_b
version:     20260425-195624
deadline:    2026-04-26T01:40:24Z   (auto-rollback in 9m58s unless confirmed)
reboot scheduled
```

Wait for the capsule to come back. `capsule info` will show:

```
active_slot:   slot_b
pending_slot:  slot_b   (auto-rollback in 9m12s unless confirmed)
last_version:  20260425-195624
```

Verify whatever you wanted to verify (workloads still scheduling, networking up, the change you made present), then:

```sh
capsulectl capsule update confirm
```

`capsule info` now shows pending cleared and slot_b as the steady-state default.

## Auto-confirm

For scripted rollouts, push with `--auto-confirm=N`:

```sh
capsulectl capsule update push build/update.tar --auto-confirm=120
```

The CLI:

1. Streams the bundle.
2. Polls `capsule info` until the capsule is reachable again on the new slot.
3. Waits `N` seconds, polling again to make sure it's still healthy.
4. Sends `confirm` automatically.

If the capsule never comes back, the deadline expires and the auto-rollback fires — same as the manual flow.

## Failure modes the system handles automatically

| What happened                           | Recovery path                                                                   |
|----------------------------------------|----------------------------------------------------------------------------------|
| Kernel panics on the new slot          | `panic=10` → reboot → GRUB default is still old slot → back on old slot.        |
| New kernel boots but breaks networking | You can't reach the capsule to confirm → deadline expires → self-reboot → old slot. |
| Update streamed but checksum bad       | `ReceiveBundle` rejects with `InvalidArgument`; nothing is written.             |
| Power loss during streaming            | Staging file is deleted on next boot; no half-applied update.                   |
| Power loss during tentative window     | Pending state persisted in SQLite; `OnStartup` checks if active_slot matches pending and acts accordingly. |

## Failure modes you handle manually

- **capsuled crash-loops on the new slot without a kernel panic.** No timer ever arms (capsuled never finishes coming up). Manual reboot picks GRUB default = old slot, recovering. A virtio-watchdog would automate this; not implemented yet.
- **Operator confirms a bad update.** That's on you. Push another bundle to roll it back.

## What "confirm" actually does

`grub.cfg` ships with `set default=0` (slot_a). On `update push` we leave that line alone and instead write a one-shot to `/EFI/BOOT/grub-once.cfg` (technically: rewrite the chained `grub.cfg` to consume the one-shot). On `confirm`, capsuled mounts the ESP rw and replaces the `set default=` line with the slot index of the now-active slot (slot_a → 0, slot_b → 1). Steady-state default flipped.

## Versions

The `VERSION` in a bundle is baked at build time. Set it explicitly:

```sh
VERSION=my-feature-20260425 make update-bundle
```

If unset, `pack.sh` defaults to a date-stamp like `20260425-195624`. The capsule rejects nothing on its end — pushing the same VERSION twice is currently allowed (it'll just bounce you between slots with the same identifier). A meaningful version makes `capsule info` and the rollback breadcrumbs useful.

## Local QEMU verification

The whole flow works in QEMU without any external dependency. QEMU reboots in place by default — same machine, just kernel restart.

```sh
make image                              # builds disk.raw + update.tar at version A
make qemu                               # boots slot_a
# in another terminal:
export CAPSULE_HOST=localhost:50000
capsulectl capsule info                 # → active_slot: slot_a

VERSION=test-v2 make update-bundle      # builds update.tar at version test-v2
capsulectl capsule update push build/update.tar
# wait for reboot...
capsulectl capsule info                 # → active_slot: slot_b, pending=slot_b
capsulectl capsule update confirm
capsulectl capsule info                 # → pending cleared, slot_b committed
```

## See also

- [architecture.md#disk-layout](architecture.md#disk-layout) — where each slot lives on disk.
- [architecture.md#boot-chain](architecture.md#boot-chain) — how GRUB picks the slot.
