<p align="center">
  <img src="assets/logo.png" alt="SandrPod" width="400"/>
</p>

<p align="center">
  <strong>开源、可自托管的 E2B 替代方案 —— 自带云。</strong>
</p>

<p align="center">
  把<em>原封不动</em>的 E2B SDK 指向你自己的基础设施。在 AWS、GCP、Azure、
  阿里云、腾讯云等 8 家云上跑 Agent 代码沙箱 —— 数据不出你的云。
</p>

<p align="center">
  <a href="https://pypi.org/project/langchain-sandrpod/"><img src="https://img.shields.io/pypi/v/langchain-sandrpod?color=3B82F6&label=langchain-sandrpod" alt="PyPI"/></a>
  <img src="https://img.shields.io/badge/E2B%20SDK-drop--in%20compatible-8B5CF6" alt="E2B compatible"/>
  <img src="https://img.shields.io/badge/clouds-8%20providers-0EA5E9" alt="Multi-cloud"/>
  <img src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go" alt="Go"/>
  <img src="https://img.shields.io/badge/license-Apache%202.0-green" alt="License"/>
</p>

---

## 简介

**SandrPod** 是一个开源、可自托管的 AI Agent 沙箱平台 —— E2B 这类托管服务的直接替代。
Agent 按需创建隔离的代码执行环境、在其中执行代码、用完即销毁，而这一切**用的就是
你已经熟悉的 E2B SDK** —— 只不过指向的是你自己拥有的基础设施。

和托管沙箱服务相比，它有两点根本不同：

1. **E2B 线协议直连。** SandrPod 实现了 E2B 完整的线协议（控制面 + `envd`），
   原封不动的 `e2b` / `e2b-code-interpreter` SDK 只需配两个环境变量即可打通 ——
   无需迁移、无需改代码。
2. **自带云。** 一个二进制即可把沙箱调度到 **8 家云**（含**阿里云、腾讯云**），
   也可以跑在普通 Docker 上，甚至完全不用 Docker 直接把一台机器变成沙箱。
   Agent 的代码和数据始终留在你自己的基础设施里。

底层由中央 API Server 通过 WebSocket 反向隧道连接工作节点（**Poder**），每个沙箱内
运行 **Toolbox** 服务提供 Shell、文件 I/O、PTY 和持久会话。工作节点**无需暴露任何
入站端口** —— 所有流量走隧道。

---

## 为什么选 SandrPod

|  | 托管版 E2B | **SandrPod** |
|---|---|---|
| **许可证** | 闭源 | Apache 2.0，开源 |
| **运行在哪** | E2B 的基础设施 | 自托管，随处部署 |
| **支持的云** | E2B 托管 | AWS · GCP · Azure · **阿里云** · **腾讯云** · DigitalOcean · Hetzner · Oracle |
| **中国云** | ✗ | ✓ 阿里云 + 腾讯云 |
| **数据驻留** | E2B 的区域 | 你的账号、你的区域 |
| **SDK** | E2B SDK | **同一套 E2B SDK**（直连）+ 原生 Python/TS + LangChain |
| **无 Docker 模式** | —— | `sandrpod-agent`：任意机器即沙箱 |
| **员工机守护** | —— | 可选的权限网关 + 决策审计 |

---

## E2B SDK 直连兼容

已有跑在 E2B SDK 上的代码？把它指向你的 SandrPod 就能直接用 ——
`Sandbox.create`、`files.*`、`commands.*`（前台/后台/PTY）、`run_code`、
`watch_dir`、`get_metrics`、`pause`/`resume` 全部支持：

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

想要真正零配置的直连（不用配环境变量，只用域名），把网关跑在泛域名后面即可 ——
`SANDRPOD_E2B_DOMAIN=sandbox.you.com` 配上 `*.sandbox.you.com` 的 DNS + TLS。
完整的接口面、线协议细节，以及对着**真实、未修改**的 E2B SDK + 真容器验证过的
覆盖矩阵，见 **[docs/E2B_COMPAT.md](docs/E2B_COMPAT.md)**。

---

## 自带云

Provider 层（`pkg/provider/`）在以下云上开通承载沙箱的 VM：

**AWS · GCP · Azure · 阿里云 · 腾讯云 · DigitalOcean · Hetzner · Oracle**

每家都有 [`docs/`](docs/) 下的开通指南（如
[AWS](docs/AWS_PROVISIONING.md)、[GCP](docs/GCP_PROVISIONING.md)、
[阿里云](docs/ALIYUN_PROVISIONING.md)、[腾讯云](docs/TENCENT_PROVISIONING.md)）。
远程执行优先用各云的托管 run-command API（AWS SSM、阿里云 CloudAssist、
Azure Run Command、腾讯 TAT、Oracle Instance Agent），没有的就走一次性密钥的
SSH 通道（GCP、DigitalOcean、Hetzner）。想跑在自己的机器上？用普通 Docker，
或用 `sandrpod-agent` 在完全没有 Docker 的机器上直接开沙箱。

### 核心特性

- **E2B SDK 直连** —— 原封不动的 `e2b` / `e2b-code-interpreter` SDK，跑在你的基础设施上
- **8 云调度** —— 含阿里云、腾讯云；调度器自动挑负载最低的节点
- **可自托管、开源** —— Apache 2.0；代码和数据不出你的云
- **即开即用沙箱** —— 秒级启动隔离的 Docker 环境
- **完整沙箱能力** —— 文件、后台命令、流式输出、PTY、有状态代码解释器、目录监听、指标、pause/resume
- **直连 Agent 模式** —— 通过 `sandrpod-agent` 把任意机器变成沙箱，无需 Docker
- **反向隧道架构** —— 工作节点主动拨入，无需暴露入站端口
- **LangChain / deepagents 原生** —— `langchain-sandrpod` 给任意 Agent 完整文件系统 + Shell
- **持久会话** —— 多次调用之间保持 Shell 状态（工作目录、环境变量）
- **SQLite 持久化** —— 一个 `-db` 参数即可持久化，默认内存模式
- **员工 PC 模式（可选）** —— 路径同意权限网关 + NDJSON 决策审计

---

## 架构

```
E2B SDK ──┐
原生 SDK  ├─→ API Server (控制面, :8080)
CLI ──────┘        ↕ WebSocket + yamux 反向隧道
              Poder (Worker) ──→ Toolbox (Sandbox 容器)

              sandrpod-agent  ──→ (直连模式，本机即沙箱)
```

| 组件 | 说明 |
|------|------|
| **API Server** | REST 控制面 + E2B 兼容网关。沙箱 CRUD、任务调度、隧道代理 |
| **Poder** | 工作节点，主动建立 WebSocket 长隧道，管理 Docker 容器生命周期 |
| **sandrpod-agent** | 将本机直接注册为沙箱，内嵌 Toolbox，无需 Docker；支持 `--permission-mode` 权限网关和审计管道 |
| **sandrpod-tray** | 员工 PC 模式的用户会话 GUI 守护进程：托盘图标、原生同意弹窗、本地设置页 |
| **Toolbox** | 运行在沙箱容器内的代码执行服务，支持 PTY、文件操作、后台进程、会话 |

> **员工 PC 模式（可选）**：当 `sandrpod-agent` 运行在真实员工笔记本而非服务器上时，可开启路径同意 + 命令黑名单 + PTY 同意的权限网关，以及将所有 allow/deny/warn 决策推送至中央 HTTP 端点的审计管道。两层均默认关闭（`--permission-mode=off`），完整说明见 **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**。

---

## 快速开始

### 1. 启动 API Server

```bash
# 内存模式（默认）
go run ./cmd/server -port 8080

# SQLite 持久化模式（推荐）
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db
```

### 2. 启动 Poder（Docker 方式）

```bash
docker run -d --name sandrpod-poder --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:8080 \
  -e SANDRPOD_TOOLBOX_IMAGE=sandrpod/toolbox:latest \
  sandrpod/poder:latest
```

> Poder 不需要暴露任何外部端口，它主动向 API Server 建立 WebSocket 反向隧道。

### 3. 用 E2B SDK 直接调用

```bash
pip install e2b
```

```python
import os
os.environ.update(E2B_API_KEY="e2b_key", E2B_API_URL="http://localhost:8080",
                  E2B_SANDBOX_URL="http://localhost:8080", E2B_VALIDATE_API_KEY="false")
from e2b import Sandbox
sbx = Sandbox.create()
print(sbx.commands.run("echo hello from SandrPod").stdout)
```

如何启用网关（`SANDRPOD_E2B_DOMAIN` / 调试监听器）及完整兼容矩阵，见
**[docs/E2B_COMPAT.md](docs/E2B_COMPAT.md)**。

### 4. 单机直连模式（无需 Docker）

```bash
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-machine
```

### 5. 员工 PC 模式（权限网关 + 审计）

```bash
# 启动托盘伴侣（用户会话，菜单栏出现 🛡 图标）
sandrpod-tray serve

# 启动 agent，开启权限网关和审计上报
go run ./cmd/agent \
  -api-url=http://localhost:8080 \
  -name=my-laptop \
  -permission-mode=prompt \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch
```

权限模式：`off`（默认，仅系统路径黑名单）| `prompt`（work_dir 外路径弹同意框）| `strict`（work_dir 外静默拒绝，适合无头服务器）。

详见 **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**。

---

## LangChain / deepagents 集成

比起 E2B SDK 更想要原生 SDK？`langchain-sandrpod` 把沙箱直接接进 deepagents：

```bash
pip install langchain-sandrpod
```

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

model = ChatOpenAI(model="gpt-4o", temperature=0)
client = SandrPodClient(api_url="http://localhost:8080")

# 获取已有沙箱
sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "写一个快速排序并运行"}]})

# 或用上下文管理器自动创建/删除
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})
```

更多示例见 [`pkg/sdk/python/langchain_sandrpod/examples/`](pkg/sdk/python/langchain_sandrpod/examples/)。

---

## CLI

```bash
pip install sandrpod-cli
sandrpod-cli set-api-url http://localhost:8080

# 沙箱操作（--provider: local | aws | gcp | azure | aliyun | tencent | digitalocean | hetzner | oracle）
sandrpod-cli list
sandrpod-cli create my-sandbox --provider local --image sandrpod/toolbox:latest
sandrpod-cli create gpu-box --provider gcp --region asia-east1-a --instance-type e2-medium
sandrpod-cli execute my-sandbox "ls /workspace"
sandrpod-cli shell my-sandbox          # 交互式 PTY
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
