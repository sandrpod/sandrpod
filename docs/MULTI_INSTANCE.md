# Running more than one control plane

A load-balanced deployment: N instances, one PostgreSQL database, each instance
given its own `-node-url` (its internal address, the one peers can reach it at).

```bash
# instance A                                             # instance B
./sandrpod-server -db postgres://… -port 8080 \          ./sandrpod-server -db postgres://… -port 8080 \
  -node-url http://10.0.1.5:8080                           -node-url http://10.0.1.6:8080
```

Workers dial one instance and their tunnel lives **in that process's memory** —
the database does not carry tunnels, only a record of which node holds each one
(`tunnel_owners`, refreshed every heartbeat, trusted for 90 s). Everything below
follows from that.

## What is routed across instances

| Request | Behaviour when it lands on the wrong node |
|---|---|
| `POST /sandboxes/execute` | **forwarded** to the node holding the tunnel |
| `GET /sandboxes/{name}/toolbox/*` (files, PTY, streams) | **forwarded** |
| E2B surface (`/sandboxes`, `/v2/sandboxes`) | **forwarded** |
| `POST /api/v1/sandboxes` (create) | **delivered** by the node holding the tunnel |

Execute and the proxy paths forward the *request*: they are stateless replays
against the node that owns the connection.

Create cannot be replayed — it admits against the wallet, writes the sandbox
record and enqueues a job — so only its **last step** crosses the wire. The
instance that received the request does everything up to the container, then
calls the owning node:

```
POST /internal/provision        X-Sandrpod-Internal: <platform token>
{ "sandbox": {...}, "job_id": "...", "poder_id": "...", "owner": "..." }
```

The owning node pushes that create down its tunnel and finalizes the record from
the worker's answer. The caller reports the peer's result. Nothing is admitted or
recorded twice, and the client never learns which node held the tunnel.

Three details worth knowing:

- **The credential is the platform token.** Both instances already share the token
  store, so no new secret exists; a request without it is refused, never trusted
  by network position.
- **A stale claim is a `409`, not a hang.** If the peer no longer holds that
  tunnel (the worker reconnected elsewhere), it says so and the caller fails the
  record with a reason instead of waiting out a timeout.
- **No forwarding loops.** The endpoint never forwards again, and
  `wantsPeerDelivery` refuses to point an instance at itself.

## Delivery is durable

Accepting a create and delivering it are two steps, and a process can die between
them. The intent is written to the database before delivery is attempted
(`jobs.request`), so any instance can finish the job:

| Step | What is durable |
|---|---|
| accept | the sandbox record, the job row, **and the exact payload to send** |
| deliver | on success the job becomes `COMPLETED`, with the container id |
| resume | an instance that finds a `PENDING` create older than 45 s claims it and delivers |

The claim is a compare-and-swap on `jobs.updated_at` (`JobRepository.ClaimJob`),
so with N instances exactly one wins: verified with two instances and one stale
job — one `resume: delivered` line, one `COMPLETED` row.

Semantics are **at-least-once**, and the worker tolerates a repeat: a pode names
the container after the sandbox, so a second delivery for the same name lands on
the same container. A job whose worker is connected to no instance fails with
that reason instead of retrying forever.

This is the transactional-outbox pattern using the database that is already
there — no broker, no new component to keep alive.

## Placement with a large fleet

With dozens of workers spread across instances, most creates will choose a worker
connected to *some other* node — which is exactly the case the provisioning call
exists for. No sticky routing is required.

The cost is one internal HTTP round-trip per create that crosses instances; the
worker's own create is unchanged (230 ms warm).

## What must be true

| | |
|---|---|
| Database | PostgreSQL for every instance — SQLite is single-instance only |
| `-node-url` | set on **every** instance, reachable from its peers, and the same string that appears in `tunnel_owners` |
| Tokens | the same signing material on every instance, otherwise a session minted by A is rejected by B |
| Rate limiting | the counters are per instance; N instances means N times the configured limit ([OPERATIONS.md](OPERATIONS.md)) |
| Migrations | run once, before starting the new instances; they are additive |

## Verifying it

```bash
# a worker attaches to A; the claim must exist and be fresh
psql -tAc "select tunnel_key, node_url, updated_at, now() from tunnel_owners"

# a create on A, then the same command through B: B must forward
curl -s -b jar -X POST http://A/api/v1/sandboxes/execute?sandbox=… -d '{"language":"bash","code":"echo ok"}'
curl -s -b jar -X POST http://B/api/v1/sandboxes/execute?sandbox=… -d '{"language":"bash","code":"echo ok"}'
```

Both return the command's output. If the second answers
`503 sandbox tunnel not available`, the claim is stale — check that the owning
instance has a non-empty `-node-url` and is receiving heartbeats.

## Two defects this verification found

Both were invisible on SQLite and on a single instance:

1. **Placement never succeeded on PostgreSQL.** `SelectBestFor` filtered
   capabilities with `instr()`, a SQLite-only function. Every query errored, and
   the scheduler's wait loop treated the error as "no room yet" and reported
   `no worker registered within 30s`. Fixed in `d445db7`; a PostgreSQL job now
   runs the store and scheduler suites on every push.
2. **Forwarding expired after 90 seconds.** The claim refresher lived in the CLI
   entry point, which an embedding build never calls, so the claim aged out while
   the fleet was healthy. Heartbeats now renew it. Fixed in `16f73c6`.
