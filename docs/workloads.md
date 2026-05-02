# Workloads

A workload is a **container** or a **microVM**, declared by a YAML manifest, applied to a running capsule. Volumes are a separate kind that workloads reference by name.

The same spec shape (`image`, `command`, `args`, `env`, `ports`, `mounts`) works for both containers and microVMs — flip `kind:` to switch.

## The universal verb: `apply -f`

```sh
capsulectl apply -f <manifest.yaml>
```

Dispatches on the manifest's `kind:` field. Idempotent — apply the same file twice and you get the same end state.

| `kind:`     | Goes to       | What it does                                              |
|-------------|---------------|-----------------------------------------------------------|
| `Container` | `WorkloadService.Apply` | Create or update a container workload.        |
| `MicroVM`   | `WorkloadService.Apply` | Create or update a Firecracker microVM.       |
| `Volume`    | `VolumeService` | Create the volume if missing; resize if smaller than requested (grow only). |

## Containers

### Minimal — `HOST` networking

```yaml
name: nginx-host
kind: Container
container:
  image: docker.io/library/nginx:1.27-alpine
  networkMode: NETWORK_MODE_HOST
```

Shares the capsule's network namespace. Ports bind directly on the capsule's IP. Simplest for "just expose this thing on the LAN."

### Bridged with port mapping

```yaml
name: nginx-bridge
kind: Container
container:
  image: docker.io/library/nginx:1.27-alpine
  networkMode: NETWORK_MODE_BRIDGE
  ports:
    - containerPort: 80
      hostPort: 8080
      protocol: tcp
```

Container gets a veth into `br0` with an IP in `172.20.254.0/24`. `hostPort:8080` → `containerPort:80` is implemented as iptables DNAT.

### With command override + env

```yaml
name: alpine-shell
kind: Container
container:
  image: docker.io/library/alpine:3.20
  command: ["/bin/sh", "-c"]
  args: ["while true; do date; sleep 5; done"]
  env:
    - name: TZ
      value: UTC
```

### With a volume

The volume must exist (or be applied in the same session before the workload):

```yaml
# 1. volume manifest
name: nginx-html
kind: Volume
size: 1GiB
```

```yaml
# 2. workload manifest
name: nginx-data
kind: Container
container:
  image: docker.io/library/nginx:1.27-alpine
  networkMode: NETWORK_MODE_BRIDGE
  ports:
    - containerPort: 80
      hostPort: 8080
  mounts:
    - volumeName: nginx-html
      mountPath: /usr/share/nginx/html
      readOnly: false
```

Apply in order:

```sh
capsulectl apply -f nginx-html.yaml
capsulectl apply -f nginx-data.yaml
```

Inside the container, `/usr/share/nginx/html` is the volume's contents. Survives restarts. Deleting the workload doesn't delete the volume — that's a separate `volume delete`.

## MicroVMs

Same spec shape under `microVm:` instead of `container:`.

### Minimal microVM

```yaml
name: alpine-vm
kind: MicroVM
microVm:
  image: docker.io/library/alpine:3.20
  vcpus: 1
  memoryMib: 256
  command: ["/bin/sh", "-c"]
  args: ["while :; do date; sleep 5; done"]
```

The OCI image is flattened into a per-VM payload disk and run inside a Firecracker microVM under runc.

### MicroVM with port mapping

```yaml
name: nginx-vm
kind: MicroVM
microVm:
  image: docker.io/library/nginx:alpine
  vcpus: 1
  memoryMib: 256
  ports:
    - containerPort: 80
      hostPort: 8080
      protocol: tcp
```

VM gets a TAP into `br0`, static IP in `172.20.254.0/24`. `hostPort:8080` → `containerPort:80` is iptables DNAT — same shape as containers.

### MicroVM with a volume

```yaml
name: alpine-vol
kind: MicroVM
microVm:
  image: docker.io/library/alpine:3.20
  vcpus: 1
  memoryMib: 256
  command: ["/bin/sh", "-c"]
  args: ["echo hi > /data/hello.txt && while :; do cat /data/hello.txt; sleep 5; done"]
  mounts:
    - volumeName: alpine-data
      mountPath: /data
```

Inside the VM, `/data` is the volume. Backing storage is the same LVM thin LV as a container would mount — same data either way.

## Sharing a volume between a container and a VM

Volumes have one mounter at a time (kernel-enforced — ext4 doesn't allow concurrent mounts). To pass data between a container and a VM:

```sh
# 1. Create the shared volume.
capsulectl apply -f examples/shared-volume.yaml

# 2. Container writes.
capsulectl apply -f examples/shared-writer-container.yaml
capsulectl workload logs --follow shared-writer
capsulectl workload delete shared-writer            # release the volume

# 3. VM reads the same data.
capsulectl apply -f examples/shared-reader-vm.yaml
capsulectl workload logs --follow shared-reader
```

If you forget to delete the first workload, the second `apply` will fail at attach time with a clear "volume in use by …" error.

## Lifecycle

```sh
capsulectl workload list                       # all workloads + status
capsulectl workload get <name>                 # full spec + status
capsulectl workload logs [-f] [-n N] <name>    # stdout/stderr
capsulectl workload logs --serial <name>       # microVMs only — Firecracker serial console
capsulectl workload exec [-t] <name> -- /bin/sh
capsulectl cp <src> <dst>                      # copy files/dirs to/from a workload (scp-style)
capsulectl workload restart <name>
capsulectl workload stop <name>                # stop, leave the row in the DB
capsulectl workload start <name>               # start a stopped workload
capsulectl workload delete <name>              # stop + delete the row
```

`workload exec` works for both containers and microVMs. For microVMs, it goes through `capsule-guest`'s vsock agent and into the runc payload — same UX as a container exec.

`cp` streams a tar archive over the same exec path, so it works uniformly for containers and microVMs. The workload image must include `/bin/sh`, `mkdir`, and `tar` (universal in busybox/alpine/debian-derived images; not in `scratch`). See [cli.md](cli.md#cp) for path semantics.

`workload logs --serial` is microVM-only; it streams the Firecracker serial console (kernel boot + `capsule-guest` + early failures). Use it when the guest agent isn't reachable (e.g. the VM's kernel didn't come up).

## Images that aren't in a registry

A workload's `image:` is normally pulled from a public or private registry on first use. When the image isn't in any registry the capsule can reach — local builds, air-gapped images, work in progress — push it directly into the capsule's containerd cache instead:

```sh
docker save myapp:dev -o /tmp/myapp.tar
capsulectl image push /tmp/myapp.tar
# or pipe stdin:
docker save myapp:dev | capsulectl image push -
```

Then reference the same ref in the manifest:

```yaml
name: myapp
kind: Container
container:
  image: myapp:dev      # found in cache; no registry pull
```

`capsulectl image list` shows what's cached. See [cli.md](cli.md#image) for full details.

## Volume operations

```sh
capsulectl apply -f volume.yaml             # create or grow
capsulectl volume create [--size 10GiB] <name>
capsulectl volume list
capsulectl volume get <name>
capsulectl volume resize <name> <size>      # grow only; volume must be detached
capsulectl volume delete [--force] <name>   # detach first, or pass --force
```

Sizes accept `K`/`M`/`G`/`T` (treated as KiB/MiB/GiB/TiB) plus the explicit `KiB`/`MiB`/`GiB`/`TiB` suffixes.

## Manifest field reference

### Workload (`Container` or `MicroVM`)

```yaml
name: <string>            # required, unique per capsule
kind: Container | MicroVM

container:                # required when kind: Container
  image: <oci ref>        # required
  command: [<string>]     # optional, overrides image entrypoint
  args: [<string>]        # optional
  env:
    - name: <string>
      value: <string>
  networkMode: NETWORK_MODE_HOST | NETWORK_MODE_BRIDGE   # default BRIDGE
  ports:
    - containerPort: <int>
      hostPort: <int>
      protocol: tcp | udp
  mounts:
    - volumeName: <string>
      mountPath: <string>
      readOnly: <bool>

microVm:                  # required when kind: MicroVM
  image: <oci ref>        # OCI flow (preferred): flattened to a payload disk
  # OR provide kernelPath + rootfsPath for an externally-built kernel + rootfs.
  kernelPath: <path on capsule>
  rootfsPath: <path on capsule>
  command: [<string>]
  args: [<string>]
  env: [...]
  vcpus: <int>            # default 1
  memoryMib: <int>        # default 256
  ports: [...]            # same shape as container.ports
  mounts: [...]           # same shape as container.mounts
```

### Volume

```yaml
name: <string>     # required
kind: Volume
size: <human-size> # required, e.g. 2GiB / 500M / 10G
```

## Worked examples in `examples/`

| File                                  | What it shows                                   |
|---------------------------------------|-------------------------------------------------|
| `nginx-host.yaml`                     | Container, host network                         |
| `nginx-bridge.yaml`                   | Container, bridged + port mapping               |
| `alpine-shell.yaml`                   | Container with command override                 |
| `nginx-with-volume.yaml`              | Container with a persistent volume              |
| `alpine-vol-container.yaml`           | Container that reads/writes a shared volume     |
| `nginx-vm.yaml`                       | MicroVM, bridged + port mapping                 |
| `alpine-vm-image.yaml`                | MicroVM from an OCI image                       |
| `alpine-vm-with-volume.yaml`          | MicroVM with a persistent volume                |
| `shared-volume.yaml`                  | Volume manifest                                 |
| `shared-writer-container.yaml`        | Container that writes to `shared`               |
| `shared-reader-vm.yaml`               | MicroVM that reads/appends `shared`             |
| `debug-shell.yaml`                    | Privileged debug container (host net)           |
| `alpine-vm.yaml`                      | MicroVM from prebuilt kernel + rootfs           |
