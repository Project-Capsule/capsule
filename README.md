# Capsule

A minimal homelab OS that runs **containers and microVMs** under one declarative gRPC API.

Capsule is an immutable, single-binary-controlled Linux distribution. `capsuled` is PID 1 on every machine; it speaks gRPC on `:50000`. You `capsulectl apply -f workload.yaml` and the reconciler makes it so, whether the workload is a container or a Firecracker microVM.

It sits in a gap the existing ecosystem doesn't fill:

- **Talos** is container-native but k8s-only. No general containers, no VMs, no kernel module API.
- **gokrazy** is Go-appliance-only — no scheduler, no workload abstraction.
- **k3OS** is archived.
- **Flatcar / Bottlerocket / Fedora CoreOS / Kairos** are conventional immutable Linux distros, not single-binary-managed.

## Vocabulary

- **Capsule** — the OS. A machine running Capsule is also called *a capsule* (same noun for OS + instance).
- **`capsuled`** — the daemon that runs as PID 1 on every capsule. Owns zombie reaping, process supervision, early mounts, and the gRPC API.
- **`capsulectl`** — the CLI on the operator's laptop. Talks to one capsule (v1) or many (future).
- **`capsule-guest`** — a tiny binary that runs as PID 1 *inside* every microVM. Exposes a gRPC agent over vsock so the host can `StartPayload / Exec / Logs / Stop`. Launches the user's OCI image under runc.

## Architecture

```
┌──── Disk layout (x86_64 EFI-less, MBR) ─────────────┐
│  Part 1  FAT32 /boot  256 MiB  kernel + initramfs   │
│  Part 2  ext4  ROOTFS          Alpine + capsuled    │
│  Part 3  ext4  PERM            state.db, volumes/…  │
└──────────────────────┬──────────────────────────────┘
                       ▼
┌──── Active rootfs (ext4, writable) ─────────────────┐
│  Alpine userland: containerd, runc, iptables, …    │
│  /sbin/init = capsuled ← PID 1                      │
│    ├─ early mounts (/proc /sys /dev /run /tmp cg2) │
│    ├─ mount PERM rw at /perm                        │
│    ├─ bring up eth0 (static via QEMU SLIRP)         │
│    ├─ supervise containerd                          │
│    ├─ serve gRPC on :50000                          │
│    ├─ reconciler: SQLite → runtime drivers          │
│    └─ reap zombies (PID 1 duty)                     │
└──────────────────────┬──────────────────────────────┘
                       ▼
         ┌─── Container driver  (containerd)
         ├─── MicroVM driver    (Firecracker + KVM)
         └─── Volume backing    (raw ext4 files)
```

### MicroVMs, smolvm-style

Every microVM boots the **same** rootfs image — a 128 MiB `vm-shared.ext4` containing busybox + `runc` + `capsule-guest` as `/sbin/init`. The user's OCI image is flattened into a **per-VM payload disk** (`payload.ext4`) attached as `/dev/vdb`. `capsule-guest` mounts `/dev/vdb` at `/oci`, writes an OCI runtime bundle, and runs `runc run payload`.

The control plane flows over **vsock**: host `capsuled` dials the guest agent at CID=3 port 52 via the Firecracker UDS. Exec, Logs, Stop all go through this channel — same API surface as containers.

The VM is the trust boundary; the container semantics (PID namespace, cgroups, dropped capabilities) are what runc gives you inside.

### Volumes

All volumes are **raw ext4 files** at `/perm/volumes/<name>.ext4`. Containers loop-mount them and bind into the OCI spec; microVMs attach them as additional virtio-blk devices (`/dev/vdc+`). Same backing store either way; a volume can move between a container and a VM without data conversion.

One mounter at a time (enforced by the kernel — ext4 doesn't support concurrent mounts).

### Networking

- **Container, host mode** — shares the capsule's net namespace.
- **Container, bridge mode** — veth into `br0` via CNI plugins, DNAT for port mappings.
- **MicroVM** — TAP device on `br0`, static IP in `172.20.254.0/24`, kernel `ip=` cmdline. `MASQUERADE` out via the capsule's uplink. Port mappings via iptables DNAT tagged with `capsule-vm:<name>` comments so teardown is O(1).

## Quickstart

### Build the image

You need Docker, Go 1.25+, `buf`, and `qemu-system-x86_64` locally.

```
make proto     # regenerate .pb.go from .proto
make image     # builds capsule/rootfs docker image → packer → build/disk.raw (~2.7 GB)
make qemu      # boots the image in QEMU (KVM if available, TCG fallback)
```

`build/disk.raw` is a bootable MBR disk image you can also `dd` onto a USB or VM.

### Talk to a running capsule

```
./build/capsulectl --capsule localhost:50000 capsule info
./build/capsulectl --capsule localhost:50000 workload apply -f examples/nginx-host.yaml
./build/capsulectl --capsule localhost:50000 workload list
./build/capsulectl --capsule localhost:50000 workload logs --follow nginx
./build/capsulectl --capsule localhost:50000 workload exec -t nginx -- /bin/sh
```

### Workload manifests

**Container:**

```yaml
name: nginx
kind: Container
container:
  image: docker.io/library/nginx:alpine
  networkMode: BRIDGE
  ports:
    - containerPort: 80
      hostPort: 8080
```

**MicroVM:**

```yaml
name: alpine-vm
kind: MicroVM
microVm:
  image: docker.io/library/alpine:3.20
  vcpus: 1
  memoryMib: 256
  ports:
    - containerPort: 80
      hostPort: 8081
  mounts:
    - volumeName: app-data
      mountPath: /data
```

Workloads with the same spec shape (`image`, `command`, `args`, `env`, `ports`, `mounts`) run as either a container or a Firecracker microVM — flip `kind`, nothing else.

## CLI reference

```
capsulectl [--capsule host:port] capsule info
capsulectl [--capsule host:port] workload apply -f <manifest.yaml>
capsulectl [--capsule host:port] workload list
capsulectl [--capsule host:port] workload get <name>
capsulectl [--capsule host:port] workload delete <name>
capsulectl [--capsule host:port] workload restart <name>
capsulectl [--capsule host:port] workload stop <name>
capsulectl [--capsule host:port] workload start <name>
capsulectl [--capsule host:port] workload logs [-f] [-n N] [--serial] <name>
capsulectl [--capsule host:port] workload exec [-t] <name> -- <cmd> [args...]
capsulectl [--capsule host:port] volume create <name>
capsulectl [--capsule host:port] volume list
capsulectl [--capsule host:port] volume get <name>
capsulectl [--capsule host:port] volume delete [--force] <name>
```

`--serial` on `workload logs` switches to the VM's serial console (kernel boot + `capsule-guest` + Firecracker) — useful when the VM fails to come up and the guest agent is unreachable.

## Repo layout

```
cmd/
  capsuled/       — PID-1 daemon main
  capsulectl/     — operator CLI
  capsule-guest/  — microVM PID-1 agent (vsock gRPC server, runs runc)
models/capsule/v1/ — proto source + generated Go (capsule.v1 package)
boot/             — PID-1 early mounts, zombie reap, supervised child spawn
core/
  workload/       — business logic for workload lifecycle (kind-agnostic)
  volume/         — volume CRUD
  reconciler/     — desired → actual tick loop
runtime/
  container/      — containerd-backed ContainerDriver
  microvm/firecracker/ — Firecracker-backed VMDriver
store/
  sqlite/         — modernc.org/sqlite persistence
  memory/         — in-memory impl for tests
controllers/      — gRPC handlers (proto ⇄ core)
router/           — gRPC server wiring (TLS, interceptors, registration)
image/
  Dockerfile      — builds the capsule rootfs
  build.sh        — docker build → rootfs.tar → packer
  Dockerfile.packer — assembles disk.raw from rootfs.tar
  pack.sh         — runs inside packer: mkfs, partition, syslinux install
examples/         — declarative manifests for the demos
```

## Design choices worth knowing

- **No internal/ package.** Layers are `models → core → store + runtime`, with concrete adapters (`store/sqlite`, `runtime/container`, `runtime/microvm/firecracker`) wired up only in `cmd/capsuled/main.go`.
- **Proto package is flat** (`capsule.v1`) with service-prefixed message names (`WorkloadGetRequest`, `VolumeGetRequest`) so the three services coexist without collisions.
- **`ExecMu` shared mutex** in `boot/` serializes `exec.Command` calls against the PID-1 reap loop. Without it, the reaper wait4s the subprocess before `exec.Cmd.Wait` can, and you get spurious "waitid: no child processes" errors on iptables / ip / mkfs.
- **Firecracker, not QEMU.** Firecracker gets you fast boot and minimal attack surface, at the cost of no virtiofs, no PCI passthrough, no USB. The `VMDriver` interface is backend-agnostic — `smolvm` and `qemu` are reserved slots in the `MicroVMBackend` enum.
- **Raw ext4 for volumes, not qcow2.** Simpler, and the thin-provisioning / snapshot / incremental-sync story is deferred until the orchestrator exists to use them.
- **Single-capsule v1.** A future orchestrator will fan out across capsules; `capsulectl` is already structured to loop over a capsule list.

## Roadmap

See [PLAN.md](./PLAN.md) for what's next and the known gotchas.

## License

TBD.
