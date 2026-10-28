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
| `GET` | `/api/v1/sandboxes` | list, scoped by credential |
| `GET` | `/api/v1/sandboxes/{name}` | one sandbox |
| `DELETE` | `/api/v1/sandboxes/{name}` | delete record and container |
| `POST` | `/api/v1/sandboxes/{name}/start` \| `/stop` | resume / pause |
| `POST` | `/api/v1/sandboxes/execute?sandbox={name}` | run code |
| `GET` | `/api/v1/sandboxes/stream?sandbox={name}` | streaming run |
| `GET` | `/api/v1/sandboxes/{name}/toolbox/*` | proxy to the in-sandbox toolbox |
| `POST` | `/api/v1/sandboxes/{name}/snapshot` | commit the container to an image |

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
| `DELETE` | `/api/v1/poders/{id}` | remove a record (and its tunnel) |
| `POST` | `/api/v1/poders/{id}/heartbeat` | workers only; a worker may heartbeat itself and nothing else |
| `GET` | `/ws/poder/connect` | the WebSocket a worker dials (not called by hand) |

## Egress

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/sandboxes/{name}/egress` | current policy, `null` when unset |
| `PUT` | `/api/v1/sandboxes/{name}/egress` | replace it (validated) |
| `DELETE` | `/api/v1/sandboxes/{name}/egress` | unset it — not the same as `PUT {}` |
| `POST` | `/api/v1/policy/decide` | per-connection verdict, for `-egress-mode=decider` |

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
`/sandboxes/{id}/pause`, and envd RPCs on `<port>-<id>.<domain>`.

## OpenAPI

The control plane serves its OpenAPI document at `/api/v1/openapi.json` on builds
that include it; the E2B surface's schema is E2B's own
(`spec/openapi.yml` upstream), which is what makes a drop-in possible.