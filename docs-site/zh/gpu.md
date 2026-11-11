# GPU 沙箱

> 创建时给 `gpu: N`，沙箱就能拿到有 NVIDIA 容器运行时的 worker 上的真实设备。
> 其余部分——生命周期、执行、文件、持久化、MCP——与 CPU 沙箱完全一致。

```bash
curl -s $API/api/v1/sandboxes -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"train","gpu":1,"image_id":"sandrpod-toolbox:ubuntu","ttl_seconds":3600}'
```

## worker 必须能给得出来

1. 宿主机上有驱动和可见的 GPU。
2. `nvidia-container-toolkit` 已安装并注册给 docker
   （`nvidia-ctk runtime configure --runtime=docker`，然后重启 docker）。

worker 是**通过 docker 守护进程**检查这件事的（`docker info` 的 `Runtimes` 里有 `nvidia`），
而不是去找 `/dev/nvidia0`——worker 自己就跑在容器里，那里看不到设备节点。只有检查通过它才
声明 `gpu` 能力位，也只有那时 GPU 沙箱才会被放上去：

```
$ docker logs sandrpod-worker | grep -i gpu
GPU support detected (nvidia container runtime registered)
```

放不上去时会明确拒绝：

```
worker capability unavailable: this sandbox needs a GPU, but 2 online local
worker(s) do not advertise "gpu"
```

## 镜像必须是 glibc

NVIDIA 注入的是 **glibc** 二进制（`nvidia-smi` 和驱动库）。默认的 `sandrpod-toolbox`
是 Alpine（musl）：设备能进去，但每个 NVIDIA 二进制都报 `required file not found`。
请用 `sandrpod-toolbox:ubuntu`（或任何 Ubuntu/Debian 镜像，包括桌面镜像）。

## 设备怎么分配

```
HostConfig.DeviceRequests: [{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}]
```

`Count` 就是要的数量，绝不用 `-1`（全部设备），所以两个 GPU 沙箱不会意外共用同一张卡。

## 成本护栏

| 变量 | 默认 | 作用 |
|---|---|---|
| `SANDRPOD_GPU_MAX_PER_OWNER` | `1` | 每个身份同时存活的 GPU 沙箱数；拒绝信息会告诉用户去停止或删除 |
| `SANDRPOD_GPU_MAX_TTL` | `2h` | 闲置 GPU 沙箱的 TTL 被**裁剪**（不是拒绝）：agent 要一天也只给两小时，并留一条日志 |

设为 `0` 可分别关闭。已终止和出错的记录不占名额。

计费在沙箱仍然占用的 vCPU 与内存之上加 `CentPerGPUHour`，所以 GPU 沙箱按它真正的成本计价，
而不是当成便宜的 CPU 沙箱。

闲置释放是 **stop** 容器（`docker stop`，不是 `pause`），这样设备会还给驱动用于计费——
reaper 用 stop 的原因就在这里。

## 端到端验证一台新 GPU worker

```bash
# 1. 宿主机：toolkit 本身能用吗？
docker run --rm --gpus 1 ubuntu:22.04 nvidia-smi

# 2. 平台层：引擎分配了设备吗？
docker inspect <沙箱容器> --format '{{json .HostConfig.DeviceRequests}}'
# → [{"Driver":"nvidia","Count":1,"Capabilities":[["gpu"]]}]

# 3. 沙箱内：负载能看到它吗？
curl -s "$API/api/v1/sandboxes/execute?sandbox=train" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"code":"nvidia-smi --query-gpu=name,memory.total --format=csv,noheader","language":"bash"}'
```
