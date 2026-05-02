# `capsulectl` reference

Operator CLI. One binary, one capsule per invocation.

## Connecting

`capsulectl` reads `CAPSULE_HOST=host:port` from the environment. Pass `--capsule host:port` on the command line to override.

```sh
export CAPSULE_HOST=192.168.10.138:50000
capsulectl capsule info
# or
capsulectl --capsule localhost:50000 capsule info
```

If neither is set, the CLI defaults to `localhost:50000`.

## Verbs

```
capsulectl [--capsule host:port] apply -f <manifest.yaml>
capsulectl [--capsule host:port] capsule info
capsulectl [--capsule host:port] capsule logs [-f] [-n N]
capsulectl [--capsule host:port] capsule update push <bundle.tar> [--auto-confirm=N]
capsulectl [--capsule host:port] capsule update confirm
capsulectl [--capsule host:port] capsule debug [-i <image>] [--keep] [-- <cmd> [args...]]
capsulectl [--capsule host:port] workload list
capsulectl [--capsule host:port] workload get <name>
capsulectl [--capsule host:port] workload delete <name>
capsulectl [--capsule host:port] workload restart <name>
capsulectl [--capsule host:port] workload stop <name>
capsulectl [--capsule host:port] workload start <name>
capsulectl [--capsule host:port] workload logs [-f] [-n N] [--serial] <name>
capsulectl [--capsule host:port] workload exec [-t] <name> -- <cmd> [args...]
capsulectl [--capsule host:port] cp <src> <dst>
capsulectl [--capsule host:port] volume create [--size 10GiB] <name>
capsulectl [--capsule host:port] volume list
capsulectl [--capsule host:port] volume get <name>
capsulectl [--capsule host:port] volume delete [--force] <name>
capsulectl [--capsule host:port] volume resize <name> <size>
capsulectl [--capsule host:port] image list
capsulectl [--capsule host:port] image push <tarball>
```

## `apply -f`

```sh
capsulectl apply -f <manifest.yaml>
```

Universal apply. Dispatches by `kind:` in the manifest:

- `Container`, `MicroVM` → workload apply (create or update).
- `Volume` → volume create-if-missing, resize-if-smaller (grow only).

Idempotent.

## `capsule`

### `capsule info`

Prints identity + runtime snapshot:

- hostname, kernel, arch, uptime
- capsule version (build ID baked into the binary)
- active slot (`slot_a` / `slot_b`)
- pending slot + auto-rollback countdown, if mid-update
- last committed version
- local time + clock skew vs your laptop
- memory, CPU, boot disk, LVM thin-pool usage

### `capsule logs [-f] [-n N]`

Stream `capsuled`'s own slog output (boot diagnostics, reconciler ticks, runtime driver events). `-f` to follow; `-n N` to start with the last N lines.

This is the **host-side** log. For workload stdout/stderr use `workload logs`.

### `capsule update push <bundle.tar> [--auto-confirm=N]`

Streams an update bundle to the capsule. The capsule writes to the inactive slot, flips the GRUB one-shot, and reboots into the new slot in tentative mode (default 10 min deadline).

Without `--auto-confirm`: returns once reboot is scheduled. You verify manually, then run `capsule update confirm`.

With `--auto-confirm=N`: the CLI waits up to ~120 s for the capsule to come back, soaks for `N` seconds verifying it's still healthy, and sends `confirm` automatically.

Bundle format: tar containing `VERSION`, `vmlinuz`, `initramfs`, `rootfs.sqsh`. Build with `make update-bundle`.

See [updates.md](updates.md) for the full flow.

### `capsule update confirm`

Commits a tentative slot. Mounts the ESP, rewrites `grub.cfg`'s `set default=` line to point at the now-active slot, clears pending state. Subsequent reboots stay on this slot.

### `capsule debug [-i <image>] [--keep] [-- <cmd> [args...]]`

Spawns a privileged debug container on the capsule with host networking and a host root-fs bind. Useful for poking at LVM, iptables, mounts, etc. when something's gone sideways. `--keep` leaves the container running after the command exits; without it, the container is auto-deleted on exit.

## `workload`

Operates on `Container` and `MicroVM` workloads (volumes have their own verbs below).

### `workload list`

Table of all workloads + their phase (`Pending` / `Running` / `Stopped` / `Failed`).

### `workload get <name>`

Full spec + status JSON.

### `workload logs [-f] [-n N] [--serial] <name>`

Stream a workload's stdout/stderr. `-f` follows; `-n N` starts with last N lines.

`--serial` (microVMs only) switches to the Firecracker serial console — kernel boot, `capsule-guest`, early failures. Use this when the guest agent is unreachable.

### `workload exec [-t] <name> -- <cmd> [args...]`

Exec into a running workload. `-t` allocates a TTY (use for interactive shells). Works for both containers and microVMs — for microVMs the exec is proxied through `capsule-guest` over vsock into the runc payload.

```sh
capsulectl workload exec -t alpine-shell -- /bin/sh
```

### `workload start | stop | restart | delete`

`start` brings up a stopped workload. `stop` leaves the row in the DB. `restart` is `stop` + `start`. `delete` stops the workload and removes its row entirely.

## `cp`

```
capsulectl cp <src> <dst>
```

Copy files or directories to/from a running workload. Exactly one of `<src>` and `<dst>` is `<workload>:<absolute-path>` (scp-style); the other is a local path.

```sh
# Local → workload
capsulectl cp ./config.toml api:/etc/app/config.toml      # rename: lands at /etc/app/config.toml
capsulectl cp ./config.toml api:/etc/app/                 # into dir: lands at /etc/app/config.toml
capsulectl cp ./assets api:/srv/                          # whole tree: lands at /srv/assets/...
capsulectl cp ./assets api:/srv/static                    # whole tree, renamed: lands at /srv/static/...

# Workload → local
capsulectl cp api:/var/log/app.log ./app.log              # single file
capsulectl cp api:/etc ./etc-backup                       # whole tree (rename root)
capsulectl cp api:/etc ./backups/                         # whole tree, into existing dir
```

When pushing into a workload, the destination resolves the way `cp`/`scp` do:

1. Trailing `/` → always treated as "into this directory" (created if missing).
2. Existing directory inside the workload → "into directory" (so `cp foo wl:/tmp` lands at `/tmp/foo`, not as a file overwriting `/tmp`).
3. Otherwise → the destination is the exact final path; the source is renamed to that path (and any existing file/dir there is overwritten).

When pulling from a workload, the same rules apply against the local destination — including the existing-directory check on your laptop.

Wire format is a tar stream. The workload image must include `/bin/sh` plus the basic POSIX shell tools (`mkdir`, `mktemp`, `mv`, `ls`, `wc`, `dirname`) and `tar` — universal in busybox/alpine/debian-derived images, **not** present in `scratch`.

## `volume`

### `volume create [--size 10GiB] <name>`

Create a thin-provisioned LVM logical volume, format ext4, register it. Default size is `10GiB`.

### `volume list`

Table of all volumes + their size + attached workload (if any).

### `volume get <name>`

Full volume metadata.

### `volume resize <name> <size>`

`lvresize` + `resize2fs`. **Grow only** — shrinks are rejected. Volume must be detached (no workload mounted).

### `volume delete [--force] <name>`

Remove a volume. Default rejects if attached; `--force` first detaches the workload (if any) before deleting.

## `image`

Manages the capsule's containerd image cache. Useful for images that aren't in any registry the capsule can reach — local builds, private/air-gapped images, or one-off testing without spinning up a registry.

### `image list`

Table of every image cached on the capsule (whether pulled by a workload or pushed by you):

```
NAME                                  DIGEST               SIZE     UPDATED
docker.io/library/alpine:3.20         sha256:abcd123456…   8.1MiB   2026-04-30T12:11:08Z
myhost/myapp:dev                      sha256:0fedba9876…   142.0MiB 2026-05-01T22:05:14Z
```

`SIZE` is total content size (manifest + config + reachable layers). `UPDATED` is bumped on (re)pull or (re)push.

### `image push <tarball>`

Streams an OCI / docker-save tar archive into the capsule's image store and unpacks it into the snapshotter so it's immediately usable as a workload `image:`.

```sh
# Build locally, save, push.
docker save myapp:dev -o /tmp/myapp.tar
capsulectl image push /tmp/myapp.tar

# Or pipe directly from `docker save`:
docker save myapp:dev | capsulectl image push -
```

The CLI prints a progress line while bytes are in flight and lists the imported refs on success. After that, reference the same ref in a manifest and apply — `capsuled` finds it cached and skips the registry pull entirely:

```yaml
name: myapp
kind: Container
container:
  image: myapp:dev      # same ref docker save used; no registry involved
```

Multi-arch tarballs are accepted; only the platform matching the capsule unpacks (others stay as content-addressed blobs in the store, which is fine).

## Sizes

Anywhere a size is accepted (`--size`, `volume resize`, `Volume.size:`):

- Bare `M` / `G` / `T` are treated as `MiB` / `GiB` / `TiB` (binary multiples).
- Explicit `KiB` / `MiB` / `GiB` / `TiB` suffixes also work.
- Plain integers are bytes.

Examples: `2GiB`, `500M`, `1024`, `10G`.
