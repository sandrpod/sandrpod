# Logging

How the SandrPod **API server** logs, how to configure it, and the exact shape
of every record. The server logs through Go's `slog`: one format and one level
apply to the whole process, so a log aggregator can parse and filter everything.

> **Scope.** This covers the API server (`cmd/server`) — the control plane where
> production triage happens. The poder, agent, and toolbox binaries still use the
> plain `log` package (unstructured text); giving them the same treatment is
> tracked in [SCALING.md](SCALING.md#known-remaining-work).

---

## Configuration

Two environment variables, both optional:

| Variable | Values | Default | Effect |
|----------|--------|---------|--------|
| `SANDRPOD_LOG_FORMAT` | `text` \| `json` | `text` | `json` for machine ingestion; `text` for humans |
| `SANDRPOD_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` | `info` | records below the level are dropped |

Set them however you run the server:

```bash
# shell
SANDRPOD_LOG_FORMAT=json SANDRPOD_LOG_LEVEL=info ./server -port 8080 -db postgres://…

# systemd drop-in (production)
[Service]
Environment=SANDRPOD_LOG_FORMAT=json
Environment=SANDRPOD_LOG_LEVEL=info
```

Logs are written to **stderr**. Under systemd they land in the journal
(`journalctl -u sandrpod-server`); to ship them elsewhere, point your log
agent at the journal or redirect stderr to a file.

**Production recommendation:** `SANDRPOD_LOG_FORMAT=json`, `SANDRPOD_LOG_LEVEL=info`.

---

## Formats

The same record in each format — text (default):

```
time=2026-07-04T09:55:06.305+08:00 level=INFO msg=http method=GET path=/api/v1/sandboxes status=200 bytes=17 dur=0s remote=::1
```

JSON (`SANDRPOD_LOG_FORMAT=json`):

```json
{"time":"2026-07-04T09:55:06.305065+08:00","level":"INFO","msg":"http","method":"GET","path":"/api/v1/sandboxes","status":200,"bytes":17,"dur":"0s","remote":"::1"}
```

Every record carries `time`, `level`, and `msg`. Structured records add typed
key/value fields (below); bridged lines (next section) carry only `msg`.

---

## Log levels

`slog`'s four levels, filtered by `SANDRPOD_LOG_LEVEL`:

| Level | What emits here |
|-------|-----------------|
| `debug` | Health and metrics probes (`GET /health`, `GET /metrics`) — **suppressed at the default `info` level**, so liveness checks don't flood the log |
| `info` | Normal requests, startup, tunnel connect/disconnect, job lifecycle |
| `warn` | Requests that returned **4xx** (client errors: bad auth, not found, quota) |
| `error` | Requests that returned **5xx**; inter-node forward failures |

At the default `info` level you see everything except the debug probes. Set
`SANDRPOD_LOG_LEVEL=debug` to include them (useful when debugging health/routing);
set `warn` to see only problems.

---

## The access record (`msg=http`)

The server emits exactly one record per HTTP request, `msg="http"`, with these
fields:

| Field | Type | Meaning |
|-------|------|---------|
| `method` | string | HTTP method (`GET`, `POST`, …) |
| `path` | string | request path (no query string) |
| `status` | int | response status code; a WebSocket upgrade (poder/agent tunnel, PTY) records `101` |
| `bytes` | int | response body bytes written |
| `dur` | string | wall-clock handling time (`0s`, `12ms`, `1.2s`) |
| `remote` | string | client IP — see below |

The record is emitted **when the handler returns**. For a long-lived WebSocket
tunnel that means the line appears at **disconnect**, and `dur` is the whole
connection lifetime — a useful signal for how long a poder stayed connected.

### Client IP behind a load balancer

`remote` is the first hop of `X-Forwarded-For` when present, otherwise the peer
address. Behind the load balancer in a [multi-instance
deployment](MULTI_INSTANCE_DEPLOYMENT.md), make sure the LB sets
`X-Forwarded-For` (the sample nginx config does) so `remote` is the real client,
not the LB.

The status→level mapping means you can isolate failing requests by level alone:
`status>=500` → `error`, `status>=400` → `warn`, health/metrics → `debug`,
everything else → `info`.

---

## Legacy lines

Existing `log.Printf` calls (startup banner, provider registration, tunnel
connect, scheduler steps) are routed through the same handler, so they inherit
the format and destination. They appear as a plain message with no extra fields:

```json
{"time":"2026-07-04T09:55:05.967+08:00","level":"INFO","msg":"API server listening on :80 (Control Plane + Tunnel Mode)"}
```

They all log at `info`. (Converting these to fully-typed fields is incremental;
the bridge guarantees consistent format meanwhile.)

---

## Querying JSON logs

With `SANDRPOD_LOG_FORMAT=json`, pipe through `jq`:

```bash
# all server-side errors
journalctl -u sandrpod-server -o cat | jq 'select(.level=="ERROR")'

# requests slower than 1s
… | jq 'select(.msg=="http" and (.dur|test("[0-9.]+s$")) and (.dur|rtrimstr("s")|tonumber) > 1)'

# 4xx/5xx by path, counted
… | jq -r 'select(.status>=400)|.path' | sort | uniq -c | sort -rn

# everything from one client IP
… | jq 'select(.remote=="203.0.113.7")'

# inter-node forward failures (multi-instance)
… | jq 'select(.msg|test("forward"))'
```

In a hosted stack (Loki, CloudWatch, Datadog, ELK) the same fields are directly
filterable — e.g. `status:>=500`, `path:"/api/v1/sandboxes/execute"`, `dur`.

---

## What's not here yet

- **Distributed tracing / span propagation** — no trace/span IDs on records yet;
  tracked in [SCALING.md](SCALING.md#known-remaining-work).
- **Worker binaries** — poder/agent/toolbox still log unstructured text.
- **Request IDs** — no per-request correlation ID is injected yet; `remote` +
  `time` + `path` are the correlation keys for now.

---

## Reference

- Setup: [`pkg/logging`](../pkg/logging/logging.go) — handler + level + std-log bridge
- Access log: [`cmd/server/accesslog.go`](../cmd/server/accesslog.go) — middleware + recorder
- Operations context: [SCALING.md](SCALING.md#observability--logs) · [MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md)
