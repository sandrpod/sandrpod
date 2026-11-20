# gVisor runtime support

> **Status**: shipped in v0.13.1 (2026-11-20), opt-in. The default runtime is
> still Docker's `runc`. The API field and CLI flag are part of this release.
>
> gVisor puts a user-space kernel (the Sentry) between the sandbox and the host
> kernel. It is not a microVM; it is a materially smaller host-kernel attack
> surface for untrusted code.

## Why

SandrPod's default isolation is per-sandbox Docker: each sandbox gets its own
container, its own bridge, resource limits, a read-only-by-policy egress proxy,
and no access to the Docker socket. That is a good boundary for trusted code and
network-level tenancy, but every container still shares the host kernel. A kernel
or runtime escape crosses tenants.

gVisor intercepts syscalls and implements the Linux ABI in user space. It
removes the largest shared-kernel attack surface without requiring hardware
virtualization, nested virtualization, or a second substrate. It is the
middle tier between hardened Docker and Kata/Firecracker.

It does **not** replace a microVM: gVisor is still a software boundary, and its
production guidance targets trusted stacks rather than arbitrary hostile code.
Use it as defense in depth, not as a formal hardware-isolation claim.

## Non-goals

- gVisor is not installed or managed by SandrPod. The worker host must install
  `runsc` and register it with Docker.
- gVisor is not the default. A deployment that does not opt in behaves exactly
  as before.
- GPU sandboxes are not supported under gVisor in this iteration. NVIDIA device
  access and CUDA compatibility need their own validation.
- Desktop/KasmVNC sandboxes are not certified. The toolbox/code path and
  headless browser path are the targets; a desktop image may need `--no-sandbox`
  and is expected to be slower.
- No Kubernetes requirement. This integrates with the same Docker worker; a
  `RuntimeClass` path can be added later for a Kubernetes-backed worker.

## Host requirements

| Requirement | Notes |
|---|---|
| Linux 5.6+ | `runsc` install guide's current minimum; x86_64 or arm64 |
| `runsc` installed and registered | `runsc install` configures Docker; verify with `docker info` → `Runtimes` contains `runsc` |
| seccomp | `systrap` (the default platform) relies on `SECCOMP_RET_TRAP` |
| cgroups | resource limits continue to work; cgroup v2 recommended |
| No KVM required | `systrap` runs inside ordinary VMs; the KVM platform is optional and needs `/dev/kvm` (bare metal or nested virtualization) |
| DNS for sandboxes | set `SANDRPOD_GVISOR_DNS` to one or more upstream resolvers; gVisor's netstack cannot use Docker's embedded `127.0.0.11`. The worker rewrites `/etc/resolv.conf` inside each runsc sandbox |
| Docker socket | unchanged: the worker already needs it |

The worker does **not** need a different VM shape, a GPU host, or nested
virtualization. A normal Linux cloud VM is enough for `systrap`.

Platform choice is a worker-level `runsc` setting (`--platform=systrap|kvm`) in
the Docker runtime arguments; SandrPod does not manage it. Prefer `systrap` on
VMs, KVM on bare metal.

## API

The native create request gains an optional `runtime`:

```json
{
  "name": "agent-01",
  "provider_type": "local",
  "runtime": "gvisor"
}
```

| Value | Meaning |
|---|---|
| omitted / `runc` | Docker's default runtime |
| `gvisor` | require a worker that advertises the `gvisor` capability, then create with Docker runtime `runsc` |

`runsc` is accepted as an alias for `gvisor`; the stored/returned value is the
canonical `gvisor`. Any other value is a `400`.

`GET /api/v1/sandboxes/{name}` and list rows carry `"runtime":"gvisor"` when the
sandbox was created that way. A record created before this field existed has an
empty runtime, which means the deployment default (`runc`).

The CLI:

```bash
sandrpod-cli create box --provider local --runtime gvisor
sandrpod-cli create box --provider local --runtime runc   # explicit default
```

The pinned-create path (`POST /api/v1/poders/{id}/sandboxes`) accepts the same
field and applies the same capability check.

## Scheduling and capability

A worker probes the Docker daemon at startup:

```
docker info → Runtimes["runsc"] present
```

If present, it advertises `gvisor` on the tunnel handshake, next to `egress`
and `gpu`. A sandbox that asks for `runtime: "gvisor"` is only placed on a
worker that advertises it. If no eligible worker does, the create answers
**409 worker capability unavailable** — the same failure shape as an egress
policy on a fleet with no capable worker.

This avoids the worst outcome: accepting the sandbox, running it under `runc`,
and reporting a gVisor runtime it never had.

## Poder behavior

`CreatePodRequest` gains `runtime`. The Docker driver resolves it:

1. `""` / `runc` → leave `HostConfig.Runtime` empty (Docker default).
2. `gvisor` → verify `runsc` is registered, then set
   `HostConfig.Runtime = "runsc"`.
3. Unknown → refuse the create.

The request also stamps `sandrpod/runtime` on the container labels and the
sandbox record, so a `docker inspect` and the API agree.

Nothing else changes in the Docker path: the same image, network, volume,
egress sidecar, resource limits, and snapshot logic apply. That is deliberate —
gVisor is a runtime swap, not a second orchestration model.

## Compatibility expectations

Must pass before calling a deployment supported:

- `execute` (bash/python/node), PTY, streaming, sessions, contexts;
- files read/write/upload/download/watch;
- MCP bridge (stdio and Streamable HTTP) **after** the sandbox gets a real
  resolver via `SANDRPOD_GVISOR_DNS`;
- egress allow/deny through the sidecar (the sidecar name is mapped in
  `/etc/hosts`; Docker's embedded DNS is not reachable from netstack);
- live egress-policy updates (`PUT /sandboxes/{id}/network`), including moving
  the `/etc/hosts` entry to the replacement sidecar's IP;
- create-from-image (the image itself must already exist);
- **not** snapshots: `docker commit` does not capture runsc's filesystem writes,
  so snapshot and fork are refused with `409` for gVisor sandboxes;
- `pause`/`start` and persistence;
- headless browser (Chrome/Obscura) + CDP/MCP (subject to the resolver above).

Expected to be tested but not promised:

- desktop images (KasmVNC/XFCE/Chrome);
- workloads using `ptrace`, raw sockets, `io_uring`, eBPF, kernel modules;
- GPU/CUDA.

Known limitations:

- file I/O and network throughput are slower than `runc`;
- some `/proc` and `/sys` views are synthesized by gVisor;
- debugging inside the sandbox is less familiar (no host `strace` view);
- the host kernel must be 5.6+ and seccomp must not be disabled.

## Security notes

gVisor reduces the host-kernel attack surface; it does not eliminate it. Keep:

- per-sandbox bridge and egress proxy;
- non-root execution where the image allows it;
- capability drops / seccomp / no-new-privileges at the Docker layer;
- patched host kernels and `runsc` versions;
- one trust tier per worker where the threat model warrants it.

For arbitrary hostile code, use gVisor **plus** a hardware boundary (Kata,
Firecracker, or a dedicated worker VM). The threat model should be written down
before claiming "untrusted multi-tenant".

## Performance

Directional, to be measured on the target host:

| Workload | Expected |
|---|---|
| CPU-bound | near native |
| syscall-heavy / process spawn | slower (2–5× in the worst cases) |
| file I/O, package installs | slower |
| network throughput | lower; netstack |
| browser page load | slower |
| sandbox start | +100–500 ms |
| memory per sandbox | +20–50 MB for Sentry/Gofer |

`runsc` platform selection is the biggest lever: `systrap` on VMs, KVM on bare
metal. Do not enable gVisor globally before measuring the actual workload.

## Rollout

1. Install `runsc` on a canary Linux worker; restart the worker; confirm the
   handshake advertises `gvisor`.
2. Create a sandbox with `runtime: "gvisor"`; run the compatibility checks.
3. Run the CLI/E2E suite against that sandbox.
4. Add the capability to more workers; keep `runc` as the default.
5. Only then consider a tenant/plan policy that defaults new sandboxes to
   gVisor.

Rollback is per-sandbox: create without `runtime` (or with `runc`) and the
worker uses Docker's default. Existing gVisor sandboxes continue to run until
deleted/stopped; the worker binary can be rolled back without touching them
because the container runtime is a host-level Docker setting.

## Install reference

```bash
# On the worker host (not inside the poder container). gVisor now publishes a
# tarball containing runsc, the containerd shim and gvisor-bin — the old bare
# runsc URL is gone.
arch=$(uname -m)   # x86_64 or aarch64
url="https://storage.googleapis.com/gvisor/releases/release/latest/${arch}"
curl -fsSL "${url}/gvisor.tar.zstd" -o /tmp/gvisor.tar.zstd
curl -fsSL "${url}/gvisor.tar.zstd.sha512" -o /tmp/gvisor.tar.zstd.sha512
(cd /tmp && sha512sum -c gvisor.tar.zstd.sha512)
sudo tar --zstd -xf /tmp/gvisor.tar.zstd -C /usr/local/bin
sudo runsc install
sudo systemctl reload docker
docker info --format '{{json .Runtimes}}'   # must contain "runsc"
# Restart the poder container afterwards: it probes the daemon at startup and
# advertises the "gvisor" capability on the next handshake.
```

`deploy/node-install.sh` does this by default (`--no-gvisor` opts out), writes
the `runsc` entry into `/etc/docker/daemon.json`, and passes upstream DNS to the
worker as `SANDRPOD_GVISOR_DNS`. Pin a gVisor release in production instead of
`latest`; record the version next to the worker image version.
