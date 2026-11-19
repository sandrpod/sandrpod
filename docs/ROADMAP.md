# SandrPod Roadmap

A product-gap analysis and prioritized roadmap. SandrPod is AI code-execution
infrastructure — fast, secure sandboxes provisioned on demand across eight
clouds, consumed by AI agents (LangChain deepagents) through a Python SDK and
CLI. Reference points for feature parity: E2B, Modal, Daytona.

_Last updated: 2026-11-19. Status legend: ☐ open · ◐ partial · ☑ done._

> **Since the July pass:** v0.9 added desktop sandboxes, GPU sandboxes and
> registry-backed snapshots; v0.10 added worker enrolment (join token → worker
> key), the Obscura headless image and five desktop variants; v0.11 made sandbox
> names workspace-scoped with a global `routing_id`; v0.12 completed the E2B
> surface (snapshots, fork, live network updates, private ports) and gave every
> non-egress sandbox its own bridge. The per-release list is
> [CHANGELOG.md](../CHANGELOG.md); breaks are in
> [UPSTREAM_CHANGES.md](UPSTREAM_CHANGES.md).

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
- **E2B wire-protocol compatibility** — the unmodified official E2B SDKs work
  against a SandrPod deployment (control plane, envd files/process/PTY,
  stateful code interpreter with chart capture, pause/resume, metrics), all
  verified against the real SDK; see E2B_COMPAT.md.
- **Employee-PC mode** — sandrpod-agent permission gate + audit pipeline +
  tray companion (separate product surface; see its own backlog note in P2).
- **Engineering hygiene** — CI, unit tests on provider mapping/pure logic,
  per-provider provisioning guides with honest validation status.

---

## P0 — production/open-source gate

These block serious external use. All must keep existing deployments working
(see [Upgrade policy](#upgrade-policy)).

### 1. Authentication & multi-tenancy ☑
Named tokens, ownership, per-owner quotas and a persisted issuance API are in
place. The legacy single `-token` still works as an implicit admin, so existing
deployments did not have to migrate.
- ☑ **Named tokens** (a tokens file: `{name, token, role}`), individually
  issuable/revocable, **hot-reloaded** on change; the legacy single `-token`
  keeps working as an implicit admin.
- ☑ **Ownership**: sandboxes/jobs carry an `owner`; user-role tokens only
  see/manage their own, admin sees all; infra endpoints are admin-only.
- ☑ Per-owner quota (`SANDRPOD_MAX_SANDBOXES_PER_OWNER`) + per-identity rate
  limiting. ☑ Token CRUD API (`GET/POST/DELETE /api/v1/tokens`); ☐ automated key
  rotation remains.

### 2. Idle reclamation (cost safety) ☑
A cloud VM left running is a cost problem, not a feature gap. Both reapers ship
opt-in and are live-validated.
- ☑ **Sandbox idle timeout** (opt-in): reap sandboxes whose `last_activity`
  exceeds a configurable TTL.
- ☑ **Empty-poder reclamation** (opt-in): a cloud poder with zero containers
  for longer than a configurable window is deleted and its VM terminated.
- ☑ Per-sandbox TTL override at create time (`ttl_seconds` request field).
- ☑ Full chain **live-validated end-to-end on GCP** (short TTLs on EC2): idle
  sandbox auto-deleted → poder emptied → VM terminated + tombstoned + record
  removed; confirmed 0 orphan VMs in GCP afterward. Loop logic also has
  deterministic unit tests (`reapEmptyPodersOnce`).

### 3. Async create + job status API ☑
A cloud create can block 2–5 minutes, and intermediate proxies routinely kill
the connection (~137 s observed). The shipped shape:
- ☑ `async` create: returns a job id immediately, provisions in the
  background, progress/errors via **`GET /api/v1/jobs/{id}`**.
- ☑ CLI polls to completion by default (`--no-wait` to just get the job id).
- ☑ Lifecycle webhooks (`SANDRPOD_WEBHOOK_URL`) for sandbox/poder transitions.

### 4. Transport security ◐
- ☑ First-class built-in TLS (`-tls-cert`/`-tls-key`); the SDKs/console speak
  `wss://` automatically. ☑ HTTPS guidance lives in the provisioning docs'
  systemd sections and the wildcard-TLS walkthrough in
  MULTI_INSTANCE_DEPLOYMENT.md Part 4.

### 5. Quotas & rate limiting ☑
The caps exist and are enforced at the create choke point:
- ☑ `SANDRPOD_MAX_SANDBOXES_PER_OWNER` cap at create time.
- ☑ Per-identity request rate limiting (`SANDRPOD_RATE_LIMIT`). ◐ Per-owner
  VM/cost caps: the hosted admission gate enforces concurrency and storage
  budgets; the engine exposes the hooks, not a billing model.

---

## P1 — competitive differentiation

What makes agent users choose (or leave) a sandbox product.

### 6. Port forwarding / preview URLs ☑
The toolbox serves `/proxy/{port}/{path}` (reverse-proxy to `127.0.0.1:{port}`
inside the sandbox), reachable end-to-end at
`/api/v1/sandboxes/{name}/toolbox/proxy/{port}/...` and via `sandrpod-cli
preview`. Live-validated end-to-end (server → tunnel → embedded toolbox →
in-sandbox web service: 200 with body, subpaths, and 502 for a dead port).
☑ Vanity `https://<port>-<sandbox>.<domain>` hostnames ship with the E2B
gateway's host router (`SANDRPOD_E2B_DOMAIN`, see E2B_COMPAT.md).

### 7. Interactive shell (PTY) ☑
The server proxies `/sandboxes/{name}/pty` end-to-end over the tunnel and
`sandrpod-cli shell <name>` opens a raw-mode terminal (Ctrl-] to exit).

### 8. Per-sandbox resource limits ☑
`cpu_cores`/`memory_mb` on create flow through to the container HostConfig
(NanoCPUs/Memory) on local/docker poders (CLI `--cpu`/`--memory`).

### 9. Custom images / templates ◐
- ☑ Per-sandbox image selection end-to-end (`image_id` → poder → container).
- ☐ A template registry / prebuilt-environment catalog story.

### 10. Snapshot / persistent workspaces ◐
- ☑ Container-commit snapshots: `POST /sandboxes/{name}/snapshot` →
  `sandrpod-cli snapshot`, producing an image reusable as `image_id`.
- ☑ Snapshots push to a registry (`SANDRPOD_REGISTRY`), so a fork works on any
  worker; list and delete shipped in v0.9.
- ☑ `/workspace` persists in a named volume (`persist_data`, default on), so a
  stopped or restarted sandbox keeps its files.
- ☐ Cross-host restore of a workspace without the same worker / registry
  (object-storage sync).

### 11. SSH key persistence (SSH-backend providers) ☑
DigitalOcean and Hetzner persist each VM's ephemeral key (PKCS8 PEM, 0600)
under `SANDRPOD_SSH_KEY_DIR`, reloaded on demand, so a control-plane restart
no longer orphans existing VMs. (GCP injects via instance metadata, so it is
unaffected.)

### 12. Deleted-poder ghost re-registration ☑
Deleting a poder now tombstones its ID for 10 minutes and rejects the dying
container's re-registration (`410 Gone`); `keep_vm` deletions are exempt.
Validated live (ghost records no longer appear).

---

## P2 — maturity & ecosystem

- **Observability** ◐ — ☑ Prometheus `/metrics` (sandbox/poder/job counts,
  fleet capacity), lifecycle webhooks, and structured `slog` access logs in the
  server. ☐ Still open: distributed tracing and structured logging in the
  worker/toolbox binaries.
- **TypeScript SDK** ☑ — `@sandrpod/sdk`, dependency-free fetch client
  mirroring the Python SDK (`pkg/sdk/typescript`).
- **Web console** ◐ — ☑ embedded SPA at `/console` (sandbox cards, stats,
  create/exec/delete). ☐ Logs/usage drill-down still to come.
- **Provider completion** ◐ — ☑ ListVMs pagination added on AWS/Aliyun/Tencent.
  ☐ Still open: Azure/Hetzner/Oracle live validation; static/priceless instance
  catalogs; Tencent/DO VPC plumbing workaround; hardcoded Oracle Flex sizing.
- **Upgrade & version management** ◐ — ☑ [UPGRADING.md](UPGRADING.md) with an
  in-place, rehearsed procedure. ☐ Still open: persisted per-poder version
  reporting for rolling-upgrade visibility.
- **Employee-PC mode review** ◐ → mostly ☑ — ☑ security-reviewed; fixed three
  gate bypasses (case-insensitive-FS path/command matching, trailing-slash/
  symlink rule paths) + the uploader's at-least-once violation + first-run
  seeding, all with regression tests. ☑ Live-validated end-to-end on real
  macOS (APFS): work_dir allow, `~/.ssh` hardlock deny, `~/.SSH` case-variant
  deny (reason "hard-locked" — the fix), strict out-of-workdir deny, audit
  NDJSON with correct reasons, and the real osascript consent dialog →
  allow-permanent → persisted grant. ☐ Still open: Windows validation with the
  tray.
- **Horizontal scale** ☑ — ☑ boundary documented in [SCALING.md](SCALING.md)
  (connection-count / single-writer ceiling); PostgreSQL + multi-instance
  forwarding, durable create delivery and the `tunnel_owners` registry are
  implemented and operated. Remaining work is capacity tuning, not a missing
  mechanism.

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
