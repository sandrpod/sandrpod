# CI

> **Status**: implemented. Updated: 2026-11

Two CI systems run this repository, and they are not interchangeable:

| Where | What runs | Why there |
|---|---|---|
| **GitHub Actions** (`.github/workflows/`) | `CI` (vet, build, unit tests, `-race`), `postgres` (multi-instance end-to-end), `images (main)` → ghcr.io, release | Public upstream: what a contributor and a release use |
| **Gitea Actions** (`.gitea/workflows/`) | `registry images` → the platform's own registry, `docs` → the docs site | The self-hosted deployment: the network it must work on is a Chinese one |

The GitHub workflows are guarded with `if: github.server_url == 'https://github.com'`
so Gitea skips them instead of reporting a failure — they download `actions/*` from
github.com, which this Gitea cannot reach.

## The network rules (learned the hard way)

Gitea's runner sits on a **Chinese network with no direct access to Docker Hub,
`proxy.golang.org` or `archive.ubuntu.com`**. Every one of these cost a red build:

| What | Symptom | Fix in the repo |
|---|---|---|
| `uses: actions/checkout@v4` | `Unable to clone .../actions/setup-go: authentication required` — the runner tries to fetch the action from the Gitea host | **No `uses:` steps in `.gitea/workflows/`.** Clone with `git` inside a `run:` step |
| Hardcoded `https://` for the clone | `GnuTLS, handshake failed: An unexpected TLS packet` — `SERVER` is the internal `http://gitea:3000` | Keep `SERVER`'s scheme: `proto="${SERVER%%://*}"` |
| `FROM golang:1.27-alpine` | Pull sits for 45 minutes, then `connection reset by peer` | Bases are mirrored into the platform registry (`sandrpod-base/*`) and selected with `--build-arg GO_IMAGE` / `RUNTIME_IMAGE` |
| `proxy.golang.org` | `dial tcp 142.251.46.81:443: i/o timeout` | `ARG GOPROXY=https://goproxy.cn,direct` in every builder stage |
| `archive.ubuntu.com` / `dl-cdn.alpinelinux.org` | apt/apk crawls until the step times out | `ARG APT_MIRROR` / `APK_MIRROR` default to `mirrors.aliyun.com` |
| `package-lock.json` resolved against npmjs | `npm ci` hangs on a host that cannot be reached | `npm_config_replace_registry_host=always` + `registry.npmmirror.com` |
| No node on the runner image | `node: command not found` | Build the docs inside `sandrpod-base/node:22-alpine` |
| Cross-repo clone | `remote: Repository not found` — `github.token` covers only this repo | The engine repo is public; clone it anonymously |

**Every override is a `--build-arg`**, so an overseas deployment keeps the official
sources: `--build-arg GOPROXY=https://proxy.golang.org,direct
--build-arg APT_MIRROR=archive.ubuntu.com`.

## Dockerfile gotcha worth its own paragraph

A `FROM` can only expand an `ARG` declared **before the first `FROM`**:

```dockerfile
ARG RUNTIME_IMAGE=alpine:3.24      # global scope
FROM ${GO_IMAGE} AS builder        # ok
FROM ${RUNTIME_IMAGE}              # ok
```

Declared next to its `FROM`, the variable is **empty** and docker answers
`base name (${RUNTIME_IMAGE}) should not be blank` — which reads like a syntax
error in the FROM line, not like a scoping problem.

## The cloud module

`sandrpod-cloud` pins the engine at a release tag (`OSS_VERSION`). It used to
resolve it through goproxy.cn, which cannot serve a private GitHub module
(`stream error ... INTERNAL_ERROR`). The job now clones the engine from Gitea and
uses the `replace => ../sandrpod` that exists for development. When you tag the
engine, bump `OSS_VERSION` in the cloud repo or CI tests the wrong surface.

## Race detector policy

`build-and-test` runs the suite with `-race`, and it is not decoration: it caught
a real class of bug. The stores hand out pointers, so a reader inspecting a record
without the lock raced with any in-place write. The rule now:

- **writers copy**: `SandboxStore.Update`, `JobStore.UpdateJob` build a copy, apply
  the update and replace the map entry;
- **`List` copies**, because a caller iterates it while a reaper writes;
- **`Get` keeps the live pointer**, because some callers legitimately mutate through
  it (copying it broke the poders liveness test).

If you add a store method that mutates a record, make it copy-on-write, or the race
job will find it in the middle of an unrelated test (the detector fails whichever
test happens to be running).

## Publishing images

`.gitea/workflows/registry-images.yml` builds `toolbox`, `poder` and `desktop` on
every push to `main`, tags them `latest` and `main-<sha>`, and pushes to
`$REGISTRY_HOST/sandrpod/*`. A node installed with `deploy/node-install.sh` can then
pull instead of building. Secrets: `REGISTRY_HOST`, `REGISTRY_USER`,
`REGISTRY_PASSWORD`.

The docs job publishes only when `DEPLOY_SSH_KEY`/`DEPLOY_HOST` are set; without
them it is a build check, which is the useful half for a fork.
