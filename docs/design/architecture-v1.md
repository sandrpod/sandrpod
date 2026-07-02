# SandrPod Technical Architecture Design v1.0

> **Note:** This document is written in Chinese. It is an internal design note from the early v1.0 planning phase (2024-03) and describes the **intended architecture blueprint**. Some sections have not been implemented or were revised during development.  
> For the current working architecture, see [`../ARCHITECTURE.md`](../ARCHITECTURE.md).  
> For the horizontal-scaling (multi-instance + PostgreSQL) design, see [`horizontal-scaling.md`](horizontal-scaling.md).

---

# SandrPod 技术架构设计 v1.0

> ⚠️ **历史文档**：本文档为 v1.0 初期设计规划（2024-03），描述的是**目标架构蓝图**，部分内容尚未实现或已在实现过程中调整。  
> 当前实际运行架构请参阅 [`../ARCHITECTURE.md`](../ARCHITECTURE.md)。  
> 横向扩展（多实例 + PostgreSQL）方案请参阅 [`horizontal-scaling.md`](horizontal-scaling.md)。

---

### 与当前实现的主要差异

| 设计文档描述 | 实际实现 | 状态 |
|-------------|---------|------|
| Proxy+Agent 暴露 `:8081` HTTP 端口 | Poder 通过 WebSocket 反向隧道连接，**不暴露任何端口** | ✅ 已实现（方式变更） |
| Toolbox 使用 Flask/Python | Toolbox 使用 Go 重写 | ✅ 已实现（语言变更） |
| `internal/` 多租户、计费、租户服务 | 尚未实现 | 🔲 未实现 |
| `internal/console/` Web 控制台 | 尚未实现 | 🔲 未实现 |
| `cmd/daemon/` SandPod Daemon | 已合并进 Toolbox | ✅ 已实现（合并） |
| `cmd/proxy/` 独立 Proxy 进程 | 已合并进 API Server 的隧道代理层 | ✅ 已实现（合并） |
| `SnapshotService`、`VolumeService` | 尚未实现 | 🔲 未实现 |
| JWT Auth + Rate Limit + Tenant Quota | 仅实现 Bearer Token 鉴权 | 🔲 部分实现 |
| 存储层（无描述） | 新增 Repository 模式 + SQLite 持久化 | ✅ 已实现（新增） |
| `sandrpod-agent` 单机直连 | 不在 v1 设计中 | ✅ 已实现（新增） |

---

> SandrPod - AI 代码执行基础设施平台

## 1. 产品定位

### 1.1 核心价值

- **极速沙箱** - 90ms 内创建可执行环境
- **无限扩展** - 支持多云、多区域、多集群
- **安全隔离** - 容器级安全，支持 GPU
- **即开即用** - API-first 设计，多语言 SDK

### 1.2 目标用户

- AI Agent 开发者
- 代码执行平台
- 在线编程教育
- 自动化测试服务
- 沙箱化部署平台

### 1.3 产品分层

```
┌─────────────────────────────────────────────────────────────┐
│                     SandrPod Cloud (闭源)                      │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │ 控制台 (Web) │  │  计费系统   │  │   多租户管理        │ │
│  └─────────────┘  └─────────────┘  └─────────────────────┘ │
├─────────────────────────────────────────────────────────────┤
│                     SandrPod Open (开源)                      │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │  API Server │  │   Poder    │  │   Provider 适配层    │ │
│  └─────────────┘  └─────────────┘  └─────────────────────┘ │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │   Proxy     │  │  SDK (Go/   │  │   SandPod Daemon    │ │
│  │             │  │  Python/TS) │  │                     │ │
│  └─────────────┘  └─────────────┘  └─────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
```

## 2. 系统架构

### 2.1 整体架构图

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              客户端层                                        │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────────┐ │
│  │ Python   │  │   Go     │  │ TypeScript│  │   CLI    │  │   Web Console │ │
│  │   SDK    │  │   SDK    │  │    SDK    │  │          │  │              │ │
│  └──────────┘  └──────────┘  └──────────┘  └──────────┘  └──────────────┘ │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                             SandrPod API (闭源)                               │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                         API Gateway                                   │   │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌──────────┐ │   │
│  │  │ Auth (JWT)  │  │ Rate Limit  │  │   Logger    │  │  Router   │ │   │
│  │  └─────────────┘  └─────────────┘  └─────────────┘  └──────────┘ │   │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌──────────┐ │   │
│  │  │  Tenant     │  │   Quota     │  │    Billing  │  │   Audit   │ │   │
│  │  └─────────────┘  └─────────────┘  └─────────────┘  └──────────┘ │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                      │                                      │
│  ┌──────────────────────────────────┴──────────────────────────────┐       │
│  │                        Core Services                              │       │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐           │       │
│  │  │  SandPod    │  │  Snapshot   │  │   Region    │           │       │
│  │  │  Service   │  │  Service   │  │   Service   │           │       │
│  │  └─────────────┘  └─────────────┘  └─────────────┘           │       │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐           │       │
│  │  │  Poder    │  │   Job      │  │   Volume   │           │       │
│  │  │  Service   │  │  Service   │  │   Service   │           │       │
│  │  └─────────────┘  └─────────────┘  └─────────────┘           │       │
│  └────────────────────────────────────────────────────────────────┘       │
│                                      │                                      │
└──────────────────────────────────────┼──────────────────────────────────────┘
                                       │
                    ┌─────────────────┼─────────────────┐
                    │                 │                 │
                    ▼                 ▼                 ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                          SandrPod Open (开源)                                   │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                        Provider Layer                                 │   │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐│   │
│  │  │    AWS     │  │   Azure    │  │    GCP     │  │   Aliyun   ││   │
│  │  │  Provider  │  │  Provider  │  │  Provider  │  │  Provider  ││   │
│  │  └─────────────┘  └─────────────┘  └─────────────┘  └─────────────┘│   │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                  │   │
│  │  │  On-Prem   │  │   K8s      │  │  Custom    │                  │   │
│  │  │  Provider  │  │  Provider  │  │  Provider  │                  │   │
│  │  └─────────────┘  └─────────────┘  └─────────────┘                  │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                        Poder Cluster                                │   │
│  │                                                                      │   │
│  │   ┌────────┐   ┌────────┐   ┌────────┐   ┌────────┐               │   │
│  │   │Poder 1│   │Poder 2│   │Poder 3│   │Poder N│               │   │
│  │   │ ┌────┐│   │ ┌────┐│   │ ┌────┐│   │ ┌────┐│               │   │
│  │   │ │Sand││   │ │Sand││   │ │Sand││   │ │Sand││               │   │
│  │   │ │box ││   │ │box ││   │ │box ││   │ │box ││               │   │
│  │   │ └────┘│   │ └────┘│   │ └────┘│   │ └────┘│               │   │
│  │   └────────┘   └────────┘   └────────┘   └────────┘               │   │
│  │                                                                      │   │
│  │   每个 Poder 是独立 Docker Host，可水平扩展                           │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 2.2 目录结构

```
SandrPod/
├── cmd/                          # 命令行入口
│   ├── SandrPod/                   # 主程序
│   │   └── main.go
│   ├── poder/                   # Poder 进程
│   │   └── main.go
│   ├── proxy/                    # Proxy 进程
│   │   └── main.go
│   └── daemon/                   # SandPod 内 Daemon
│       └── main.go
│
├── internal/                     # 内部业务逻辑 (闭源)
│   ├── api/                      # SandrPod API 服务
│   │   ├── gateway/             # API 网关
│   │   ├── services/             # 业务服务
│   │   │   ├── SandPod/         # 沙箱服务
│   │   │   ├── billing/         # 计费服务
│   │   │   └── tenant/          # 租户服务
│   │   └── handlers/            # HTTP 处理
│   │
│   └── console/                 # 控制台 Web
│       ├── pages/
│       └── components/
│
├── apis/                        # API 定义 (开源)
│   ├── SandrPod/                  # 主 API
│   │   └── v1/
│   │       ├── openapi.yaml
│   │       └── generated/
│   └── toolbox/                 # Toolbox API
│       └── v1/
│
├── pkg/                         # 共享包 (开源)
│   ├── provider/                # Provider 抽象层
│   │   ├── interface.go        # Provider 接口定义
│   │   ├── factory.go          # 工厂模式实现
│   │   ├── aws/
│   │   ├── azure/
│   │   ├── gcp/
│   │   ├── aliyun/
│   │   └── onprem/
│   │
│   ├── poder/                  # Poder 核心
│   │   ├── interface.go        # Poder 接口定义
│   │   ├── base.go             # 基础抽象实现
│   │   ├── docker.go           # Docker 实现
│   │   └── manager/
│   │
│   ├── sandpod/                  # SandPod 核心
│   │   ├── interface.go        # SandPod 接口定义
│   │   ├── state_machine.go    # 状态机实现
│   │   └── sandpod.go          # 基础 SandPod 实现
│   │
│   ├── toolbox/                  # Toolbox API (代码执行服务)
│   │   ├── api.go             # HTTP API 服务
│   │   ├── executor.go        # 代码执行器
│   │   └── sandbox.go         # 沙箱隔离
│   │
│   ├── registry.go              # SandPod 注册表
│   │
│   ├── sdk/                     # 多语言 SDK
│   │   ├── go/
│   │   ├── python/
│   │   └── typescript/
│   │
│   └── common/                  # 公共工具
│       ├── config/
│       ├── logger/
│       └── errors/
│
├── docs/
│   └── design/
│       └── architecture-v1.md
│
├── docker/                      # Docker 配置
│   ├── Dockerfile.poder
│   ├── Dockerfile.daemon
│   └── docker-compose.yaml
│
├── scripts/                     # 部署脚本
│   ├── install.sh
│   └── upgrade.sh
│
├── go.mod
├── go.sum
└── README.md
```

## 3. 核心数据模型

### 3.1 Entity Relationship

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Entity 关系图                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  Tenant (租户)                                                              │
│  ├── id: UUID                                                              │
│  ├── name: string                                                          │
│  ├── plan: FREE | PRO | ENTERPRISE                                         │
│  ├── quota: TenantQuota                                                   │
│  └── created_at: timestamp                                                  │
│          │                                                                 │
│          │ 1:N                                                            │
│          ▼                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                           Region (区域)                               │   │
│  │  ├── id: UUID                                                        │   │
│  │  ├── tenant_id: FK → Tenant                                          │   │
│  │  ├── provider_id: FK → Provider (nullable, null=公共区域)            │   │
│  │  ├── name: string                                                    │   │
│  │  ├── location: string                                                │   │
│  │  └── status: ACTIVE | INACTIVE                                       │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│          │                                                                 │
│          │ 1:N                                                            │
│          ▼                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                          Poder (计算节点)                            │   │
│  │  ├── id: UUID                                                        │   │
│  │  ├── region_id: FK → Region                                          │   │
│  │  ├── name: string                                                    │   │
│  │  ├── api_url: string                                                │   │
│  │  ├── api_key: string (encrypted)                                     │   │
│  │  ├── capacity: Resources {cpu, memory, disk, gpu}                    │   │
│  │  ├── current_usage: Resources                                        │   │
│  │  ├── status: READY | DRAINING | OFFLINE                              │   │
│  │  └── last_heartbeat: timestamp                                       │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│          │                                                                 │
│          │ 1:N                                                            │
│          ▼                                                                 │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                         SandPod (沙箱实例)                           │   │
│  │  ├── id: UUID                                                        │   │
│  │  ├── tenant_id: FK → Tenant                                          │   │
│  │  ├── region_id: FK → Region                                          │   │
│  │  ├── poder_id: FK → Poder                                          │   │
│  │  ├── name: string                                                    │   │
│  │  ├── class: SMALL | MEDIUM | LARGE | CUSTOM                          │   │
│  │  ├── state: CREATING | STARTING | STARTED | STOPPING | STOPPED ...  │   │
│  │  ├── desired_state: STARTED | STOPPED | DESTROYED                    │   │
│  │  ├── resources: SandPodResources                                      │   │
│  │  ├── snapshot_id: FK → Snapshot                                      │   │
│  │  ├── volumes: []Volume                                              │   │
│  │  ├── auto_stop_minutes: int                                         │   │
│  │  ├── created_at: timestamp                                           │   │
│  │  └── destroyed_at: timestamp (nullable)                               │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 数据结构定义

```go
// pkg/SandPod/entity.go

type Tenant struct {
    ID        string     `json:"id" db:"id"`
    Name      string     `json:"name" db:"name"`
    Plan      Plan       `json:"plan" db:"plan"`
    Quota     TenantQuota `json:"quota" db:"quota"`
    CreatedAt time.Time  `json:"created_at" db:"created_at"`
}

type TenantQuota struct {
    MaxSandPodes    int     `json:"max_SandPodes"`
    MaxCPUPerSandPod int    `json:"max_cpu_per_SandPod"`
    MaxMemoryGB     int     `json:"max_memory_gb"`
    MaxDiskGB       int     `json:"max_disk_gb"`
    MaxSnapshots    int     `json:"max_snapshots"`
}

type Region struct {
    ID           string   `json:"id" db:"id"`
    TenantID     *string `json:"tenant_id,omitempty" db:"tenant_id"`
    ProviderID   *string `json:"provider_id,omitempty" db:"provider_id"`
    Name         string  `json:"name" db:"name"`
    DisplayName  string  `json:"display_name" db:"display_name"`
    Location     string  `json:"location" db:"location"`
    IsPublic     bool    `json:"is_public" db:"is_public"`
    Status       string  `json:"status" db:"status"`
}

type Poder struct {
    ID              string    `json:"id" db:"id"`
    RegionID        string    `json:"region_id" db:"region_id"`
    Name            string    `json:"name" db:"name"`
    APIURL         string    `json:"api_url" db:"api_url"`
    APIKey         string    `json:"-" db:"api_key"` // encrypted
    CPU            float64   `json:"cpu" db:"cpu"`
    MemoryGB       float64   `json:"memory_gb" db:"memory_gb"`
    DiskGB         float64   `json:"disk_gb" db:"disk_gb"`
    GPU            int       `json:"gpu" db:"gpu"`
    GPUType        string    `json:"gpu_type,omitempty" db:"gpu_type"`
    Status         string    `json:"status" db:"status"`
    AvailabilityScore float64 `json:"availability_score" db:"availability_score"`
    LastHeartbeat  time.Time `json:"last_heartbeat" db:"last_heartbeat"`
}

type SandPod struct {
    ID              string           `json:"id" db:"id"`
    TenantID        string          `json:"tenant_id" db:"tenant_id"`
    RegionID        string          `json:"region_id" db:"region_id"`
    PoderID        string          `json:"poder_id" db:"poder_id"`
    Name            string          `json:"name" db:"name"`
    Class           SandPodClass    `json:"class" db:"class"`
    State           SandPodState    `json:"state" db:"state"`
    DesiredState    DesiredState    `json:"desired_state" db:"desired_state"`
    CPU             int            `json:"cpu" db:"cpu"`
    MemoryGB        int            `json:"memory_gb" db:"memory_gb"`
    DiskGB          int            `json:"disk_gb" db:"disk_gb"`
    GPU             int            `json:"gpu" db:"gpu"`
    SnapshotID      string         `json:"snapshot_id" db:"snapshot_id"`
    AutoStopMinutes int            `json:"auto_stop_minutes" db:"auto_stop_minutes"`
    Labels          map[string]string `json:"labels" db:"labels"`
    Env             map[string]string `json:"env,omitempty" db:"env"`
    CreatedAt       time.Time      `json:"created_at" db:"created_at"`
    StoppedAt       *time.Time     `json:"stopped_at,omitempty" db:"stopped_at"`
    DestroyedAt      *time.Time     `json:"destroyed_at,omitempty" db:"destroyed_at"`
}

type SandPodState string

const (
    SandPodStateCreating   SandPodState = "CREATING"
    SandPodStateStarting  SandPodState = "STARTING"
    SandPodStateStarted   SandPodState = "STARTED"
    SandPodStateStopping  SandPodState = "STOPPING"
    SandPodStateStopped   SandPodState = "STOPPED"
    SandPodStateDestroying SandPodState = "DESTROYING"
    SandPodStateDestroyed SandPodState = "DESTROYED"
    SandPodStateError     SandPodState = "ERROR"
    SandPodStateResizing  SandPodState = "RESIZING"
)

type DesiredState string

const (
    DesiredStateStarted   DesiredState = "STARTED"
    DesiredStateStopped   DesiredState = "STOPPED"
    DesiredStateDestroyed DesiredState = "DESTROYED"
)
```

## 4. Provider 抽象层

### 4.1 接口定义

```go
// pkg/provider/interface.go

type Provider interface {
    // 元信息
    Name() string
    DisplayName() string

    // VM 生命周期
    CreateVM(ctx context.Context, req *CreateVMRequest) (*VMInfo, error)
    DeleteVM(ctx context.Context, vmID string) error
    GetVM(ctx context.Context, vmID string) (*VMInfo, error)
    ListVMs(ctx context.Context) ([]*VMInfo, error)

    // 远程执行
    ExecuteCommand(ctx context.Context, vmID, command string) (*CommandResult, error)

    // 健康检查
    WaitUntilRunning(ctx context.Context, vmID string, timeout time.Duration) error
    GetHealthStatus(ctx context.Context, vmID string) (*HealthStatus, error)

    // 基础设施查询
    ListRegions(ctx context.Context) ([]string, error)
    ListInstanceTypes(ctx context.Context, region string) ([]*InstanceType, error)
    GetDefaultImage(ctx context.Context, region string) (string, error)

    // 清理
    Cleanup(ctx context.Context) error
}

type CreateVMRequest struct {
    Name          string
    Region        string
    InstanceType  string
    ImageID       string
    NetworkConfig *NetworkConfig
    DiskConfig    *DiskConfig
    PoderConfig  *PoderBootstrapConfig
    Tags          map[string]string
}

type PoderBootstrapConfig struct {
    APIURL       string // SandrPod API 地址
    APIKey       string // Poder 认证密钥
    PoderVersion string
    LogLevel     string
}

type VMInfo struct {
    ID           string
    Name         string
    Region       string
    InstanceType string
    State        VMState
    PublicIP     string
    PrivateIP    string
    CreatedAt    time.Time
}

type HealthStatus struct {
    VMReady      bool
    DockerReady  bool
    PoderReady  bool
    APIReachable bool
}
```

### 4.2 Provider 工厂

```go
// pkg/provider/factory.go

type Factory struct {
    providers map[string]Provider
    mu        sync.RWMutex
}

func NewFactory() *Factory {
    return &Factory{providers: make(map[string]Provider)}
}

func (f *Factory) Register(p Provider) error {
    f.mu.Lock()
    defer f.mu.Unlock()

    if _, exists := f.providers[p.Name()]; exists {
        return fmt.Errorf("provider %s already registered", p.Name())
    }
    f.providers[p.Name()] = p
    return nil
}

func (f *Factory) Get(name string) (Provider, error) {
    f.mu.RLock()
    defer f.mu.RUnlock()

    p, ok := f.providers[name]
    if !ok {
        return nil, fmt.Errorf("provider %s not found", name)
    }
    return p, nil
}

func (f *Factory) List() []Provider {
    f.mu.RLock()
    defer f.mu.RUnlock()

    providers := make([]Provider, 0, len(f.providers))
    for _, p := range f.providers {
        providers = append(providers, p)
    }
    return providers
}

// 使用示例
func init() {
    factory := provider.NewFactory()

    // 注册 Provider
    factory.Register(aws.NewProvider(cfg.AWS))
    factory.Register(azure.NewProvider(cfg.Azure))
    factory.Register(aliyun.NewProvider(cfg.Aliyun))
    factory.Register(onprem.NewProvider(cfg.OnPrem))
}
```

## 5. API 设计

### 5.1 REST API 端点

```
基础路径: /api/v1

┌─────────────────────────────────────────────────────────────────────────────┐
│                              SandPod API                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  POST   /SandPodes                    创建沙箱                              │
│  GET    /SandPodes                    列出沙箱 (分页)                       │
│  GET    /SandPodes/:id                获取沙箱详情                          │
│  DELETE /SandPodes/:id                销毁沙箱                              │
│  POST   /SandPodes/:id/start          启动沙箱                              │
│  POST   /SandPodes/:id/stop           停止沙箱                              │
│  POST   /SandPodes/:id/resize         调整资源                              │
│  GET    /SandPodes/:id/usage          获取资源使用                          │
│                                                                             │
│  POST   /SandPodes/:id/toolbox-proxy-url  获取 Toolbox 代理 URL            │
│  GET    /SandPodes/:id/status         获取沙箱状态                          │
│                                                                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                              Region API                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  GET    /regions                      列出区域                              │
│  GET    /regions/:id                   获取区域详情                          │
│  GET    /regions/:id/poders           列出区域下的 Poder                  │
│                                                                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                              Poder API                                     │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  GET    /poders                      列出所有 Poder                      │
│  GET    /poders/:id                   获取 Poder 详情                     │
│  POST   /poders/:id/drain             排空 Poder                          │
│  POST   /poders/:id/activate          激活 Poder                          │
│                                                                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                              Snapshot API                                   │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  GET    /snapshots                     列出快照                              │
│  POST   /snapshots                     创建快照                            │
│  GET    /snapshots/:id                 获取快照详情                          │
│  DELETE /snapshots/:id                 删除快照                            │
│                                                                             │
├─────────────────────────────────────────────────────────────────────────────┤
│                              Billing API (闭源)                              │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  GET    /billing/usage                 获取使用量                          │
│  GET    /billing/invoices              获取账单                            │
│  GET    /billing/quota                  获取配额                            │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 5.2 API 请求/响应示例

```yaml
# POST /api/v1/SandPodes
# 创建沙箱请求

{
    "name": "my-ai-agent",
    "region": "cn-beijing",
    "class": "SMALL",           # cpu: 2, memory: 4, disk: 20
    "snapshot": "ubuntu-22.04",
    "gpu": 0,
    "auto_stop_minutes": 15,
    "labels": {
        "env": "production",
        "team": "ai-platform"
    },
    "env": {
        "OPENAI_API_KEY": "sk-xxx"
    }
}

# 响应

{
    "id": "sb-xxxxxxxx-xxxx",
    "name": "my-ai-agent",
    "region": "cn-beijing",
    "class": "SMALL",
    "state": "CREATING",
    "resources": {
        "cpu": 2,
        "memory_gb": 4,
        "disk_gb": 20,
        "gpu": 0
    },
    "snapshot": "ubuntu-22.04",
    "auto_stop_minutes": 15,
    "labels": {
        "env": "production",
        "team": "ai-platform"
    },
    "toolbox_proxy_url": "https://proxy.SandrPod.cn/toolbox/sb-xxxx",
    "created_at": "2024-01-15T10:30:00Z"
}
```

## 6. Poder 设计

### 6.1 Poder 架构

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Poder Process                                  │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  HTTP API Server (:3000)                                                   │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                         Routes                                      │   │
│  │  POST /SandPodes/create    - 创建沙箱                                │   │
│  │  POST /SandPodes/:id/start - 启动沙箱                                │   │
│  │  POST /SandPodes/:id/stop  - 停止沙箱                                │   │
│  │  POST /SandPodes/:id/destroy- 销毁沙箱                                │   │
│  │  GET  /SandPodes/:id/info  - 沙箱信息                                │   │
│  │  GET  /metrics             - 监控指标                                │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                      │                                      │
│  ┌──────────────────────────────────┴──────────────────────────────┐       │
│  │                        Services                                  │       │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐           │       │
│  │  │  SandPod    │  │   Health    │  │   Metrics   │           │       │
│  │  │  Manager    │  │   Checker   │  │   Collector  │           │       │
│  │  └─────────────┘  └─────────────┘  └─────────────┘           │       │
│  └────────────────────────────────────────────────────────────────┘       │
│                                      │                                      │
│  ┌──────────────────────────────────┴──────────────────────────────┐       │
│  │                       Docker Client                              │       │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐           │       │
│  │  │   Create    │  │    Start    │  │    Stop     │           │       │
│  │  └─────────────┘  └─────────────┘  └─────────────┘           │       │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐           │       │
│  │  │   Destroy   │  │   Inspect   │  │   Exec      │           │       │
│  │  └─────────────┘  └─────────────┘  └─────────────┘           │       │
│  └────────────────────────────────────────────────────────────────┘       │
│                                      │                                      │
│                                      ▼                                      │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                         Docker Engine                                │   │
│  │                                                                       │   │
│  │   ┌────────┐  ┌────────┐  ┌────────┐  ┌────────┐                   │   │
│  │   │SandPod1│  │SandPod2│  │SandPod3│  │SandPodN│                   │   │
│  │   │ ┌────┐│  │ ┌────┐│  │ ┌────┐│  │ ┌────┐│                   │   │
│  │   │ │Dae- ││  │ │Dae- ││  │ │Dae- ││  │ │Dae- ││                   │   │
│  │   │ │mon  ││  │ │mon  ││  │ │mon  ││  │ │mon  ││                   │   │
│  │   │ │:2280 ││  │ │:2280 ││  │ │:2280 ││  │ │:2280 ││                   │   │
│  │   │ └────┘│  │ └────┘│  │ └────┘│  │ └────┘│                   │   │
│  │   └────────┘  └────────┘  └────────┘  └────────┘                   │   │
│  │                                                                       │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 6.2 SandPod 生命周期

```go
// pkg/poder/docker/SandPod.go

func (d *DockerClient) CreateSandPod(ctx context.Context, req *CreateSandPodRequest) (*SandPodInfo, error) {
    // 1. 检查状态
    state, _ := d.GetState(ctx, req.SandPodID)
    if state == SandPodStateStarted {
        return &SandPodInfo{State: state}, nil
    }

    // 2. 拉取镜像
    if err := d.PullImage(ctx, req.Snapshot, req.Registry); err != nil {
        return nil, fmt.Errorf("pull image: %w", err)
    }

    // 3. 创建容器
    container, err := d.CreateContainer(ctx, req)
    if err != nil {
        return nil, fmt.Errorf("create container: %w", err)
    }

    // 4. 启动容器
    if err := d.StartContainer(ctx, container.ID); err != nil {
        return nil, fmt.Errorf("start container: %w", err)
    }

    // 5. 等待 Daemon 就绪
    daemonVersion, err := d.WaitForDaemon(ctx, container.IP, 60*time.Second)
    if err != nil {
        return nil, fmt.Errorf("wait for daemon: %w", err)
    }

    return &SandPodInfo{
        ContainerID:   container.ID,
        DaemonVersion: daemonVersion,
        State:         SandPodStateStarted,
    }, nil
}

func (d *DockerClient) StartSandPod(ctx context.Context, SandPodID string) error {
    // 1. 获取容器
    container, err := d.GetContainer(ctx, SandPodID)
    if err != nil {
        return err
    }

    // 2. 如果已运行，直接返回
    if container.State.Running {
        return nil
    }

    // 3. 启动容器
    if err := d.StartContainer(ctx, container.ID); err != nil {
        return err
    }

    // 4. 等待 Daemon
    _, err = d.WaitForDaemon(ctx, container.IP, 60*time.Second)
    return err
}

func (d *DockerClient) StopSandPod(ctx context.Context, SandPodID string) error {
    container, err := d.GetContainer(ctx, SandPodID)
    if err != nil {
        return err
    }

    // SIGTERM 优雅停止，30s 后 SIGKILL
    return d.StopContainer(ctx, container.ID, 30*time.Second)
}

func (d *DockerClient) DestroySandPod(ctx context.Context, SandPodID string) error {
    container, err := d.GetContainer(ctx, SandPodID)
    if err != nil {
        if IsNotFound(err) {
            return nil // 已经不存在
        }
        return err
    }

    // 强制删除容器
    return d.ForceRemoveContainer(ctx, container.ID)
}
```

## 7. SandPod Daemon 设计

### 7.1 Daemon 架构

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        SandPod Daemon (:2280)                                │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  HTTP Server (Gin)                                                          │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                                                                       │   │
│  │   GET  /version              版本信息                                 │   │
│  │   POST /init                 初始化 (接收配置)                       │   │
│  │                                                                       │   │
│  │   ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐  │   │
│  │   │   /process/*    │  │    /files/*     │  │    /git/*       │  │   │
│  │   │   代码执行       │  │    文件操作       │  │    Git 操作     │  │   │
│  │   └──────────────────┘  └──────────────────┘  └──────────────────┘  │   │
│  │   ┌──────────────────┐  ┌──────────────────┐  ┌──────────────────┐  │   │
│  │   │   /session/*    │  │    /pty/*       │  │   /lsp/*        │  │   │
│  │   │   会话管理       │  │   终端           │  │   语言服务       │  │   │
│  │   └──────────────────┘  └──────────────────┘  └──────────────────┘  │   │
│  │                                                                       │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 7.2 核心 API

```yaml
# Process API

POST /process/execute
{
    "command": "python3 -c 'print(\"Hello\")'",
    "cwd": "/workspace",
    "timeout": 300
}

Response:
{
    "exit_code": 0,
    "result": "Hello\n"
}

---

# Files API

GET /files/?path=/workspace
POST /files/upload
POST /files/folder
POST /files/move
DELETE /files/?path=/workspace/file.txt

---

# Session API (持久会话)

POST /session
{"session_id": "main"}

POST /session/main/exec
{"command": "cd /workspace && python3 app.py"}

GET /session/main/command/:cmd_id/logs

---

# PTY API (交互式终端)

POST /pty
{"id": "main", "cwd": "/workspace"}

WebSocket /pty/main/connect
```

## 8. SDK 设计

### 8.1 Python SDK 示例

```python
# SandrPod-sdk-python

from SandrPod import SandrPod, SandrPodConfig

# 初始化
client = SandrPod(SandrPodConfig(
    api_key="fb_xxxx",
    api_url="https://api.SandrPod.cn"
))

# 创建沙箱
SandPod = client.SandPod.create(
    name="my-agent",
    region="cn-beijing",
    snapshot="ubuntu-22.04",
    class="SMALL",
    auto_stop_minutes=15
)

print(f"SandPod {SandPod.id} is {SandPod.state}")

# 执行代码
result = SandPod.process.code_run('''
import numpy as np
import matplotlib.pyplot as plt

x = np.linspace(0, 10, 100)
y = np.sin(x)

plt.plot(x, y)
plt.title("Sine Wave")
plt.savefig("wave.png")
print("Chart saved!")
''')

print(result.stdout)
print(f"Exit code: {result.exit_code}")

# 文件操作
SandPod.files.upload("local_file.txt", "/workspace/file.txt")
content = SandPod.files.download("/workspace/output.txt")

# 持久会话
SandPod.session.create("my-session")
SandPod.session.exec("my-session", "source venv/bin/activate")
SandPod.session.exec("my-session", "python app.py")

# 交互式终端
pty = SandPod.process.create_pty_session("main")
pty.write("ls -la\n")
output = pty.read()
pty.resize(80, 24)
pty.close()

# 销毁
client.SandPod.destroy(SandPod.id)
```

### 8.2 Go SDK

```go
// SandrPod-sdk-go

package SandrPod

import (
    "context"
    "fmt"
)

func Example() {
    client := NewSandrPod(Config{
        APIKey: "fb_xxxx",
        APIURL: "https://api.SandrPod.cn",
    })

    ctx := context.Background()

    // 创建沙箱
    SandPod, err := client.SandPod.Create(ctx, &CreateSandPodRequest{
        Name:   "my-agent",
        Region: "cn-beijing",
        Snapshot: "ubuntu-22.04",
        Class:   SandPodClassSmall,
    })
    if err != nil {
        panic(err)
    }

    // 执行代码
    result, err := SandPod.Process.CodeRun(ctx, `
        package main

        import "fmt"
        func main() {
            fmt.Println("Hello from SandrPod!")
        }
    `)
    if err != nil {
        panic(err)
    }

    fmt.Printf("Output: %s\n", result.Stdout)
    fmt.Printf("Exit Code: %d\n", result.ExitCode)

    // 销毁
    client.SandPod.Destroy(ctx, SandPod.ID)
}
```

## 9. 安全设计

### 9.1 认证与授权

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              认证流程                                        │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  1. API Key 认证                                                            │
│     Client ──→ API Key (fb_xxxx) ──→ SandrPod API                            │
│                                      │                                      │
│                                      ▼                                      │
│                               验证 Tenant                                   │
│                               查询 Quota                                    │
│                               记录审计日志                                  │
│                                                                             │
│  2. SandPod 访问控制                                                        │
│     Client ──→ SandPod Auth Token ──→ SandPod Proxy                          │
│                    │                                                         │
│                    ▼                                                         │
│              验证 Token                                                     │
│              确认 SandPod 归属                                               │
│              检查 Tenant 权限                                               │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 9.2 网络隔离

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              网络安全                                        │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  SandPod 网络隔离:                                                          │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                     iptables 规则                                      │   │
│  │                                                                       │   │
│  │   INPUT:   DROP ALL (默认拒绝)                                        │   │
│  │   OUTPUT:  DROP ALL (默认拒绝)                                        │   │
│  │                                                                       │   │
│  │   # 允许 DNS                                                          │   │
│  │   -A OUTPUT -p udp --dport 53 -j ACCEPT                             │   │
│  │   -A INPUT  -p udp --sport 53 -j ACCEPT                              │   │
│  │                                                                       │   │
│  │   # 允许特定 CIDR (白名单)                                            │   │
│  │   -A OUTPUT -d 10.0.0.0/8 -j ACCEPT                                 │   │
│  │                                                                       │   │
│  │   # 允许 API Server                                                    │   │
│  │   -A OUTPUT -d api.SandrPod.cn -j ACCEPT                              │   │
│  │                                                                       │   │
│  │   # 块所有其他流量 (可选)                                              │   │
│  │   -A OUTPUT -j DROP                                                  │   │
│  │   -A INPUT  -j DROP                                                   │   │
│  │                                                                       │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

## 10. 部署架构

### 10.1 生产环境部署

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              生产部署架构                                    │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│                         ┌─────────────────┐                                 │
│                         │   Load Balancer │                                 │
│                         │   (SandrPod API)  │                                 │
│                         └────────┬────────┘                                 │
│                                  │                                          │
│         ┌───────────────────────┼───────────────────────┐                 │
│         │                       │                       │                   │
│         ▼                       ▼                       ▼                   │
│  ┌─────────────┐        ┌─────────────┐        ┌─────────────┐            │
│  │  SandrPod    │        │  SandrPod    │        │  SandrPod    │            │
│  │  API Pod   │        │  API Pod   │        │  API Pod   │            │
│  │  (x3)     │        │  (x3)     │        │  (x3)     │            │
│  └─────┬──────┘        └─────┬──────┘        └─────┬──────┘            │
│        │                    │                    │                     │
│        └────────────────────┼────────────────────┘                     │
│                             │                                           │
│                             ▼                                           │
│                    ┌─────────────────┐                                  │
│                    │   Redis Cluster  │  (会话/缓存)                      │
│                    └────────┬────────┘                                  │
│                             │                                           │
│                             ▼                                           │
│                    ┌─────────────────┐                                  │
│                    │  PostgreSQL     │  (主数据)                        │
│                    └────────┬────────┘                                  │
│                             │                                           │
└─────────────────────────────┼─────────────────────────────────────────────┘
                              │
                              │
┌─────────────────────────────┼─────────────────────────────────────────────┐
│                             ▼                                              │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                        AWS / Azure / 自建                             │   │
│  │                                                                      │   │
│  │   ┌────────┐   ┌────────┐   ┌────────┐   ┌────────┐             │   │
│  │   │ Poder  │   │ Poder  │   │ Poder  │   │ Poder  │             │   │
│  │   │ Node 1  │   │ Node 2  │   │ Node 3  │   │ Node N  │             │   │
│  │   │ (EC2)   │   │ (ECS)   │   │ (VM)    │   │ (BM)    │             │   │
│  │   └────────┘   └────────┘   └────────┘   └────────┘             │   │
│  │                                                                      │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

### 10.2 Poder 部署

```yaml
# 每个 Poder 节点上运行

services:
  SandrPod-poder:
    image: SandrPod/poder:latest
    container_name: SandrPod-poder
    restart: always
    environment:
      - SANDRPOD_API_URL=https://api.SandrPod.cn
      - SANDRPOD_API_KEY=${RUNNER_API_KEY}
      - SANDRPOD_RUNNER_PORT=3000
      - DOCKER_HOST=unix:///var/run/docker.sock
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - poder-data:/data
    ports:
      - "3000:3000"
    privileged: true
    network_mode: host
    # 使用 host 网络简化 iptables 管理

volumes:
  poder-data:
```

## 11. 开源闭源边界

### 11.1 开源部分 (SandrPod Open)

```
SandrPod/
├── pkg/
│   ├── provider/           # Provider 抽象层 + 各云厂商实现
│   ├── poder/             # Poder 核心逻辑
│   │   ├── docker/        # Docker 操作
│   │   └── executor/       # 任务执行
│   ├── SandPod/            # SandPod 核心
│   └── sdk/                # 多语言 SDK
│
├── apis/
│   └── SandrPod/v1/          # API 定义 (OpenAPI)
│
└── scripts/                # 部署脚本
```

### 11.2 闭源部分 (SandrPod Cloud)

```
SandrPod/  (不在 GitHub 公开)
│
└── internal/
    ├── api/                 # API 服务实现
    │   ├── gateway/         # 网关 (认证、限流)
    │   ├── services/        # 业务逻辑
    │   │   ├── SandPod/     # 沙箱管理
    │   │   ├── billing/    # 计费 ⭐
    │   │   ├── tenant/      # 租户 ⭐
    │   │   └── quota/       # 配额 ⭐
    │   └── handlers/       # HTTP 处理
    │
    ├── console/             # 控制台 Web ⭐
    │   ├── pages/
    │   ├── components/
    │   └── hooks/
    │
    └── billing/             # 计费系统 ⭐
        ├── billing.go
        ├── invoice.go
        └── payment.go
```

### 11.3 商业化功能

| 功能 | 开源 | 闭源 |
|------|------|------|
| SandPod CRUD | ✅ | |
| 代码执行 | ✅ | |
| 文件操作 | ✅ | |
| Git 操作 | ✅ | |
| PTY 终端 | ✅ | |
| 多云 Provider | ✅ | |
| 多租户 | | ⭐ |
| 计费系统 | | ⭐ |
| 控制台 | | ⭐ |
| 审计日志 | | ⭐ |
| SSO/OIDC | | ⭐ |
| 团队协作 | | ⭐ |
| API 统计 | | ⭐ |
| 告警通知 | | ⭐ |

## 12. 技术栈选型

### 12.1 后端

| 组件 | 技术 | 说明 |
|------|------|------|
| API 服务 | Go | 高性能、易部署 |
| 数据库 | PostgreSQL | 主数据存储 |
| 缓存 | Redis | 会话、队列、缓存 |
| 队列 | Redis Stream | 异步任务 |
| 容器 | Docker | 沙箱隔离 |
| 网络 | iptables | 沙箱网络隔离 |

### 12.2 前端

| 组件 | 技术 | 说明 |
|------|------|------|
| 框架 | React 19 | 成熟生态 |
| UI | Tailwind CSS | 快速开发 |
| 状态 | Zustand | 轻量 |
| 部署 | Vercel | 边缘部署 |

### 12.3 SDK

| 语言 | 框架 |
|------|------|
| Go | 标准库 + gRPC |
| Python | requests + websockets |
| TypeScript | axios + websockets |

## 13. 下一步

- [ ] 设计数据库 Schema
- [ ] 设计 Provider 适配器实现
- [ ] 设计计费系统数据模型
- [ ] 设计多租户隔离方案
- [ ] 编写核心模块代码
