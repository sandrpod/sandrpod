# GPU sandboxes

> **Status**: shipped. Updated: 2026-11

A GPU sandbox gets real devices from a worker that has the NVIDIA container
runtime. Everything else — lifecycle, exec, files, persistence, MCP — behaves
exactly as on a CPU sandbox.

## Create one

```bash
# REST
curl -s $API/api/v1/sandboxes -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"gpu-job","gpu":1,"image_id":"sandrpod-toolbox:ubuntu","ttl_seconds":3600}'
```

```python
# MCP tool call
sandbox_create(name="gpu-job", gpu=1, image="sandrpod-toolbox:ubuntu")
```

## What has to be true on the worker

1. An NVIDIA driver and a visible GPU on the host.
2. `nvidia-container-toolkit` installed and registered with docker
   (`nvidia-ctk runtime configure --runtime=docker`, then restart docker).

The worker detects this at startup **through the docker daemon**
(`docker info` → `Runtimes` contains `nvidia`), not by looking for `/dev/nvidia0`:
the pode itself runs in a container, where the device node is invisible. A worker
that passes the check advertises the `gpu` capability, and only then will a GPU
sandbox be placed on it.

```
$ docker logs sandrpod-worker | grep -i gpu
GPU support detected (nvidia container runtime registered)
```

Placement refuses clearly when nothing can take the work:

```
worker capability unavailable: this sandbox needs a GPU, but 2 online local
worker(s) do not advertise "gpu" (upgrade the poder to a build that has it, or
drop the requirement)
```

## The sandbox image must be glibc

NVIDIA's runtime injects **glibc** binaries (`nvidia-smi` and the driver
libraries). The default `sandrpod-toolbox` image is Alpine (musl), so a GPU
sandbox created on it gets the device but every NVIDIA binary fails with
`required file not found`.

Use the glibc variant:

```
sandrpod-toolbox:ubuntu      # docker/Dockerfile.toolbox.ubuntu
```

Any Ubuntu/Debian-based image works (the desktop images do too).

## How the device is allocated

The driver maps the count into docker's `DeviceRequests`:

```json
HostConfig.DeviceRequests: [{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}]
```

`Count` is the number asked for, never `-1` (all devices), so two GPU sandboxes
cannot collide on the same card.

## Cost guardrails

A GPU sandbox costs ten to thirty times a CPU one, so the platform bounds it per
owner:

| Variable | Default | Effect |
|---|---|---|
| `SANDRPOD_GPU_MAX_PER_OWNER` | `1` | Live GPU sandboxes per identity; the refusal explains itself and points at stop/delete |
| `SANDRPOD_GPU_MAX_TTL` | `2h` | An idle GPU sandbox's TTL is **clamped**, not refused — an agent asking for a day gets two hours and a log line |

`0` disables either half (trusted internal deployments).

Terminated and errored sandbox records do not hold a slot: a user cannot clean up
a record the platform already gave up on.

## Idle release frees the card

The idle reaper **stops** the container (`docker stop`, not `pause`), which
returns the GPU to the driver for billing — that is the whole reason the reaper
uses stop. The next `start` resumes the same container for free.

## Verification recipe

If you are setting up a GPU worker for the first time, this is the fastest way to
prove the platform end to end:

```bash
# 1. host-level: does the toolkit work at all?
docker run --rm --gpus 1 ubuntu:22.04 nvidia-smi

# 2. platform-level: did the engine allocate the device?
docker inspect <sandbox-container> --format '{{json .HostConfig.DeviceRequests}}'
# → [{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}]

# 3. in-sandbox: can the workload see it?
curl -s "$API/api/v1/sandboxes/execute?sandbox=gpu-job" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"code":"nvidia-smi --query-gpu=name,memory.total --format=csv,noheader","language":"bash"}'
```
