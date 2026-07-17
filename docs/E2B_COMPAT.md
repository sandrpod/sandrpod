# E2B Wire-Protocol Compatibility

The **unmodified** official E2B SDKs (`e2b`, `@e2b/code-interpreter`,
`e2b-code-interpreter`) work against a SandrPod deployment by pointing them at
your domain — no code changes, only `E2B_DOMAIN` + an API key ("true drop-in").

Status: **shipped and verified against the real, unmodified E2B SDKs**
(`e2b` v2.30.0 + `e2b-code-interpreter`, real Docker toolbox container) — see
the compatibility matrix in §4. The wire contract was reverse-engineered from
E2B's Apache-2.0 sources (`e2b-dev/E2B`, `e2b-dev/infra`).

---

## 1. How the E2B SDK actually talks to a backend

E2B has **two planes**, reached at **two different hostnames** derived from a
single configurable domain:

```
E2B_DOMAIN (default e2b.app)
  ├── control plane   →  https://api.<domain>          (REST, OpenAPI 3.0)
  └── per-sandbox envd →  https://<port>-<sandboxID>.<domain>   (connect-rpc + HTTP)
```

### 1a. Control plane (`api.<domain>`)
- OpenAPI 3.0 spec: `spec/openapi.yml` in `e2b-dev/E2B`. JS SDK uses
  `openapi-fetch` against a generated `schema.gen`; Python SDK mirrors it.
- **Auth header:** `X-API-KEY: e2b_<40 hex>` (primary) or
  `Authorization: Bearer <accessToken>`.
- **API-key format is validated client-side:** `/^e2b_[0-9a-f]+$/`
  (`packages/js-sdk/src/api/index.ts`). So SandrPod-issued keys handed to the
  E2B SDK must either match `e2b_<hex>` or the user must pass
  `validateApiKey: false`. **We should issue keys in the `e2b_<hex>` shape** to
  keep it truly zero-config.
- Manages: create / list / get / kill sandbox, set timeout, pause & resume,
  templates, snapshots, volumes, teams, metadata, env vars, network rules.

### 1b. envd (per-sandbox daemon, `<port>-<sandboxID>.<domain>`)
- The in-sandbox daemon the SDK hits for **filesystem, process/commands, PTY**,
  reached by a hostname that encodes the sandbox ID and a port.
- Protocol: **connect-rpc** (connectrpc.com) over HTTP with protobuf messages,
  spec under `spec/envd/` _(verify exact services/messages during impl)_. Plus
  plain HTTP for file up/download and the code-interpreter.
- `EnvdVersion` is tracked per sandbox in the control-plane schema — the SDK
  does version compares (`compare-versions`) and changes behavior by envd
  version, so we must report a compatible `envd_version`.

### 1c. Code interpreter (`@e2b/code-interpreter`)
- `run_code()` is a **stateful Jupyter kernel** (variables persist across calls;
  returns `logs` (stdout/stderr streams), `text`, and `results` incl. charts as
  images/base64). Backed by a Jupyter server inside the sandbox, reached through
  envd/HTTP. This is a superset of plain command exec.

---

## 2. Enabling it on your deployment

**Production (host-routed, what the SDK expects by default):**

1. **Wildcard DNS + TLS** for `*.<your-domain>` pointing at the server (or a
   TLS-terminating proxy in front of it). A full walkthrough — including a
   Caddy config with wildcard certificates — is in
   [MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md) Part 4; it
   works the same on a single instance.
2. Set `SANDRPOD_E2B_DOMAIN=<your-domain>` on the server. This activates the
   host router: `api.<domain>` → control plane,
   `<port>-<sandboxID>.<domain>` → envd (tunnel → toolbox).
3. Issue an `e2b_<hex>`-shaped key: `sandrpod-cli token create <name>` (see
   [AUTH_AND_KEYS.md](AUTH_AND_KEYS.md)). Point the SDK at your deployment:

   ```bash
   export E2B_DOMAIN=<your-domain>
   export E2B_API_KEY=e2b_…
   ```

**Local debugging (no DNS/TLS needed):** set `SANDRPOD_E2B_DEBUG_PORT=3333` —
a plain-HTTP listener serving the gateway in path mode. SDK env:
`E2B_API_URL` + `E2B_SANDBOX_URL` = `http://<host>:3333`,
`E2B_VALIDATE_API_KEY=false` (or use an `e2b_`-shaped key). For
`e2b-code-interpreter` use `E2B_DEBUG=true` — the debug listener also binds
`:49983`/`:49999` because the SDK hardcodes those under debug.

---

## 3. How it maps to SandrPod

| E2B plane | SandrPod implementation |
|---|---|
| Control plane `api.<domain>` | E2B-shaped REST surface (`pkg/e2bcompat` + `cmd/server/e2bgateway.go`): create/list/get/kill, set-timeout, pause/resume, metrics, metadata; `X-API-KEY` auth with `e2b_<hex>` keys mapped to the named-token/owner model (ownership + quotas still apply) |
| Domain routing | Host-header router: `<port>-<sandboxID>.<domain>` → tunnel → toolbox; non-envd ports go through the generic port proxy (e.g. the in-sandbox MCP gateway on `:50005`, see [E2B_MCP_COMPAT.md](E2B_MCP_COMPAT.md)) |
| envd filesystem / process / PTY | connect-rpc **Filesystem** + **Process** services (PTY rides Process) backed by the toolbox `/files/*`, `/process`, `/pty/*` |
| code interpreter `run_code` | Stateful kernel in the toolbox (one resident interpreter per context; variables persist) + the code-interpreter HTTP contract; matplotlib charts are captured as PNG into `Execution.results` |
| templates | E2B `templateID` ↔ SandrPod `image_id` |

Key architectural point: SandrPod's **tunnel already gives us exactly the
control-plane-proxies-to-in-sandbox-daemon topology** E2B has. envd ≈ our
toolbox. So the work was **protocol adaptation**, not new architecture. In
multi-instance deployments the E2B surface is fully cross-node: a request
landing on any instance is forwarded to the node holding the sandbox's tunnel
(see [MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md)).

---

## 4. Verified compatibility

Built in `pkg/e2bcompat` (gateway + wire types) and `cmd/server/e2bgateway.go`
(backends over scheduler/store/toolbox), enabled by `SANDRPOD_E2B_DOMAIN`.

Legend: ☑ verified against the real unmodified SDK · ◐ built + unit-tested,
not yet exercised by the real SDK.

| Surface | SDK call | Status | Notes |
|---|---|---|---|
| Control: create | `Sandbox.create` | ☑ | E2B schema, `X-API-KEY`, `e2b_<hex>` keys; maps to a local sandbox |
| Control: get/list/kill | `getInfo`/`list`/`kill` | ☑ | owner-scoped; metadata round-trips via labels + filter |
| Control: set timeout / refresh | `setTimeout` | ☑ | maps to per-sandbox `ttl_seconds` |
| Control: pause/resume | `pause`/`resume`/`connect` | ☑ | freezes the container via the poder (docker pause); `connect` auto-resumes; verified live |
| Control: metrics | `get_metrics` | ☑ | toolbox reads `/proc` (cpu/mem) + statfs (disk); verified live |
| envd filesystem | `files.list/stat/makeDir/rename/remove` | ☑ | connect handlers + toolbox mapping; verified live |
| envd file content | `files.read`/`write`/`write_files` | ☑ | plain-HTTP; batch multipart write; verified live |
| envd watch | `files.watch_dir` | ☑ | fsnotify watcher, poll-based CreateWatcher/GetWatcherEvents/RemoveWatcher; verified live |
| envd process | `commands.run` (fg+bg) / `list`/`kill`/`send_stdin`/`connect` | ☑ | full pid-addressed table; real streaming; verified live |
| PTY | `pty.create/send_stdin/resize/kill` | ☑ | rides the Process service (pty flags); verified live |
| code interpreter | `runCode` + contexts | ☑ | **stateful** kernel + create/list/restart/remove contexts; **matplotlib charts** captured as PNG → `Execution.results[].png`; verified live |
| metadata | create/list `metadata` | ☑ | stored in labels, filterable |
| env vars | `envVars` | ◐ | accepted on create; per-process injection via the process table's `envs` |
| MCP gateway | in-sandbox `mcp-gateway` on `:50005` | ☑ | E2B-style shim over the native bridge; reached via the generic port proxy; verified live over a real TLS vanity domain |
| cross-node routing | any SDK call on a multi-instance cluster | ☑ | requests are forwarded to the node holding the sandbox's tunnel; verified live with a real cloud poder |

### Verified against the REAL unmodified E2B SDK over HTTP + a real container (2026-07-03)

Ran the official `e2b` **v2.30.0** and `e2b-code-interpreter` Python SDKs against
a local SandrPod over plain HTTP — no TLS, no wildcard DNS — with a **real
Docker toolbox container** behind a docker-run poder. The base surface is
**23/23**; waves 1–3 (below) then closed the rest of the SDK — background
commands, PTY, watch_dir, metrics, pause/resume — each verified the same way.

Base SDK (`E2B_API_URL`+`E2B_SANDBOX_URL`=`http://host:3333`,
`E2B_VALIDATE_API_KEY=false`) — **17/17**:
`Sandbox.create` (real container), `is_running`, `get_info`, `set_timeout`,
`list`, `kill`; `files.write/read/exists/list/make_dir/rename/remove/get_info`;
`commands.run` (echo / `python3` / pwd).

Code interpreter (`E2B_DEBUG=true`, single sandbox) — **6/6**:
stateful `run_code` (`a=100` then `a*2` → `200`), stdout capture, multiline
loops, error capture (`1/0` → `ZeroDivisionError`), imports
(`math.sqrt(16)` → `4.0`).

Getting there fixed a stack of issues only the real SDK + real container reveal:
`POST /sandboxes/{id}/connect`; envd auth via `X-Access-Token`; sandbox routing
via `E2b-Sandbox-Id`/`E2b-Sandbox-Port`; multipart upload parsing; array-shaped
write response; connect streaming-request **envelope** stripping (5-byte prefix);
`cmd`/`argv` → shell translation; the `/v2` control-plane prefix; the toolbox
mapping (`/files` returns `{files:[…]}`, delete needs `DELETE`, exec is
`/process`); per-sandbox `e2b_<hex>` envd tokens; and the fact that E2B's
`get_host` for the code-interpreter port ignores `E2B_SANDBOX_URL` — it only
works under `E2B_DEBUG=true` at `localhost:49999`, so the debug listener binds
`:49983`+`:49999` and treats the `debug_sandbox_id` placeholder via the
single-sandbox resolver. E2B also splits sandbox IDs on `-`, so gateway-issued
names contain none.

#### Full SDK-surface sweep (waves 1–3, 2026-07-03)

A follow-up pass closed the remaining gaps and verified each against the real
container over HTTP:

- **Wave 1** — `files.write_files` (batch multipart), `files.get_info`,
  `commands.list`, code-interpreter contexts (`create/list/restart/remove`,
  stateful: `z=7` then `z*6=42`, `NameError` after restart).
- **Wave 2** — the full **Process service**: `commands.run(background=True)`
  (real pid), `commands.list`/`kill`/`send_stdin`/`connect`, incremental
  streaming (`on_stdout` gets 3 separate chunks), and **PTY**
  (`pty.create/send_stdin/resize/kill` — a real terminal session with bracketed
  paste + prompt). This also surfaced and fixed a **poder streaming bug**: the
  `/toolbox/` proxy used a 30 s client timeout + non-flushing `io.Copy`, which
  stalled every long-lived stream — now a no-timeout client + `flushingCopy`
  (benefits all tunnel streaming, not just E2B).
- **Wave 3** — `files.watch_dir` (fsnotify → CREATE/WRITE events), `get_metrics`
  (real `/proc` cpu/mem + statfs disk), and `pause`/`resume` (poder docker
  pause; `Sandbox.connect` auto-resumes and runs commands after).

The production **host-routed path was also verified over a real wildcard TLS
domain**: with only `E2B_DOMAIN` set, the real SDK created a sandbox on a cloud
VM and ran the full ops suite through `api.<domain>` /
`<port>-<id>.<domain>` (deployment recipe in
[MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md) Part 4).

---

## 5. Remaining edges & non-goals

Known edges (by design or awaiting demand):

- **`envVars`** is accepted on create and injected per-process via the process
  table; it hasn't been exercised by the real SDK yet (the one remaining ◐).
- **Binary-protobuf connect path** exists (covers the Filesystem/Process/watch
  messages) but current E2B SDKs negotiate `connect+json`, so the binary path
  sees no real-SDK traffic.
- **Multi-sandbox code-interpreter under `E2B_DEBUG`** — that debug mode is
  single-sandbox by design; production uses host-based routing.
- **`pause` semantics**: E2B pauses via VM snapshot; SandrPod freezes the
  container in place (`docker pause`). The sandbox ID stays valid for resume,
  which is what the SDK observes.

Non-goals: teams/billing/dashboard endpoints beyond what `create` transitively
needs; exact parity of E2B's network-transform rules; byte-for-byte error
message parity (status codes + error shape suffice).
