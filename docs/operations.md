# Operations runbook

Breakglass procedures for things capsule doesn't (yet) expose as a first-class verb. Everything here goes through `capsulectl capsule debug` — see [cli.md](cli.md#capsule-debug) for the basics of that command.

## Debug container quick-reference

Two papercuts to know about every time you open a debug session:

- **Match the host's Alpine version** with `-i`. The debug container has `/sbin`, `/usr/sbin`, `/usr/bin` bind-mounted from the host, so host binaries (`apk`, `pvs`, ...) end up in the container's PATH but want host libraries the container doesn't have. The default image is `alpine:3.20`; the host today is `alpine:3.23`. Mismatch → "Error loading shared library libapk.so..." on the first command.
  ```sh
  capsulectl capsule debug -i docker.io/library/alpine:3.23 -- /bin/sh
  ```
- **Install your toolchain.** The debug image is bare Alpine. For storage work you almost always want:
  ```sh
  apk add --no-cache lvm2 e2fsprogs sfdisk parted util-linux
  ```
  (TODO: a prebuilt `capsule-debug` image with this baked in.)

## Grow the PERM partition

`PERM` (partition 4) is created at `PERM_SIZE_MIB=2048` (2 GiB) by `image/pack.sh`. On a real disk that's almost always too small — the LVM PV inside backs both `/perm` (capsule state) and the thinpool (every user volume + containerd snapshotter). When `capsule info` shows `volume pool` close to full, this is the move.

PERM is the **last** partition on the disk, so you can grow it online without disturbing anything. The exact sequence is mechanical; do it from a debug session:

```sh
capsulectl capsule debug -i docker.io/library/alpine:3.23 -- /bin/sh -c '
set -ex
apk add --no-cache lvm2 e2fsprogs sfdisk parted util-linux >/dev/null

# 1. Save the partition table BEFORE touching anything. Lands on /perm so
#    it survives the debug container being torn down.
sfdisk -d /dev/sda > /perm/parts-backup-$(date +%Y%m%d-%H%M%S).sfdisk

# 2. Grow p4 to fill the disk. --no-reread is required because /perm is
#    mounted; we re-read explicitly with partprobe afterwards.
printf ", +\n" | sfdisk --no-reread -N 4 /dev/sda
partprobe /dev/sda

# 3. Tell LVM about the new PV size.
pvresize /dev/sda4

# 4. Grow whichever LV needs the room:
#    (a) the thinpool (where user volumes live — the usual answer)
lvextend -l +100%FREE /dev/capsule/thinpool
#    (b) /perm itself (capsule state, container image cache — rarely needed)
# lvextend -L +5G --resizefs /dev/capsule/meta

# 5. Verify.
sfdisk -l /dev/sda
pvs; vgs; lvs
df -h /perm
'
```

Then `capsulectl capsule info` should show the new `volume pool` total.

### Why `--no-reread`

`sfdisk` defaults to refusing to write a partition table that's currently in use. `--no-reread` skips the *initial* re-read check (which is what trips on `/perm` being mounted). The MBR write itself is safe; we use `partprobe` afterwards to update the kernel's view of `/dev/sda4`. For the *last* partition this is reliable — no other partition's offsets change.

### What growing the thinpool does (and doesn't)

`lvextend -l +100%FREE /dev/capsule/thinpool` extends both `thinpool_tdata` (where bytes live) and `thinpool_tmeta` (the per-block-allocation metadata) — LVM auto-grows the metadata LV proportionally when you extend the data LV by a large factor. After a 50× grow you'll see metadata go from ~4 MiB to ~100 MiB, which is correct.

Existing user volumes do **not** automatically grow when the thinpool grows — the thinpool just has more room for them to grow into. To resize an individual volume:

```sh
capsulectl volume resize <name> <size>   # grow only; volume must be detached
```

### Rolling back

If you need to undo (only meaningful if you haven't already extended the LV and written data past the old end), restore the partition table from the backup:

```sh
sfdisk /dev/sda < /perm/parts-backup-YYYYMMDD-HHMMSS.sfdisk
partprobe /dev/sda
```

Once the thinpool has grown and started using the new PV extents, rollback isn't an option — at that point the only "shrink" path is destroy + re-image.

### When this should be a real verb

This sequence is mechanical enough that it ought to be `capsulectl capsule grow-disk` (or similar). Punted because (a) it's a one-time operation per machine in practice and (b) doing it safely from inside `capsuled` itself is awkward — `capsuled` is the thing using `/perm`. The breakglass path here is good enough until that changes.
