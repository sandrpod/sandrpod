# SandrPod

> AI 代码执行基础设施平台

## 项目简介

SandrPod 是一个面向 AI 时代的代码执行基础设施平台，提供极速、安全、可扩展的沙箱执行环境。

### 核心特性

- **90ms 极速创建** - 从代码到执行的超低延迟
- **无限水平扩展** - 支持多云、多区域、多集群
- **容器级安全** - 完美的隔离性和资源限制
- **API-First 设计** - 多语言 SDK，易于集成
- **开源核心** - Provider 适配层、Poder、SDK 完全开源

### 核心模块

```
SandrPod/
├── pkg/
│   ├── provider/           # 云厂商适配层 (AWS, Azure, GCP, 阿里云等)
│   ├── poder/              # Pod 执行器 (Docker, K8s)
│   ├── sandpod/            # SandPod 核心 (状态机, 生命周期)
│   └── registry.go         # 实例注册表
├── cmd/
│   └── sandrpod/           # 主程序入口
└── docs/
    └── design/              # 架构设计文档
```

## 快速开始

```bash
# 启动本地 SandrPod
go run ./cmd/sandrpod -port 8080 -region local -provider docker

# API 请求
curl http://localhost:8080/health
```

## 架构设计

- [架构设计](docs/design/architecture-v1.md)
- [SSH Gateway 架构](../docs/design/ssh-gateway-architecture.md) (参考 Daytona)

## License

- SandrPod Open: Apache 2.0
- SandrPod Cloud: 专有许可证


```
docker run -d --name sandrpod-poder \
    --network sandrpod \
    -p 8081:8081 \
    -v /Users/alice/.docker/run/docker.sock:/var/run/docker.sock \
    -e API_URL=http://host.docker.internal:8080 \
    -e PROXY_HOST=192.168.0.2 \
    -e SANDRPOD_TOOLBOX_IMAGE=sandrpod-toolbox:dev \
    -e REGION=local \
    -e PROVIDER_TYPE=local \
    sandrpod/poder:test

docker save -o sandrpodimage.tar sandrpod/poder:v0.1.0 sandrpod/server:v0.1.0 sandrpod-toolbox:dev

```