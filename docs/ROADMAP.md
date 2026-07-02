# SandrPod Roadmap

A product-gap analysis and prioritized roadmap. SandrPod is AI code-execution
infrastructure — fast, secure sandboxes provisioned on demand across eight
clouds, consumed by AI agents (LangChain deepagents) through a Python SDK and
CLI. Reference points for feature parity: E2B, Modal, Daytona.

_Last updated: 2026-07-02. Status legend: ☐ open · ◐ partial · ☑ done._

---

## Where the product is solid today

- **Multi-cloud provisioning** — 8 providers (AWS, Aliyun, Azure, GCP, Tencent,
  DigitalOcean, Hetzner, Oracle) behind one interface; 5 validated end-to-end on
  live accounts. Two remote-exec backends (managed run-command / SSH) both
  proven live.
- **Cross-cloud architecture** — workers dial back over a WebSocket/yamux
  tunnel, so sandboxes on any cloud attach to one control plane with no inbound
  ports on workers.
- **Capacity-aware scheduling** — full poders (max containers) are skipped and
  a new VM is provisioned automatically; existing poders are reused (~3 s
  sandbox creation on a warm poder).
- **Sandbox feature base** — lifecycle (create/start/stop/delete), code
  execution (python/node/bash) with real exit codes, SSE streaming, stateful
  sessions, a full file API, logs; CLI + Python SDK cover all of it.
- **Employee-PC mode** — sandrpod-agent permission gate + audit pipeline +
  tray companion (separate product surface; see its own backlog note in P2).
- **Engineering hygiene** — CI, unit tests on provider mapping/pure logic,
  per-provider provisioning guides with honest validation status.

---

## P0 — production/open-source gate

These block serious external use. All must keep existing deployments working
(see [Upgrade policy](#upgrade-policy)).

### 1. Authentication & multi-tenancy ◐
Today the control plane has a **single shared bearer token**: anyone holding it
can delete every sandbox and terminate every VM. Needed, in order:
- ◐ **Named tokens** (a tokens file: `{name, token, role}`), so tokens are
  individually issuable/revocable; the legacy single `-token` keeps working.
- ◐ **Ownership**: sandboxes carry an `owner` (the token name that created
  them); user-role tokens only see/manage their own, admin sees all.
- ☐ Real tenancy later: per-tenant quotas, token CRUD API, key rotation.

### 2. Idle reclamation (cost safety) ◐
Cloud VMs currently run **forever** until a poder is deleted manually. For a
product that provisions cloud resources on behalf of users, not reclaiming idle
capacity is a trust problem, not just a feature gap.
- ◐ **Sandbox idle timeout** (opt-in): reap sandboxes whose `last_activity`
  exceeds a configurable TTL.
- ◐ **Empty-poder reclamation** (opt-in): a cloud poder with zero containers
  for longer than a configurable window is deleted and its VM terminated.
- ☐ Per-sandbox TTL override at create time (`ttl` request field).

### 3. Async create + job status API ◐
`POST /sandboxes` for a cloud provider blocks 2–5 minutes; intermediate proxies
routinely kill the connection (~137 s observed), and the job store has **no
user-facing read endpoint**, so a failed create is invisible to the caller.
Provisioning is already detached from the request context (a disconnect no
longer aborts it), but the right shape is:
- ◐ `async` create: return a job id immediately, provision in the background,
  expose progress/errors via **`GET /api/v1/jobs/{id}`**.
- ◐ CLI polls to completion by default (`--no-wait` to just get the job id).
- ☐ Webhooks / event stream for lifecycle transitions.

### 4. Transport security ☐
The documented production deployment is plain HTTP with the bearer token in
cleartext. Needed: first-class TLS (built-in cert config or a hard requirement
+ documented reverse-proxy pattern), and HTTPS-only guidance in every
provisioning doc.

### 5. Quotas & rate limiting ◐
Nothing stops a loop from provisioning unbounded VMs.
- ◐ `SANDRPOD_MAX_SANDBOXES_PER_OWNER` cap at create time.
- ☐ Per-endpoint rate limiting; per-owner VM/cost caps.

---

## P1 — competitive differentiation

What makes agent users choose (or leave) a sandbox product.

### 6. Port forwarding / preview URLs ☐
The most common agent task is "start a web service" — and today there is **no
way to reach it**. E2B/Daytona expose `https://<sandbox>.<domain>` previews.
The reverse tunnel already carries HTTP; extending it with per-sandbox port
routing is the natural next step.

### 7. Interactive shell (PTY) ☐
The toolbox already implements a PTY over WebSocket (`/pty/create`, `/pty/`),
but nothing exposes it: no `sandrpod-cli shell <name>`, no SDK support, and
WS pass-through over the tunnel is unverified. Finish the last mile.

### 8. Per-sandbox resource limits ☐
Containers run unconstrained; one busy sandbox can starve the other nine on
the same VM. Wire CPU/memory limits through the create request into the
container spec.

### 9. Custom images / templates ☐
The toolbox image is fixed per poder. Agent users want prebuilt environments
(deps preinstalled). Needs per-sandbox image selection end-to-end plus a
template registry story.

### 10. Snapshot / persistent workspaces ☐
Deleting a sandbox destroys all state; long agent tasks can't survive
interruption. Options: workspace volume persistence, container commit
snapshots, or object-storage sync.

### 11. SSH key persistence (SSH-backend providers) ☐
GCP/DO/Hetzner hold each VM's ephemeral SSH key **in process memory** — a
server restart orphans management of existing VMs (they can be reclaimed, not
bootstrapped/probed). Persist keys encrypted at rest, or move to cloud-native
key mechanisms.

### 12. Deleted-poder ghost re-registration ◐
A deleted poder's container often heartbeats once more and re-registers,
leaving an OFFLINE ghost record that needs a manual `poder delete --keep-vm`
(hit three times during live testing). Tombstone deleted poder IDs and reject
their re-registration.

---

## P2 — maturity & ecosystem

- **Observability** ☐ — structured logging with levels, Prometheus metrics,
  lifecycle events. Today it's `log.Printf` all the way down.
- **TypeScript SDK** ☐ — half the agent ecosystem is TS (LangChain.js, Vercel
  AI); only Python exists today.
- **Web console** ☐ — sandbox list/logs/usage dashboard; currently CLI/API only.
- **Provider completion** ☐ — Azure/Hetzner/Oracle not yet live-validated;
  ListVMs pagination missing on AWS/Aliyun/Tencent; instance catalogs are
  static and priceless; Tencent/DO VPC plumbing is a workaround; Oracle Flex
  sizing is hardcoded (1 OCPU/6 GB).
- **Upgrade & version management** ☐ — poder image version skew is real (old
  VMs keep old poders); needs version reporting + rolling upgrade guidance.
- **Employee-PC mode review** ☐ — the permission/audit/tray surface hasn't had
  the same review + live-validation pass the cloud path got.
- **Horizontal scale** ☐ — single server instance (SQLite/in-memory); the real
  ceiling is tunnel connection count (~30–40k goroutines per 10k sandboxes).
  Document the boundary; revisit multi-instance later.

---

## Upgrade policy

Every roadmap item must be deployable onto an existing installation (systemd
binary + SQLite DB + env drop-ins + already-running poders/VMs) without manual
surgery:

1. **New behavior ships opt-in or backward compatible.** The legacy single
   `-token` remains valid; idle reclamation and async create are off unless
   enabled; old CLI/SDK calls keep working against a new server.
2. **Schema changes are additive and self-migrating.** The SQLite store
   migrates on startup (`ALTER TABLE ... ADD COLUMN` guarded by inspection);
   in-memory stores need nothing.
3. **No poder protocol breaks.** Already-deployed poder containers (older
   images) must keep registering and serving; protocol additions must be
   optional fields.
4. **Documented path.** See [UPGRADING.md](UPGRADING.md) for the concrete
   steps per release.
