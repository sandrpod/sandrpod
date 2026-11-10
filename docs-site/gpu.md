# GPU sandboxes

> `gpu: N` on a create gives the sandbox real devices from a worker with the NVIDIA
> container runtime. Everything else — lifecycle, exec, files, persistence, MCP —
> behaves exactly as on a CPU sandbox.

```bash
curl -s $API/api/v1/sandboxes -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"train","gpu":1,"image_id":"sandrpod-toolbox:ubuntu","ttl_seconds":3600}'
```

## The worker must be able to give it

1. A driver and a visible GPU on the host.
2. `nvidia-container-toolkit` installed and registered with docker
   (`nvidia-ctk runtime configure --runtime=docker`, then restart docker).

The worker checks this **through the docker daemon** (`docker info` → `Runtimes`
contains `nvidia`), not by looking for `/dev/nvidia0` — the worker itself runs in a
container, where the device node is invisible. Only then does it advertise the `gpu`
capability, and only then will a GPU sandbox be placed on it:

```
$ docker logs sandrpod-worker | grep -i gpu
GPU support detected (nvidia container runtime registered)
```

Placement refuses clearly when nothing can take it:

```
worker capability unavailable: this sandbox needs a GPU, but 2 online local
worker(s) do not advertise "gpu"
```

## The image must be glibc

NVIDIA injects **glibc** binaries (`nvidia-smi` and the driver libraries). The
default `sandrpod-toolbox` image is Alpine (musl): the device arrives and every
NVIDIA binary fails with `required file not found`. Use
`sandrpod-toolbox:ubuntu` (or any Ubuntu/Debian image, including the desktop ones).

## Allocation

```
HostConfig.DeviceRequests: [{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}]
```

`Count` is exactly what was asked for, never `-1` (all devices), so two GPU sandboxes
cannot share a card by accident.

## Cost guardrails

| Variable | Default | Effect |
|---|---|---|
| `SANDRPOD_GPU_MAX_PER_OWNER` | `1` | Live GPU sandboxes per identity; the refusal tells the user to stop or delete one |
| `SANDRPOD_GPU_MAX_TTL` | `2h` | An idle GPU sandbox's TTL is **clamped** (not refused): an agent asking for a day gets two hours and a log line |

`0` disables either half. Terminated and errored records do not hold a slot.

Billing adds `CentPerGPUHour` on top of the vCPU and memory the sandbox reserves, so
a GPU sandbox is priced as what it is rather than as a cheap CPU one.

Idle release **stops** the container (`docker stop`, not `pause`), which returns the
device for billing — that is why the reaper uses stop.

## Verify a new GPU worker end to end

```bash
# 1. host: does the toolkit work at all?
docker run --rm --gpus 1 ubuntu:22.04 nvidia-smi

# 2. platform: did the engine allocate the device?
docker inspect <sandbox-container> --format '{{json .HostConfig.DeviceRequests}}'
# → [{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}]

# 3. in-sandbox: can the workload see it?
curl -s "$API/api/v1/sandboxes/execute?sandbox=train" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"code":"nvidia-smi --query-gpu=name,memory.total --format=csv,noheader","language":"bash"}'
```
