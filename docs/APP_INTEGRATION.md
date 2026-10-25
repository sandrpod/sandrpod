# What changes for the app

Everything in `docs/` describes how the machinery works. This page answers the
question an application team actually asks: **if I upgrade, what do I see, and
what do I have to do?**

Short answer: the SDK and HTTP API do not change, and every new layer is off by
default. Inside the sandbox, three things are different, and four kinds of tool
are expected to break — listed below so nobody discovers them at 2am.

---

## 1. The calling side: nothing

No new required parameters. Existing calls work unchanged:

```python
sb = client.get_sandbox("my-sandbox")          # same
client.exec(sb, "python -c 'print(1)'")        # same
```

New capabilities are optional, and mostly for whoever runs the platform:

| Call | Who uses it |
|---|---|
| `POST /api/v1/sandboxes` with `network` | platform: pin a policy at creation |
| `GET`/`PUT /api/v1/sandboxes/{name}/egress` | platform: change it later |
| `POST /api/v1/policy/decide` | the agent itself (`-egress-mode=decider`) |

---

## 2. Inside the sandbox: three visible differences

### 2.1 The environment says where the network is

```bash
HTTP_PROXY=http://sandbox:<token>@127.0.0.1:3128
HTTPS_PROXY=http://sandbox:<token>@127.0.0.1:3128
SSL_CERT_FILE=/home/me/.sandrpod/egress-ca/trust-bundle.pem
NODE_EXTRA_CA_CERTS=…  REQUESTS_CA_BUNDLE=…  CURL_CA_BUNDLE=…  GIT_SSL_CAINFO=…
OPENAI_API_KEY=fake_value_4b1f…          # when masking is on
```

Practically every client library honours these, so "network works" is unchanged.
Two details worth knowing:

- the credential variable holds a **sentinel**, not the secret. Code that
  compares it, hashes it, or ships it to a third party is looking at a fake —
  that is the point;
- `NO_PROXY` is set **empty** on purpose. It is a bypass list, so a value
  inherited from the agent's own environment would let every proxy-aware client
  step around the policy. If your workload needs a direct connection to one
  host, that is an operator decision (the allow-list), not an environment
  variable.

### 2.2 Failures are structured, not timeouts

| Situation | What the app sees |
|---|---|
| Host not on the allow-list | `403` + `X-Deny-Reason: not_allowed` |
| Explicitly denied host | `403` + `denied_domain` (operator's reason in the body) |
| Name resolves to loopback/metadata | `403` + `denied_address` |
| Policy service unreachable (`decider`) | `403` + `decider_unavailable` |
| Client ignores the proxy (Linux netns) | `no route to host` / DNS failure |
| Masked credential over plain HTTP | `403` + `masked_credential_cleartext` |

Read `X-Deny-Reason` rather than the message: it is stable, and it separates
"policy said no" from "the network is broken" — which is the difference between
changing approach and retrying forever.

### 2.3 File failures come from two different layers

- **Permission gate** (the toolbox file API): `403` with a model-facing
  sentence, e.g.
  `/root/.ssh is hard-locked by the operator's policy and cannot be read or written; do not retry…`
- **Kernel** (bubblewrap/Seatbelt, for a process that calls `open(2)`): an
  ordinary `EACCES`/`EROFS` from the tool itself.

The second is deliberate. Turning a kernel denial into a platform error would
tell the model "this is a policy" and invite it to look for a way around.

---

## 3. What is expected to break

| Tool | Why | Fix |
|---|---|---|
| Certificate pinning / mTLS clients | for a terminated host the certificate is SandrPod's | ask the operator to add the host to `-credential-terminate-exclude`; that host then has no masking |
| Clients that require HTTP/2 | the terminated path speaks HTTP/1.1 only, so masking cannot be skipped by a protocol upgrade | allow fallback, or exclude the host |
| Anything that hard-codes direct connections | on Linux the sandbox has no route except the proxy | intended |
| Installers that write `.git/hooks`, `.bashrc`, `.mcp.json`, IDE launch configs | mandatory deny, enforced by the gate and the kernel | install into the work directory instead |
| Reading `~/.ssh`, `~/.aws`, browser profiles | hardlocks | use a masked credential instead |

---

## 4. Four rules for application code

1. **Use `https://`.** A masked credential is only substituted on a terminated
   TLS connection; over plain HTTP it is refused rather than leaked.
2. **Do not pin certificates.** If you must, get the host excluded and accept
   that it carries no masking.
3. **Branch on `X-Deny-Reason`.** Retrying a `not_allowed` is a loop; asking the
   operator is an answer.
4. **Write inside `work_dir`.** That is the only writable tree, and the only one
   where persistence files are still protected.

---

## 4a. The LangChain / deepagents backend

`pkg/sdk/python/langchain_sandrpod` implements deepagents'
`SandboxBackendProtocol` (files + a shell). Two things worth knowing:

**Egress is not part of that protocol.** `SandboxBackendProtocol` has no network
concept, and `SandboxProvider.get_or_create(**kwargs)` rejects unknown keywords,
so upstream will never call an egress API. Ours exposes one anyway —
`client.sandbox(..., egress={...})` and `sb.get_egress()` / `set_egress()` /
`clear_egress()` — because the two people who need it are the operator shaping a
deployment and the author narrowing a single run below that default. An agent
cannot widen what it was given: the policy lives in the sandbox record and the
server validates it.

**Conformance.** langchain ships `SandboxIntegrationTests` for backend
implementations. Run against this one: **69/86**, and — the part that matters —
**every remaining failure is one deepagents' own `LocalShellBackend` also
fails** (glob path shape, binary previews, async/large-payload cases). The
interesting overlap: we pass 18 tests upstream fails (write with unicode, spaces,
long content; edit; large stdout; `ls` of nested directories), because the
toolbox does those natively. Three were genuinely ours and are fixed: `write`
now creates *new* files only (the protocol says "error if file exists", and
overwriting is what `edit` is for), `read(limit=0)` returns nothing instead of
everything, and `upload_files` creates the parent directory the protocol makes it
responsible for.

## 5. Three deployment profiles

### A. Local development — do nothing

All layers default to `off`. `go run ./cmd/server` + `go run ./cmd/poder`
behaves exactly as before.

### B. Container fleet (poder)

Configured entirely on the control plane; the worker needs no new flags.

```bash
curl -X POST $API/api/v1/sandboxes -H 'Content-Type: application/json' -d '{
  "name": "build-1", "provider": "local",
  "network": {
    "allowed_domains": ["proxy.golang.org", "*.npmjs.org", "*.pypi.org"],
    "denied_domains": ["*.internal"],
    "denied_domain_reasons": {"*.internal": "internal services are not reachable from sandboxes"}
  }
}'
```

`poder` gives each pod an `internal` docker network plus the `egress-proxy`
sidecar; the container has no route except the proxy.

### C. Employee machine (`sandrpod-agent`)

```bash
# 1. Egress allow-list only
sandrpod-agent -api-url=https://api.example.com -name=me-laptop -token=… \
  -egress-mode=enforce -egress-policy=~/.sandrpod/egress.json

# 2. + kernel floor (files and network become walls, Linux)
sandrpod-agent … -permission-mode=prompt -os-sandbox=auto

# 3. + credential masking (full stack)
sandrpod-agent … \
  -permission-mode=prompt -os-sandbox=auto \
  -egress-mode=decider \
  -credential-mask=OPENAI_API_KEY,ANTHROPIC_API_KEY,GITHUB_TOKEN \
  -credential-mask-files=~/.aws/credentials \
  -credential-terminate-exclude=internal.corp.example
```

Choosing `-egress-mode` is the one decision worth thinking about:

| Value | Rules live | A tightened rule lands | Control plane down |
|---|---|---|---|
| `enforce` | local file | on restart | fine |
| `server` | both (polled copy) | ≤ `-egress-sync` (60s) | keeps the last copy |
| `decider` | server only | next connection (≤10s cache) | **all egress denied** |

Credentials for masking come from the agent's **own environment** — injected by
whoever starts it (launchd/systemd/your platform), never from a config file.

---

## 6. Ten-minute acceptance check

After enabling anything, this tells you it is actually on:

```bash
# 1. Startup reports each layer (the only place that proves it took effect)
#    os sandbox: bwrap active — work_dir=… writable, 15 hard-locked path(s) masked, 2 persistence path(s) read-only
#    os sandbox: network namespace removed — the relay at 127.0.0.1:3128 is the only route out
#    egress: credential masking on for [OPENAI_API_KEY]; TLS is terminated for allowed hosts except []

# 2. Inside the sandbox
curl -s -o /dev/null -w '%{http_code}\n' https://example.com        # 200 if allow-listed
curl -s -o /dev/null -w '%{http_code}\n' https://not-allowed.test   # 403
curl -s --noproxy '*' https://example.com                           # could not resolve host

# 3. The audit says which secret went where (names only)
jq -r 'select(.source=="egress") | "\(.host) \(.decision) terminated=\(.terminated) creds=\(.credentials)"' \
  ~/.sandrpod/audit/active.log | tail
```

A full manual walkthrough of every layer, with expected output, is in
[PLAYGROUND.md](PLAYGROUND.md).

See also: [CREDENTIALS.md](CREDENTIALS.md), [EGRESS_POLICY.md](EGRESS_POLICY.md),
[OS_SANDBOX.md](OS_SANDBOX.md), [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md).