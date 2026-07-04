# Multi-Instance Deployment (Load Mode)

A step-by-step guide to running SandrPod as **N active API instances behind a
load balancer, sharing one PostgreSQL** — the "load mode" from
[SCALING.md](SCALING.md). This is what you deploy when a single box can no longer
hold the connection count (tens of thousands of concurrent poders/agents).

> **Do you need this?** For hundreds to low thousands of poders, one instance on
> a modest box with SQLite or a single Postgres is simpler and fine — see
> [SCALING.md](SCALING.md). Reach for multi-instance when you are approaching
> five figures of concurrent tunnels, or you want zero-downtime rolling upgrades.

All instances are **active** and serve traffic (this is load balancing, not
HA/failover). A poder's tunnel terminates on whichever instance it dialed;
requests that land elsewhere are transparently reverse-proxied to the owner.

---

## Topology

```
                          ┌─────────────────────────┐
   E2B / SDK / CLI  ──────▶  Load Balancer (TLS)     │   api.example.com
   poders / agents  ──────▶  :443  least_conn + WS   │  *.example.com (E2B envd)
                          └───────────┬─────────────┘
                    ┌─────────────────┼─────────────────┐
                    ▼                 ▼                 ▼
             ┌────────────┐    ┌────────────┐    ┌────────────┐
             │  server 1  │◀──▶│  server 2  │◀──▶│  server 3  │   inter-node
             │ node-url   │    │ node-url   │    │ node-url   │   forwarding
             │10.0.1.10   │    │10.0.1.11   │    │10.0.1.12   │   (private net)
             └─────┬──────┘    └─────┬──────┘    └─────┬──────┘
                   └─────────────────┼─────────────────┘
                                     ▼
                        ┌──────────────────────────┐
                        │   PostgreSQL (shared)     │  state + tunnel_owners
                        └──────────────────────────┘
```

- **Load balancer** — the only public entry point. Spreads inbound tunnels
  across instances and terminates TLS. Must speak WebSocket and not idle
  long-lived connections.
- **Server instances** — identical config except a unique `-node-url`. Each
  records the tunnels it holds in the shared `tunnel_owners` table; any instance
  can serve any request by forwarding to the owner over the **private** network.
- **PostgreSQL** — the single source of truth for sandboxes, poders, jobs,
  tokens, and tunnel ownership.

---

## Prerequisites

- A PostgreSQL 14+ instance reachable from every server node (managed RDS /
  Cloud SQL / self-hosted all fine).
- N Linux hosts (or containers) for the server nodes, on a **private network**
  where they can reach each other directly (for inter-node forwarding).
- A load balancer that supports HTTP/1.1 WebSocket upgrades and long idle
  timeouts (nginx, HAProxy, or a cloud L7 LB — examples below use nginx).
- A wildcard TLS cert if you use the E2B drop-in (`*.example.com` + the apex).
  Let's Encrypt issues wildcards only via **DNS-01**, so use an ACME client with
  a DNS-provider plugin (e.g. `lego` or Caddy's DNS plugins) — see Part 4.

---

## Part 1 — PostgreSQL

Create the database and a least-privilege role:

```sql
CREATE USER sandrpod WITH PASSWORD 'CHANGE_ME';
CREATE DATABASE sandrpod OWNER sandrpod;
```

The DSN each node uses:

```
postgres://sandrpod:CHANGE_ME@db.internal:5432/sandrpod?sslmode=require
```

**Schema** is created automatically on first boot (concurrent first-boot from
several nodes is handled — the migration retries on the `CREATE TABLE` race).
No manual migration step is required.

**Connection budget.** Each node opens a pool of up to **20** connections
(`SetMaxOpenConns(20)`). Size Postgres `max_connections` for the whole fleet:

```
max_connections ≥ (number_of_nodes × 20) + admin_headroom
```

e.g. 10 nodes → set `max_connections` to at least 250. If you run many nodes,
put **PgBouncer** (transaction pooling) in front and point the DSN at it.

---

## Part 2 — Server nodes

### The one distinction that matters: `-public-url` vs `-node-url`

| Flag | Value | Who uses it | Reaches through |
|------|-------|-------------|-----------------|
| `-public-url` | the **LB** URL, e.g. `https://api.example.com` | cloud VMs / poders dialing home; clients | the load balancer |
| `-node-url` | **this node's private** URL, e.g. `http://10.0.1.10:8080` | peer instances forwarding a request to the tunnel owner | direct, node-to-node |

`-public-url` is the same on every node. `-node-url` is **unique per node** and
must be an address the *other nodes* can reach directly — do **not** point it at
the load balancer (that would double-hop and defeat the loop guard). Omitting
`-node-url` disables forwarding and puts the node in single-instance mode.

### systemd unit (per node)

Identical on every node except `SANDRPOD_NODE_URL`:

```ini
# /etc/systemd/system/sandrpod-server.service
[Unit]
Description=SandrPod API server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=sandrpod
# --- shared across all nodes ---
Environment=SANDRPOD_TOKEN=SHARED_ADMIN_TOKEN
Environment=SANDRPOD_PUBLIC_URL=https://api.example.com
Environment=SANDRPOD_E2B_DOMAIN=example.com
Environment=SANDRPOD_E2B_PROVIDER=aws
Environment=SANDRPOD_E2B_REGION=ap-southeast-1
Environment=SANDRPOD_E2B_INSTANCE_TYPE=t3.medium
# --- unique per node ---
Environment=SANDRPOD_NODE_URL=http://10.0.1.10:8080
# cloud provider credentials come from a root-only drop-in (see note)
ExecStart=/opt/sandrpod/server -port 8080 \
  -db 'postgres://sandrpod:CHANGE_ME@db.internal:5432/sandrpod?sslmode=require'
Restart=always
RestartSec=2
# each tunnel costs file descriptors; raise the limit well above the fleet size
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

> **Credentials** (cloud keys, DB password) belong in a root-only drop-in
> (`systemd/system/sandrpod-server.service.d/creds.conf`, `chmod 600`), never in
> the unit committed to git. Provider env vars follow the same names the
> single-node deployment uses.

Bring up each node, then confirm it registered its node URL:

```bash
systemctl enable --now sandrpod-server
curl -s http://10.0.1.10:8080/health          # -> ok, on each node directly
```

### Run the idle reapers on exactly ONE node

The idle-sandbox and idle-poder reapers scan the **shared** store, so if every
node runs them they duplicate work (N delete attempts per idle sandbox, N
`DeleteVM` calls per empty poder — idempotent but noisy and wasteful of provider
API calls). Set the reaper timers on **one designated node only**:

```ini
# drop-in on ONE node: sandrpod-server.service.d/reaper.conf
[Service]
Environment=SANDRPOD_SANDBOX_IDLE_TIMEOUT=2h
Environment=SANDRPOD_PODER_IDLE_TIMEOUT=30m
```

Leave both unset (the default, `0` = disabled) on all other nodes. The reaper
node removes idle sandbox records and terminates empty cloud VMs via the
provider API from anywhere; a container whose tunnel lives on another node is
reclaimed when its now-empty poder/VM is reaped (the VM termination is the
backstop). Per-tenant caps like `SANDRPOD_MAX_SANDBOXES_PER_OWNER` are stateless
lookups and can stay set on **every** node.

---

## Part 3 — Load balancer

The LB has two jobs: **spread** inbound tunnels across nodes, and **not** idle
them out. No sticky sessions are needed — inter-node forwarding handles a request
that lands on a non-owning node.

### nginx example

```nginx
upstream sandrpod_nodes {
    least_conn;                    # spread long-lived tunnels evenly by conn count
    server 10.0.1.10:8080 max_fails=2 fail_timeout=10s;
    server 10.0.1.11:8080 max_fails=2 fail_timeout=10s;
    server 10.0.1.12:8080 max_fails=2 fail_timeout=10s;
    keepalive 64;
}

# WebSocket upgrade plumbing (poder tunnel + PTY shell)
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    http2 off;                     # keep tunnels on HTTP/1.1 for clean WS upgrades
    server_name api.example.com *.example.com;   # apex control plane + E2B envd hosts

    ssl_certificate     /etc/ssl/sandrpod/fullchain.pem;
    ssl_certificate_key /etc/ssl/sandrpod/privkey.pem;

    # Long-lived tunnels and streaming exec must not be idled out.
    proxy_read_timeout  1h;
    proxy_send_timeout  1h;

    location / {
        proxy_pass http://sandrpod_nodes;
        proxy_http_version 1.1;
        proxy_set_header Upgrade           $http_upgrade;      # WebSocket
        proxy_set_header Connection        $connection_upgrade;
        proxy_set_header Host              $host;              # E2B routes on Host — preserve it
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;                                  # flush streamed output promptly
    }

    location = /health { proxy_pass http://sandrpod_nodes; access_log off; }
}
```

Critical points:

- **`least_conn`** — tunnels are long-lived and uneven; balance by active
  connection count, not round-robin, so nodes don't drift out of balance.
- **`Host $host`** — the E2B gateway routes envd traffic on the Host header
  (`<port>-<id>.example.com`). If the LB rewrites Host, envd routing breaks.
- **Long `proxy_read_timeout`** — a poder holds one tunnel open indefinitely;
  a short read timeout would sever it. (yamux keepalives ride inside the
  connection; the LB just has to leave it open.)
- **No stickiness required** — a client request for a sandbox owned by another
  node is forwarded there internally. Round-robin/least-conn for clients is fine.

Cloud L7 LBs (ALB, GCLB, Aliyun SLB) work equally well: enable WebSocket, raise
the idle timeout to ≥1h, health-check `GET /health`, and preserve the Host
header.

---

## Part 4 — TLS and the E2B drop-in

If you expose the E2B-compatible surface, point `E2B_DOMAIN` at your apex and
terminate a **wildcard** cert at the LB. Clients then need only:

```bash
export E2B_DOMAIN=example.com
export E2B_API_KEY=e2b_...          # issued via `sandrpod-cli token create` (see AUTH_AND_KEYS.md)
```

The E2B envd/code surface is **fully multi-instance**: an envd or `run_code`
request that lands on a node not holding the sandbox's tunnel is reverse-proxied
to the owning node (same mechanism, same loop guard as the native API). No
special LB routing for E2B is needed beyond preserving the Host header.

Issue the wildcard cert with an ACME **DNS-01** client (Let's Encrypt requires
DNS-01 for wildcards). A typical `lego` invocation, driven by your DNS provider's
API credentials, plus a `systemd` timer for renewal:

```bash
# issue *.example.com + example.com via the DNS provider plugin (creds via env)
lego --accept-tos --email ops@example.com \
     --dns <provider> \
     --domains '*.example.com' --domains 'example.com' run
# renew from a daily systemd timer; reload the LB in the timer's ExecStartPost
lego --dns <provider> --domains '*.example.com' --domains 'example.com' renew --days 30
```

Install the resulting `fullchain`/`privkey` at the LB paths referenced above and
reload the LB after each renewal.

---

## Part 5 — First boot & upgrades

- **First boot** — start the nodes; the schema self-creates and concurrent
  first-boot is safe. If you prefer determinism, start one node, let it migrate,
  then start the rest.
- **Rolling upgrade** — drain and restart one node at a time. When a node stops,
  its `tunnel_owners` rows expire after `ownerTTL` (90 s) and its poders
  reconnect through the LB to another node, which re-claims them. Roll one node,
  wait for its poders to re-home, then the next. No global downtime.
- **Token issuance is cluster-wide** — `sandrpod-cli token create` on any node
  writes to the shared store; peers validate the new key via a hash lookup
  immediately and converge their in-memory index within ≤30 s. Revocation is
  eventual on the same ≤30 s bound (see Known limits).

---

## Part 6 — Verify the cluster

A quick cross-node smoke test proves forwarding works end to end:

```bash
# 1. Every node is healthy directly and through the LB
for n in 10.0.1.10 10.0.1.11 10.0.1.12; do curl -s http://$n:8080/health; done
curl -s https://api.example.com/health

# 2. Create a sandbox (its poder dials the LB and lands on some node)
sandrpod-cli --api-url https://api.example.com create smoke --provider aws --region ap-southeast-1

# 3. Exec against it repeatedly. The LB spreads these requests across ALL nodes;
#    whichever node lacks the tunnel forwards to the owner. Every call must
#    return the same hostname — proof that non-owning nodes route correctly.
for i in $(seq 5); do
  sandrpod-cli --api-url https://api.example.com execute smoke "hostname"
done

sandrpod-cli --api-url https://api.example.com delete smoke
```

All five `execute` calls returning the sandbox's hostname (not a 503) confirms
inter-node forwarding. Watch `sandrpod_poders` in `/metrics` spread across nodes.

---

## Part 7 — Operate

### Observability

`GET /metrics` (Prometheus, admin-gated) on each node — scrape all of them and
sum. Key series and what to watch are in [SCALING.md](SCALING.md#observability):
`containers_in_use / container_capacity` for saturation, rising
`sandrpod_jobs{status="PENDING"}` for a provisioning backlog.

### Capacity model

The per-box ceiling is **connection count**, not throughput: ~100k tunnels on
one instance is ~300–400k goroutines + several GB. The LB spreads the fleet:
10 nodes ⇒ ~10k tunnels each, comfortable. The full model is in
[SCALING.md](SCALING.md#capacity-model-the-100k-agent-question).

### Tuning knobs

| Concern | Knob | Default |
|---------|------|---------|
| PG pool per node | `SetMaxOpenConns` (code) | 20 |
| Crashed-node ownership expiry | `ownerTTL` (code) | 90 s |
| Ownership refresh cadence | refresher (code) | 20 s |
| Hot-path activity write throttle | `activityPersistEvery` (code) | 30 s |
| Idle sandbox reaping | `SANDRPOD_SANDBOX_IDLE_TIMEOUT` (one node) | off |
| Empty cloud poder reaping | `SANDRPOD_PODER_IDLE_TIMEOUT` (one node) | off |
| Per-tenant blast radius | `SANDRPOD_MAX_SANDBOXES_PER_OWNER` | unlimited |
| Log format / level | `SANDRPOD_LOG_FORMAT=json` · `SANDRPOD_LOG_LEVEL` | text · info |

Set `SANDRPOD_LOG_FORMAT=json` on every node in production so a log aggregator
can parse the per-request access records and lifecycle events. Cross-instance
token revocation is immediate (Postgres `LISTEN/NOTIFY`), with a 30 s reload as
backstop.

### Known limits (inherited from SCALING.md)

- **Token revocation is eventual (≤30 s)** across nodes — the issuing node drops
  it immediately, peers converge on the next index re-sync. A Postgres
  `LISTEN/NOTIFY` channel would make it instant.
- **Heartbeat write volume** — each poder's ~10 s heartbeat is a DB write; a very
  large fleet benefits from batching these.
- **Reapers are single-node by recommendation**, not by lock — if you set the
  reaper env on more than one node they still work but duplicate provider calls.

---

## Checklist

- [ ] PostgreSQL reachable from all nodes; `max_connections ≥ nodes×20 + headroom`
- [ ] Every node has a **unique** `-node-url` on the private network
- [ ] Every node shares `-public-url` (the LB) and the same `-db` DSN + tokens
- [ ] Reaper env (`*_IDLE_TIMEOUT`) set on **one** node only
- [ ] LB: WebSocket enabled, idle timeout ≥1h, `least_conn`, Host preserved
- [ ] LB health check on `/health`
- [ ] (E2B) wildcard cert at the LB; `E2B_DOMAIN` set on every node
- [ ] `LimitNOFILE` raised well above the expected per-node tunnel count
- [ ] Cross-node smoke test (Part 6) passes
```
