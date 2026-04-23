<p align="center">
  <img src="assets/logo.png" alt="SandrPod" width="400"/>
</p>

<p align="center">
  <strong>Lightweight sandbox infrastructure for AI agents</strong>
</p>

<p align="center">
  <a href="https://pypi.org/project/langchain-sandrpod/"><img src="https://img.shields.io/pypi/v/langchain-sandrpod?color=3B82F6&label=langchain-sandrpod" alt="PyPI"/></a>
  <img src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go" alt="Go"/>
  <img src="https://img.shields.io/badge/license-Apache%202.0-green" alt="License"/>
</p>

---

## Overview

**SandrPod** is a lightweight, open-source sandbox infrastructure platform built for AI agents. It provides fast, isolated code execution environments that agents can create, run code in, and tear down on demand.

At its core, SandrPod connects a central API server to worker nodes (Poder) via WebSocket reverse tunnels, with each sandbox running a Toolbox service that handles shell execution, file I/O, and persistent sessions. No open ports required on the worker side — everything flows through the tunnel.

### Key Features

- **Instant sandboxes** — spin up isolated execution environments in seconds via Docker
- **Agent-native API** — clean REST interface purpose-built for programmatic control
- **LangChain integration** — `langchain-sandrpod` connects sandboxes directly to deepagents, giving any LLM agent a full filesystem and shell
- **Persistent sessions** — maintain shell state (working directory, environment variables) across multiple calls
- **SQLite persistence** — optional durable state with a single `-db` flag; memory mode by default
- **Multi-node scheduling** — connect multiple Poder workers across regions; the scheduler picks the least-loaded node automatically
- **Direct agent mode** — register any machine as a sandbox without Docker using `sandrpod-agent`
- **Reverse tunnel architecture** — Poder dials in to the API Server; no inbound ports required on worker nodes

---

## Architecture

```
Client → API Server (Control Plane, :8080)
              ↕ WebSocket + yamux reverse tunnel
         Poder (Worker) ──→ Toolbox (Sandbox container)

         sandrpod-agent  ──→ (direct mode, local machine as sandbox)
```

| Component | Description |
|-----------|-------------|
| **API Server** | REST control plane. Handles sandbox CRUD, job scheduling, and proxies requests through tunnels |
| **Poder** | Worker node. Maintains a persistent WebSocket connection to the API Server and manages Docker container lifecycle |
| **sandrpod-agent** | Registers the local machine directly as a sandbox with an embedded Toolbox — no Docker required |
| **Toolbox** | Code execution service running inside each sandbox container. Supports PTY, file operations, and sessions |

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

### 3. Direct Agent Mode (no Docker)

```bash
go run ./cmd/agent -api-url=http://localhost:8080 -name=my-machine
```

### 4. Execute Code

```bash
curl -X POST "http://localhost:8080/api/v1/sandboxes/execute?sandbox=my-sandbox" \
  -H "Content-Type: application/json" \
  -d '{"language":"python","code":"print(\"hello world\")"}'
```

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

# Connect to an existing sandbox
sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "Write a quicksort and run it"}]})

# Or use the context manager to auto-create and clean up
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    result = agent.invoke({"messages": [...]})

# Direct file I/O (without the agent)
sb.upload_files([("/workspace/data.csv", csv_bytes)])
sb.download_files(["/workspace/output.txt"])
sb.read("/workspace/output.txt")    # → str
sb.execute("ls /workspace")         # → ExecuteResponse
```

See [`pkg/sdk/python/langchain_sandrpod/examples/`](pkg/sdk/python/langchain_sandrpod/examples/) for full examples.

---

## CLI

```bash
pip install sandrpod-cli

sandrpod-cli set-api-url http://localhost:8080

# Sandbox operations
sandrpod-cli list
sandrpod-cli create my-sandbox --provider-type local --image sandrpod/toolbox:latest
sandrpod-cli execute my-sandbox "ls /workspace"
sandrpod-cli delete my-sandbox

# Poder management
sandrpod-cli poder list
sandrpod-cli poder delete <poder-id>
```

---

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/sandboxes` | List sandboxes |
| POST | `/api/v1/sandboxes` | Create sandbox |
| DELETE | `/api/v1/sandboxes/{name}` | Delete sandbox |
| POST | `/api/v1/sandboxes/execute` | Execute code |
| GET | `/api/v1/sandboxes/{name}/toolbox/*` | Proxy to Toolbox (file upload/download, etc.) |
| GET | `/api/v1/poders` | List Poder nodes |
| DELETE | `/api/v1/poders/{id}` | Delete a Poder record |

---

## Building

```bash
# Local build
go build -o server  ./cmd/server
go build -o poder   ./cmd/poder
go build -o agent   ./cmd/agent

# Cross-compile to dist/
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
