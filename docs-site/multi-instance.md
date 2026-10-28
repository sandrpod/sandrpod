---
title: More than one instance
description: A load-balanced control plane on PostgreSQL — what is forwarded, what is delivered, and what survives a crash.
---

# More than one instance

N control-plane instances behind a load balancer, one PostgreSQL database, each
instance given its own `-node-url` (its internal address, reachable by peers).

```bash
./server -db postgres://… -port 8080 -node-url http://10.0.1.5:8080   # A
./server -db postgres://… -port 8080 -node-url http://10.0.1.6:8080   # B
```

A worker dials one instance and its tunnel lives **in that process's memory**. The
database only records which node holds it (`tunnel_owners`, refreshed every
heartbeat, trusted for 90 s). Everything below follows from that one fact.

## What crosses instances

| Request | Behaviour on the wrong node |
|---|---|
| `POST /sandboxes/execute` | **forwarded** to the node holding the tunnel |
| `/sandboxes/{name}/toolbox/*` (files, PTY, streams) | **forwarded** |
| E2B surface | **forwarded** |
| `POST /api/v1/sandboxes` (create) | **delivered** by the owning node (`POST /internal/provision`) |

Execute and the proxy paths forward the whole request: a stateless replay against
the node that owns the connection.

Create cannot be replayed — it admits against the wallet, writes the record and
enqueues a job — so only its **last step** crosses the wire. The receiving
instance does everything up to the container, then asks the owning node to push
it down its tunnel.

No sticky routing is required, which is what makes a fleet of dozens of workers
usable.

## A create survives the process that accepted it

Accepting and delivering are two steps; a process can die in between. The payload
is committed to `jobs.request` before delivery is attempted, so any instance can
finish the job:

| Step | Durable |
|---|---|
| accept | sandbox record, job row, and the exact payload to send |
| deliver | on success the job becomes `COMPLETED` |
| resume | an instance finding a `PENDING` create older than 45 s claims and delivers it |

The claim is a compare-and-swap on `jobs.updated_at`, so exactly one of N
instances delivers. Semantics are **at-least-once**, and the worker tolerates a
repeat because a pode names the container after the sandbox.

A job whose sandbox record is gone, or already `ERROR`/`STOPPED`, is failed
rather than retried: retrying a deleted sandbox would resurrect it on every pass.

## What must be true

| | |
|---|---|
| Database | PostgreSQL for every instance; SQLite is single-instance only |
| `-node-url` / `CLOUD_NODE_URL` | set on every instance, the same string that appears in `tunnel_owners` |
| Tokens | the same signing material everywhere, or a session minted by A is rejected by B |
| `/internal/` | must reach the control plane, not a SPA catch-all — a portal that serves `index.html` for unknown paths answers the peer with HTML and a 200 |
| Rate limiting | counters are per instance: N instances means N times the configured limit |
| Migrations | run once, before starting the new instances; they are additive |

## Verifying it

```bash
# the claim must exist and be fresh
psql -tAc "select tunnel_key, node_url, updated_at, now() from tunnel_owners"

# a create on A, the same command through B — the second must forward
curl -s -X POST http://A/api/v1/sandboxes/execute?sandbox=… -d '{"language":"bash","code":"echo ok"}'
curl -s -X POST http://B/api/v1/sandboxes/execute?sandbox=… -d '{"language":"bash","code":"echo ok"}'
```

`503 sandbox tunnel not available` on the second means the claim is stale: check
that the owning instance has a non-empty `-node-url` and is receiving heartbeats.

`hack/multi-instance-check.sh` in the repository does all of this end to end —
two instances, one database, a real worker, a planted lost delivery — and runs in
CI.