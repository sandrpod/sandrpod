---
title: Python SDK
description: Drive SandrPod from Python — the native client, or the E2B SDK if you already use it.
---

# Python SDK

Two options, depending on what your code already imports.

## If you use E2B

Nothing to install beyond `e2b`. Point it at your server:

```python
import os
os.environ["E2B_DOMAIN"] = "sandbox.example.com"
os.environ["E2B_API_KEY"] = "e2b_..."

from e2b import Sandbox
sb = Sandbox.create()
print(sb.commands.run("echo hi").stdout)
sb.kill()
```

Details, key formats and what is verified: [E2B drop-in](/e2b).

## The native client

`pip install sandrpod-cli` also installs the client library the CLI uses.

```python
from sandrpod_cli.client import SandrPodClient

client = SandrPodClient(api_url="https://cloud.example.com", token="sk_...")

sb = client.create_sandbox("agent-01", provider="local")
result = client.execute("agent-01", "python3 -c 'print(2**10)'")
print(result["stdout"], result["exit_code"])

client.delete_sandbox("agent-01")
```

Or with plain HTTP, which is the whole contract the client wraps:

```python
import requests

API, TOKEN = "https://cloud.example.com", "sk_..."
H = {"X-Sandrpod-Token": TOKEN, "Content-Type": "application/json"}

requests.post(f"{API}/api/v1/sandboxes", headers=H,
              json={"name": "agent-01", "provider_type": "local"}).raise_for_status()

r = requests.post(f"{API}/api/v1/sandboxes/execute?sandbox=agent-01", headers=H,
                  json={"language": "python", "code": "print(2**10)"}).json()
print(r["stdout"])

requests.delete(f"{API}/api/v1/sandboxes/agent-01", headers=H)
```

## Files and streaming

```python
client.write_file("agent-01", "/workspace/in.txt", b"hello")
client.read_file("agent-01", "/workspace/in.txt")
for chunk in client.stream("agent-01", "for i in range(3): print(i)"):
    print(chunk, end="")
```

## Long-lived work

A sandbox stays up as long as its TTL allows. For an agent loop that runs for
hours, create once and reuse it; the filesystem persists between `execute` calls,
so state can live in files even though each call is a new process.

```python
sb = client.create_sandbox("work", provider="local", ttl_seconds=4 * 3600)
```

`ttl_seconds` reaps an idle sandbox; activity resets the clock.

## Errors

The client raises with the server's message. Two worth handling explicitly:

| Error | Meaning |
|---|---|
| `503 sandbox tunnel not available` | the worker holding this sandbox is not connected; retry after a few seconds, or recreate |
| `403 owner_required` | the credential is a member, not an owner: spending and access decisions belong to the owner |
