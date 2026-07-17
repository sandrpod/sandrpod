---
name: sandrpod-cli
description: SandrPod CLI for sandbox and worker management. Use when working with sandboxes (list/create/delete/start/stop), executing code (execute/stream/shell, stateful run with contexts), file operations (fs ls/cat/write/grep/upload/download/watch), MCP config (mcp ls/add/rm/url/tools), Poder worker nodes, API tokens, per-sandbox stats, preview ports, and snapshots.
---

# SandrPod CLI Skill

## Installation

```bash
pip install sandrpod-cli
```

## Configuration

```bash
export SANDRPOD_API_URL=http://localhost:8080
export SANDRPOD_API_TOKEN=<token>          # if the server requires auth
sandrpod-cli --api-url http://localhost:8080 list   # or per-command flag
```

## Sandboxes

```bash
sandrpod-cli list
sandrpod-cli get <name>                     # details
sandrpod-cli env <name>                     # runtime env (arch/OS/shell)
sandrpod-cli create <name> --provider local --image ghcr.io/sandrpod/toolbox:latest
sandrpod-cli create <name> --provider gcp --region asia-east1-a --instance-type e2-medium
sandrpod-cli create <name> --ttl 3600 --cpu 2 --memory 2048
sandrpod-cli create <name> --no-wait        # async: returns a job id
sandrpod-cli job get <job-id>               # poll async create
sandrpod-cli start|stop|delete <name>
sandrpod-cli logs <name> [--tail N]
sandrpod-cli stats <name>                   # per-sandbox CPU/mem/disk
sandrpod-cli snapshot <name> [--image repo:tag]
sandrpod-cli preview <name> <port> [path]   # reach a web service inside
```

## Execute code

```bash
sandrpod-cli execute <name> "ls /workspace"       # one-shot (stateless)
sandrpod-cli stream <name> "long-running-cmd"     # streaming output
sandrpod-cli shell <name>                         # interactive PTY (Ctrl-] to exit)

# Stateful interpreter (Jupyter-style; variables persist per context)
sandrpod-cli context create <name>                # prints ctx id
sandrpod-cli run <name> "z = 10" --context <ctx-id>
sandrpod-cli context list|restart|rm <name> [ctx-id]

# Legacy stateful shell sessions (cd/env persist)
sandrpod-cli session create|exec|list|delete <name> [session-id] [cmd]
```

## Files

```bash
sandrpod-cli fs ls|cat|write|mkdir|rm|mv|search|grep|info <name> ...
sandrpod-cli fs upload|download <name> <local> <remote>
sandrpod-cli fs replace <name> <file> <pattern> <new-value>
sandrpod-cli fs watch <name> <path> [--recursive]   # live fs events
```

## MCP (per-sandbox MCP bridge)

```bash
sandrpod-cli mcp ls <name>                          # servers in mcp.json
sandrpod-cli mcp add <name> <server> 'npx -y @modelcontextprotocol/server-github'
sandrpod-cli mcp rm <name> <server>
sandrpod-cli mcp url <name>                         # the /mcp endpoint
sandrpod-cli mcp tools <name> [--mcp-token <t>]     # aggregated tool list
```

## Admin

```bash
sandrpod-cli token create <name> [--role user|admin]  # bare key shown once
sandrpod-cli token list|rm <prefix>
sandrpod-cli poder list|get|delete <poder-id> [-y] [--keep-vm]
sandrpod-cli metrics                                   # server Prometheus text
```
