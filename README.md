# Capsule

A minimal homelab OS that runs **containers and microVMs** under one declarative gRPC API.

Capsule is an immutable, single-binary-controlled Linux distribution. `capsuled` is PID 1 on every machine; it speaks gRPC on `:50000`. You `capsulectl apply -f workload.yaml` and the reconciler makes it so, whether the workload is a container or a Firecracker microVM.

It sits in a gap the existing ecosystem doesn't fill:

- **Talos** is container-native but k8s-only. No general containers, no VMs, no kernel module API.
- **gokrazy** is Go-appliance-only — no scheduler, no workload abstraction.
- **k3OS** is archived.
- **Flatcar / Bottlerocket / Fedora CoreOS / Kairos** are conventional immutable Linux distros, not single-binary-managed.

## Lineage

Spiritually closer to *original* CoreOS — the pre-Red-Hat era, when it was a scrappy, opinionated, immutable OS with A/B image updates and a small declarative surface — than to any of its descendants. Capsule is a single-host take on that idea: no clustering, no etcd, just SQLite and a reconciler that owns the box.

## Documentation

The operator's manual lives in [`docs/`](./docs/README.md):

- [Getting started](./docs/getting-started.md) — build the image, boot it (QEMU or real hardware), connect with `capsulectl`, deploy your first workload.
- [Workloads](./docs/workloads.md) — containers and microVMs, with and without volumes. All manifest shapes.
- [`capsulectl` reference](./docs/cli.md) — every subcommand and flag.
- [Architecture](./docs/architecture.md) — disk layout, boot chain, runtime drivers, networking, volumes, state.
- [A/B updates](./docs/updates.md) — push a new OS bundle to a running capsule, confirm or auto-rollback.

## Quick taste

```sh
make image                                          # → build/disk.raw + build/update.tar
make qemu                                           # boot it locally (UEFI)
export CAPSULE_HOST=localhost:50000
./build/capsulectl capsule info
./build/capsulectl apply -f examples/nginx-host.yaml
./build/capsulectl workload list
```

See [`docs/getting-started.md`](./docs/getting-started.md) for the full walkthrough.

## Roadmap

See [PLAN.md](./PLAN.md) for what's next and the known gotchas.

## License

TBD.
