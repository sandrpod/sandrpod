---
title: Quickstart
description: A control plane and a worker in sixty seconds, then a sandbox running your code.
---

# Quickstart

Three ways in. Pick the one that matches what you already have.

## 1. The published stack (fastest)

```bash
curl -O https://raw.githubusercontent.com/sandrpod/sandrpod/main/docker/docker-compose.yml
docker compose up -d
```

| Container | What it is |
|---|---|
| `server` | control plane on `localhost:8080` |
| `poder` | a Docker worker; dials the control plane over a reverse tunnel |
| `toolbox` | pulled by the worker at sandbox-create time, one per sandbox |

The worker needs the Docker socket. The control plane needs nothing else.

Verify:

```bash
curl -s localhost:8080/health
# {"mode":"control-plane+tunnel","status":"ok","version":"0.7.0", ...}
```

## 2. A sandbox, then code

```bash
pip install sandrpod-cli

export SANDRPOD_API_URL=http://localhost:8080
export SANDRPOD_API_TOKEN=change-me          # the -token you started the server with

sandrpod-cli create demo --provider local
sandrpod-cli execute demo "echo hello from a sandbox you own"
sandrpod-cli delete demo
```

`create` waits for `RUNNING` by default. `--no-wait` returns the job id so a
long provisioning run does not block a script.

## 3. The same thing over HTTP

```bash
curl -s -X POST localhost:8080/api/v1/sandboxes \
  -H "X-Sandrpod-Token: change-me" \
  -H 'Content-Type: application/json' \
  -d '{"name":"demo","provider_type":"local"}'

curl -s -X POST "localhost:8080/api/v1/sandboxes/execute?sandbox=demo" \
  -H "X-Sandrpod-Token: change-me" \
  -H 'Content-Type: application/json' \
  -d '{"language":"bash","code":"uname -a"}'
```

## Where the token comes from

- `-token` on the control plane, or `SANDRPOD_TOKEN`.
- It is the operator credential: it can see every sandbox and every worker.
- A deployment that hands out per-workspace credentials issues them separately
  (an `sk_` key, or an `e2b_` key for the E2B surface). See [API keys](/api#authentication).

## Next

- [Your first sandbox](/first-sandbox) — the same steps with the moving parts explained.
- [E2B drop-in](/e2b) — point an existing E2B client at your own server.
- [Architecture](/architecture) — what those three containers actually do.