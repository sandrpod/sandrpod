# SandrPod Horizontal Scaling Design

> **Note:** This document is written in Chinese. It is an internal design note describing a future path from single-node SQLite to multi-instance + PostgreSQL, with zero-downtime migration and no breaking changes to the existing API or Poder tunnel protocol.

---

# SandrPod 横向扩展设计

> **目标**：从单节点 SQLite 平滑演进到多实例 + PostgreSQL，全程不破坏现有 API 与 Poder 协议，零停机切换。

---

## 一、现状回顾

```
Client
  │
  ▼
API Server (单实例, :18090)
  ├─ 状态: SQLite (./data/sandrpod.db)
  ├─ tunnelStore: 进程内 map（yamux 连接）
  ├─ directStore: 进程内 map（agent 直连）
  │
  ├──WS反向隧道── Poder-A (Docker worker)
  ├──WS反向隧道── Poder-B (Docker worker)
  └──WS反向隧道── sandrpod-agent (本机直连)
```

**瓶颈**：
- API Server 单点，进程崩溃即全服务中断
- `tunnelStore` / `directStore` 是进程内 `map`，多实例无法共享
- SQLite 单写线程，写 QPS 上限约 1000 ops/s
- `PollJobs` 依赖 `SetMaxOpenConns(1)` 做写串行，多实例会竞争

---

## 二、演进路线（三个阶段）

```
阶段 0 (当前)          阶段 1                  阶段 2
─────────────          ──────────────────────  ──────────────────────────
单实例 + SQLite   →    多实例 + PostgreSQL  →  多实例 + PG + Redis Pub/Sub
                        (共享 DB, 无状态)       (隧道路由去中心化)
```

---

## 三、阶段一：多实例 + PostgreSQL（核心目标）

### 3.1 架构图

```
                     ┌─────────────────────────────┐
                     │         负载均衡              │
                     │   (Nginx / ALB / Caddy)      │
                     │   WebSocket sticky session   │
                     └──────┬──────────┬────────────┘
                            │          │
               ┌────────────▼──┐  ┌───▼────────────┐
               │  API Server 1 │  │  API Server 2  │
               │  :8080        │  │  :8080         │
               │  tunnelStore  │  │  tunnelStore   │  ← 各自的进程内 map
               │  directStore  │  │  directStore   │
               └──────┬────────┘  └───┬────────────┘
                      │               │
                      └───────┬───────┘
                              │
                   ┌──────────▼──────────┐
                   │    PostgreSQL        │
                   │  sandboxes / poders │
                   │  jobs               │
                   └─────────────────────┘
```

**关键设计原则**：
- WebSocket 反向隧道（Poder / agent）**绑定到建立连接的那个实例**，不跨实例共享
- 路由请求时，先查 DB 得到 `poder_id`，再判断本实例是否持有该隧道；否则反向代理到持有它的实例
- `PollJobs` 改用 PostgreSQL `SELECT … FOR UPDATE SKIP LOCKED`，天然支持多消费者

---

### 3.2 仅换驱动：`pkg/store/postgres/`

Repository 接口不变（`sandpod.SandboxRepository` 等），只增加 PostgreSQL 实现包。

#### 3.2.1 驱动选型

```go
// go.mod 新增
pgx/v5  github.com/jackc/pgx/v5          // 原生 PG 驱动，支持 pgx pool
```

#### 3.2.2 Open()

```go
// pkg/store/postgres/db.go
func Open(dsn string) (*pgxpool.Pool, error) {
    cfg, _ := pgxpool.ParseConfig(dsn)
    cfg.MaxConns = 20                     // 按实例数 × 连接数规划
    cfg.MinConns = 2
    cfg.MaxConnLifetime = 30 * time.Minute
    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    if err != nil { return nil, err }
    return pool, Migrate(pool)
}
```

#### 3.2.3 Schema 差异（SQLite → PostgreSQL）

| SQLite | PostgreSQL | 说明 |
|--------|-----------|------|
| `TEXT PRIMARY KEY` | `TEXT PRIMARY KEY` | 无变化 |
| `DATETIME` (RFC3339 string) | `TIMESTAMPTZ` | 原生时间类型，直接比较 |
| `INTEGER` | `BIGINT` | |
| `REAL` | `DOUBLE PRECISION` | |
| `datetime('now')` | `NOW()` | |

```sql
-- pkg/store/postgres/schema.go  (关键差异片段)

CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,
    status       TEXT NOT NULL DEFAULT 'PENDING',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- ... 其余列与 SQLite 完全相同
);

CREATE INDEX IF NOT EXISTS idx_jobs_status_created
    ON jobs(status, created_at)
    WHERE status = 'PENDING';         -- 部分索引，只索引待处理行
```

#### 3.2.4 PollJobs — `SELECT FOR UPDATE SKIP LOCKED`

这是多实例最关键的改动，替代 SQLite 的 `SetMaxOpenConns(1)` 单写串行：

```go
// pkg/store/postgres/job_repo.go

func (r *jobRepo) PollJobs(jobTimeout time.Duration, limit int) ([]*sandpod.Job, error) {
    ctx := context.Background()
    tx, _ := r.pool.Begin(ctx)
    defer tx.Rollback(ctx)

    now := time.Now().UTC()
    cutoff := now.Add(-jobTimeout)

    // Step 1: 重置超时的 IN_PROGRESS → PENDING
    tx.Exec(ctx, `
        UPDATE jobs SET status='PENDING', updated_at=$1
        WHERE status='IN_PROGRESS' AND updated_at < $2`,
        now, cutoff,
    )

    // Step 2: 原子抢占 PENDING 行，SKIP LOCKED 让多实例互不干扰
    rows, _ := tx.Query(ctx, `
        SELECT id FROM jobs
        WHERE status = 'PENDING'
        ORDER BY created_at
        LIMIT $1
        FOR UPDATE SKIP LOCKED`,   // ← 核心：跳过已被其他实例锁定的行
        limit,
    )
    var ids []string
    for rows.Next() {
        var id string; rows.Scan(&id)
        ids = append(ids, id)
    }
    rows.Close()

    if len(ids) == 0 {
        tx.Commit(ctx); return nil, nil
    }

    // Step 3: 批量标记 IN_PROGRESS（单条 UPDATE … WHERE id = ANY($1)）
    tx.Exec(ctx, `
        UPDATE jobs SET status='IN_PROGRESS', updated_at=$1 WHERE id = ANY($2)`,
        now, ids,
    )

    tx.Commit(ctx)

    // Step 4: 读完整行
    result := make([]*sandpod.Job, 0, len(ids))
    for _, id := range ids {
        if j, ok, _ := r.getByID(id); ok {
            result = append(result, j)
        }
    }
    return result, nil
}
```

**对比**：

| 方案 | 并发安全机制 | 多实例 |
|------|------------|--------|
| SQLite `SetMaxOpenConns(1)` | 单写线程 | ❌ 无法多实例 |
| PostgreSQL `FOR UPDATE SKIP LOCKED` | 行级锁 + 跳过 | ✅ N 个实例同时 Poll |

---

### 3.3 隧道路由：本实例优先 + 跨实例反代

Poder / agent 的 yamux WebSocket 连接绑定在**建立连接的那个实例**上，是进程内状态，无法放进 DB。

#### 3.3.1 Poder 注册时记录 `server_id`

```sql
-- poders 表新增列
ALTER TABLE poders ADD COLUMN server_id TEXT NOT NULL DEFAULT '';
ALTER TABLE poders ADD COLUMN server_addr TEXT NOT NULL DEFAULT '';
```

```go
// 在 /ws/poder/connect 中
poderStore.Register(&RegisterPoderRequest{
    ...
    ServerID:   os.Getenv("SERVER_ID"),     // 实例唯一 ID，启动时生成
    ServerAddr: "http://server-1:8080",     // 内网地址
})
```

#### 3.3.2 请求路由逻辑

```go
// sandboxTunnel() 扩展后的逻辑

func sandboxTunnel(name string, ...) (*SandboxInfo, *tunnel.PoderTunnel, bool) {
    sb, ok := ss.Get(name)
    if !ok { http.NotFound(...); return nil, nil, false }

    // 1. 先查本实例的 tunnelStore
    if t, ok := tunnelStore.Get(sb.PoderID); ok {
        return sb, t, true       // 本实例持有，直接返回
    }

    // 2. 本实例没有 → 查 DB 找持有该隧道的实例地址
    poder, _ := poderStore.Get(sb.PoderID)
    if poder.ServerID == "" || poder.ServerAddr == "" {
        http.Error(w, "poder tunnel not available", 503); return nil, nil, false
    }
    if poder.ServerID == myServerID {
        // 同一实例但隧道已断（Poder 下线），直接报错
        http.Error(w, "poder tunnel disconnected", 503); return nil, nil, false
    }

    // 3. 跨实例反向代理
    reverseProxy(poder.ServerAddr, r, w)
    return nil, nil, false   // 已由反代处理，不再走后续逻辑
}
```

#### 3.3.3 内网反代（轻量实现）

```go
// pkg/proxy/internal.go

func ReverseProxy(targetBase string, r *http.Request, w http.ResponseWriter) {
    target, _ := url.Parse(targetBase + r.URL.RequestURI())
    req, _ := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
    for k, v := range r.Header { req.Header[k] = v }
    req.Header.Set("X-Forwarded-For", r.RemoteAddr)
    req.Header.Set("X-Internal-Proxy", "true")  // 防循环
    
    resp, err := http.DefaultClient.Do(req)
    if err != nil { http.Error(w, "internal proxy failed", 502); return }
    defer resp.Body.Close()
    
    for k, v := range resp.Header { w.Header()[k] = v }
    w.WriteHeader(resp.StatusCode)
    io.Copy(w, resp.Body)
}
```

对于 WebSocket (PTY / stream) 的跨实例转发，使用 `gorilla/websocket` 做 WS 双向代理（参见阶段二）。

---

### 3.4 负载均衡：WebSocket Sticky Session

Poder / agent 的 WebSocket 必须路由到**同一个实例**（yamux 连接建在那里）。

**推荐配置（Nginx）**：

```nginx
upstream sandrpod_api {
    # ip_hash 对 /ws 路由保持 sticky（同源 IP 打到同实例）
    ip_hash;
    server server-1:8080;
    server server-2:8080;
    keepalive 64;
}

server {
    listen 80;

    # WebSocket 升级头
    location /ws/ {
        proxy_pass         http://sandrpod_api;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade    $http_upgrade;
        proxy_set_header   Connection "upgrade";
        proxy_read_timeout 3600s;    # yamux 长连接
    }

    # 普通 API（任意实例）
    location /api/ {
        proxy_pass         http://sandrpod_api;
        proxy_http_version 1.1;
    }
}
```

> **注意**：`ip_hash` 对同 IP 的多个 Poder 不够精确，生产推荐在 WebSocket 握手 URL 中带 `?server_id=preferred` hint，由 Nginx `$arg_server_id` 做路由。或直接给 Poder 发配固定实例地址，绕过 LB。

---

### 3.5 main.go 变更：启用 PostgreSQL

```go
// cmd/server/main.go  新增 flag
pgDSN = flag.String("db-pg", "", "PostgreSQL DSN: postgres://user:pass@host/db?sslmode=disable")

// 建仓逻辑新增 pg 分支
case strings.HasPrefix(*dbDSN, "postgres://") || *pgDSN != "":
    dsn := *pgDSN
    if dsn == "" { dsn = *dbDSN }
    pool, err := pgstore.Open(dsn)
    if err != nil { log.Fatalf("pg open: %v", err) }
    defer pool.Close()
    stores = podpkg.Stores{
        Sandboxes: pgstore.NewSandboxRepo(pool),
        Poders:    pgstore.NewPoderRepo(pool),
        Jobs:      pgstore.NewJobRepo(pool),
    }
    log.Printf("Using PostgreSQL: %s", maskPassword(dsn))
```

所有 handler、Scheduler、cleanupOfflinePoders 代码**一行不改**，因为它们只依赖 `sandpod.XxxRepository` 接口。

---

### 3.6 从 SQLite 迁移到 PostgreSQL

```bash
# 1. 导出 SQLite 数据
sqlite3 data/sandrpod.db .dump > /tmp/sandrpod-dump.sql

# 2. 用脚本转换类型（主要是 DATETIME → TIMESTAMPTZ）
python3 scripts/migrate-sqlite-to-pg.py /tmp/sandrpod-dump.sql > /tmp/sandrpod-pg.sql

# 3. 导入 PostgreSQL
psql $PG_DSN < /tmp/sandrpod-pg.sql

# 4. 切换启动参数（滚动重启，不停服）
# 停掉 SQLite 实例 → 启动 PG 实例（数据已同步）
go run ./cmd/server -port 8080 -db postgres://...
```

---

## 四、阶段二：隧道路由去中心化（Redis Pub/Sub）

> 阶段一的跨实例反代是**同步**的，如果持有隧道的实例宕机，反代立即失败。
> 阶段二引入 Redis 做实例间信令，让任意实例都能感知隧道在哪里。

```
                    ┌─────────────────────┐
                    │    Redis Pub/Sub     │
                    │  channel: tunnels   │
                    └───────┬─────────────┘
                            │  publish / subscribe
          ┌─────────────────┼──────────────────┐
          │                 │                  │
  ┌───────▼──────┐  ┌───────▼──────┐  ┌───────▼──────┐
  │ API Server 1 │  │ API Server 2 │  │ API Server 3 │
  │              │  │              │  │              │
  │ tunnelStore  │  │ tunnelStore  │  │ tunnelStore  │
  └──────────────┘  └──────────────┘  └──────────────┘
          │                                   │
          └── Poder-A ──────────── Poder-B ───┘
```

**信令协议**（Redis 消息）：

```json
// Poder 上线
{ "event": "poder_up",   "poder_id": "poder-abc", "server_id": "srv-1", "addr": "http://srv-1:8080" }

// Poder 下线
{ "event": "poder_down", "poder_id": "poder-abc", "server_id": "srv-1" }
```

各实例订阅 `tunnels` channel，维护 `remotePoderMap: poderID → serverAddr`。路由时不再查 DB，直接查本地缓存的 `remotePoderMap`。

---

## 五、实施检查清单

### 阶段一

- [ ] 新建 `pkg/store/postgres/` 包（`db.go`, `schema.go`, `sandbox_repo.go`, `poder_repo.go`, `job_repo.go`）
- [ ] `job_repo.go` 用 `FOR UPDATE SKIP LOCKED` 实现 `PollJobs`
- [ ] `poders` 表新增 `server_id`, `server_addr` 列；`Register()` 写入
- [ ] `sandboxTunnel()` 增加跨实例反代路径
- [ ] `pkg/proxy/internal.go` 轻量反代（HTTP + WebSocket 双版本）
- [ ] `cmd/server/main.go` 新增 `-db postgres://…` 分支
- [ ] Nginx/ALB sticky session 配置
- [ ] 集成测试：`docker-compose` 起 2 个 server + 1 个 PG，验证 PollJobs 并发安全
- [ ] 迁移脚本 `scripts/migrate-sqlite-to-pg.py`
- [ ] 更新 `CLAUDE.md` 和 `docs/ARCHITECTURE.md`

### 阶段二（可选）

- [ ] Redis Pub/Sub 信令层 `pkg/signal/`
- [ ] `tunnelStore` 扩展为 local + remote 双层路由
- [ ] 压测：100 Poder × 10 并发请求，验证 P99 延迟

---

## 六、关键数字参考

| 指标 | SQLite（单实例） | PostgreSQL（多实例） |
|------|---------------|-------------------|
| 写 QPS（jobs） | ~1,000/s | ~50,000+/s |
| 并发 API Server | 1 | N（推荐 2–4） |
| `PollJobs` 并发安全 | `MaxOpenConns(1)` | `FOR UPDATE SKIP LOCKED` |
| RTO（实例宕机） | 服务中断 | 秒级（LB 摘除节点） |
| RPO（数据丢失） | 0（WAL） | 0（同步复制）或秒级（异步） |
| 存储容量 | 文件系统上限 | 无限（水平分片可选） |

---

## 七、不需要改动的部分

得益于 Repository 模式的接口隔离，以下代码**完全不用改**：

| 组件 | 原因 |
|------|------|
| 所有 HTTP handler | 只依赖 `sandpod.XxxRepository` 接口 |
| `Scheduler` | 只调用 `PoderRepository.SelectBest` |
| `cleanupOfflinePoders` | 只调用 `PoderRepository` 接口 |
| `pkg/sandpod/` 领域模型 | 纯数据结构，无存储依赖 |
| Poder / sandrpod-agent | 完全不感知存储层 |
| CLI / Python SDK | 纯 HTTP 客户端 |
| `cmd/poder` | 纯 HTTP 客户端 + Docker |

**唯一需要修改的是** `cmd/server/main.go` 的建仓分支，以及新增 `pkg/store/postgres/` 实现包。

---

*文档版本: v1.0 — 2026-04-19*
