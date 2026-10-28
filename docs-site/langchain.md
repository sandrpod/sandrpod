---
title: LangChain & deepagents
description: Give a deepagents agent a sandbox you own, in two lines.
---

# LangChain & deepagents

`sandrpod` ships a LangChain backend, so a deepagents agent can use a sandbox on
hardware you own instead of a hosted service.

```bash
pip install langchain-sandrpod
```

```python
import os
from langchain_sandrpod import SandrPodClient
from deepagents import create_deep_agent

client = SandrPodClient(api_url=os.environ["SANDRPOD_API_URL"],
                        token=os.environ["SANDRPOD_API_TOKEN"])

sb = client.get_sandbox("my-sandbox")
agent = create_deep_agent(model=model, backend=sb)
result = agent.invoke({"messages": [{"role": "user", "content": "plot the data in /workspace"}]})
```

Or let the client manage the sandbox lifetime:

```python
with client.sandbox("temp-sb") as sb:
    agent = create_deep_agent(model=model, backend=sb)
    agent.invoke({"messages": [{"role": "user", "content": "ls"}]})
# sandbox deleted on exit
```

## Environment

| Variable | Meaning |
|---|---|
| `SANDRPOD_API_URL` | your control plane |
| `SANDRPOD_API_TOKEN` | an `sk_` workspace key, or the platform token |

## If you already use `langchain-e2b`

Both backends drive the same twelve behaviours in the deepagents integration. The
E2B one works against SandrPod unchanged once `E2B_DOMAIN` points at your server —
see [E2B drop-in](/e2b). Choosing between them is a matter of which package you
already depend on, not which is more capable.

## Recipes

| Want | Do |
|---|---|
| Isolate one task | `with client.sandbox(...)` — deleted on exit |
| Reuse a prepared environment | `snapshot` a sandbox after setup, then create from that image |
| Stop an agent reaching the network | create with `--allow-domain` and list only what it needs |
| Keep a long conversation's files | create once, pass the same sandbox to every turn |
