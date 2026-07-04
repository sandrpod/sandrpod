# Scaling & Operations

Where SandrPod's control plane stands today, the practical limits, and how to
observe and grow it.

## Architecture recap

A single API server holds the authoritative state (sandboxes, poders, jobs) in
SQLite (or memory) and multiplexes every Poder over one WebSocket+yamux reverse
tunnel each. Work runs on Poders/Toolboxes; the server is a control plane and
proxy, not an execution host.

## Where the ceiling actually is

The bottleneck is **connection count, not request throughput**. The tunnel
multiplexes HTTP/WS streams over one TCP connection per Poder at sub-millisecond
overhead, so proxied calls are cheap. What grows with fleet size is goroutines
and per-connection memory:

- Each Poder tunnel and each in-flight stream costs goroutines. At tens of
  thousands of Poders the server's goroutine count (roughly a few per
  connection) and heap become the real constraint, well before CPU on proxying.
- SQLite serialises writes (single writer). Job polling and status updates are
  the write-hot path; at very high fleet churn this serialisation, not read
  latency, is what saturates first.

For most deployments (hundreds to low thousands of Poders) a single server
instance on a modest box is fine. Plan a migration path before you approach
five figures of concurrent Poders.

## Growing past one instance (load mode)

Multiple active API instances behind a load balancer, all serving traffic and
sharing one PostgreSQL. For a step-by-step production install (PostgreSQL, per-node
systemd units, the load-balancer config, TLS, and a cross-node smoke test) see
[MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md). Implemented:

1. **Shared store** — `-db postgres://…`; the same `pkg/store/sqldb`
   repositories run on Postgres (connection pool; job claim via
   `SELECT … FOR UPDATE SKIP LOCKED` so concurrent pollers on different instances
   claim disjoint jobs).
2. **Tunnel affinity, solved by inter-node forwarding** — a poder's yamux tunnel
   still terminates on one instance, but each instance records ownership in the
   `tunnel_owners` table (keyed by poder id / agent sandbox name) under its
   `-node-url`. Any instance resolving a sandbox whose tunnel is remote
   reverse-proxies the request — **HTTP and WebSocket (PTY)** alike — to the
   owning node. An `X-Sandrpod-Forwarded` loop-guard stops a stale owner map from
   looping; a per-instance refresher keeps ownership fresh and `ownerTTL` (90 s)
   expires a crashed node's rows so routing recovers as its poders reconnect.
   **Set a unique `-node-url` on every instance.**
3. **Token coherence** — issued keys live in the shared store; an instance
   validates a peer-issued key via a hash lookup (then caches it), and a periodic
   index re-sync converges revocations (≤30 s).
4. **Load balancer** — must allow long-lived WebSocket connections (poders dial
   in and stay) and spread them across instances; that spread *is* the connection
   sharding below.

### Capacity model (the ~100k-agent question)

The per-box ceiling is **connection count**, not throughput. Each agent/poder
holds one persistent tunnel (a yamux session ≈ a few keepalive goroutines), so
~100k tunnels on a single instance is ~300–400k goroutines + several GB — the
wall. Horizontal sharding moves past it: the load balancer spreads the ~100k
inbound tunnels across N instances (~100k/N each) and inter-node forwarding lets
any instance serve any request. Ten instances ⇒ ~10k tunnels each — comfortable.

Hot paths are kept cheap so the shared DB isn't the new bottleneck: the
per-request `LastActivity` write is throttled to once per sandbox per 30 s, and
job claiming is one indexed `SKIP LOCKED` query. Per-tunnel throughput is
sub-millisecond and yamux-multiplexed, so it is not the limit.

## Observability

`GET /metrics` renders Prometheus text (admin-gated when auth is on):

- `sandrpod_sandboxes{state=...}` — sandboxes by state
- `sandrpod_poders{state=...}` — poders ONLINE/OFFLINE
- `sandrpod_poder_container_capacity` / `sandrpod_poder_containers_in_use` —
  fleet headroom
- `sandrpod_jobs{status=...}` — job pipeline health

Watch `containers_in_use / container_capacity` for saturation, and rising
`sandrpod_jobs{status="PENDING"}` for a provisioning backlog (no free Poder,
slow VM boots).

## Cost controls

- `SANDRPOD_SANDBOX_IDLE_TIMEOUT` / per-sandbox `ttl_seconds` reap idle
  sandboxes.
- `SANDRPOD_PODER_IDLE_TIMEOUT` reclaims empty cloud Poders (terminates the VM).
- `-max-sandboxes-per-owner` and `-rate-limit` bound per-tenant blast radius.

## Observability & logs

The server emits structured logs via `slog`, configured by environment:

- `SANDRPOD_LOG_FORMAT=json` (aggregators) or `text` (default, humans)
- `SANDRPOD_LOG_LEVEL=debug|info|warn|error` (default `info`)

Every request produces one access record (`msg=http` with method, path, status,
bytes, duration, client IP); 4xx logs at warn, 5xx at error, health/metrics
probes at debug. Legacy `log.Printf` lines are routed through the same handler,
so the whole process shares one format and level.

See **[LOGGING.md](LOGGING.md)** for the full record schema, level semantics,
the client-IP/`X-Forwarded-For` behavior behind a load balancer, and `jq`
recipes for querying JSON logs.

## Known remaining work

- Distributed tracing / span propagation (structured request logging is shipped;
  tracing is the remaining piece).
- Heartbeat write volume: each poder's ~10 s heartbeat is a DB write, so a very
  large fleet benefits from batching those.
- Persisted per-Poder version reporting for rolling-upgrade visibility.
- Structured logging in the poder/agent/toolbox binaries (the server control
  plane has it; the workers still use plain `log`).
