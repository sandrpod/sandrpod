<p align="center">
  <img src="assets/logo.png" alt="SandrPod" width="400"/>
</p>

<p align="center">
  <strong>Self-hosted execution infrastructure for AI agents.</strong><br/>
  Run agent code on any cloud, any machine — or none at all. Speak any SDK. Keep full control.
</p>

<p align="center">
  <em>Your clouds. Your machines. Your rules.</em>
</p>

<p align="center">
  <a href="https://pypi.org/project/langchain-sandrpod/"><img src="https://img.shields.io/pypi/v/langchain-sandrpod?color=3B82F6&label=langchain-sandrpod" alt="PyPI"/></a>
  <img src="https://img.shields.io/badge/self--hosted-open%20source-16A34A" alt="Self-hosted"/>
  <img src="https://img.shields.io/badge/clouds-8%20providers-0EA5E9" alt="Multi-cloud"/>
  <img src="https://img.shields.io/badge/E2B%20SDK-drop--in-8B5CF6" alt="E2B compatible"/>
  <img src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go" alt="Go"/>
  <img src="https://img.shields.io/badge/license-Apache%202.0-green" alt="License"/>
</p>

---

## Overview

**SandrPod** is the control plane for AI agent code execution — open source and
self-hosted. It turns your own infrastructure into a fleet of on-demand sandboxes
that agents create, run code in, and tear down, while you keep ownership of where
that code runs, what it can touch, and where the data lives.

Hosted sandbox services make you rent their runtime in their region. SandrPod
inverts that: **you** bring the substrate — any cloud, plain Docker, or a bare
machine — and SandrPod is the thin, portable layer that schedules, tunnels, and
governs execution across it. It rests on three pillars:

- **Run anywhere you own** — 8 clouds (incl. Aliyun & Tencent), Docker, or a
  machine with no Docker at all.
- **Speak any SDK** — a native REST/Python/TS API, LangChain/deepagents, and the
  **unmodified E2B SDK** as a drop-in.
- **Stay in control** — reverse-tunnel workers with zero inbound ports, an opt-in
  permission gate + decision audit, and self-hosted data that never leaves your infra.

---

## The three pillars

### 🌍 Run anywhere you own

One binary schedules sandboxes across whatever infrastructure you have:

**AWS · GCP · Azure · Aliyun · Tencent · DigitalOcean · Hetzner · Oracle** —
plus plain **Docker**, or a **bare machine with no Docker** via `sandrpod-agent`.

Aliyun and Tencent make China-region and data-residency deployments first-class —
something hosted services don't offer. Each provider has a guide under [`docs/`](docs/)
([AWS](docs/AWS_PROVISIONING.md), [GCP](docs/GCP_PROVISIONING.md),
[Aliyun](docs/ALIYUN_PROVISIONING.md), [Tencent](docs/TENCENT_PROVISIONING.md), …).
Remote exec uses each cloud's managed run-command API where it exists (AWS SSM,
Aliyun CloudAssist, Azure Run Command, Tencent TAT, Oracle Instance Agent) and an
ephemeral-key SSH path elsewhere (GCP, DigitalOcean, Hetzner).

### 🔌 Speak any SDK

SandrPod isn't tied to one client. It exposes a native REST API and Python/TS
SDKs, a first-class **LangChain/deepagents** backend, **and** the full **E2B
wire protocol** — so the *unmodified* `e2b` / `e2b-code-interpreter` SDKs work
against it with nothing but two env vars. Bring the ecosystem you already use;
migrate nothing.

### 🛡️ Stay in control

- **Zero inbound ports.** Workers dial *out* to the control plane over a WebSocket
  reverse tunnel — run them behind NAT, in a private subnet, or on a laptop.
- **Governance, not just isolation.** Opt-in employee-PC mode adds a per-machine
  permission gate (path consent, command denylist, PTY consent) and a decision
  audit pipeline (NDJSON + central HTTP upload) — turning "run agent code" into
  "*govern* what agents may touch on real machines."
- **Your data stays yours.** Self-hosted, Apache 2.0, no phone-home.

---

## SandrPod vs. hosted sandbox services

|  | Hosted (E2B, Modal, …) | **SandrPod** |
|---|---|---|
| **License** | Closed | Apache 2.0, open source |
| **Where it runs** | Their infra / region | Your cloud, Docker, or bare metal |
| **Clouds** | Vendor-managed | AWS · GCP · Azure · **Aliyun** · **Tencent** · DO · Hetzner · Oracle |
| **China regions** | ✗ | ✓ Aliyun + Tencent |
| **No-Docker mode** | — | `sandrpod-agent`: any machine becomes a sandbox |
| **SDKs** | Their SDK | Native + LangChain + **drop-in E2B** |
| **Inbound ports on workers** | n/a | **None** (reverse tunnel) |
| **Governance / audit** | — | Opt-in permission gate + decision audit |
| **Data residency** | Their region | Your account, your region |

---

## Drop-in E2B compatibility

One example of "speak any SDK": already have code on the E2B SDK? Point it at your
SandrPod and it just works — `Sandbox.create`, `files.*`, `commands.*`
(foreground/background/PTY), `run_code`, `watch_dir`, `get_metrics`, `pause`/`resume`:

```python
import os
os.environ["E2B_API_KEY"]     = "e2b_your_key"          # issued by your SandrPod
os.environ["E2B_API_URL"]     = "https://sandbox.you.com"   # control plane
os.environ["E2B_SANDBOX_URL"] = "https://sandbox.you.com"   # per-sandbox envd

from e2b import Sandbox                # the real, unmodified e2b SDK
sbx = Sandbox.create()
sbx.files.write("/tmp/hi.txt", "hello from my own cloud")
print(sbx.commands.run("cat /tmp/hi.txt").stdout)   # → hello from my own cloud

from e2b_code_interpreter import Sandbox as CI
ci = CI.create()
print(ci.run_code("import numpy as np; np.arange(6).sum()").text)   # → 15
```

For a zero-config drop-in (no env vars, just a domain), run the gateway behind a
wildcard domain — `SANDRPOD_E2B_DOMAIN=sandbox.you.com` + `*.sandbox.you.com` DNS
+ TLS. Full surface, wire-protocol details, and the coverage matrix (verified
against the **real, unmodified** E2B SDK over a real container) are in
**[docs/E2B_COMPAT.md](docs/E2B_COMPAT.md)**.

---

## Architecture

```
E2B SDK ──┐
Native SDK ├─→ API Server (Control Plane, :8080)
LangChain ─┤         ↕ WebSocket + yamux reverse tunnel
CLI ───────┘    Poder (Worker) ──→ Toolbox (Sandbox container)

                sandrpod-agent  ──→ (direct mode, any machine as a sandbox)
```

| Component | Description |
|-----------|-------------|
| **API Server** | The control plane: native REST + E2B-compatible gateway, sandbox CRUD, scheduling, tunnel proxying |
| **Poder** | Worker node. Persistent WebSocket tunnel to the control plane; manages Docker container lifecycle |
| **sandrpod-agent** | Registers the local machine directly as a sandbox with an embedded Toolbox — no Docker required |
| **sandrpod-tray** | Optional user-session GUI for employee-PC mode: tray icon + consent prompts + local settings page |
| **Toolbox** | Execution service inside each sandbox. PTY, file ops, background processes, sessions |

> **Employee-PC mode (opt-in)**: when `sandrpod-agent` runs on a real employee
> laptop rather than a server, enable a per-PC permission gate (path consent +
> command denylist + PTY consent) and a decision-audit pipeline that ships every
> allow/deny/warn event to a central HTTP endpoint. Both are off by default
> (`--permission-mode=off`) — see **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**.

---

## Getting Started

### 1. Start the control plane

```bash
# In-memory mode (default)
go run ./cmd/server -port 8080

# SQLite persistence (recommended)
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db
```

### 2. Add a worker (Docker)

```bash
docker run -d --name sandrpod-poder --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:8080 \
  -e SANDRPOD_TOOLBOX_IMAGE=sandrpod/toolbox:latest \
  sandrpod/poder:latest
```

> No inbound ports required — Poder dials out to the control plane over a WebSocket reverse tunnel.

Or turn **any machine into a sandbox** with no Docker at all:

```bash
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-machine
```

### 3. Run code — pick your SDK

```python
# Native
from langchain_sandrpod import SandrPodClient
sb = SandrPodClient(api_url="http://localhost:8080").get_sandbox("my-sandbox")
sb.execute("echo hello from SandrPod")

# …or the unmodified E2B SDK (set E2B_API_URL/E2B_SANDBOX_URL to your server)
from e2b import Sandbox
print(Sandbox.create().commands.run("echo hello from SandrPod").stdout)
```

### 4. Govern it (employee-PC mode)

```bash
sandrpod-tray serve                       # user-session tray + consent dialogs (🛡)
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-laptop \
  -permission-mode=prompt \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch
```

Permission modes: `off` (default) | `prompt` (consent dialog outside `work_dir`) | `strict` (silent deny outside `work_dir`).
Full architecture, `permissions.json` schema, tray CLI, and audit protocol: **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**.

---

## LangChain / deepagents Integration

```bash
pip install langchain-sandrpod
```

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

model = ChatOpenAI(model="gpt-4o", temperature=0)
client = SandrPodClient(api_url="http://localhost:8080")

sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "Write a quicksort and run it"}]})

# Or auto-create + clean up
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})
```

The backend also exposes richer per-sandbox capabilities directly:

```python
sb.run_code("x = 40", context="ctx1")          # stateful Jupyter-style kernel
sb.run_code("x + 2", context="ctx1")["text"]   # → "42" (x persisted)
sb.metrics()                                    # {cpu_count, cpu_used_pct, mem_*, disk_*}
with sb.watch_dir("/workspace") as w:           # filesystem watch
    events = w.get_new_events()
```

See [`pkg/sdk/python/langchain_sandrpod/examples/`](pkg/sdk/python/langchain_sandrpod/examples/) for full examples.

---

## CLI

```bash
pip install sandrpod-cli
sandrpod-cli set-api-url http://localhost:8080

# --provider: local | aws | gcp | azure | aliyun | tencent | digitalocean | hetzner | oracle
sandrpod-cli list
sandrpod-cli create my-sandbox --provider local --image sandrpod/toolbox:latest
sandrpod-cli create gpu-box --provider gcp --region asia-east1-a --instance-type e2-medium
sandrpod-cli execute my-sandbox "ls /workspace"     # one-shot (stateless)
sandrpod-cli stream my-sandbox "make build"         # real-time streamed output
sandrpod-cli run my-sandbox "z = 10" --context c1   # stateful kernel — z persists in context c1
sandrpod-cli stats my-sandbox                       # live CPU / memory / disk
sandrpod-cli fs watch my-sandbox /workspace         # print filesystem events
sandrpod-cli shell my-sandbox                       # interactive PTY
sandrpod-cli delete my-sandbox

# Poder management
sandrpod-cli poder list
sandrpod-cli poder delete <poder-id>
```

---

## API Reference

SandrPod speaks **two** HTTP surfaces: its own native REST API, and — when the
E2B gateway is enabled — the full E2B control-plane + `envd` protocol.

**Native API**

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/sandboxes` | List sandboxes |
| POST | `/api/v1/sandboxes` | Create sandbox |
| DELETE | `/api/v1/sandboxes/{name}` | Delete sandbox |
| POST | `/api/v1/sandboxes/execute` | Execute code |
| GET | `/api/v1/sandboxes/{name}/toolbox/*` | Proxy to Toolbox (file upload/download, etc.) |
| GET | `/api/v1/poders` | List Poder nodes |
| DELETE | `/api/v1/poders/{id}` | Delete a Poder record |

**E2B-compatible API** — `POST /sandboxes`, `GET /sandboxes/{id}`,
`/sandboxes/{id}/{pause,resume,metrics,connect}`, and the `envd`
Filesystem/Process connect-RPC services. See [docs/E2B_COMPAT.md](docs/E2B_COMPAT.md).

---

## Building

```bash
# One-shot: all platforms + sandrpod-tray (skips missing toolchains gracefully)
make build-all

# Local build (agent/server are CGO-free; tray requires CGO + native libs)
go build -o server        ./cmd/server
go build -o poder         ./cmd/poder
go build -o agent         ./cmd/agent
go build -o sandrpod-tray ./cmd/sandrpod-tray   # CGO required

# Cross-compile to dist/ (agent + server only, CGO=0)
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/server-linux-amd64 ./cmd/server
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-linux-amd64 ./cmd/agent
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -ldflags="-s -w" -o dist/sandrpod-agent-darwin-arm64 ./cmd/agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o dist/sandrpod-agent-windows-amd64.exe ./cmd/agent

# Docker images (amd64)
docker buildx build --platform linux/amd64 -f docker/Dockerfile.poder   -t sandrpod/poder:latest   --load .
docker buildx build --platform linux/amd64 -f docker/Dockerfile.toolbox -t sandrpod/toolbox:latest --load .
```

---

## License

Apache 2.0
