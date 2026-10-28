---
title: Release notes
description: What each release changed, and which of those changes break something.
---

# Release notes

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
