# SandrPod

> AI 代码执行基础设施平台

## 项目简介

SandrPod 是一个面向 AI 时代的代码执行基础设施平台，提供极速、安全、可扩展的沙箱执行环境。

### 核心特性

- **90ms 极速创建** - 从代码到执行的超低延迟
- **无限水平扩展** - 支持多云、多区域、多集群
- **容器级安全** - 完美的隔离性和资源限制
- **API-First 设计** - 多语言 SDK，易于集成
- **反向隧道架构** - Poder 主动拨入 API Server，无需暴露外部端口

### 架构概览

```
Client → API Server (Control Plane)
              ↕ WebSocket + yamux 反向隧道
         Poder (Worker)  ←── 主动拨入，无需外部端口
              ↓
         Toolbox (Sandbox 容器)
```

- **API Server** (`cmd/server`)：REST API 控制面，处理沙箱 CRUD、任务调度，通过 yamux 隧道代理请求到 Poder
- **Poder** (`cmd/poder`)：工作节点，主动建立 WebSocket 长连接注册到 API Server；从 API Server 轮询创建/删除任务；管理 Docker 容器生命周期
- **Toolbox** (`cmd/toolbox`)：运行在每个沙箱容器内的代码执行服务，提供 HTTP API 支持代码执行、PTY、文件操作

### 核心模块

```
sandrpod/
├── cmd/
│   ├── server/             # API Server (控制面)
│   ├── poder/              # Poder (工作节点)
│   └── toolbox/            # Toolbox (沙箱内执行器)
└── pkg/
    ├── provider/           # 云厂商适配层 (AWS, 阿里云等)
    ├── poder/              # Pod 执行器实现 (Docker)
    ├── sandpod/            # 沙箱状态类型定义
    ├── toolbox/            # 代码执行引擎 (PTY, 文件操作)
    ├── tunnel/             # WebSocket + yamux 反向隧道
    └── sdk/python/         # Python SDK & CLI
```

## 快速开始

### 1. 构建

```bash
go build -o server ./cmd/server
go build -o poder ./cmd/poder
go build -o toolbox ./cmd/toolbox

# 构建 Docker 镜像
docker build -f docker/Dockerfile.poder   -t sandrpod/poder:dev .
docker build -f docker/Dockerfile.toolbox -t sandrpod/toolbox:dev .
```

### 2. 启动 API Server

```bash
go run ./cmd/server -port 8080
```

### 3. 启动 Poder（Docker 镜像，推荐）

Poder 需要挂载 Docker socket 以管理沙箱容器，并接入 `sandrpod` 网络以访问 toolbox。

```bash
# 创建 sandrpod 网络（首次）
docker network create sandrpod

# 启动 Poder 容器
docker run -d --name sandrpod-poder \
    --network sandrpod \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -e API_URL=http://host.docker.internal:8080 \
    -e REGION=local \
    -e PROVIDER_TYPE=local \
    sandrpod/poder:dev
```

> **注意**：Poder 不需要暴露任何外部端口，它主动向 API Server 建立 WebSocket 反向隧道。

### 4. 本地开发（直接运行）

```bash
# macOS，使用 Docker Desktop socket
go run ./cmd/poder \
    -api-url=http://localhost:8080 \
    -region=local \
    -docker-host=unix://$HOME/.docker/run/docker.sock
```

### 5. 创建沙箱并执行代码

```bash
# 创建沙箱
curl -X POST http://localhost:8080/api/v1/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"name":"my-sandbox","provider_type":"local","image":"sandrpod/toolbox:dev"}'

# 执行代码
curl -X POST "http://localhost:8080/api/v1/sandboxes/execute?sandbox=my-sandbox" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"print(\"hello world\")"}'
```

### 6. 使用 CLI

```bash
# 安装 CLI（开发模式）
pip install -e pkg/sdk/python

# 配置 API 地址
sandrpod-cli set-api-url http://localhost:8080

# 常用命令
sandrpod-cli list
sandrpod-cli create my-sandbox --provider local --image sandrpod/toolbox:dev
sandrpod-cli execute my-sandbox 'print("hello")' -l python
sandrpod-cli stop my-sandbox
sandrpod-cli start my-sandbox
sandrpod-cli delete my-sandbox
```

## API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/sandboxes` | 创建沙箱 |
| GET  | `/api/v1/sandboxes` | 列出沙箱 |
| GET  | `/api/v1/sandboxes/{name}` | 获取沙箱详情 |
| POST | `/api/v1/sandboxes/{name}/start` | 启动沙箱 |
| POST | `/api/v1/sandboxes/{name}/stop` | 停止沙箱 |
| DELETE | `/api/v1/sandboxes/{name}` | 删除沙箱 |
| POST | `/api/v1/sandboxes/execute` | 执行代码 |
| GET  | `/api/v1/sandboxes/stream` | 流式执行 |
| GET  | `/api/v1/poders` | 列出 Poder 节点 |
| GET  | `/ws/poder/connect` | Poder 通过 WebSocket 注册（隧道入口） |
| GET  | `/api/v1/jobs/poll` | Poder 轮询待处理任务 |

## 网络拓扑

| 服务 | 容器端口 | 宿主机端口 | 说明 |
|------|---------|-----------|------|
| API Server | 8080 | 8080 | 控制面 REST API |
| Poder | — | — | 无外部端口，主动拨入 API Server |
| Toolbox | 8080 | — | 仅在 sandrpod 网络内访问 |

## 沙箱状态机

```
PENDING → STARTING → RUNNING → STOPPING → STOPPED
                   ↘                    ↗
                    ERROR          TERMINATED
```

## License

- SandrPod Open: Apache 2.0
- SandrPod Cloud: 专有许可证
