# SandrPod Documentation

Start with the project [README](../README.md) ([中文](../README.zh.md)) for
what SandrPod is and a quick start. This directory holds the deep-dive docs,
grouped below. Docs marked **(中文)** are written in Chinese.

## Architecture & concepts

| Doc | What it covers |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) **(中文)** | The implemented architecture: API Server, Poder workers, Toolbox, reverse tunnel, state machine, request flows |
| [ROADMAP.md](ROADMAP.md) | Product-gap analysis vs E2B/Modal/Daytona and the prioritized plan |

## Deployment & operations

| Doc | What it covers |
|---|---|
| [MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md) | N server instances behind a load balancer with shared PostgreSQL; cross-node tunnel routing |
| [SCALING.md](SCALING.md) | Capacity model: what actually limits a deployment and when to go multi-instance |
| [UPGRADING.md](UPGRADING.md) | In-place upgrades: compatibility policy, additive schema migration, rollback |
| [AUTH_AND_KEYS.md](AUTH_AND_KEYS.md) | Platform API tokens: issuance (`sandrpod-cli token`), roles, hash-at-rest, revocation |
| [LOGGING.md](LOGGING.md) | Log surfaces across server / poder / toolbox / agent and how to read them |

## Cloud providers

One guide per provider — credentials, env vars, instance defaults, and the
remote-exec backend each cloud uses (managed run-command API vs SSH with
per-VM ephemeral keys):

[AWS](AWS_PROVISIONING.md) ·
[Aliyun](ALIYUN_PROVISIONING.md) ·
[Azure](AZURE_PROVISIONING.md) ·
[GCP](GCP_PROVISIONING.md) ·
[Tencent](TENCENT_PROVISIONING.md) ·
[DigitalOcean](DIGITALOCEAN_PROVISIONING.md) ·
[Hetzner](HETZNER_PROVISIONING.md) ·
[Oracle](ORACLE_PROVISIONING.md)

## E2B compatibility

| Doc | What it covers |
|---|---|
| [E2B_COMPAT.md](E2B_COMPAT.md) | Using the unmodified E2B SDK against SandrPod: wire-protocol surface, domain routing, config |
| [E2B_MCP_COMPAT.md](E2B_MCP_COMPAT.md) **(中文)** | The in-sandbox `mcp-gateway` shim (`:50005`) and how it maps to SandrPod's native MCP bridge |

## MCP (Model Context Protocol)

| Doc | What it covers |
|---|---|
| [MCP_BRIDGE.md](MCP_BRIDGE.md) | User guide: aggregate stdio/remote MCP servers from `mcp.json` into one `/mcp` endpoint; hot reload, tool filtering, permission grants |
| [MCP_AUTH.md](MCP_AUTH.md) **(中文)** | Protecting the bridge: two-layer auth (platform header + `mcp_token`), manifest exemption, and native OAuth for remote servers (Notion-style) |

## Employee-PC mode

| Doc | What it covers |
|---|---|
| [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) **(中文)** | The consent gate (work_dir → hardlock → permanent → session → ask), native dialogs, `sandrpod-tray`, and the decision-audit upload pipeline |

## Agent skills

The repo ships two agent skills under [`skills/`](../skills/) (Claude-skill
format, usable by any agent framework that reads SKILL.md files):
[`sandrpod-cli`](../skills/sandrpod-cli/SKILL.md) — the full CLI surface as a
cheat sheet; [`register-mcp`](../skills/register-mcp/SKILL.md) — how an agent
running inside a sandbox safely self-registers MCP servers in `mcp.json`.

## design/ — historical archive

Early design notes kept for archaeology; each carries a header pointing to
the doc that describes current behavior. Not maintained:
[architecture v1](design/architecture-v1.md) ·
[horizontal scaling](design/horizontal-scaling.md) ·
[MCP transport bridge draft](design/mcp-transport-bridge.md) ·
[MCP auth header conflict fix](design/mcp-auth-header-conflict-fix.md)
