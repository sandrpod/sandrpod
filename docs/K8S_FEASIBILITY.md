# Kubernetes 调度：可行性分析（尚未实施）

> **状态**: 分析稿，2026-11。结论：**可行**，接口层已经留好缝，但有 4 处语义需要重新定义，
> 其中快照一处会反向影响现在的实现选择。
> 本文只做判断和取舍，不含实施步骤。

---

## 0. 一段话结论

现在这套架构之所以能换掉 Docker 而不动控制面，是因为**控制面从不直接操作容器**：它只跟
`Poder` 接口和一条反向隧道打交道。K8s 支持 = 再写一个 `K8sPoder` 实现，控制面、调度器、
MCP、计费都不用改。但「暂停/恢复」和「快照」这两个语义是 Docker 特有的，在 K8s 上要重新
定义；其中快照的定义会反过来决定现在该不该继续把数据放在本地卷里。

---

## 1. 现状：哪些是「缝」，哪些是「墙」

### 缝（换实现即可，控制面无感）

| 缝 | 位置 | 说明 |
|---|---|---|
| `Poder` 接口 | `pkg/poder/interface.go` | CreatePod / DeletePod / Pause / Unpause / Logs / Exec / Snapshot / Health，已完整抽象 |
| 反向隧道 | `pkg/tunnel` | worker 主动外连，**不需要入站端口**——K8s 里同样不需要 LoadBalancer/Ingress |
| 调度 | `pkg/sandpod/scheduler.go` | 按 region / provider / capability / tenant 选 worker，与容器运行时无关 |
| 能力位 | `X-Poder-Capabilities` | `egress` / `gpu` 已经是声明式，K8s worker 可声明同样两个 |
| 数据面 | `/workspace` 命名卷 | 语义是「一块持久盘」，K8s 里就是 PVC |
| 对外入口 | 端口代理 → preview 域名 | 只依赖「能到 toolbox:8080」，不依赖 docker bridge |

### 墙（Docker 语义，K8s 没有对应物）

| 墙 | 现在怎么用 | K8s 上怎么办 |
|---|---|---|
| **pause/stop** | `docker stop` 释放 CPU/内存/GPU，容器和数据都在 | 没有「停」。三种映射：①删 Pod 留 PVC（数据在，进程没了）②缩容到 0 ③`Job.suspend`（1.24+，暂停未完成的 Job）。我们的语义（释放资源 + 保留数据）最自然是 ① |
| **docker commit** | 快照 = 镜像层 + 卷 | K8s **没有**容器文件系统快照。要么 `VolumeSnapshot`（卷快照）+ 固定基础镜像，要么 `docker save` 到对象存储再导入 |
| ImagePull 依赖 | 本地镜像 + 无仓库 | K8s 必须能拉镜像 → 强制上 registry（这恰好也是现在快照跨节点缺的那一环） |
| 容器 IP | docker bridge 内网 IP，pode 直接访问 | Pod IP。集群内可达，但需要 NetworkPolicy 配合隔离 |
| 宿主机 docker.sock | pode 挂在 socket 上 | 改成 ServiceAccount + RBAC（只能管自己名字空间里的 Pod/PVC） |

---

## 2. 语义重定义：三处要说清楚

### 2.1 「停止」 = 删 Pod，留 PVC

现在的承诺是：**停止后 `/workspace` 数据保留，start 恢复**。K8s 上：

```
CreatePod  → Job/Pod + PVC(workspace)
Stop       → 删 Pod（PVC 保留）
Start      → 用同一 PVC 重建 Pod
```

用户可见行为完全一致（快照/文件都在），只有两点差别：
- 启动比 docker start 慢（几秒到几十秒，取决于镜像是否已在节点）
- 容器内的临时状态（例如桌面会话里的窗口）不会恢复——这一点现在也没有保证

结论：**可以做到语义等价**，代价是启动延迟。

### 2.2 快照 = 卷快照，而不是容器快照

现在的快照是 `docker commit`：把容器文件系统做成镜像，再把 `/workspace` 卷拷过去。
K8s 没有这个能力。可行的映射：

| 方案 | 保留什么 | 代价 |
|---|---|---|
| `VolumeSnapshot` + 固定镜像 | 只有 `/workspace` 数据 | 用户「装了某个包」这类镜像层改动丢失 |
| 卷快照 + 提交镜像到 registry（podman/docker-in-docker 或 kaniko） | 数据 + 镜像层 | 复杂、慢，且需要特权构建 |
| 只做卷快照，镜像层改动明确不支持 | 数据 | 最简单，语义清晰 |

**这条会反向影响今天的实现**：如果认了「K8s 快照 = 卷快照」，那现在就该把
「快照」的产品语义定义成 *「/workspace 的时间点副本」*，而不是「整个容器的镜像」。
否则将来 K8s 版会变成一个**能力更弱**的功能，用户会觉得被降级。

### 2.3 出网策略

现在是「每 pod 一个 internal network + 网关上的代理」。K8s 里对应 `NetworkPolicy` +
一个 egress gateway，**能力更强**（按 namespace/label 生效）。这块是 K8s 的加分项，
不是负担。

---

## 3. 工作量评估（粗）

| 模块 | 规模 | 备注 |
|---|---|---|
| `K8sPoder`（Pod/Job + PVC + logs + exec + capability 探测） | 1500–2500 行 | 用 client-go，不引 CRD：Pod + PVC + NetworkPolicy 足够 |
| 镜像仓库接入（worker `docker push` / `docker pull`） | 300–500 行 + 部署 | 跨节点快照/分叉的前提 |
| 快照语义改造（若采纳卷快照） | 500 行 + 数据迁移 | 影响现在的快照/分叉 |
| Helm chart + RBAC + ServiceAccount | 1 个 chart | 无状态控制面已有，主要是 worker |
| 文档 / 故障排查 | —— | —— |

**结论：一个 2–3 周的工程**，且不需要动控制面核心。

---

## 4. 现在该做什么、不该做什么

### 现在别做

- **不要**现在写 K8s 执行器：还没有用户在多节点集群上跑，抽象会猜错。
- **不要**为了 K8s 提前砍掉 docker 能力（单机 docker 是最低门槛的自托管形态，是卖点）。

### 现在可以做（低成本、将来省事）

1. **把「停止」的承诺写成契约**：「停止 = 释放算力 + 保留 /workspace + 下次启动重建运行时」。
   这正是 K8s 能做到的语义，写进文档比将来改实现便宜。
2. **重新定义快照的对外语义**为「/workspace 的时间点副本」，镜像层改动的保留明确标注为
   Docker 专有能力（今天的实现可以继续，但文案别承诺「整个容器状态」）。
3. **镜像仓库当基础设施**：哪怕现在只有单机，也把「镜像从哪来」抽象成配置项
   （本地 / registry），快照跨节点的缺口和 K8s 的需求是同一件事。
4. **能力位继续用**：`egress` / `gpu` 已经是声明式，K8s worker 直接声明，调度器零改动。

### 判断依据

如果未来 6 个月出现「一个客户要把 agent 跑在自己的 K8s 集群里」——那就要做；
如果客户都是单机/几台 VM，K8s 的收益（多租户隔离、弹性、GPU 调度）抵不过复杂度。

---

## 5. 风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| 快照语义在 K8s 上缩水 | 用户观感降级 | 现在就把文案和语义定成「卷快照」 |
| 启动延迟（重建 Pod） | 「秒级恢复」变「十秒级」 | 保留 warm pool / 用 suspend Job |
| RBAC 权限过大 | 安全 | 每租户一个 namespace + 最小权限 ServiceAccount |
| 两套执行器长期并存 | 维护成本翻倍 | 接口已经隔开，CI 里两套都要跑契约测试 |
