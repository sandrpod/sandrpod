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
| `api.sandbox.example.com` | E2B control plane: `POST /sandboxes`, list, get, kill, set-timeout, pause/resume, metadata, snapshots, fork, network updates |
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

Against a real deployment, with the published `e2b` package and no patches.
Re-run on **v0.12.1** (2026-11-19): `e2b` 2.64.0 passes **38/38**; 2.30.0 passes
37/38 because that client predates `Sandbox.fork`.

| Call | Result |
|---|---|
| `Sandbox.create()` | sandbox id, `envdAccessToken` |
| `sb.commands.run("echo …")` | stdout and exit code, streaming and background |
| `sb.files.write` / `sb.files.read` | round-trip, batch writes, signed download/upload URLs |
| `sb.pty.*` | interactive PTY, resize, kill |
| `sb.kill()` | terminated |
| `sb.create_snapshot()` / `Sandbox.list_snapshots()` | snapshot id (`repo:tag`), list, delete |
| `sb.fork()` | N children from a snapshot of the source |
| `sb.update_network()` | replaces a running sandbox's egress sidecar |
| `create(network={"allow_public_traffic": False})` | every public port requires a credential |
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
- `Sandbox.create(envs={...})` is applied to the sandbox container, so commands,
  PTYs and the code-interpreter kernel inherit it; a per-command `envs=` still
  overrides it for that call.
- `create_snapshot`, `list_snapshots` and `delete_snapshot` map onto worker
  snapshot images. The id is the Docker `repo:tag`, and deletion removes both
  the local tag and the registry copy when one exists.
- `fork()` snapshots the source, then starts the requested children **on the
  worker that holds that image**. E2B returns per-child success/error, so one
  refused child does not hide the rest.
- `update_network()` replaces the egress sidecar of a running sandbox. Adding a
  first policy to a sandbox that never had one is refused with `409`: Docker
  cannot move a live container onto a different network.
- `cpu_count` is honoured for E2B creates (the default is 2 vCPU); `get_info`,
  list and `get_metrics` report the values stored on the record.
- `download_url`/`upload_url` carry the SDK's signed URL and work without an
  extra header, as they do on E2B.
- `get_host(port)` URLs for ordinary services are fetchable without a credential
  (the unguessable hostname is the capability). The toolbox control port
  **8080 is never proxied**. Desktop (5900/6080/6901) and debugger (9222/9223)
  ports still need the console's signed URL or an API key, since they expose an
  interactive machine. `network={"allow_public_traffic": False}` on a create
  makes **every** port of that sandbox require a credential; the deployment-wide
  `SANDRPOD_E2B_PRIVATE_PORTS=1` does the same for all sandboxes. envd,
  code-interpreter and signed file URLs keep authenticating either way.
  Anonymous public-port traffic is rate-limited per client IP and sandbox
  (50 req/s, burst 100); `SANDRPOD_E2B_ANON_RATE` changes the rate and `0`
  disables the limit.
- `autoPause` is accepted: the idle timeout stops the sandbox and keeps
  `/workspace`. Process memory is not preserved, so `autoPauseMemory=true` is
  refused rather than silently degraded. `diskSizeMB` is reported as 2048, and
  `get_metrics` falls back to that declared size if the toolbox is unavailable.
- The full endpoint-by-endpoint status table is in
  [E2B_COMPAT.md](https://github.com/sandrpod/sandrpod/blob/main/docs/E2B_COMPAT.md).

## Next

- [REST API](/api) — the native surface, which does the same things with SandrPod's vocabulary.
- [Workers and tunnels](/workers) — where those sandboxes actually run.