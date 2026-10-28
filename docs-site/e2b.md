---
title: E2B drop-in
description: Point the unmodified E2B SDK at your own server — one environment variable, no code changes.
---

# E2B drop-in

If your agent already speaks E2B, it can speak to your own hardware. The SDK is
not patched, forked, or wrapped: set the domain and it builds the URLs itself.

## Server side

The E2B-compatible surface is switched on by a domain:

```bash
# control plane
SANDRPOD_E2B_DOMAIN=sandbox.example.com      # the surface's base domain
SANDRPOD_E2B_PROVIDER=local                  # which substrate new E2B sandboxes land on
SANDRPOD_E2B_REGION=local                    # must match the workers you run
```

With that set, two host families answer:

| Host | What it serves |
|---|---|
| `api.sandbox.example.com` | E2B control plane: `POST /sandboxes`, list, get, kill, set-timeout, pause/resume, metadata |
| `<port>-<sandboxID>.sandbox.example.com` | the sandbox itself: envd RPCs (`commands.run`, `files.*`), and any service on that port |

Both need DNS (`api.` and a `*` wildcard) and TLS. A reverse proxy that terminates
TLS for those hosts is enough; the paths are not rewritten.

## Client side

```python
import os
os.environ["E2B_DOMAIN"] = "sandbox.example.com"
os.environ["E2B_API_KEY"] = "e2b_..."       # see "Keys" below

from e2b import Sandbox

sb = Sandbox.create(timeout=300)
try:
    print(sb.commands.run("uname -s").stdout)
    sb.files.write("/tmp/notes.txt", "written from the SDK")
    print(sb.files.read("/tmp/notes.txt"))
finally:
    sb.kill()
```

That is the whole integration. The SDK's own validation, retries and streaming
behave exactly as they do against the hosted service.

## Keys

The E2B SDK validates the key **client-side**, before it sends anything: it wants
`e2b_<hex>` and refuses other spellings with `AuthenticationException`.

A hosting deployment hands out two spellings of one credential:

| Spelling | Used by |
|---|---|
| `sk_<hex>` | the CLI, the native REST API |
| `e2b_<hex>` | the E2B SDK (same store, same revocation, same workspace) |

On the commercial console both are minted from **Settings → API keys**; the
`format` field picks the spelling. On a self-hosted single-tenant server the
platform token works for both.

## What is verified

Against a real deployment, with the published `e2b` package and no patches:

| Call | Result |
|---|---|
| `Sandbox.create()` | sandbox id, `envdAccessToken` |
| `sb.commands.run("echo …")` | stdout and exit code |
| `sb.files.write` / `sb.files.read` | round-trip |
| `sb.kill()` | terminated |
| `GET <port>-<id>.…/health` | envd health |
| `POST /filesystem.Filesystem/Stat` | directory entry |

Ownership is enforced on this surface exactly as on the native API: a sandbox
created by one workspace is not visible to another, and placement is scoped to
that workspace's workers.

## The half that is easy to get wrong

Two failure modes are worth knowing before they cost you an afternoon:

1. **Routing by path only.** Mounting `/sandboxes` is enough for create and kill,
   and *not* enough for `commands.run`: envd lives at `<port>-<id>.<domain>` and
   its paths (`/health`, `/process`) are the native API's names too. If the host
   is not routed, the request reaches the control plane, which answers `200` with
   its own health document — the sandbox looks alive and every command is lost.
2. **Region mismatch.** E2B's default substrate region is `local`. A worker
   registered in another region is invisible to it, and creation fails with
   `no available local poder found` while the fleet is perfectly healthy.

## Compatibility notes

- `templateID` that looks like a container image (`repo/image:tag`) is used
  directly; other values fall back to the deployment's default image.
- The code-interpreter surface (`/execute`, `/contexts`) is served when
  `SANDRPOD_E2B_DOMAIN` is set; stateful contexts need the Jupyter side enabled on
  the worker.
- The full endpoint-by-endpoint status table is in
  [E2B_COMPAT.md](https://github.com/sandrpod/sandrpod/blob/main/docs/E2B_COMPAT.md).

## Next

- [REST API](/api) — the native surface, which does the same things with SandrPod's vocabulary.
- [Workers and tunnels](/workers) — where those sandboxes actually run.