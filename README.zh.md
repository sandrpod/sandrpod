<p align="center">
  <img src="assets/logo.png" alt="SandrPod" width="400"/>
</p>

<p align="center">
  <strong>面向 AI Agent 的自托管执行基础设施。</strong><br/>
  在任意云、任意机器 —— 甚至没有 Docker 的裸机上跑 Agent 代码。想用哪套 SDK 就用哪套，全程你说了算。
</p>

<p align="center">
  <em>你的云。你的机器。你的规则。</em>
</p>

<p align="center">
  <a href="https://pypi.org/project/langchain-sandrpod/"><img src="https://img.shields.io/pypi/v/langchain-sandrpod?color=3B82F6&label=langchain-sandrpod" alt="PyPI"/></a>
  <img src="https://img.shields.io/badge/self--hosted-open%20source-16A34A" alt="Self-hosted"/>
  <img src="https://img.shields.io/badge/clouds-8%20providers-0EA5E9" alt="Multi-cloud"/>
  <img src="https://img.shields.io/badge/E2B%20SDK-drop--in-8B5CF6" alt="E2B compatible"/>
  <img src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go" alt="Go"/>
  <img src="https://img.shields.io/badge/license-Apache%202.0-green" alt="License"/>
</p>

---

## 简介

**SandrPod** 是 AI Agent 代码执行的控制平面 —— 开源、自托管。它把你自己的基础设施
变成一队按需沙箱，Agent 在其中创建、执行代码、用完销毁，而**运行位置、能碰什么、
数据落在哪**，始终归你掌控。

托管沙箱服务让你在**它们**的区域租**它们**的 runtime。SandrPod 反过来：**底座由你
自带** —— 任意云、普通 Docker、甚至一台裸机 —— SandrPod 只是那层轻薄、可移植的
调度 / 隧道 / 治理层，把执行铺在你的基础设施之上。它立在三根支柱上：

- **跑在你拥有的任何地方** —— 8 家云（含阿里云、腾讯云）、Docker，或完全无需 Docker 的裸机。
- **想说哪套 SDK 就说哪套** —— 原生 REST/Python/TS API、LangChain/deepagents，以及
  **原封不动的 E2B SDK** 直连。
- **全程你说了算** —— 零入站端口的反向隧道 worker、可选的权限网关 + 决策审计、
  数据自托管不出你的基础设施。

---

## 三根支柱

### 🌍 跑在你拥有的任何地方

一个二进制，把沙箱调度到你手上任何基础设施：

**AWS · GCP · Azure · 阿里云 · 腾讯云 · DigitalOcean · Hetzner · Oracle** ——
外加普通 **Docker**，或通过 `sandrpod-agent` 跑在**完全没有 Docker 的裸机**上。

阿里云和腾讯云让**中国区部署 / 数据驻留**成为一等公民 —— 这是托管服务给不了的。
每家云都有 [`docs/`](docs/) 下的开通指南（[AWS](docs/AWS_PROVISIONING.md)、
[GCP](docs/GCP_PROVISIONING.md)、[阿里云](docs/ALIYUN_PROVISIONING.md)、
[腾讯云](docs/TENCENT_PROVISIONING.md)…）。远程执行优先用各云的托管 run-command API
（AWS SSM、阿里云 CloudAssist、Azure Run Command、腾讯 TAT、Oracle Instance Agent），
没有的走一次性密钥 SSH（GCP、DigitalOcean、Hetzner）。

### 🔌 想说哪套 SDK 就说哪套

SandrPod 不绑定单一客户端。它同时提供原生 REST API 和 Python/TS SDK、一等的
**LangChain/deepagents** 后端，**以及**完整的 **E2B 线协议** —— 让*原封不动*的
`e2b` / `e2b-code-interpreter` SDK 只需两个环境变量就能打通。你已经在用的生态直接搬过来，
零迁移。

### 🛡️ 全程你说了算

- **零入站端口。** worker 主动**拨出**到控制平面，走 WebSocket 反向隧道 ——
  可以跑在 NAT 后、内网子网、甚至一台笔记本上。
- **治理，而不只是隔离。** 可选的员工 PC 模式加上每台机器的权限网关（路径同意、
  命令黑名单、PTY 同意）和决策审计管道（NDJSON + 中央 HTTP 上报）—— 把"跑 agent 代码"
  升级成"**管控** agent 在真实机器上能碰什么"。
- **数据始终归你。** 自托管、Apache 2.0、不回传。

---

## SandrPod vs. 托管沙箱服务

|  | 托管版（E2B、Modal…） | **SandrPod** |
|---|---|---|
| **许可证** | 闭源 | Apache 2.0，开源 |
| **运行在哪** | 它们的基础设施 / 区域 | 你的云、Docker 或裸机 |
| **支持的云** | 厂商托管 | AWS · GCP · Azure · **阿里云** · **腾讯云** · DO · Hetzner · Oracle |
| **中国区** | ✗ | ✓ 阿里云 + 腾讯云 |
| **无 Docker 模式** | —— | `sandrpod-agent`：任意机器即沙箱 |
| **SDK** | 它们自家的 | 原生 + LangChain + **E2B 直连** |
| **worker 入站端口** | 不适用 | **零**（反向隧道） |
| **治理 / 审计** | —— | 可选权限网关 + 决策审计 |
| **数据驻留** | 它们的区域 | 你的账号、你的区域 |

---

## E2B SDK 直连兼容

"想说哪套 SDK 就说哪套"的一个例子：已有跑在 E2B SDK 上的代码？把它指向你的 SandrPod
就能直接用 —— `Sandbox.create`、`files.*`、`commands.*`（前台/后台/PTY）、`run_code`、
`watch_dir`、`get_metrics`、`pause`/`resume` 全支持：

```python
import os
os.environ["E2B_API_KEY"]     = "e2b_your_key"          # 由你的 SandrPod 签发
os.environ["E2B_API_URL"]     = "https://sandbox.you.com"   # 控制面
os.environ["E2B_SANDBOX_URL"] = "https://sandbox.you.com"   # 每沙箱 envd

from e2b import Sandbox                # 真正的、未经修改的 e2b SDK
sbx = Sandbox.create()
sbx.files.write("/tmp/hi.txt", "hello from my own cloud")
print(sbx.commands.run("cat /tmp/hi.txt").stdout)   # → hello from my own cloud

from e2b_code_interpreter import Sandbox as CI
ci = CI.create()
print(ci.run_code("import numpy as np; np.arange(6).sum()").text)   # → 15
```

想要零配置直连（不用配环境变量，只用域名），把网关跑在泛域名后面即可 ——
`SANDRPOD_E2B_DOMAIN=sandbox.you.com` 配 `*.sandbox.you.com` 的 DNS + TLS。
完整接口面、线协议细节，以及对着**真实、未修改**的 E2B SDK + 真容器验证过的覆盖矩阵，
见 **[docs/E2B_COMPAT.md](docs/E2B_COMPAT.md)**。

---

## 架构

```
E2B SDK ──┐
原生 SDK  ├─→ API Server (控制平面, :8080)
LangChain ─┤         ↕ WebSocket + yamux 反向隧道
CLI ───────┘    Poder (Worker) ──→ Toolbox (Sandbox 容器)

                sandrpod-agent  ──→ (直连模式，任意机器即沙箱)
```

| 组件 | 说明 |
|------|------|
| **API Server** | 控制平面：原生 REST + E2B 兼容网关、沙箱 CRUD、调度、隧道代理 |
| **Poder** | 工作节点，主动建立 WebSocket 长隧道，管理 Docker 容器生命周期 |
| **sandrpod-agent** | 将本机直接注册为沙箱，内嵌 Toolbox，无需 Docker；支持 `--permission-mode` 权限网关和审计管道 |
| **sandrpod-tray** | 员工 PC 模式的用户会话 GUI 守护进程：托盘图标、原生同意弹窗、本地设置页 |
| **Toolbox** | 运行在沙箱容器内的执行服务，支持 PTY、文件操作、后台进程、会话 |

> **员工 PC 模式（可选）**：当 `sandrpod-agent` 运行在真实员工笔记本而非服务器上时，可开启每台机器的权限网关（路径同意 + 命令黑名单 + PTY 同意）和将所有 allow/deny/warn 决策推送至中央 HTTP 端点的审计管道。两层均默认关闭（`--permission-mode=off`），详见 **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**。

---

## 快速开始

### 1. 启动控制平面

```bash
# 内存模式（默认）
go run ./cmd/server -port 8080

# SQLite 持久化模式（推荐）
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db
```

### 2. 加一个 worker（Docker）

```bash
docker run -d --name sandrpod-poder --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:8080 \
  -e SANDRPOD_TOOLBOX_IMAGE=sandrpod/toolbox:latest \
  sandrpod/poder:latest
```

> 无需暴露任何入站端口 —— Poder 主动拨出到控制平面，走 WebSocket 反向隧道。

或者把**任意机器变成沙箱**，完全不用 Docker：

```bash
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-machine
```

### 3. 跑代码 —— SDK 随你挑

```python
# 原生
from langchain_sandrpod import SandrPodClient
sb = SandrPodClient(api_url="http://localhost:8080").get_sandbox("my-sandbox")
sb.execute("echo hello from SandrPod")

# …或原封不动的 E2B SDK（把 E2B_API_URL/E2B_SANDBOX_URL 设为你的服务器）
from e2b import Sandbox
print(Sandbox.create().commands.run("echo hello from SandrPod").stdout)
```

### 4. 管控它（员工 PC 模式）

```bash
sandrpod-tray serve                       # 用户会话托盘 + 同意弹窗（🛡）
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-laptop \
  -permission-mode=prompt \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch
```

权限模式：`off`（默认）| `prompt`（work_dir 外弹同意框）| `strict`（work_dir 外静默拒绝）。
完整架构、`permissions.json` 结构、tray CLI、审计协议见 **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**。

---

## LangChain / deepagents 集成

```bash
pip install langchain-sandrpod
```

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

model = ChatOpenAI(model="gpt-4o", temperature=0)
client = SandrPodClient(api_url="http://localhost:8080")

sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "写一个快速排序并运行"}]})

# 或用上下文管理器自动创建/删除
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})
```

backend 还直接暴露了更丰富的每沙箱能力：

```python
sb.run_code("x = 40", context="ctx1")          # 有状态 Jupyter 式内核
sb.run_code("x + 2", context="ctx1")["text"]   # → "42"（x 已保留）
sb.metrics()                                    # {cpu_count, cpu_used_pct, mem_*, disk_*}
with sb.watch_dir("/workspace") as w:           # 文件系统监视
    events = w.get_new_events()
```

更多示例见 [`pkg/sdk/python/langchain_sandrpod/examples/`](pkg/sdk/python/langchain_sandrpod/examples/)。

---

## CLI

```bash
pip install sandrpod-cli
sandrpod-cli set-api-url http://localhost:8080

# --provider: local | aws | gcp | azure | aliyun | tencent | digitalocean | hetzner | oracle
sandrpod-cli list
sandrpod-cli create my-sandbox --provider local --image sandrpod/toolbox:latest
sandrpod-cli create gpu-box --provider gcp --region asia-east1-a --instance-type e2-medium
sandrpod-cli execute my-sandbox "ls /workspace"     # 一次性（无状态）
sandrpod-cli stream my-sandbox "make build"         # 实时流式输出
sandrpod-cli run my-sandbox "z = 10" --context c1   # 有状态内核 —— z 在 context c1 内保留
sandrpod-cli stats my-sandbox                       # 实时 CPU / 内存 / 磁盘
sandrpod-cli fs watch my-sandbox /workspace         # 打印文件系统事件
sandrpod-cli shell my-sandbox                       # 交互式 PTY
sandrpod-cli delete my-sandbox

# Poder 管理
sandrpod-cli poder list
sandrpod-cli poder delete <poder-id>
```

---

## API 端点

SandrPod 提供**两套** HTTP 接口：自有的原生 REST API，以及（启用 E2B 网关后）
完整的 E2B 控制面 + `envd` 协议。

**原生 API**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/sandboxes` | 列出沙箱 |
| POST | `/api/v1/sandboxes` | 创建沙箱 |
| DELETE | `/api/v1/sandboxes/{name}` | 删除沙箱 |
| POST | `/api/v1/sandboxes/execute` | 执行代码 |
| GET | `/api/v1/sandboxes/{name}/toolbox/*` | 代理到 Toolbox（文件上传/下载等） |
| GET | `/api/v1/poders` | 列出 Poder 节点 |
| DELETE | `/api/v1/poders/{id}` | 删除 Poder 记录 |

**E2B 兼容 API** —— `POST /sandboxes`、`GET /sandboxes/{id}`、
`/sandboxes/{id}/{pause,resume,metrics,connect}`，以及 `envd` 的
Filesystem/Process connect-RPC 服务。详见 [docs/E2B_COMPAT.md](docs/E2B_COMPAT.md)。

---

## 构建

```bash
# 一键构建：全平台 + sandrpod-tray（缺少工具链时自动跳过）
make build-all

# 本地构建（agent/server 无需 CGO；tray 需要 CGO + 原生库）
go build -o server        ./cmd/server
go build -o poder         ./cmd/poder
go build -o agent         ./cmd/agent
go build -o sandrpod-tray ./cmd/sandrpod-tray   # 需要 CGO

# 跨平台编译（仅 agent + server，CGO=0）
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/server-linux-amd64 ./cmd/server
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-linux-amd64 ./cmd/agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o dist/sandrpod-agent-darwin-arm64 ./cmd/agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-windows-amd64.exe ./cmd/agent

# Docker 镜像（amd64）
docker buildx build --platform linux/amd64 -f docker/Dockerfile.poder   -t sandrpod/poder:latest   --load .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.toolbox -t sandrpod/toolbox:latest --load .
```

---

## License

Apache 2.0
