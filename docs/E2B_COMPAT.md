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
| Control: pause/resume | `pause`/`resume` | ☐ | returns unsupported (SandrPod has no snapshot-pause yet) |
| envd filesystem | `files.list/stat/makeDir/rename/remove` | ◐ | connect-JSON handlers + toolbox mapping; unit-tested with fakes |
| envd file content | `files.read`/`write` | ◐ | plain-HTTP; toolbox download/upload mapping |
| envd process | `commands.run` | ◐ | connect server-stream (start→data→end→eos); toolbox `execute` |
| code interpreter | `runCode` | ◐ | **stateful** kernel verified with real python3; NDJSON stream; charts need jupyter in the image |
| metadata | create/list `metadata` | ☑ | stored in labels, filterable |
| PTY | `pty.*` | ☐ | envd PTY is bidirectional connect streaming — not yet |
| env vars | `envVars` | ☐ | accepted on create; per-process injection not wired |

### Verified against the REAL unmodified E2B SDK over HTTP (2026-07-03)

Ran the official `e2b` Python SDK **v2.30.0** against a local SandrPod over
plain HTTP — no TLS, no wildcard DNS — using the SDK's own overrides
(`E2B_API_URL` + `E2B_SANDBOX_URL` = `http://host:<SANDRPOD_E2B_DEBUG_PORT>`,
`E2B_VALIDATE_API_KEY=false`). A direct-mode `sandrpod-agent` (embedded toolbox,
no Docker) served as the sandbox. Passing end-to-end with the unmodified SDK:

- ☑ `Sandbox.connect()` (control plane, HTTP)
- ☑ `Sandbox.list()` (control plane, `/v2/sandboxes`)
- ☑ `files.write()` / `files.read()` — real file round-trip
- ☑ `commands.run("echo …")` — real stdout returned

Getting there fixed a stack of issues only the real SDK reveals: the
`POST /sandboxes/{id}/connect` endpoint, envd auth via the `X-Access-Token`
header, sandbox routing via the `E2b-Sandbox-Id`/`E2b-Sandbox-Port` headers,
multipart file-upload parsing, the array-shaped write response, connect
streaming-request **envelope** stripping (the 5-byte frame prefix), the
`cmd`/`argv` → shell translation, and the `/v2` control-plane prefix. Also: E2B
splits sandbox IDs on `-`, so gateway-issued names must not contain one.

Remaining (honest): `e2b-code-interpreter` `run_code` — the CI SDK builds a
code-interpreter URL that the fixed `E2B_SANDBOX_URL` override didn't capture in
this harness (no `/execute` reached the gateway), so the stateful interpreter is
built + unit-tested but not yet real-SDK-verified. And a production drop-in
(vanity domain) still needs wildcard DNS + TLS; the binary-protobuf connect path
exists but this SDK version used `connect+json`.

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
