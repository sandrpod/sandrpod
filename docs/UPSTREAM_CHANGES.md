# v0.6.0 → v0.7.0: what changed, who it affects, how to undo it

A release review needs three answers per change: **what breaks**, **who breaks**,
and **how to roll back**. This is that list. The narrative version is
[RELEASE_NOTES.md](RELEASE_NOTES.md); this one is for a diff review and an
upgrade plan.

Scope: 50 Go files, +3,497 / −2,519. Nothing in this release is on by default.

---

## 1. Breaking changes

All three are **Go API** changes. No HTTP request or response shape changed, no
flag or environment variable was removed, and the database only gains columns.

### 1.1 `SelectBestFor` takes a tenant

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

### 1.2 The version ldflags symbol moved

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

### 1.3 `cmd/server` is no longer where the server lives

`pkg/server` now holds `New`, `Main`, the handlers and the routes. `cmd/server`
is a thin `main` that calls `Main()`.

**Who breaks:** nobody can import `package main`, so this is only a source-layout
change. Code that read `cmd/server/*.go` for reference now reads `pkg/server`.

---

## 2. Behaviour changes (same shape, different meaning)

These do not break compilation and do not change any JSON field name. They change
what the values mean.

### 2.1 An unknown resource reading is negative

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

### 2.2 A join-token worker can heartbeat

`POST /api/v1/poders/{id}/heartbeat` now accepts a worker credential for **its
own** record. Before, it was admin-only, so join-token workers connected and then
stopped updating — visible as `403` in the log and an `ONLINE` worker whose state
never changed.

**Who breaks:** nobody. This is a widening. Anything that relied on the `403`
(an alert on it, say) should be removed.

### 2.3 A refusal at the create choke point fails the record

A sandbox blocked by an admission rule is now marked `ERROR` (job and record)
instead of remaining `PENDING` forever. Anything polling for `PENDING` to
disappear will now see it resolve.

---

## 3. New capabilities (additive, off by default)

| Addition | What it enables |
|---|---|
| `pkg/server.New` / `Main` / `NewE2BGateway` | embed the control plane in another program |
| `Config.Authenticator` / `Admission` / `Events` | plug in your own identity, quota and billing |
| `Config.E2BAPIKeys` + `NewSandbox.Tenant` | the E2B surface resolves its own keys and keeps tenant placement |
| `RoleWorker` + `workerRoute` | a join token can handshake and poll — and nothing else |
| `poders.workspace` column + `X-Poder-Workspace` | a worker belongs to a workspace |
| Sandbox create from `4.2 s` to `230 ms` | image is inspected, not pulled, when present |

---

## 4. What is **not** affected

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

## 5. Upgrade order

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

## 6. Verify your own deployment

```bash
# 1. No removed flags (should print nothing)
diff <(server-old -help 2>&1 | grep -oE '^\s+-[a-z-]+') \
     <(server-new -help 2>&1 | grep -oE '^\s+-[a-z-]+')

# 2. Version stamping still works
docker run --rm ghcr.io/sandrpod/server:v0.7.0 -version

# 3. Unknown readings are negative, not 0.5
curl -s -H "X-Sandrpod-Token: $TOKEN" http://localhost:8080/api/v1/poders \
  | jq '.poders[].usage'

# 4. Tenant placement holds
#    (two workspaces, a sandbox each, then check poder_id differs)
```
