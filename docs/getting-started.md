# Getting started

End-to-end: build the image, boot a capsule, point `capsulectl` at it, deploy a workload.

## Prerequisites

On your build/operator machine (macOS or Linux):

- Docker
- Go 1.25+
- [`buf`](https://buf.build) — `brew install bufbuild/buf/buf`
- `qemu-system-x86_64` (only if you want to boot under QEMU)
- `qemu-img` and OVMF/edk2 firmware (the `qemu` Homebrew formula ships these)

The image build runs in Docker, so the host architecture doesn't matter. The output is always a `linux/amd64` bootable disk.

## Build

```sh
make proto      # regenerate .pb.go from .proto (only needed when proto changes)
make image      # → build/disk.raw  +  build/update.tar
make capsulectl # → build/capsulectl  (host-architecture binary)
```

`make image` runs the full pipeline: builds the rootfs Docker image, flattens it to a squashfs, partitions a 4-partition disk, installs GRUB EFI, seeds both A/B slots, and writes both `build/disk.raw` (full bootable disk) and `build/update.tar` (streaming update bundle for an already-running capsule).

For tighter iteration on a *running* capsule, `make update-bundle` skips disk assembly and produces only `build/update.tar`.

## Boot a capsule

### Option 1 — QEMU on your laptop (UEFI)

```sh
make qemu
```

That runs:

```
qemu-system-x86_64 \
  -m 2G -smp 2 \
  -drive if=pflash,format=raw,unit=0,readonly=on,file=$(OVMF_CODE) \
  -drive if=pflash,format=raw,unit=1,file=$(EFI_VARS) \
  -drive if=virtio,format=raw,file=build/disk.raw \
  -netdev user,id=n0,hostfwd=tcp::50000-:50000 \
  -device virtio-net-pci,netdev=n0 \
  -device virtio-rng-pci \
  -serial mon:stdio -nographic
```

You should see GRUB → kernel → `[capsuled]` log lines on the serial console. The capsule listens on `:50000`, mapped to `localhost:50000` on the host.

`Ctrl-A x` quits QEMU.

If your OVMF firmware lives somewhere other than `/opt/homebrew/share/qemu/`, override `OVMF_CODE=` / `OVMF_VARS=` on the make line.

### Option 2 — real hardware (USB or NVMe)

`build/disk.raw` is a complete bootable disk. Write it to a USB stick or the target machine's internal disk:

```sh
sudo dd if=build/disk.raw of=/dev/<target> bs=4M status=progress conv=fsync
```

Plug the USB into a UEFI-capable machine, boot from it (you may need to enable UEFI boot in the firmware menu), and watch HDMI for the ASCII `CAPSULE` banner — it prints the DHCP'd IP a few seconds after kernel hands off.

> **Note:** Capsule needs UEFI. Legacy/CSM BIOS won't load GRUB EFI from the ESP.

DHCP comes up automatically. If your network gives the capsule `192.168.10.138`, it'll show:

```
   ____    _    ____  ____  _   _ _     _____
  / ___|  / \  |  _ \/ ___|| | | | |   | ____|
 | |     / _ \ | |_) \___ \| | | | |   |  _|
 | |___ / ___ \|  __/ ___) | |_| | |___| |___
  \____/_/   \_\_|   |____/ \___/|_____|_____|

  192.168.10.138:50000
```

## Talk to it

`capsulectl` reads `CAPSULE_HOST` from the environment, so the typical workflow is:

```sh
export CAPSULE_HOST=192.168.10.138:50000   # or localhost:50000 for QEMU
./build/capsulectl capsule info
```

You should get something like:

```
hostname:           capsule
kernel:             6.18.24-0-lts (Linux)
arch:               x86_64
uptime:             3m41s
capsule_version:    20260425-195624
active_slot:        slot_a
pending_slot:       (none)
last_version:       (none)
local_time:         2026-04-26T01:30:24Z   skew: 0s
memory:             1.9 GiB available / 2.0 GiB total
cpu:                2× Intel(R) ...
disk:               /dev/vda  19 GiB total
thinpool:           38 MiB used / 12 GiB total
```

If you'd rather pass the host explicitly, use `--capsule host:port`. `--capsule` overrides `CAPSULE_HOST`.

## Deploy your first workload

The simplest container — runs `nginx` on the capsule, network-mode `HOST` so port 80 binds directly:

```sh
./build/capsulectl apply -f examples/nginx-host.yaml
./build/capsulectl workload list
./build/capsulectl workload logs --follow nginx-host
```

`apply -f` is the universal verb — it dispatches by `kind:` in the manifest. `Container`, `MicroVM`, and `Volume` all use it.

When you're done:

```sh
./build/capsulectl workload delete nginx-host
```

See [workloads.md](workloads.md) for the full menu of manifest shapes (containers, microVMs, with and without volumes).

## What's next

- [workloads.md](workloads.md) — Container vs microVM, with and without volumes, port mappings, host vs bridge networking.
- [updates.md](updates.md) — Push a new OS bundle to a running capsule and roll back if it doesn't come up healthy.
- [cli.md](cli.md) — Every `capsulectl` subcommand and flag.
- [architecture.md](architecture.md) — How the boot, runtime, networking, and update layers actually work.

## Troubleshooting

**`capsulectl ... : connection refused`** — capsule isn't reachable. Check the IP/port shown on the HDMI banner; check `CAPSULE_HOST`; check that the capsule and your laptop are on the same L2 (no firewalled routers between).

**Banner shows the wrong IP** — `udhcpc` ran before the link came up, or your network's DHCP lease changed. Reboot the capsule or `capsulectl capsule logs` to see the lease history.

**`local_time` skew is huge** — the capsule's one-shot NTP failed at boot (no internet, blocked UDP/123, etc). The skew column on `capsule info` shows it. Pending-slot rollback deadlines are computed in the capsule's own clock frame, so even with skew the rollback works correctly — but the displayed countdown will look wrong relative to your laptop's clock.

**QEMU boots into a "no bootable device" loop** — OVMF firmware path wrong; check `OVMF_CODE` / `OVMF_VARS` exist. On macOS the Homebrew QEMU paths are `/opt/homebrew/share/qemu/edk2-x86_64-code.fd` and `/opt/homebrew/share/qemu/edk2-i386-vars.fd`.

**`make image` very slow on Apple Silicon** — the Docker build runs `linux/amd64` images under emulation; expect minutes. The kernel and rootfs only rebuild when their inputs change, so subsequent builds are fast.
