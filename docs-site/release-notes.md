---
title: Release notes
description: What each release changed, and which of those changes break something.
---

# Release notes

## 0.13.10

Fixed JSON Schema compliance on platform `/mcp` (`sandbox_list`).

| Area | What changed |
|---|---|
| Platform MCP | Omitted empty `required` instead of emitting `"required": null` on `sandbox_list` (fixes strict MCP clients rejecting the tool), and added optional `limit` / `offset` pagination arguments to `sandbox_list` |

## 0.13.9

Unified platform MCP (`/mcp`) to discover and call any tool inside any sandbox.

| Area | What changed |
|---|---|
| Platform MCP | Replaced the 5 hardcoded `desktop_*` forwarding tools on `/mcp` with `sandbox_mcp_list` and `sandbox_mcp_call` (15 tools total), letting one static URL reach desktop, headless browser (Obscura), and `mcp.json` tools inside any sandbox |
| Routing & performance | Forwards via `/api/v1/sandboxes/{sandbox}/mcp` (works for both container and direct-agent sandboxes), caches `Mcp-Session-Id` per `(caller, sandbox)` for 5 minutes with transparent retry on session expiry |
| Security | Moves platform Bearer tokens to `X-Sandrpod-Token` (stripped before the tunnel) and forwards only an explicit `mcp_token`, so platform API keys never reach the sandbox |

## 0.13.8

Lifecycle events a metering or audit consumer can bill by.

| Area | What changed |
|---|---|
| Events | `sandbox.stopped` when a sandbox leaves `RUNNING`/`STOPPING` for `STOPPED`/`ERROR`, `sandbox.started` when it resumes — emitted from REST start/stop, E2B pause/resume, the idle reaper, heartbeat reconciliation and the job-status report |
| Payload | Same owner / CPU / memory / GPU / runtime fields as the other sandbox events, plus `from` |
| Compatibility | Additive: creation keeps `sandbox.running`, deletion keeps its event, and sinks that ignore unknown events are unaffected |

## 0.13.7

Three security fixes on the worker boundary and the file gate. If you run
employee-PC mode (`sandrpod-agent`) or issue API keys, upgrade.

| Area | What changed |
|---|---|
| File search | `files.find` / `files.search` authorize every path they read, not just the root: hard-locked files are skipped, symlinks cannot leave the authorized directory, `../` cannot climb out, and a FIFO can no longer hang a search |
| API keys | Revoking the last key no longer turns the server into an anonymous-admin endpoint; the index latches once it has held a key, the last key is refused unless a static token exists, and the refusal is atomic with the delete |
| Jobs | `GET /api/v1/jobs/poll` is removed: no client used it, and any worker credential — including a tenant's provision token — could read every tenant's pending create requests, environment variables included |

## 0.13.6

Five more AWS findings, each checked on a live account.

| Area | What changed |
|---|---|
| AWS state | A terminated instance reports `TERMINATED`, not `STOPPED` — a caller waiting for removal no longer sees a machine that never goes away |
| AWS region | `GetVM` reports the region it was asked for rather than the availability zone in that field |
| AWS cleanup | `Cleanup` refuses to run unless `SANDRPOD_AWS_ALLOW_CLEANUP=1`: it terminates every SandrPod instance in the region regardless of workspace |
| AWS capacity | `ListInstanceTypes` asks `DescribeInstanceTypes` (1 373 types in `us-east-1`, paginated); the old static list is the offline fallback |
| AWS billing | The EBS volume is tagged alongside the instance, so cost allocation and tag-based orphan searches see it |

## 0.13.5

The cloud providers, audited against what the scheduler assumes and then run on real accounts.

| Area | What changed |
|---|---|
| AWS | IMDSv2 required with a hop limit of 1; a per-call `ClientToken` so an SDK retry cannot bill a second instance; a public IP without a subnet and a region that differs from `AWS_REGION` are refused up front; `SANDRPOD_SPOT_AWS` requests spot; only `Success` counts as a successful SSM command; deleting an already-terminated instance is not an error; a create whose instance is not running is reported, not returned as pending |
| Tencent | TAT commands get 15 minutes (the default is 60 seconds, which a Docker install plus a cold image pull exceeds); commands that never reached the instance are retried; spot requires a max price (`SANDRPOD_SPOT_MAX_PRICE[_TENCENT]`); a delete racing an in-flight termination retries; zone/region mismatch is refused; a NAT deployment's "no public IP" is honoured |
| Deployment | On a mainland network, pin `SANDRPOD_TOOLBOX_IMAGE` / `SANDRPOD_EGRESS_IMAGE` so the scheduler pre-pulls them — the first create otherwise times out on the image pull |

## 0.13.4

A security narrowing, a scheduling fix, and spot capacity.

| Area | What changed |
|---|---|
| Security | a worker credential may only use the tunnel handshake, the job poll, its own heartbeat, the enrolment exchange, and the two calls an agent makes directly (capabilities, egress decisions); the sandbox API is refused in the middleware from an explicit list |
| Scheduling | an async create is `PROVISIONING` while a VM boots, so the resume pass no longer fails a multi-minute provisioning as a lost delivery; abandoned rows are reclaimed after 30 minutes |
| Cloud | Aliyun spot behind `SANDRPOD_SPOT_ALIYUN` (`SpotAsPriceGo`); off by default, on-demand unchanged |
| Ops | `poder -version`; `deploy/prometheus/sandrpod-alerts.yml` with five alerts built on `/metrics` |

## 0.13.3

Exports `server.VerifyProvisionToken` for embedding builds.

| Area | What changed |
|---|---|
| Embedding | a program that supplies its own `Config.Authenticator` never runs the built-in token scheme; it can now verify the per-VM credential the scheduler mints, instead of rejecting every provisioned worker |

No behaviour change for a server that uses the built-in authenticator.

## 0.13.2

Two fixes: one narrows what a provisioned machine is allowed to do, the other
makes a delivery that was never confirmed resumable.

| Area | What changed |
|---|---|
| Security | a VM the scheduler provisions gets a per-VM credential (worker role, scoped to the requesting workspace, signed with the platform secret) instead of the platform token, so it cannot administer the fleet |
| Reliability | the outbox claim compares the timestamp a row actually carries; a job written by an import, a `psql` insert or an older build no longer sits `PENDING` forever with nothing in the log |
| Testing | `SANDRPOD_RESUME_STALE_AFTER` / `SANDRPOD_RESUME_INTERVAL` let the multi-instance check run the resumer on a fast clock instead of racing the production cadence |

No schema change. Workers do not have to move to use either fix.

## 0.13.1

Fixes persistence on the pinned-create path used by the console.

| Area | What changed |
|---|---|
| Persistence | a create that pins a worker now sets `persist_data` by default, as the scheduled path already did; stopping and starting a desktop sandbox no longer loses `/workspace` |

## 0.13.0

The first release with an **opt-in gVisor runtime**.

| Area | What changed |
|---|---|
| Runtime | `runtime: "gvisor"` (CLI `--runtime gvisor`) runs a sandbox under `runsc`; workers advertise a `gvisor` capability, and a request without a capable worker is refused with `409` |
| Node install | `deploy/node-install.sh` installs and registers gVisor by default; `--no-gvisor` opts out |
| Networking | gVisor sandboxes get an upstream resolver through `SANDRPOD_GVISOR_DNS`; the egress sidecar is mapped in `/etc/hosts`, including after live network updates |
| Limits | snapshots/fork are refused with `409` under gVisor (`docker commit` does not capture runsc writes); GPU + gVisor is a `400` |
| CLI | `sandrpod-cli 0.7.3` adds `--runtime` |

## 0.12.1

Fixes for the E2B snapshot and fork paths added in 0.12.0.

| Area | What changed |
|---|---|
| E2B fork | the child is created on the worker that already holds the source snapshot instead of being scheduled somewhere the image does not exist |
| E2B snapshots | the ID returned by `create_snapshot` is the familiar `repo:tag` Docker actually stores, so `list_snapshots` and `delete_snapshot` see the same value |
| Placement | a pinned create follows the same tenant rule as normal scheduling; an operator-owned sandbox can fork back onto its source worker |

## 0.12.0

The E2B surface became complete enough to run the SDK without caveats, and
same-workspace sandboxes stopped sharing a network.

| Area | What changed |
|---|---|
| E2B snapshots | `create_snapshot`, `list_snapshots` and `delete_snapshot` map onto worker snapshot images; deletion removes both the local tag and the registry copy |
| E2B fork | `fork` snapshots the source and starts the requested count, returning per-item success/error |
| E2B network updates | `PUT /sandboxes/{id}/network` replaces a running sandbox's egress sidecar; adding a policy to a sandbox that never had one is refused because Docker cannot move a live container |
| Private ports | `allowPublicTraffic=false` makes every public in-sandbox port require a signed URL or API key |
| Isolation | every non-egress sandbox gets its own bridge (`sandrpod-s-<hash>`); the worker attaches itself and removes it with the sandbox |
| Limits | CPU/memory/storage have safe defaults instead of reaching Docker as "unlimited"; ownerless legacy records are admin-only |
| Egress | the sidecar image is pulled on demand instead of failing a policy create on a fresh worker |
| CLI | `sandrpod-cli` 0.7.2 is released independently (snapshot `--timeout`, `fs grep` envelope handling, 0600 config) |

## 0.7.1

Fixes for the paths that only exist on a real deployment.

| Area | What changed |
|---|---|
| PostgreSQL placement | worker selection used a SQLite-only SQL function, so on PostgreSQL no query matched and the scheduler reported "no worker registered within 30s". Placement works. |
| Multi-instance routing | a create now crosses instances (`POST /internal/provision`) instead of failing when the chosen worker is tunneled to a peer; the tunnel-owner claim is renewed by heartbeats, so forwarding no longer expires 90 s after a worker connects |
| Delivery durability | a create carries its payload in `jobs.request` and is resumed by any instance after a crash; the claim is a compare-and-swap, so exactly one delivers |
| Deleted sandboxes | the resumer no longer re-creates a sandbox whose record was deleted |
| E2B envd | envd hosts are routed to the gateway (they used to reach the control plane), and per-sandbox envd tokens are accepted when a hosting build supplies its own key resolver |
| CLI | `sandrpod-cli` 0.7.1 published; `egress`, `session` and the current `fs` semantics are in the released package |

## 0.7.0

The release that made the engine embeddable.

| Change | Breaking? |
|---|---|
| `pkg/server` holds the API server (`cmd/server` is a thin `main`) | Go API: the version ldflags symbol moved |
| `SelectBestFor(region, provider, require)` gained a `tenant` parameter | Go API: compile error, one-line migration |
| `Config.Authenticator` / `Admission` / `Events` | additive |
| `RoleWorker`: a join token reaches two routes and nothing else | additive |
| An unknown resource reading is `-1` instead of `0.5` | behaviour: dashboards that multiply by 100 must handle the sign |
| Sandbox create from 4.2 s to 230 ms (warm image) | — |

Per-change detail, with the migration for each break:
[UPSTREAM_CHANGES.md](https://github.com/sandrpod/sandrpod/blob/main/docs/UPSTREAM_CHANGES.md).

## Support policy

The latest tagged release is supported. Older tags receive security fixes for 90
days. `main` is development and is not a release.

## Changelog

The running changelog, including fixes that did not warrant a release note, is
[CHANGELOG.md](https://github.com/sandrpod/sandrpod/blob/main/CHANGELOG.md).
