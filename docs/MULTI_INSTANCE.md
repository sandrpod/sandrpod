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
| `POST /api/v1/sandboxes` (create) | **not forwarded** → the sandbox is marked `ERROR` |

Create is the exception because the request itself must reach the worker that was
placed: the tunnel is a Go object in the owning process, so a peer cannot present
it. Forwarding the *request* would re-run placement and admission on the peer and
create the record twice.

**Consequence for routing.** Route creates to an instance that holds a worker —
either

- make the load balancer **sticky for `POST /api/v1/sandboxes` and
  `POST /portal/*`** by cookie (the portal session already carries a workspace,
  and a workspace's workers are usually all on one node), or
- pin the worker: `POST /api/v1/sandboxes` with `poder` set, on the node that
  holds that poder, or
- run creates on a single instance and everything else on the fleet.

The condition is visible: the record goes to `ERROR` with
`worker tunnel is held by another instance`, and `GET /healthz` keeps reporting
the fleet accurately.

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
