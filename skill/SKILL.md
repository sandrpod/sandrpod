---
name: sandrpod-cli
description: SandrPod CLI for sandbox and Poder management. Use when working with sandboxes (list/create/delete/start/stop/execute), Poder worker nodes (poder list/delete), file operations (fs ls/cat/write/grep), and persistent sessions (session create/exec/list/delete).
---

# SandrPod CLI Skill

## Installation

```bash
pip install sandrpod-cli
```

## Configuration

```bash
# Set API URL (saved to ~/.sandrpod-cli/config.yaml)
sandrpod-cli set-api-url http://localhost:8080

# Or use environment variable / per-command flag
export SANDRPOD_API_URL=http://localhost:8080
sandrpod-cli --api-url http://localhost:8080 list
```

## Sandbox Commands

```bash
sandrpod-cli list
sandrpod-cli create <name> --region local --provider-type local --image sandrpod/toolbox:latest
sandrpod-cli get <name>
sandrpod-cli start <name>
sandrpod-cli stop <name>
sandrpod-cli delete <name>

# Execute code (stateless — chain with && to maintain state)
sandrpod-cli execute <name> "echo hello"
sandrpod-cli execute <name> "cd /tmp && pwd"
```

## Poder Commands

```bash
sandrpod-cli poder list
sandrpod-cli poder delete <poder-id>        # prompts for confirmation
sandrpod-cli poder delete <poder-id> -y     # skip confirmation
```

## Session Commands (Stateful Shell)

Sessions maintain `cd` and environment variables across calls.

```bash
sandrpod-cli session create <name>
sandrpod-cli session exec <name> <session-id> "cd /tmp"
sandrpod-cli session exec <name> <session-id> "pwd"   # outputs /tmp
sandrpod-cli session list <name>
sandrpod-cli session delete <name> <session-id>
```

## File Operations

```bash
sandrpod-cli fs ls    <name> --path=/workspace
sandrpod-cli fs cat   <name> /workspace/file.txt
sandrpod-cli fs write <name> /workspace/file.txt "content"
sandrpod-cli fs grep  <name> "TODO" --path=/workspace
```
