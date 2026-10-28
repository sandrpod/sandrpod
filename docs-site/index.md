---
title: What SandrPod is
description: Execution infrastructure for AI agents that runs on hardware you own — any cloud, any machine, or none at all.
---

# Execution infrastructure for AI agents

SandrPod turns the machines you already pay for into on-demand sandboxes for
agent code. It is a control plane, a worker, and an in-sandbox execution engine —
three processes you can run on one laptop, one server, or a fleet across eight
cloud providers.

The agent does not know the difference. Neither does its SDK.

<div class="pillars">

**Run anywhere you own**
: Eight cloud providers, plain Docker, or a bare machine with no Docker at all
  (`sandrpod-agent`). No inbound ports on workers: they dial out.

**Speak any SDK**
: Native REST, Python, TypeScript, LangChain / deepagents, and the **unmodified
  E2B SDK** as a drop-in. Change one environment variable, not your code.

**Stay in control**
: Self-hosted data, opt-in permission gate, decision audit, per-sandbox egress
  allow-lists, and credential masking the sandbox cannot see through.

</div>

## Sixty seconds

```bash
curl -O https://raw.githubusercontent.com/sandrpod/sandrpod/main/docker/docker-compose.yml
docker compose up -d
```

That pulls three published images and starts a control plane on
`localhost:8080` plus one Docker worker that dials back over a reverse tunnel.
No Go toolchain, no build, no clone.

```bash
pip install sandrpod-cli
sandrpod-cli create demo --provider local
sandrpod-cli execute demo "echo hello from a sandbox you own"
```

## Where each reader should start

| You want to | Read |
|---|---|
| Run a sandbox in the next five minutes | [Quickstart](/quickstart) |
| Keep using your E2B code | [E2B drop-in](/e2b) |
| Drive it from a script or an agent | [CLI](/cli) · [REST API](/api) |
| Understand the moving parts | [Architecture](/architecture) |
| Put it in front of real traffic | [Self-host in production](/deploy) |
| Run it as a paid service | [docs.sandrpod.com](https://docs.sandrpod.com) is the engine; the console is a commercial build |

## What this is not

SandrPod is not a hosted product you sign up for. The engine is Apache 2.0 and
runs entirely on infrastructure you control. There is a hosted console at
[cloud.sandrpod.com](https://cloud.sandrpod.com) built on the same engine, but
nothing in this documentation requires it.