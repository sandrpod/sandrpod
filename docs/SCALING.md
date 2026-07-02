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

## Growing past one instance

The store layer is already behind repository interfaces (`pkg/sandpod/repo.go`)
with a SQLite backend, so the intended path is:

1. Swap SQLite for a networked store (Postgres) behind the same interfaces — the
   DDL is written to be Postgres-compatible.
2. Run multiple stateless-ish API servers against that shared store. The open
   item is tunnel affinity: a Poder's tunnel terminates on one server, so
   proxied calls must reach that server (sticky routing or a tunnel-registry
   lookup). This is the main horizontal-scale work still outstanding (P2).
3. Front the servers with a load balancer that supports long-lived WebSocket
   connections and does not idle them out.

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

## Known remaining work (P2)

- Multi-instance tunnel affinity / registry (above).
- Persisted per-Poder version reporting for rolling-upgrade visibility.
- Structured request logging and tracing.
