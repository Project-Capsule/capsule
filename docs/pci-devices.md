# PCI devices and GPUs (proposal)

> **Status:** Proposal. Not implemented. Containers only in v1; microVM passthrough is explicitly deferred — see *Why microVMs can't do this today* below.

## Summary

Capsule today has no way to give a workload access to host hardware beyond the standard `/dev` nodes a container always sees (`/dev/null`, `/dev/random`, `/dev/console`, etc.). This proposal adds **registered devices**: the operator explicitly registers a host device (a PCI device, a `/dev/dri/*` render node, a USB-serial port — anything with a `/dev` node) under a logical name, and workloads reference it by name in their spec. The capsuled container driver translates that into the right OCI runtime bits (bind into the container's mount namespace + cgroup device-allow). Manifests stay portable because they reference the *name*, not the host-specific PCIe bus address.

The motivating workload is an **Intel Arc Pro B50** for AI/inference workloads, but the same mechanism covers FPGAs, video-capture cards, USB-serial, and anything else with a Linux `/dev` interface.

**v1 is containers only.** microVM passthrough is a meaningful architectural change (different VMM); we explain why below and sketch the future path.

## Goals

- A **device registry** owned by capsuled: `capsulectl device add gpu0 --dev-node /dev/dri/renderD128 --dev-node /dev/dri/card0`. Operator-explicit; capsuled never offers hardware automatically.
- A **`devices:` field on `ContainerSpec`** that references registry entries by name. Workloads are portable across nodes — each node maintains its own name → host-path mapping.
- **No host-side AI/compute stack.** The host kernel exposes `/dev/dri/renderD128`; the container brings its own oneAPI / OpenVINO / Level Zero / CUDA / whatever-userspace it needs. This keeps the Capsule rootfs minimal and avoids host-vs-container version skew.
- **Sharing by default** (multiple containers can hold the same device simultaneously and the kernel driver arbitrates) — which is how `/dev/dri/renderD128` semantically works.
- A clear, documented answer for *why GPU access in microVMs doesn't work today*, and what would unblock it.

## Non-goals (v1)

- **microVM passthrough.** Firecracker can't, full stop. Documented below; planned via smolvm/libkrun in a follow-up.
- **vGPU / SR-IOV slicing.** Carving one physical GPU into N virtual GPUs is its own stack (`xe` doesn't expose VFIO-mdev yet; SR-IOV on Arc is firmware-gated). Defer.
- **Exclusive-mode reservations.** Telling capsuled "this container *owns* the GPU and nothing else may touch it" is a useful future feature for workloads that hate noisy-neighbor; v1 is sharing-only.
- **Tiers 2/3/4 of the driver-tier story.** v1 ships only Tier 1 (drivers baked into the image alongside `linux-lts`). The driver-pack mechanism, system-workload-as-driver-container, and custom-kernel options are designed below and explicitly deferred.
- **Hotplug.** Plugging/unplugging hardware while workloads run is undefined. Stop the workload first.

## Why microVMs can't do this today

This is the load-bearing constraint that pushes v1 to containers-only. Firecracker is **architecturally incapable** of GPU or any PCIe passthrough — not "missing a flag," not "unimplemented":

- Firecracker exposes **no PCI bus to the guest**. Only virtio-mmio devices: virtio-block, virtio-net, virtio-vsock, virtio-rng, virtio-balloon. That's the entire device list.
- There is no VFIO support, no IOMMU plumbing, no `-device vfio-pci` equivalent. The Firecracker maintainers have closed every PCI/VFIO RFE on the project; passthrough is explicitly out of scope.
- There is no virtio-gpu device either, so even Vulkan-over-virtio (the shared-GPU-without-passthrough trick) is unavailable.

This is the right choice for Firecracker's stated mission (serverless functions in seconds, locked-down attack surface). It is the wrong choice for "give my AI VM a GPU." The fix is **a different VMM** for the GPU workload path, not a Firecracker patch — see *Future: microVMs via smolvm/libkrun* below.

## The driver question

There's a real distinction between two different things people call "the Intel Arc driver":

| Layer        | What it is                              | Where it runs     | Capsule's role        |
|--------------|-----------------------------------------|-------------------|-----------------------|
| Kernel driver | `xe` (Battlemage gen, Arc Pro B50) or `i915` (Alchemist gen). Upstream in Linux. | Host kernel        | Capsule ships it (already in `linux-lts` 6.18 module tree). Auto-loads on PCIe enumeration. Exposes `/dev/dri/card0` + `/dev/dri/renderD128`. |
| Userspace stack | Intel Compute Runtime (`intel-opencl-icd`), Level Zero loader, oneAPI, OpenVINO, Intel Media Driver, Mesa iris/anv. | Container image    | Capsule does **not** ship this. Workloads bring their own — e.g. `intel/oneapi-basekit`, `intel/intel-extension-for-pytorch`, or custom images layering Level Zero on top of Ubuntu/Debian. |

The user's intuition was right: an AI workload on Arc B50 needs *something more than the upstream `xe`* — but that "something more" is **userspace libraries that live in the container image**, not a different host kernel driver. This is the same model NVIDIA users follow (Intel's is cleaner because there's no kernel-userspace ABI match required — Level Zero talks to `xe` over a stable uAPI; pinning the host kernel doesn't pin the container's oneAPI version).

So the host's only responsibility is:

1. Load the right kernel driver. For Arc Pro B50: `modprobe xe` (probably auto-loaded by udev on PCIe scan, but Capsule's PID-1 `capsuled` doesn't run udev — we'd add it to `modules-load.d` alongside the TPM/dm-crypt list from the encryption proposal).
2. Expose `/dev/dri/...` to allowed containers.
3. Get out of the way.

If a future device needs a kernel driver that isn't in `linux-lts` upstream, that's the *out-of-tree driver* problem (see future work).

## Concept: registered devices

A **device** in Capsule is a named tuple of one-or-more host `/dev` node paths, recorded in SQLite and addressable by name.

### SQLite schema

```
CREATE TABLE devices (
  name         TEXT PRIMARY KEY,             -- 'gpu0', 'fpga-a', 'usb-modem'
  class        TEXT NOT NULL,                -- 'gpu' | 'fpga' | 'serial' | 'generic'
  pci_address  TEXT,                         -- '0000:01:00.0', informational
  description  TEXT,                         -- 'Intel Arc Pro B50 16GB', informational
  created_at   INTEGER NOT NULL
);

CREATE TABLE device_nodes (
  device_name  TEXT NOT NULL REFERENCES devices(name) ON DELETE CASCADE,
  path         TEXT NOT NULL,                -- '/dev/dri/renderD128'
  mode         TEXT NOT NULL,                -- 'rw' | 'r'
  PRIMARY KEY (device_name, path)
);
```

A single registered device can bundle multiple `/dev` nodes — for a GPU, both `/dev/dri/card0` (display/KMS) and `/dev/dri/renderD128` (compute/render) get exposed together. For a USB-serial, just `/dev/ttyUSB0`.

`pci_address` and `description` are informational — they show up in `capsulectl device list` so operators can correlate the logical name with physical hardware. capsuled doesn't use the PCI address for anything; the `/dev` node paths are the source of truth at runtime.

### Device-management CLI

```
capsulectl device add gpu0 \
    --class gpu \
    --dev /dev/dri/renderD128 \
    --dev /dev/dri/card0 \
    --description "Intel Arc Pro B50"

capsulectl device list
# NAME    CLASS  NODES                                    DESCRIPTION
# gpu0    gpu    /dev/dri/{card0,renderD128}              Intel Arc Pro B50

capsulectl device get gpu0
capsulectl device remove gpu0    # refuses if any workload references it
```

`device add` does sanity checks: every `--dev` path must exist on the host and be a character or block device. capsuled does **not** auto-discover hardware — the operator is the one who decides "yes, this `/dev/dri/renderD128` should be offerable to workloads." Avoids the foot-gun of an unexpected `/dev` node showing up after a kernel module load and silently becoming available.

For convenience, `capsulectl device suggest` (future) could scan `/sys/class/drm/`, `/sys/bus/pci/devices/`, etc. and print suggested `device add` invocations — operator copies the one they want. Same explicit-consent model.

## Container path

Workload spec gains a `devices` list:

```yaml
kind: Container
spec:
  image: intel/oneapi-basekit:2026.0
  command: [python3, /app/inference.py]
  devices:
    - name: gpu0
```

At workload apply, capsuled:

1. Looks up `gpu0` in the `devices` table. Errors out if not registered (with a clear "register the device first" message).
2. Stats every `device_nodes.path` on the host. Errors if any are missing (kernel driver didn't load, device unplugged).
3. Reads each node's major/minor.
4. For each path, adds an OCI `linux.devices` entry (so the node is bind-mounted into the container's mount namespace) and a `linux.resources.devices` entry (so the container's cgroup-v2 device controller permits access).

Concretely, the OCI bits look like:

```json
{
  "linux": {
    "devices": [
      { "path": "/dev/dri/renderD128", "type": "c", "major": 226, "minor": 128, "fileMode": 432, "uid": 0, "gid": 0 },
      { "path": "/dev/dri/card0",      "type": "c", "major": 226, "minor": 0,   "fileMode": 432, "uid": 0, "gid": 0 }
    ],
    "resources": {
      "devices": [
        { "allow": true, "type": "c", "major": 226, "minor": 128, "access": "rw" },
        { "allow": true, "type": "c", "major": 226, "minor": 0,   "access": "rw" }
      ]
    }
  }
}
```

containerd / runc do the rest. The container sees `/dev/dri/renderD128` and `/dev/dri/card0` as if it had them natively. Multiple containers can hold these nodes simultaneously — the `xe` driver's internal scheduler arbitrates GPU time the same way it does for two host processes.

### What about GIDs?

`/dev/dri/render*` on most distros belongs to a `render` group, and `/dev/dri/card*` to `video`. Containers need to either run as a user in those groups or as root. Capsule's `linux.devices` injection mode-sets the nodes `0666` in the container's namespace, sidestepping the GID-mapping headache. If the workload's image relies on a specific UID/GID, that's the image's choice — but the device nodes won't gate access by group inside the container.

## Failure scenarios

| Failure                                              | Symptom                                | Recovery                                                                                       |
|------------------------------------------------------|----------------------------------------|------------------------------------------------------------------------------------------------|
| Device registered, host `/dev` node missing at apply  | `Apply` rejects workload with `device 'gpu0' references /dev/dri/renderD128 which does not exist on this host` | Operator checks `dmesg \| grep xe`. If the module didn't load: register it in `modules-load.d` and reboot, or `modprobe xe` from a debug session for a one-off |
| Kernel driver crashed or device hung                  | Container starts, calls into Level Zero, gets `-EIO`/`-ENODEV` | DRM subsystem reset; in practice a reboot. Same outcome as on bare metal — Capsule doesn't make this worse |
| Hardware unplugged (eGPU dock disconnected)           | Same as above; `/dev/dri/...` may even vanish | Workload errors. Operator reconnects + restarts workload |
| Multiple workloads contend for GPU time               | Slower throughput per workload         | Expected. Linux DRM scheduler arbitrates. No Capsule-level fairness work needed; if a workload needs exclusivity, future `--exclusive` flag is the answer |
| Operator removes device while a workload uses it      | `device remove gpu0` is refused while any workload references it | Stop the workload first |
| capsuled crash with workload mid-start                | Either the OCI bundle had the device list applied or it didn't; runc's `create` is the atomic boundary | Standard workload-restart path. No device-state to clean up on the host |
| Device removed from registry but a workload's spec still names it | At next apply, the workload errors with "unknown device 'gpu0'" | Operator either re-adds the registry entry or removes the device from the workload spec |
| `xe` and `i915` race to claim the same PCI device     | Whichever wins owns it. On a B50, only `xe` should match the PCI ID — this shouldn't happen | If it does: `/etc/modprobe.d/capsule-xe.conf` with `blacklist i915` for cards we've explicitly committed to `xe` |

## Future: microVMs via smolvm / libkrun

Out of scope for v1, sketched here so the v1 design doesn't paint us into a corner.

**Two complementary mechanisms** libkrun exposes that Firecracker doesn't:

1. **VFIO passthrough.** Bind the GPU to `vfio-pci` on the host, hand the BDF to libkrun, guest gets exclusive access via a real PCIe device. Same model as QEMU/Cloud-Hypervisor passthrough. Needed for: oneAPI compute, OpenVINO inference, anything that wants the full Intel Arc userspace stack inside the guest.
2. **virtio-gpu / Venus.** Vulkan-over-virtio. Host keeps the kernel driver, guest sees a virtual Vulkan device, multiple guests can share one GPU. Useful for Vulkan-compute workloads and any rendering. smolvm already supports this on the host side; what's missing is exposing it through smolvm's CLI/Smolfile.

What this would look like in Capsule:

- A second microVM driver: `runtime/microvm/smolvm/` alongside `runtime/microvm/firecracker/`. Same `Driver` interface.
- capsuled routes microVM workloads with `devices:` (or a `gpu: { mode: shared|passthrough }` block) to the smolvm driver. Everything else stays on Firecracker — Firecracker is still the right call for the "fast tiny VM, no hardware" case.
- The device registry is reused as-is. Mode `shared` → Venus path. Mode `passthrough` → VFIO path. capsuled handles the host-side prep (binding to `vfio-pci`, releasing back to `xe` on workload stop).
- libkrun ships VFIO support upstream; smolvm needs a CLI/config surface to expose it. Since we contribute to smolvm, this work happens there and lands in Capsule once it's available.

We deliberately don't add Cloud Hypervisor or QEMU as a third VMM — smolvm covers the same ground via libkrun and we already have an upstream relationship there.

## When the host doesn't have the driver

There's a real spectrum of "this device needs a driver `linux-lts` doesn't have." It includes open-source drivers that aren't upstream yet, vendor drivers that *will* never be upstream (NVIDIA proprietary, some industrial hardware), and drivers that only ship as binary blobs per-kernel-ABI. Capsule's immutable-rootfs + A/B-update shape makes "just `apk add` it" not an option, so we need a real story.

Four tiers, in increasing order of operational cost. v1 implements **Tier 1** as the foundation and **Tier 3** as the recommended path for proprietary drivers; the others are documented escape hatches.

### Tier 1: bake into the image

Best for **open-source drivers we're willing to track in the Capsule image release cadence**.

- Driver source becomes a build dep in `image/Dockerfile`.
- Compiles against `linux-lts` headers during `make image`.
- Resulting `.ko` lands in `/lib/modules/<kver>/extra/` inside the squashfs.
- Auto-loads via `image/etc/modules-load.d/capsule.conf`.

Cost: every `linux-lts` bump must keep the driver building. Every driver update requires a Capsule image release + A/B push. Acceptable for drivers that move slowly (ZFS, vendor-but-open drivers like select IPU/NPU drivers).

### Tier 2: driver pack on /perm

For **closed-source drivers** or **open drivers we want to update independently of the OS image**.

- Operator: `capsulectl driver install <pack.tar>`.
- Pack contents (a small, well-defined archive format):
  ```
  manifest.yaml                    # name, version, kernel_abi: 6.18.24-0-lts
  modules/<module-name>.ko         # pre-built .ko (one or more)
  modules/<module-name>.ko.sig     # detached signature (if secure boot is on)
  firmware/<files>                 # blobs that go into /lib/firmware
  udev/<rules>                     # optional udev rules
  ```
- capsuled validates: `kernel_abi` matches the running kernel, signature (if present) verifies against a trusted key, no path-traversal in entries.
- Installs to `/perm/drivers/<name>/`. **/perm survives A/B slot flips**, so the driver persists across OS updates as long as the new slot's kernel ABI matches.
- On boot, capsuled's early init pass walks `/perm/drivers/*`, loads modules whose `kernel_abi` matches the running kernel, logs warnings (and surfaces in `capsule info`) for any that don't match the current kernel.
- A `capsulectl driver list` shows installed packs, their kernel-ABI match status, and whether each is loaded.

Cost: vendor (or operator) must build/ship a pack per kernel version. When Capsule's `linux-lts` bumps, every driver pack needs a refresh — and until refreshed, the device is unavailable.

### Tier 3: driver container

The **NVIDIA gpu-operator pattern**. Best for vendors who already ship this model, and for closed-source drivers that need to build at runtime against the running kernel's headers.

- A new workload kind / flag: **system workload** — runs as part of node init, before user workloads start. Marked via `kind: SystemWorkload` or `spec.systemPriority: true` (TBD).
- The driver container is privileged, has the host PID + mount + module namespaces (so it can `insmod` into the host kernel), and bind-mounts `/lib/modules/<kver>/` for headers if it builds, or just `/lib/modules/<kver>/extra/` for staging if it ships pre-built `.ko`s per ABI.
- Vendor (e.g. NVIDIA) maintains the container. Operator just declares it in a manifest: `image: nvcr.io/nvidia/driver:565.x.x-capsule`.
- Once loaded, the kernel module is normal — user workloads consume `/dev/nvidia*` via the regular device-registry mechanism.
- Lifecycle is fully decoupled from Capsule's OS image. `linux-lts` bumps → next boot → driver container rebuilds against the new kernel and reloads. The operator never edits a Capsule rootfs.

This is the right answer for NVIDIA, AMD ROCm, and any vendor who's already done the work to ship a driver container.

### Tier 4: custom kernel (last resort)

If the driver needs deep in-tree integration (extensive kernel patches, not a modular driver). Capsule would ship an alternate kernel variant — `linux-capsule-<variant>` — built separately. Maintenance is painful. Defer until forced.

### Cross-cutting: signing and secure boot

The encrypted-volumes proposal binds the TPM seal to PCR 7 (secure-boot policy). If a Capsule node has secure boot enabled, the kernel can be configured (`module.sig_enforce`) to refuse unsigned modules — a meaningful integrity property. Out-of-tree modules then need one of:

- **Capsule-controlled MOK** (Machine Owner Key) enrolled in the bootloader. Capsule signs all Tier-1 and Tier-2 modules at build time with this key. Tier-3 containers either ship pre-signed modules or invoke `mokutil`-equivalent during their build.
- **Vendor-signed modules** trusted via a vendor-key enrolled alongside the MOK (e.g., NVIDIA signs their kernel modules; their public key gets added to the MOK list at adoption).
- **`module.sig_enforce=0` on the kernel cmdline**, accepting that unsigned modules can load. Defeats one of the integrity guarantees secure boot offers but doesn't disable the rest (boot chain measurement still works, PCR 7 still meaningful).

v1 likely doesn't enable secure boot by default, so this is a future-work surface — but the proposal should call it out now so we don't paint ourselves into a corner.

### How to choose

| Driver characteristic                                | Recommended tier  |
|------------------------------------------------------|-------------------|
| Open source, moves slowly, broadly useful            | Tier 1 (in image) |
| Open source, want independent update cadence         | Tier 2 (pack)     |
| Closed source, vendor ships a container              | Tier 3 (container)|
| Closed source, vendor only ships `.ko` blobs         | Tier 2 (pack)     |
| Needs kernel-tree patches, not modular               | Tier 4 (custom kernel) |
| Already upstream                                     | Just `modules-load.d` — no tier needed |

## Open questions

- **Should the device registry persist a checksum of the host `/dev` node major/minor at registration time** and warn if it changes (a kernel upgrade renumbering DRM minors, say)? Probably yes — cheap to check, prevents silent device-mismatch bugs.
- **Allow workloads to declare device requirements without a hard-fail at apply time** (e.g., `devices: [{name: gpu0, optional: true}]` to enable graceful degradation)? Useful for inference workloads that have a CPU fallback. Not v1; revisit when there's demand.
- **PCIe topology in `device add`.** Should we record the IOMMU group at registration time, so the future VFIO path can fail-fast if the operator's group isolation is broken? Cheap to add now, useful later. Probably yes.
- **What's the right answer for `card0` vs `renderD128` for headless compute workloads?** Render-node-only (`renderD128`) is sufficient for Level Zero / OpenCL compute and avoids needing `video` group access. Default to render-node-only and let operators add card0 explicitly when they need KMS/display? Probably.
- **How do we surface "this workload has a GPU" in `capsulectl workload list` / `get`?** Useful for inventory; not blocking.

## Implementation pointers

- Proto: `models/capsule/v1/device.proto` (new) — `DeviceService` with `Add`, `Remove`, `Get`, `List`. `ContainerSpec` grows `repeated DeviceRef devices = N;` where `DeviceRef { string name = 1; }`.
- Schema: new `devices` + `device_nodes` tables in `store/sqlite/sqlite.go`. No changes to `volumes` / `workloads`.
- Logic: new `core/device/service.go` with `Add`, `Remove`, `Get`, `List`, plus an `OCIBits(ctx, names []string) ([]oci.Device, []oci.LinuxDeviceCgroup, error)` helper consumed by the container driver.
- Runtime: `runtime/container/driver.go` reads `spec.GetDevices()`, calls `deviceService.OCIBits(...)`, appends to the existing `oci.WithSpec` builder list. ~40 LOC of new code, no architectural change.
- Image: `image/etc/modules-load.d/capsule.conf` (already proposed in encrypted-volumes.md) grows entries for graphics modules — `xe` is the relevant one for Arc Pro B50; `i915` for older Intel discrete; `amdgpu` for AMD discrete. Auto-load is best-effort: if the hardware isn't present, `modprobe` no-ops harmlessly.
- CLI: `capsulectl device {add,remove,list,get,suggest}`. `workload apply` validates device references at submission, not at runtime, so manifests fail fast.

Containers driver is the only runtime that changes. microVM driver (Firecracker) explicitly errors out if a workload declares `devices:` — pointing the operator at this doc.
