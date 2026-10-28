---
title: CLI
description: The sandrpod-cli reference — every command verified against a live deployment.
---

# CLI

```bash
pip install sandrpod-cli          # or: uv tool install sandrpod-cli
sandrpod-cli --version            # 0.7.0
```

```bash
export SANDRPOD_API_URL=https://cloud.example.com
export SANDRPOD_API_TOKEN=sk_...          # or the platform token on a single-tenant server
```

Every command below was run against a deployed server at version 0.7.0.

## Sandboxes

| Command | What it does |
|---|---|
| `create <name> [--provider p] [--region r] [--instance-type t] [--image i]` | create and wait for `RUNNING` |
| `create <name> --no-wait` | return a job id immediately; poll with `job get` |
| `create <name> --poder <id>` | skip the scheduler, place on a named worker |
| `create <name> --ttl 3600 --cpu 2 --memory 2048` | idle TTL, cores, MiB |
| `create <name> --allow-domain a.example --deny-domain b.example` | egress allow-list at creation |
| `list` / `get <name>` / `delete <name…>` | inventory |
| `start <name>` / `stop <name>` | resume / pause |
| `env <name>` | arch, OS, shell inside the sandbox |
| `stats <name>` | live CPU, memory, disk for that sandbox |
| `logs <name> [--tail N]` | container log |

## Code

| Command | What it does |
|---|---|
| `execute <name> "<code>"` | one shot, stateless |
| `stream <name> "<code>"` | same, output as it is produced |
| `shell <name>` | interactive PTY over WebSocket (`Ctrl-]` to exit) |
| `snapshot <name> [--image repo:tag]` | commit the container to an image |
| `preview <name> <port> [path]` | fetch a service running inside the sandbox |

## Files

| Command | Notes |
|---|---|
| `fs ls\|cat\|info\|mkdir\|rm` | paths are inside the sandbox |
| `fs write <name> <path> <content>` | content is an argument, not a local file |
| `fs upload <name> <local> <remote>` / `fs download <name> <remote> <local>` | binary-safe |
| `fs mv <name> <src> <dst>` / `fs replace <name> <path> <old> <new>` | |
| `fs search <name> <pattern>` / `fs grep <name> <pattern>` | **pattern only** — no path argument |

## Egress

| Command | What it does |
|---|---|
| `egress get <name>` | current allow-list; `null` when unconfigured |
| `egress set <name> <file\|-\|json>` | replace the policy |
| `egress clear <name>` | remove it (asks for confirmation; `-y` in automation) |

The policy applies when the container is created. Setting it on a running sandbox
stores it for the next creation — see [Egress policy](/egress).

## Stateful work

| Command | What it does |
|---|---|
| `run <name> "<code>" --context <id>` | execute in a stateful kernel whose variables persist |
| `context create\|list\|restart\|rm <name>` | manage kernels |
| `session list\|attach <name>` | long-lived shells |

Contexts require the code-interpreter path; on a deployment without it, `run`
executes statelessly.

## Jobs and history

| Command | What it does |
|---|---|
| `job get <job-id>` | status, error, result of an async create |

## Operator commands

These need the platform token, not a workspace key.

| Command | What it does |
|---|---|
| `poder list\|get\|delete` | the worker fleet; `delete --keep-vm` keeps the cloud VM |
| `metrics` | the control plane's Prometheus document |
| `token create <name> [--role user\|admin]` | mint an engine token (`e2b_<hex>`), printed once |
| `token list` / `token rm <prefix>` | inventory and revocation |
| `health` | the control plane's own health answer |

## Exit codes and errors

`0` success, non-zero failure with the server's message on stderr. The two worth
recognising:

```
Error: Error (HTTP 503): sandbox tunnel not available
```

The sandbox exists but its worker is not connected to this instance — see
[Troubleshooting](/troubleshooting).

```
Error: Error (HTTP 409): worker capability unavailable
```

The request needs a capability (an egress-enforcing worker, say) that no eligible
worker advertises. Refusing is deliberate: the alternative is a sandbox reported
as walled that runs unwalled.