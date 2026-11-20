# Upgrade breaks by release

A release review needs three answers per change: **what breaks**, **who breaks**,
and **how to roll back**. The current release is **v0.13.1**; the running list is
[CHANGELOG.md](../CHANGELOG.md). The first section covers everything since
v0.8.0, the second preserves the detailed v0.6.0 → v0.7.0 review.

The native HTTP API has stayed additive across these releases. The breaks below
are credentials, placement/name semantics, and defaults that used to mean
"unlimited".

## Unreleased — gVisor runtime (additive)

`POST /api/v1/sandboxes` and the pinned-create route accept an optional
`runtime` (`runc` or `gvisor`). Omitted means the existing default. A gVisor
request is refused with `409` when no online worker advertises the `gvisor`
capability, and `runtime=gvisor` plus `gpu>0` is a `400` in this iteration.
No existing request or response field changed; `SandboxInfo` gains
`runtime` (empty for rows created before this build).

## v0.8.0 → v0.13.0

### v0.11.0 — sandbox names are workspace-scoped

`POST /api/v1/sandboxes` still takes the name you choose, but it is no longer a
global key. Each record also carries an immutable `routing_id` (`sb-<hex>`), and
that is what preview hosts and the E2B surface address.

**Who breaks:** a client that stored a display name and assumed it was unique
across the deployment, or that builds `<port>-<name>.<domain>` itself.

**Migrate:** read `routing_id` from create/get/list and use it in bookmarks and
scripts. Lookups by display name still resolve inside the caller's workspace.

**Roll back:** v0.10.x reads the same rows; display names are still present.

### v0.12.0 — defaults stopped being zero, and ownerless records became private

- A create that omits `cpu_cores`, `memory_mb` or `storage_mb` now reserves
  1 vCPU / 1 GiB / 2 GiB. Previously the zero reached Docker, which reads zero as
  **unlimited**. A deployment that relied on unbounded sandboxes must set the
  fields explicitly.
- Records with an empty owner (legacy rows from before ownership existed) are
  visible only to an admin. A member key that used to list them now sees nothing.
- Every non-egress sandbox gets its own bridge (`sandrpod-s-<hash>`) instead of a
  shared tenant bridge. Anything that addressed a sandbox by the old shared
  network name must use the tunnel/toolbox path.

**Roll back:** v0.11.x ignores the new columns and reads the records; containers
are recreated on the old network model.

### v0.8.0 → v0.10.3 — the join token became an invitation

A join token is now one hour, one workspace, exchanged once for a per-machine
**worker key** (180 days, revocable per machine). A worker stores the key under
`/var/lib/sandrpod/worker-key` and ignores the token on its command line after
enrolment.

**Who breaks:** only a hosted build that issued long-lived join tokens as the
permanent worker credential. A self-hosted server with a static `-token` keeps
working unchanged.

**Migrate:** upgrade the control plane first, then let the worker reconnect; it
enrols automatically and persists the key. Give the worker's `/var/lib/sandrpod`
a durable volume, or every recreate needs a fresh invitation.

**Roll back:** the previous worker image still accepts the invitation; the worker
key file is additive and ignored by older builds.

### v0.13.0 — no new breaks

Snapshot ids are now the familiar `repo:tag`, so a client that stored the
`docker.io/...` spelling returned by v0.12.0 should re-list snapshots once.
Forks and pinned creates use the same tenant rule as placement.

---

## v0.6.0 → v0.7.0: what changed, who it affects, how to undo it

A release review needs three answers per change: **what breaks**, **who breaks**,
and **how to roll back**. This is that list. The narrative version is
[RELEASE_NOTES.md](RELEASE_NOTES.md); this one is for a diff review and an
upgrade plan.

Scope: 50 Go files, +3,497 / −2,519. Nothing in this release is on by default.

---

### 1. Breaking changes

All three are **Go API** changes. No HTTP request or response shape changed, no
flag or environment variable was removed, and the database only gains columns.

#### 1.1 `SelectBestFor` takes a tenant

```go
// v0.6.0
func (s *PoderStore) SelectBestFor(region, providerType string, require []string) (*PoderInfo, error)

// v0.7.0
func (s *PoderStore) SelectBestFor(region, providerType string, require []string, tenant string) (*PoderInfo, error)
```

**Who breaks:** any Go code calling it. It fails to compile — the good kind of
break. Inside this repository there is one caller (the scheduler); outside it,
the commercial build, which moved in lockstep.

**Migration:** pass `""` for the old behaviour (any worker), or `Identity.Tenant`
to scope placement to one tenant. `SelectBest` still exists and is unchanged for
unscoped callers.

**Why it changed:** a sandbox was placed on any capable worker regardless of who
owned it. The commercial build runs several tenants on one fleet.

#### 1.2 The version ldflags symbol moved

```bash
# v0.6.0
go build -ldflags "-X main.version=v0.6.0" ./cmd/server

# v0.7.0
go build -ldflags "-X github.com/sandrpod/sandrpod/pkg/server.version=v0.7.0" ./cmd/server
```

**Who breaks:** only custom builds that stamp a version. The failure is silent —
the binary reports `dev` instead of erroring — which is why it is listed here.

**Why it changed:** the API server moved from `package main` into `pkg/server`,
so there is no `main.version` to set.

#### 1.3 `cmd/server` is no longer where the server lives

`pkg/server` now holds `New`, `Main`, the handlers and the routes. `cmd/server`
is a thin `main` that calls `Main()`.

**Who breaks:** nobody can import `package main`, so this is only a source-layout
change. Code that read `cmd/server/*.go` for reference now reads `pkg/server`.

---

### 2. Behaviour changes (same shape, different meaning)

These do not break compilation and do not change any JSON field name. They change
what the values mean.

#### 2.1 An unknown resource reading is negative

`GET /api/v1/poders` → `usage.cpu_usage` / `usage.memory_usage`.

| | v0.6.0 | v0.7.0 |
|---|---|---|
| worker can read `/proc` | fraction 0..1 | fraction 0..1 (unchanged) |
| worker cannot read `/proc` | **`0.5`** | **`-1`** |

**Who breaks:** dashboards that multiply by 100. `-1 × 100` renders "−100%",
which is at least obviously wrong; the old `0.5` rendered a confident, healthy
"50%" for a worker that knew nothing.

**Fix:** treat negative as unknown — `if v < 0 { show("—") } else { v*100 }`.

**Why it changed:** the pode gated its `/proc` read behind a Docker API call that
fails inside a container, so every containerised worker reported `0.5`. A number
nobody can trust argues against looking at the dashboard at all.

#### 2.2 A join-token worker can heartbeat

`POST /api/v1/poders/{id}/heartbeat` now accepts a worker credential for **its
own** record. Before, it was admin-only, so join-token workers connected and then
stopped updating — visible as `403` in the log and an `ONLINE` worker whose state
never changed.

**Who breaks:** nobody. This is a widening. Anything that relied on the `403`
(an alert on it, say) should be removed.

#### 2.3 A refusal at the create choke point fails the record

A sandbox blocked by an admission rule is now marked `ERROR` (job and record)
instead of remaining `PENDING` forever. Anything polling for `PENDING` to
disappear will now see it resolve.

---

### 3. New capabilities (additive, off by default)

| Addition | What it enables |
|---|---|
| `pkg/server.New` / `Main` / `NewE2BGateway` | embed the control plane in another program |
| `Config.Authenticator` / `Admission` / `Events` | plug in your own identity, quota and billing |
| `Config.E2BAPIKeys` + `NewSandbox.Tenant` | the E2B surface resolves its own keys and keeps tenant placement |
| `RoleWorker` + `workerRoute` | a join token can handshake and poll — and nothing else |
| `poders.workspace` column + `X-Poder-Workspace` | a worker belongs to a workspace |
| Sandbox create from `4.2 s` to `230 ms` | image is inspected, not pulled, when present |

---

### 4. What is **not** affected

Verified by diffing the two tags, not by reading the release notes:

| Surface | Evidence |
|---|---|
| Database | `ALTER TABLE … ADD COLUMN` only — `sandboxes.cpu_cores`, `sandboxes.memory_mb`, `poders.workspace`. No `DROP`, no `RENAME`, so a 0.6 binary still runs against a 0.7 schema |
| Flags and env vars | the flag set is identical; `-rate-limit` is new |
| HTTP API fields | no `json` tag was removed (a struct moved files, which shows up as a diff but not as a shape change) |
| Python CLI, `langchain-sandrpod`, TypeScript SDK | no interface change |
| Docker image names and tags | unchanged |
| E2B surface paths and semantics | unchanged |

---

### 5. Upgrade order

1. **Workers first.** A 0.6 worker ignores the tenant header. A 0.7 control plane
   refuses to place a tenant-scoped sandbox onto it — `409 worker capability
   unavailable` — rather than co-locating two tenants on one machine.
2. **Control plane.** Migrations are additive; the old release still runs.
3. **Agents** last; they only call the API.

**Rollback:** point the image tag back. The schema is forward-compatible by
construction, so a rollback needs no migration. The one thing a rollback does not
undo is a tenant-scoped placement decision already made — move those sandboxes
explicitly.

---

### 6. Verify your own deployment

```bash
# 1. No removed flags (should print nothing)
diff <(server-old -help 2>&1 | grep -oE '^\s+-[a-z-]+') \
     <(server-new -help 2>&1 | grep -oE '^\s+-[a-z-]+')

# 2. Version stamping still works
docker run --rm ghcr.io/sandrpod/server:v0.13.1 -version

# 3. Unknown readings are negative, not 0.5
curl -s -H "X-Sandrpod-Token: $TOKEN" http://localhost:8080/api/v1/poders \
  | jq '.poders[].usage'

# 4. Tenant placement holds
#    (two workspaces, a sandbox each, then check poder_id differs)
```
