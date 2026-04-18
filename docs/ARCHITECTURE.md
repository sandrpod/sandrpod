# SandrPod 分布式架构

## 组件说明

### 1. API Server (Control Plane + Central Proxy)
- 部署: 一套，生产环境高可用
- 端口: 8080
- 职责:
  - 控制平面: CRUD 操作、Job 管理
  - Central Proxy: 代码执行路由（代理到 Worker Proxy）

### 2. Proxy+Agent (Worker Node)
- 部署: 每个 Worker 节点一个
- 端口: 8081 (映射到宿主机)
- 职责:
  - Agent: 轮询 API Server 执行 CREATE/DELETE sandbox 任务
  - Worker Proxy: 转发代码执行请求到 Toolbox

### 3. Toolbox (Sandbox)
- 部署: 每个 Sandbox 一个容器
- 端口: 8080 (容器内部)
- 职责: 代码执行服务 (Flask HTTP API)

## 网络架构

```
┌─────────────────────────────────────────────────────────────┐
│                     sandrpod 网络                            │
│                                                              │
│  ┌─────────────────────────────────────────────────────┐   │
│  │              API Server (8080)                       │   │
│  │  ┌─────────────────┐  ┌─────────────────────────┐   │   │
│  │  │  Control Plane  │  │   Central Proxy        │   │   │
│  │  │  - CRUD        │  │   - /execute (同步)     │   │   │
│  │  │  - Job 管理    │  │   - /stream (SSE)      │   │   │
│  │  └─────────────────┘  └───────────┬─────────────┘   │   │
│  └───────────────────────────────────┼─────────────────┘   │
│                                      │                       │
│                                      │ 查询 sandbox.ProxyURL  │
│                                      ▼                       │
│  ┌─────────────────────────────────────────────────────┐   │
│  │         Proxy+Agent (Worker Node)                   │   │
│  │  ┌───────────────┐  ┌─────────────────────────┐   │   │
│  │  │     Agent     │  │   Worker Proxy         │   │   │
│  │  │  - Poll Jobs  │  │   - /execute           │   │   │
│  │  │  - CREATE/    │  │   - /stream            │   │   │
│  │  │    DELETE     │  │   - /health            │   │   │
│  │  └───────┬───────┘  └───────────┬─────────────┘   │   │
│  └──────────┼──────────────────────┼─────────────────┘   │
│             │                      │                        │
└─────────────┼──────────────────────┼──────────────────────┘
              │                      │
              │ Job 结果含 ProxyURL   │ 转发请求到 Toolbox
              ▼                      ▼
         ┌─────────────────────┐
         │      Toolbox        │
         │    (容器内 :8080)    │
         └─────────────────────┘
```

## 工作流程

### 创建 Sandbox
1. 客户端 → API Server: POST /api/v1/sandboxes
2. API Server: 创建 Job (CREATE_SANDBOX)
3. Proxy+Agent: 轮询获取 Job
4. Proxy+Agent → Docker: 创建容器
5. Proxy+Agent → API Server: 更新 Job 状态 (含 IP, ProxyURL)
6. API Server: 更新 Sandbox 信息

### 执行代码 (同步)
1. 客户端 → API Server: POST /api/v1/sandboxes/execute
2. API Server: 查询 Sandbox 获取 ProxyURL
3. API Server → Worker Proxy: 转发请求
4. Worker Proxy → Toolbox: 转发请求
5. Toolbox: 执行代码
6. 响应沿原路返回客户端

### 流式输出
1. 客户端 → API Server: GET /api/v1/sandboxes/stream
2. API Server → Worker Proxy: 转发请求 (SSE)
3. Worker Proxy → Toolbox: 转发请求
4. Toolbox: 流式返回输出

## 端口映射

| 服务 | 容器端口 | 宿主机端口 | 说明 |
|------|----------|-----------|------|
| API Server | 8080 | 8080 | 控制平面 + Central Proxy |
| Proxy+Agent | 8081 | 8081 | Worker Proxy |
| Toolbox | 8080 | 18080 | 代码执行 (测试用) |

## 环境变量

### API Server
- `PORT`: 监听端口 (默认 8080)
- `TOKEN`: 认证 token (可选)

### Proxy+Agent
- `API_URL`: API Server 地址 (默认 http://api:8080)
- `REGION`: 区域 (默认 local)
- `PORT`: Proxy 端口 (默认 8081)
- `PROXY_HOST`: 外部可访问的 host 地址 (必需，用于返回 ProxyURL)
- `POLL_INTERVAL`: 轮询间隔 (默认 5s)

## 部署命令

### 1. 创建网络
```bash
docker network create sandrpod
```

### 2. 启动 API Server
```bash
# 直接运行
go run ./cmd/api

# 或 Docker
docker run -d --name sandrpod-api \
  --network sandrpod \
  -p 8080:8080 \
  sandrpod-api:prod
```

### 3. 启动 Proxy+Agent
```bash
# 直接运行 (需要 Docker socket)
docker run -d --name sandrpod-proxy-agent \
  --network sandrpod \
  -p 8081:8081 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://api:8080 \
  -e PROXY_HOST=192.168.0.2 \
  -e REGION=local \
  sandrpod-proxy-agent:dev

# 或 Docker Compose
docker-compose -f docker/docker-compose.proxy-agent.yml up -d
```

## 兼容 Daytona SDK

API Server 端点兼容 Daytona SDK 的请求格式:

### 创建 Sandbox
```bash
curl -X POST http://192.168.0.2:8080/api/v1/sandboxes \
  -H "Content-Type: application/json" \
  -d '{
    "name": "test-sandbox",
    "region": "local",
    "instance_type": "standard"
  }'
```

### 执行代码
```bash
curl -X POST http://192.168.0.2:8080/api/v1/sandboxes/execute?sandbox=test-sandbox \
  -H "Content-Type: application/json" \
  -d '{
    "language": "bash",
    "code": "echo hello && whoami"
  }'
```

### 流式输出
```bash
curl -N "http://192.168.0.2:8080/api/v1/sandboxes/stream?sandbox=test-sandbox&language=bash&code=echo%20hello"
```