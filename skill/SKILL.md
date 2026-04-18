---
name: sandrpod-cli
description: SandrPod CLI for sandbox and session management. Use when working with sandboxes (create/delete/start/stop/execute), persistent sessions (session create/exec/list/get/delete), or file operations (fs ls/cat/write). Commands: sandrpod-cli list, create, delete, start, stop, logs, execute, session, fs.
---

# SandrPod CLI Skill

SandrPod is an AI code execution infrastructure platform providing fast, secure, and scalable sandbox environments.

## CLI Installation

```bash
pip install -e pkg/sdk/python
```

## Configuration

```bash
# Set API URL (saves to ~/.sandrpod-cli/config.yaml)
sandrpod-cli set-api-url http://localhost:18080

# Show current API URL
sandrpod-cli get-api-url

# Or use environment variable
export SANDRPOD_API_URL=http://localhost:18080

# Or use CLI flag
sandrpod-cli --api-url http://localhost:18080 list
```

**Config priority**: CLI flag > Environment > Config file > Default

## Sandbox Commands

```bash
# List all sandboxes
sandrpod-cli list

# Create sandbox
sandrpod-cli create mybox --region local --provider-type local

# Get sandbox info
sandrpod-cli get mybox

# Start/Stop sandbox
sandrpod-cli start mybox
sandrpod-cli stop mybox

# Delete sandbox
sandrpod-cli delete mybox

# Get logs
sandrpod-cli logs mybox --tail=100

# Execute code (stateless)
sandrpod-cli execute mybox "echo hello"
sandrpod-cli execute mybox "cd /tmp && pwd"  # cd state NOT persisted
```

## Session Commands (Stateful Shell)

Sessions maintain state (cd, environment variables) across commands.

```bash
# Create session
sandrpod-cli session create mybox
sandrpod-cli session create mybox --session-id custom-id

# Execute in session (cd/env persist)
sandrpod-cli session exec mybox <session-id> "cd /tmp"
sandrpod-cli session exec mybox <session-id> "pwd"  # outputs /tmp

# List sessions
sandrpod-cli session list mybox

# Get session info
sandrpod-cli session get mybox <session-id>

# Delete session
sandrpod-cli session delete mybox <session-id>
```

## File Operations

```bash
# List directory
sandrpod-cli fs ls mybox --path=/workspace

# Read file
sandrpod-cli fs cat mybox /workspace/test.txt

# Write file
sandrpod-cli fs write mybox /workspace/test.txt "hello world"

# Search files (grep)
sandrpod-cli fs grep mybox "TODO" --path=/workspace

# File info
sandrpod-cli fs info mybox /workspace/test.txt
```

## Architecture

```
Client → API Server (Control Plane) → Proxy+Agent (Worker) → Toolbox (Sandbox)
```

- **Server** (port 8080): REST API control plane
- **Poder** (port 8081): Worker node service with Docker executor
- **Toolbox**: Code execution service inside sandbox container

## Project Structure

```
cmd/server/main.go      # API Server
cmd/poder/main.go      # Proxy + Agent
cmd/toolbox/main.go    # Toolbox service
pkg/poder/docker.go    # Docker Poder implementation
pkg/toolbox/           # Toolbox code execution engine
```
