---
title: REST API
description: The native control-plane API — sandboxes, workers, execution, egress, and how to authenticate against it.
---

# REST API

Base URL is whatever you run the control plane at. Every endpoint below talks
JSON, and every one is reachable from a script, an SDK, or `curl`.

## Authentication

Two headers are accepted, and they mean different things:

| Header | Credential | Scope |
|---|---|---|
| `X-Sandrpod-Token` | platform token | everything, every workspace |
| `X-Sandrpod-Token` | `sk_…` workspace key | one workspace |
| `X-Sandrpod-Token` | `e2b_…` E2B key | one workspace (the E2B spelling of the same key) |
| `Cookie: sandrpod_session` | browser session | the signed-in workspace |

The platform token is the operator credential: it can list every sandbox and
delete every worker. Do not ship it inside an agent.

## Sandboxes

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/v1/sandboxes` | create. `async:true` returns `202` with a job id |
| `GET` | `/api/v1/sandboxes` | list, scoped by credential; `limit`/`offset`, response carries `has_more` |
| `GET` | `/api/v1/sandboxes/{name}` | one sandbox |
| `DELETE` | `/api/v1/sandboxes/{name}` | delete record and container |
| `POST` | `/api/v1/sandboxes/{name}/start` \| `/stop` | resume / pause |
| `POST` | `/api/v1/sandboxes/execute?sandbox={name}` | run code |
| `GET` | `/api/v1/sandboxes/stream?sandbox={name}` | streaming run |
| `GET` | `/api/v1/sandboxes/{name}/logs?tail=N` | container log |
| `GET` | `/api/v1/sandboxes/{name}/toolbox/*` | proxy to the in-sandbox toolbox |
| `POST` | `/api/v1/sandboxes/{name}/snapshot` | commit the container to an image |
| `GET` | `/api/v1/snapshots` | snapshot images across online workers (deduped by tag) |
| `DELETE` | `/api/v1/snapshots?image={repo:tag}` | remove one (`poder_id` narrows it to one worker) |

`{name}` is the workspace-scoped name from the create request: two workspaces may
both use `mysandbox`, and a request resolves the name within the caller's
workspace. Every sandbox object also carries `routing_id`, the server-wide key
the wildcard preview hosts use (`<port>-<routing_id>.<domain>`); treat it as
opaque and let `/preview/{port}` mint URLs instead of building them.

Create body:

```json
{
  "name": "agent-01",
  "provider_type": "local",
  "region": "local",
  "cpu_cores": 1,
  "memory_mb": 1024,
  "ttl_seconds": 3600,
  "network": { "allowed_domains": ["pypi.org", "github.com"] }
}
```

A `network` policy on a fleet with no egress-capable worker answers **409** rather
than starting an unenforced sandbox.

The create body also takes `env_vars` (applied to the container), `storage_mb`
(the workspace reservation), `labels`, `private_ports: true` (every public port
requires a credential), `persist_data`, `gpu`, `poder_id` (skip placement), and
`async`. CPU/memory/storage defaults are 1 vCPU / 1024 MiB / 2048 MiB when
omitted.

Long-running access to a sandbox uses the same `{name}` prefix:
`/api/v1/sandboxes/{name}/session/*` (long-lived shell sessions),
`/api/v1/sandboxes/{name}/mcp` (the native MCP bridge), `/pty` (raw terminal),
and `/toolbox/*` (files, processes, stats). The CLI wraps these; the paths are
stable if you are writing your own client.

`GET /api/v1/jobs/{id}` returns an asynchronous create's status, error and
result. `PATCH` on the same path is the worker's status report and needs an
admin credential.

## Execution

```bash
curl -s -X POST "$API/api/v1/sandboxes/execute?sandbox=agent-01" \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"language":"bash","code":"echo hi","timeout":20}'
```

```json
{"exit_code":0,"stdout":"hi\n","stderr":"","started_at":"…","ended_at":"…"}
```

`language` is `bash`, `python`, `node`, or `sh`. `timeout` is seconds.

## Workers

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/poders` | inventory with state, resources, capabilities, region |
| `POST` | `/api/v1/poders/{id}/sandboxes` | create on that worker, skipping placement |
| `DELETE` | `/api/v1/poders/{id}` | remove a record (and its tunnel) |
| `POST` | `/api/v1/poders/{id}/heartbeat` | workers only; a worker may heartbeat itself and nothing else |
| `POST` | `/api/v1/poders/enroll` | worker exchanges a join invitation for its worker key |
| `GET` | `/ws/poder/connect` | the WebSocket a worker dials (not called by hand) |

## Egress

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/sandboxes/{name}/egress` | current policy, `null` when unset |
| `PUT` | `/api/v1/sandboxes/{name}/egress` | replace it (validated) |
| `DELETE` | `/api/v1/sandboxes/{name}/egress` | unset it — not the same as `PUT {}` |
| `POST` | `/api/v1/policy/decide` | per-connection verdict, for `-egress-mode=decider` |

## Tokens

Admin-only, and only on a build started with a persistent `-db`. Issued keys use
the `e2b_<hex>` spelling, so the same key works on both the native and E2B
surfaces. Only the hash is stored; the raw key is returned once.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/tokens` | list names, prefixes, roles and creation times |
| `POST` | `/api/v1/tokens` | `{"name":"…","role":"user"\|"admin"}` → `201` with the raw key |
| `DELETE` | `/api/v1/tokens/{prefix}` | revoke by the printed prefix |

A hosted deployment may disable this route in favour of per-workspace keys
minted from its console.

## Operations

| Method | Path | Notes |
|---|---|---|
| `GET` | `/health` | liveness and version |
| `GET` | `/healthz` | the deeper check: database, workers, payments, disk |
| `GET` | `/metrics` | Prometheus; needs an admin credential |
| `GET` | `/api/v1/capabilities` | what this control plane supports (`egress_policy`, …) |

## Errors

| Status | Meaning |
|---|---|
| `400` | malformed body or an invalid name |
| `401` | no or unknown credential |
| `403` | valid credential, not allowed (a worker reaching the fleet view, a member spending) |
| `404` | no such sandbox |
| `409` | conflict: name taken, sandbox not `RUNNING`, or a capability no worker has |
| `429` | rate limited; `Retry-After` says how long |
| `503` | the worker that owns the sandbox is not connected to any instance |

## E2B-compatible surface

A second, E2B-shaped surface can be mounted on its own domain. It is documented
in [E2B drop-in](/e2b); the paths are `/sandboxes`, `/sandboxes/{id}`,
`/sandboxes/{id}/pause`, `/snapshots`, `/sandboxes/{id}/fork`,
`/sandboxes/{id}/network`, and envd RPCs on `<port>-<id>.<domain>`.

## OpenAPI

The native surface has no served OpenAPI document; this page and the CLI are the
contract. The E2B surface follows E2B's own schema (`spec/openapi.yml`
upstream), which is what makes a drop-in possible.