<p align="center">
  <img src="assets/logo.png" alt="SandrPod" width="400"/>
</p>

<p align="center">
  <strong>The open-source, self-hostable E2B alternative — bring your own cloud.</strong>
</p>

<p align="center">
  Point the <em>unmodified</em> E2B SDK at your own infrastructure. Run agent code
  sandboxes on AWS, GCP, Azure, Aliyun, Tencent, and more — no data leaves your cloud.
</p>

<p align="center">
  <a href="https://pypi.org/project/langchain-sandrpod/"><img src="https://img.shields.io/pypi/v/langchain-sandrpod?color=3B82F6&label=langchain-sandrpod" alt="PyPI"/></a>
  <img src="https://img.shields.io/badge/E2B%20SDK-drop--in%20compatible-8B5CF6" alt="E2B compatible"/>
  <img src="https://img.shields.io/badge/clouds-8%20providers-0EA5E9" alt="Multi-cloud"/>
  <img src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go" alt="Go"/>
  <img src="https://img.shields.io/badge/license-Apache%202.0-green" alt="License"/>
</p>

---

## Overview

**SandrPod** is an open-source, self-hostable sandbox platform for AI agents — a
drop-in alternative to hosted services like E2B. Agents create isolated code
execution environments, run code in them, and tear them down on demand, using
**the same E2B SDK you already know** — pointed at infrastructure you own.

Two things make it different from a hosted sandbox service:

1. **Drop-in E2B compatibility.** SandrPod speaks E2B's exact wire protocol
   (control plane + `envd`). The unmodified `e2b` / `e2b-code-interpreter` SDKs
   work against it with nothing but two env vars. No migration, no code changes.
2. **Bring your own cloud.** One binary schedules sandboxes across **eight cloud
   providers** — including **Aliyun and Tencent** — or on plain Docker, or on a
   bare machine with no Docker at all. Your agents' code and data stay on your
   infrastructure.

Under the hood a central API server connects to worker nodes (**Poder**) over
WebSocket reverse tunnels; each sandbox runs a **Toolbox** service for shell,
file I/O, PTY, and persistent sessions. Workers need **no inbound ports** —
everything flows through the tunnel.

---

## Why SandrPod

|  | Hosted E2B | **SandrPod** |
|---|---|---|
| **License** | Closed | Apache 2.0, open source |
| **Where it runs** | E2B's infra | Self-host anywhere |
| **Clouds** | E2B-managed | AWS · GCP · Azure · **Aliyun** · **Tencent** · DigitalOcean · Hetzner · Oracle |
| **China clouds** | ✗ | ✓ Aliyun + Tencent |
| **Data residency** | E2B's region | Your account, your region |
| **SDK** | E2B SDK | **The same E2B SDK** (drop-in) + native Python/TS + LangChain |
| **No-Docker mode** | — | `sandrpod-agent`: any machine becomes a sandbox |
| **Employee-PC guardrails** | — | Opt-in permission gate + decision audit |

---

## Drop-in E2B compatibility

Already have code on the E2B SDK? Point it at your SandrPod and it just works —
`Sandbox.create`, `files.*`, `commands.*` (foreground/background/PTY),
`run_code`, `watch_dir`, `get_metrics`, `pause`/`resume`:

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

For a true zero-config drop-in (no env vars, just a domain), run the gateway
behind a wildcard domain — `SANDRPOD_E2B_DOMAIN=sandbox.you.com` with
`*.sandbox.you.com` DNS + TLS. The full surface, wire-protocol details, and the
verified coverage matrix (tested against the **real, unmodified** E2B SDK over a
real container) are in **[docs/E2B_COMPAT.md](docs/E2B_COMPAT.md)**.

---

## Bring your own cloud

The provider layer (`pkg/provider/`) provisions sandbox-hosting VMs across:

**AWS · GCP · Azure · Aliyun · Tencent · DigitalOcean · Hetzner · Oracle**

Each has a provisioning guide under [`docs/`](docs/) (e.g.
[AWS](docs/AWS_PROVISIONING.md), [GCP](docs/GCP_PROVISIONING.md),
[Aliyun](docs/ALIYUN_PROVISIONING.md), [Tencent](docs/TENCENT_PROVISIONING.md)).
Remote execution uses each cloud's managed run-command API where available (AWS
SSM, Aliyun CloudAssist, Azure Run Command, Tencent TAT, Oracle Instance Agent)
and falls back to an ephemeral-key SSH path for the rest (GCP, DigitalOcean,
Hetzner). Prefer to run on your own hardware? Use plain Docker, or
`sandrpod-agent` for a machine with no Docker at all.

### Key features

- **Drop-in E2B SDK** — the unmodified `e2b` / `e2b-code-interpreter` SDKs, on your infra
- **8-cloud scheduling** — including Aliyun & Tencent; the scheduler picks the least-loaded node
- **Self-hostable & open** — Apache 2.0; your code and data never leave your cloud
- **Instant sandboxes** — isolated Docker environments in seconds
- **Full sandbox surface** — files, background commands, streaming, PTY, stateful code interpreter, directory watch, metrics, pause/resume
- **Direct agent mode** — register any machine as a sandbox without Docker via `sandrpod-agent`
- **Reverse-tunnel architecture** — workers dial in; no inbound ports required
- **LangChain / deepagents native** — `langchain-sandrpod` gives any agent a filesystem + shell
- **Persistent sessions** — shell state (cwd, env) survives across calls
- **SQLite persistence** — durable state with one `-db` flag; in-memory by default
- **Employee-PC mode (opt-in)** — path-consent permission gate + NDJSON decision audit

---

## Architecture

```
E2B SDK ─┐
Native SDK ├─→ API Server (Control Plane, :8080)
CLI ──────┘         ↕ WebSocket + yamux reverse tunnel
               Poder (Worker) ──→ Toolbox (Sandbox container)

               sandrpod-agent  ──→ (direct mode, local machine as sandbox)
```

| Component | Description |
|-----------|-------------|
| **API Server** | REST control plane + E2B-compatible gateway. Sandbox CRUD, job scheduling, tunnel proxying |
| **Poder** | Worker node. Persistent WebSocket tunnel to the API Server; manages Docker container lifecycle |
| **sandrpod-agent** | Registers the local machine directly as a sandbox with an embedded Toolbox — no Docker required |
| **sandrpod-tray** | Optional user-session GUI for employee-PC mode: tray icon + consent prompts + local settings page |
| **Toolbox** | Code execution service inside each sandbox. PTY, file ops, background processes, sessions |

> **Employee-PC mode (opt-in)**: when `sandrpod-agent` runs on a real
> employee laptop rather than a server, you can enable a per-PC permission gate
> (path consent + command denylist + PTY consent) and a decision-audit pipeline
> that ships every allow/deny/warn event to a central HTTP endpoint. Both layers
> are off by default (`--permission-mode=off`) and fully described in
> **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)**.

---

## Getting Started

### 1. Start the API Server

```bash
# In-memory mode (default)
go run ./cmd/server -port 8080

# SQLite persistence (recommended)
go run ./cmd/server -port 8080 -db sqlite:./data/sandrpod.db
```

### 2. Start a Poder Worker (Docker)

```bash
docker run -d --name sandrpod-poder --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:8080 \
  -e SANDRPOD_TOOLBOX_IMAGE=sandrpod/toolbox:latest \
  sandrpod/poder:latest
```

> Poder requires no open inbound ports. It establishes a WebSocket reverse tunnel to the API Server on startup.

### 3. Talk to it with the E2B SDK

```bash
pip install e2b
```

```python
import os
os.environ.update(E2B_API_KEY="e2b_key", E2B_API_URL="http://localhost:8080",
                  E2B_SANDBOX_URL="http://localhost:8080", E2B_VALIDATE_API_KEY="false")
from e2b import Sandbox
sbx = Sandbox.create()
print(sbx.commands.run("echo hello from SandrPod").stdout)
```

See **[docs/E2B_COMPAT.md](docs/E2B_COMPAT.md)** to enable the gateway
(`SANDRPOD_E2B_DOMAIN` / debug listener) and for the full compatibility matrix.

### 4. Direct Agent Mode (no Docker)

```bash
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-machine
```

### 5. Employee-PC Mode (permission gate + audit)

```bash
# Start the tray companion (user-session, shows 🛡 icon in the menu bar)
sandrpod-tray serve

# Start the agent with permission gate and audit upload
go run ./cmd/agent \
  -api-url=http://localhost:8080 \
  -name=my-laptop \
  -permission-mode=prompt \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch
```

Permission modes: `off` (default, legacy behavior) | `prompt` (consent dialog for paths outside `work_dir`) | `strict` (silent deny outside `work_dir`, headless servers).

See **[docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md)** for the full architecture, `permissions.json` schema, `sandrpod-tray` CLI reference, and audit HTTP protocol.

---

## LangChain / deepagents Integration

Prefer a native SDK over the E2B one? `langchain-sandrpod` wires sandboxes
straight into deepagents:

```bash
pip install langchain-sandrpod
```

```python
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent
from langchain_openai import ChatOpenAI

model = ChatOpenAI(model="gpt-4o", temperature=0)
client = SandrPodClient(api_url="http://localhost:8080")

# Connect to an existing sandbox
sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "Write a quicksort and run it"}]})

# Or use the context manager to auto-create and clean up
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})
```

See [`pkg/sdk/python/langchain_sandrpod/examples/`](pkg/sdk/python/langchain_sandrpod/examples/) for full examples.

---

## CLI

```bash
pip install sandrpod-cli
sandrpod-cli set-api-url http://localhost:8080

# Sandbox operations (--provider: local | aws | gcp | azure | aliyun | tencent | digitalocean | hetzner | oracle)
sandrpod-cli list
sandrpod-cli create my-sandbox --provider local --image sandrpod/toolbox:latest
sandrpod-cli create gpu-box --provider gcp --region asia-east1-a --instance-type e2-medium
sandrpod-cli execute my-sandbox "ls /workspace"
sandrpod-cli shell my-sandbox          # interactive PTY
sandrpod-cli delete my-sandbox

# Poder management
sandrpod-cli poder list
sandrpod-cli poder delete <poder-id>
```

---

## API Reference

SandrPod exposes **two** HTTP surfaces: its own native REST API, and — when the
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
