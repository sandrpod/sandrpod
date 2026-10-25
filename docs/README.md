# SandrPod Documentation

Start with the project [README](../README.md) ([中文](../README.zh.md)) for
what SandrPod is and a quick start. This directory holds the deep-dive docs,
grouped below. Docs marked **(中文)** are written in Chinese.

## Architecture & concepts

| Doc | What it covers |
|---|---|
| [ARCHITECTURE.md](ARCHITECTURE.md) ([中文](ARCHITECTURE.zh.md)) | The implemented architecture: API Server, Poder workers, Toolbox, reverse tunnel, E2B gateway, permission gate, MCP bridge, state machine, request flows |
| [ROADMAP.md](ROADMAP.md) | Product-gap analysis vs E2B/Modal/Daytona and the prioritized plan |
| [SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md) (中文) | Anthropic Sandbox Runtime (srt) 拆解、与 SandrPod 的差异矩阵、实测记录，以及按优先级/工期排好的可借鉴清单 |

## Releasing

Every pinned version in the repo — the reference compose file, the eight
provider guides, the architecture headers, the Python SDK — is rewritten in one
pass, and CI fails if any of them disagree:

```bash
make bump-version VERSION=v0.6.0    # rewrite them all
make check-versions                 # what CI runs
```

## Deployment & operations

| Doc | What it covers |
|---|---|
| [PRODUCTION_DEPLOYMENT.md](PRODUCTION_DEPLOYMENT.md) | **The reference production deployment**: one host, PostgreSQL, wildcard TLS, the E2B surface on — with the acceptance sweep that says it works |
| [MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md) | N server instances behind a load balancer with shared PostgreSQL; cross-node tunnel routing |
| [SCALING.md](SCALING.md) | Capacity model: what actually limits a deployment and when to go multi-instance |
| [UPGRADING.md](UPGRADING.md) | In-place upgrades: compatibility policy, additive schema migration, rollback |
| [AUTH_AND_KEYS.md](AUTH_AND_KEYS.md) | Platform API tokens: issuance (`sandrpod-cli token`), roles, hash-at-rest, revocation |
| [LOGGING.md](LOGGING.md) | Log surfaces across server / poder / toolbox / agent and how to read them |

## Cloud providers

One guide per provider — credentials, env vars, instance defaults, and the
remote-exec backend each cloud uses.

That backend decides how much setup you are in for, so it is worth knowing
before you pick one to try first:

- **SSH, per-VM ephemeral keys** — GCP, DigitalOcean, Hetzner. An API token (or
  service-account JSON) is the whole credential story.
- **Managed run-command** — AWS (SSM), Aliyun (CloudAssist), Azure (Run
  Command), Tencent (TAT), Oracle (Instance Agent). These additionally need the
  launched VM to carry an identity the run-command service recognises — on AWS
  that is `AWS_IAM_INSTANCE_PROFILE`, and it is the single most-missed
  prerequisite.

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
| [E2B_MCP_COMPAT.md](E2B_MCP_COMPAT.md) ([中文](E2B_MCP_COMPAT.zh.md)) | The in-sandbox `mcp-gateway` shim (`:50005`) and how it maps to SandrPod's native MCP bridge |

## MCP (Model Context Protocol)

| Doc | What it covers |
|---|---|
| [MCP_BRIDGE.md](MCP_BRIDGE.md) | User guide: aggregate stdio/remote MCP servers from `mcp.json` into one `/mcp` endpoint; hot reload, tool filtering, permission grants |
| [MCP_AUTH.md](MCP_AUTH.md) ([中文](MCP_AUTH.zh.md)) | Protecting the bridge: two-layer auth (platform header + `mcp_token`), manifest exemption, and native OAuth for remote servers (Notion-style) |

## Employee-PC mode

| Doc | What it covers |
|---|---|
| [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) ([中文](PERMISSION_AND_AUDIT.zh.md)) | The consent gate (hardlock → work_dir → permanent → session → ask), native dialogs, `sandrpod-tray`, and the decision-audit upload pipeline |
| [RELEASE_NOTES.md](RELEASE_NOTES.md) | 0.6.0: what this version adds, the upgrade order (workers → server → agents), the Go API breaks, and the limits it does not hide |
| [APP_INTEGRATION.md](APP_INTEGRATION.md) ([中文](APP_INTEGRATION.zh.md)) | What an application team sees after upgrading: the (empty) API diff, the three visible differences inside the sandbox, the four kinds of tool that break by design, and three deployment profiles |
| [PLAYGROUND.md](PLAYGROUND.md) ([中文](PLAYGROUND.zh.md)) | A manual walkthrough that verifies every layer — gate, kernel, netns, egress, masking — with the real output of each check |
| [CREDENTIALS.md](CREDENTIALS.md) ([中文](CREDENTIALS.zh.md)) | Letting an agent use a credential it can never read: sentinel values, TLS termination on the proxy, substitution in headers/bodies/files and AWS SigV4 re-signing — with the trust it costs stated plainly, and the excludes that keep mTLS and pinning working |
| [OS_SANDBOX.md](OS_SANDBOX.md) ([中文](OS_SANDBOX.zh.md)) | The kernel floor under the other two: bubblewrap / Seatbelt wrapping of every spawned process, so `open()` and `connect()` fail where the gate and the proxy cannot see them — with the platform that is verified and the one that is not |
| [EGRESS_POLICY.md](EGRESS_POLICY.md) ([中文](EGRESS_POLICY.zh.md)) | What a sandboxed process may connect to: the allow-list format, the resolved-address check that stops an allowed name reaching loopback/metadata, the deny reasons the model sees, and the honest boundary (filter, not wall) |

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
