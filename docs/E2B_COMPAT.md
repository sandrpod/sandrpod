# E2B Wire-Protocol Compatibility — Implementation Blueprint

Goal: make the **unmodified** official E2B SDKs (`e2b`, `@e2b/code-interpreter`,
`e2b-code-interpreter`) work against a SandrPod deployment by pointing them at
our domain — no code changes for the user, only `E2B_DOMAIN` + an API key. This
is the "true drop-in" level (what Tencent AGSX / PPIO advertise as
"E2B-interface compatible").

Status: **blueprint** — reverse-engineered from E2B's Apache-2.0 sources
(`e2b-dev/E2B`, `e2b-dev/infra`) on 2026-07-03. Endpoint- and proto-level detail
marked _(verify during impl)_ must be pulled from the specs before coding each
phase.

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

## 2. Mapping to SandrPod

| E2B plane | SandrPod component | Fit today | Work needed |
|---|---|---|---|
| Control plane `api.<domain>` | **API server** (`cmd/server`) | partial — we have create/list/get/delete/timeout | Add an **E2B-shaped REST surface** (`/sandboxes` with E2B's exact request/response schemas), `X-API-KEY` auth, `e2b_<hex>` key issuance, pause/resume, template & snapshot endpoints (map to our snapshot), env vars, metadata |
| Domain routing to a sandbox | **tunnel + toolbox** | we already proxy `/sandboxes/{name}/toolbox/*` and `/proxy/{port}` | Add **hostname-based routing** (`<port>-<sandboxID>.<domain>`) that resolves to the same tunnel→toolbox path; needs a wildcard-DNS + host header router in the server |
| envd filesystem | toolbox `/files/*` | good coverage | Wrap in **envd's connect-rpc service shape** (Filesystem service: stat/list/read/write/watch) |
| envd process/commands | toolbox `/process`, `/execute` | good coverage | Wrap in envd's **Process service** (start/stream/sendInput/sendSignal) |
| envd PTY | toolbox `/pty/*` | good coverage | Map to envd PTY RPCs |
| code interpreter `run_code` | toolbox `/execute` is **one-shot, stateless** | ❌ gap | Add a **persistent Jupyter kernel** in the toolbox image + the code-interpreter HTTP contract (execution id, streamed logs, rich `results`) |
| templates | our `image_id` | conceptual match | Map E2B `templateID` ↔ our image/tag; expose the minimal template endpoints the SDK calls on create |

Key architectural point: SandrPod's **tunnel already gives us exactly the
control-plane-proxies-to-in-sandbox-daemon topology** E2B has. envd ≈ our
toolbox. So the work is **protocol adaptation**, not new architecture.

---

## 3. The hard parts (rank-ordered by risk)

1. **envd connect-rpc protocol** — the highest-uncertainty piece. Must
   reproduce E2B's protobuf services (Filesystem, Process, and the PTY/exec
   surface) over connect-rpc so the SDK's generated client speaks to us. Pull
   `spec/envd/*.proto` and generate matching Go handlers; back them with the
   existing toolbox operations. Test with the real SDK's `files`/`commands`.
2. **Sandbox domain routing** — the SDK reaches envd at
   `<port>-<sandboxID>.<domain>`. Needs wildcard DNS (`*.<domain>`) + TLS and a
   host-header router in the server that maps `<sandboxID>` → tunnel/toolbox.
   Without this, only the control plane works and in-sandbox ops break.
3. **Code-interpreter Jupyter semantics** — persistent kernel + rich results
   (charts). New toolbox capability (bundle jupyter, proxy exec to the kernel,
   surface `results`/`logs`/`text`). Needed for `@e2b/code-interpreter`, the
   most-used SDK.
4. **API-key shape + auth** — issue `e2b_<hex>` keys; accept `X-API-KEY`; map to
   our named-token/owner model so quotas/ownership still apply.
5. **Template/snapshot mapping** — reconcile E2B's template/snapshot IDs with
   our `image_id` + docker-commit snapshots.

---

## 4. Phased plan (each phase independently shippable + testable vs the real SDK)

**Phase 0 — pin the contract.** Vendor E2B's `spec/openapi.yml` and
`spec/envd/*.proto` into `docs/e2b-spec/`; generate Go types. Stand up a
conformance harness that runs the real `e2b` / `e2b-code-interpreter` SDK
against a local SandrPod with `E2B_DOMAIN` overridden. Every later phase is
"make more of that harness pass."

**Phase 1 — control plane.** Implement the E2B-shaped `/sandboxes` REST surface
on the server (create/list/get/kill/set-timeout, then pause/resume) with
`X-API-KEY` auth and `e2b_<hex>` keys, mapping onto our existing sandbox store +
scheduler. Milestone: `Sandbox.create()`, `.getInfo()`, `.setTimeout()`,
`.kill()`, `Sandbox.list()` pass against SandrPod.

**Phase 2 — domain routing + envd filesystem/process.** Add wildcard host
routing (`<port>-<sandboxID>.<domain>`) → tunnel→toolbox, and implement envd's
connect-rpc Filesystem + Process services backed by the toolbox. Milestone:
`sbx.files.*` and `sbx.commands.run()` pass.

**Phase 3 — code interpreter.** Add the Jupyter kernel to the toolbox image and
the code-interpreter HTTP contract. Milestone: `sbx.run_code("x=1")` then
`run_code("x+=1; x")` returns `2` with streamed logs + rich results.

**Phase 4 — long tail.** PTY, env vars, metadata, network egress rules,
templates/snapshots parity, MCP config — as demand dictates.

---

## 5. Compatibility test strategy

The definition of done is **the real E2B SDK passing**, not our reimplementation
of it. Phase 0's harness pins `pip install e2b-code-interpreter` /
`npm i @e2b/code-interpreter` at fixed versions, sets `E2B_DOMAIN` +
`E2B_API_KEY` at a local SandrPod, and asserts the quickstart + lifecycle +
files + commands + run_code flows. Track a compatibility matrix (SDK method →
pass/fail/NA) in this doc as phases land.

---

## 6. Non-goals (for now)

- Teams/billing/dashboard endpoints beyond what `create` transitively needs.
- Exact parity of E2B's network-transform rules and MCP-server bundling.
- Byte-for-byte error message parity (status codes + error shape suffice).

---

## 7. Implementation status (as of 2026-07-03)

Built in `pkg/e2bcompat` (gateway + wire types) and `cmd/server/e2bgateway.go`
(backends over scheduler/store/toolbox), enabled by `SANDRPOD_E2B_DOMAIN`.

Legend: ☑ built + unit-tested to the spec · ◐ built, needs live/real-SDK
verification · ☐ not yet.

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
| code interpreter | `runCode` + contexts | ☑ | **stateful** kernel + create/list/restart/remove contexts; verified live; charts need jupyter in the image |
| metadata | create/list `metadata` | ☑ | stored in labels, filterable |
| env vars | `envVars` | ◐ | accepted on create; per-process injection via the process table's `envs` |

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

Still out of scope: multi-sandbox code-interpreter under `E2B_DEBUG` (that mode
is single-sandbox by design — production uses host-based routing); a production
vanity-domain drop-in still needs wildcard DNS + TLS. The hand-rolled
binary-protobuf connect path exists (and now covers the Process/watch messages)
but this SDK negotiated `connect+json`. E2B `pause` is a VM snapshot; SandrPod
freezes in place — the sandbox ID stays valid for resume, which is what the SDK
observes.

### What "◐ needs live verification" means honestly

Three things stand between the current build and an **unmodified E2B SDK passing
end-to-end**, none of which can be exercised without a running deployment:

1. **Wildcard DNS + TLS** for `*.<domain>` so the SDK can reach
   `<port>-<sandboxID>.<domain>`. Deployment infra, not code.
2. **Binary-protobuf connect path.** The handlers speak connect **JSON**
   (valid per the connect spec and unit-tested). E2B's JS/Python clients may
   default to binary protobuf; if so, a buf-generated binary path must be added
   alongside the JSON one.
3. **A real toolbox + jupyter image** to confirm the envd↔toolbox field
   mapping and rich code-interpreter results.

The conformance harness in §5 (run the real `e2b`/`e2b-code-interpreter` SDK
against a local SandrPod) is the gate that turns every ◐ into ☑. Until then the
wire contract is verified against the spec with unit tests, not against the SDK.
