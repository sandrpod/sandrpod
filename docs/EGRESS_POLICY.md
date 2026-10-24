# Egress policy

> Opt-in filter on what a sandboxed process may connect to. Off by default
> (`-egress-mode=off`), the same posture as the permission gate: a sandbox in
> a container or a cloud VM inherits its network from its substrate, and the
> mode that needs this is the one where sandrpod-agent **is** the machine.

`docs/PERMISSION_AND_AUDIT.md` lists what the consent gate does not cover, and
"network egress" used to be first on that list: an agent that could run `curl`
could send out whatever it had read. This document is the layer that closes the
part of that gap a userspace proxy can close — and, just as importantly, the
part it cannot.

---

## 1. Where this sits

| Layer | What it does | In this build |
|---|---|---|
| **Egress policy** (`pkg/egress`) | Decides which destinations a sandboxed process may reach, and refuses the rest with a reason the model can act on | ✅ this document |
| **Resolved-address check** (`pkg/egress/guard.go`) | Refuses an allow-listed *name* that resolves into loopback, the LAN, a cloud metadata endpoint or this host | ✅ this document |
| **Kernel floor** (network namespace, Seatbelt, bwrap, WFP) | Stops a process that ignores the proxy variables | ❌ not in this build — [roadmap](#7-what-this-does-not-do) |

That last row is the honest part. The proxy is a **filter**: it covers every
tool that honours `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`, which is most of
what an agent actually runs (curl, wget, npm, pip, cargo, go, git over http,
API SDKs, package managers). It does not cover a process that opens a socket
directly and ignores the environment. Treat it as "the agent cannot casually
walk data out", not as "the machine is airgapped".

---

## 2. Quick start

```bash
# 1. Write a policy. An empty allow list means "no egress at all", so every
#    policy starts by naming what the work actually needs.
cat > ~/.sandrpod/egress.json <<'JSON'
{
  "allowed_domains": ["api.github.com", "*.npmjs.org", "registry.npmjs.org:443"],
  "denied_domains": ["*.internal.corp"],
  "denied_domain_reasons": {
    "*.internal.corp": "internal hosts are not reachable from this sandbox; ask the operator for a mirror"
  },
  "denied_resolved_addresses": ["10.0.0.0/8", "192.168.0.0/16"]
}
JSON

# 2. Start the agent with it.
sandrpod-agent -api-url=https://api.example.com -name=my-laptop \
  -permission-mode=prompt \
  -egress-mode=enforce -egress-policy=~/.sandrpod/egress.json
```

The agent logs what it will enforce, then hands every spawned process the
proxy address:

```
egress: enforcing from /Users/me/.sandrpod/egress.json (http=127.0.0.1:52311 socks=127.0.0.1:52312)
egress: allow=[*.npmjs.org api.github.com registry.npmjs.org:443]
egress: deny=[*.internal.corp]
```

From inside the sandbox:

```console
$ curl https://api.github.com/zen
Keep it logically awesome.

$ curl https://example.org/
example.org:443 is not on the egress allow list
curl: (56) ... # HTTP 403, X-Deny-Reason: not_allowed
```

`-egress-mode=enforce` with no `-egress-policy` is a startup error. An empty
allow list denies everything, and an agent that starts with one looks, from
the outside, exactly like an agent whose network is broken — better to refuse
to start and say why.

---

## 3. The policy file

JSON, snake_case, unknown keys rejected (a typo'd key otherwise yields an empty
allow list that silently denies everything).

| Field | Meaning |
|---|---|
| `allowed_domains` | Destinations that may be reached. Empty = no egress. |
| `denied_domains` | Checked **first**, deny unconditionally. `*` denies everything. |
| `denied_domain_reasons` | Maps a `denied_domains` entry (exact string) to the reason the model sees instead of the generic one. |
| `denied_resolved_addresses` | Extra CIDRs an allowed *name* must not resolve to. |
| `strict_allowlist` | Explicit "undecided means denied". Already the behaviour; the flag exists so a future per-connection prompt cannot quietly loosen it. |

### Entry grammar

| Entry | Matches |
|---|---|
| `example.com` | exactly `example.com`, any port |
| `*.example.com` | `a.example.com`, `a.b.example.com` — **not** `example.com` itself |
| `api.example.com:8443` | that host, only on 8443 |
| `127.0.0.1:3000` | IPv4 literal, pinned to that port |
| `[::1]:8080` | IPv6 literals are bracketed, RFC 3986-style |
| `*` | everything (useful in `denied_domains`, a footgun in `allowed_domains`) |

`*` is only supported as a leading `*.` label; `api.*.example.com` is rejected.
Entries are host[:port] — a URL, a path or a trailing slash is a configuration
error, reported at load time.

Precedence is the part that matters, and it is the same order
[sandbox-runtime](https://github.com/anthropics/sandbox-runtime) uses:

1. a `denied_domains` match **denies** — a broader allow can never rescue it;
2. an `allowed_domains` match allows the connection *subject to the address
   check below*;
3. nothing matched → denied, with a reason that says what to add.

---

## 4. The resolved-address check

The allow list matches names. Whoever controls a permitted name's DNS — or any
label under a permitted wildcard — controls what it resolves to. Without a
second check, allowing `*.example.com` would also allow reaching `127.0.0.1`,
the LAN and `169.254.169.254` the day someone points a subdomain at them.

So before dialling an allowed **hostname**, the proxy resolves it once, drops
every address in a denied class, and connects to one that survived. The address
that passed the check is the one dialled; there is no second lookup (a second
lookup is a TOCTOU window and a DNS-rebinding vector).

Denied by default, always:

- loopback (`127.0.0.0/8`, `::1`), unspecified (`0.0.0.0/8`, `::`)
- link-local (`169.254.0.0/16`, `fe80::/10`) and multicast (`224.0.0.0/4`, `ff00::/8`)
- the cloud metadata endpoints that live outside link-local:
  `100.100.100.200`, `168.63.129.16`, `192.0.0.192`, `fd00:ec2::254`,
  `fd20:ce::254`, `fd00:c1::a9fe:a9fe`, `fd00:42::42`
- every address assigned to one of **this host's own interfaces** — a service
  bound to `0.0.0.0` answers on the LAN address exactly as it does on loopback

Not denied by default: private-use space (`10/8`, `172.16/12`, `192.168/16`,
`fc00::/7`). An intranet hostname is a legitimate thing to allow, so it is the
operator's call; list the ranges in `denied_resolved_addresses` when it is not.

**The exception**: an IP literal in `allowed_domains` is an explicit decision —
`127.0.0.1:3000` means exactly that port on that host, and the class check does
not second-guess it. The same address reached through a *name* is a different
act and stays refused, which is what the guard is for.

A name that resolves to no address at all is reported as a **connection
failure**, not a policy denial: the model should be able to tell "DNS is
broken" from "policy said no", because the fix is different.

---

## 5. What the sandbox sees

Every process the toolbox spawns — one-shot `Execute`, a persistent session
shell, a PTY, the code-interpreter kernel, procmgr starts — inherits:

```
HTTP_PROXY, http_proxy, HTTPS_PROXY, https_proxy = http://sandbox:<token>@127.0.0.1:<port>
ALL_PROXY, all_proxy                             = socks5://sandbox:<token>@127.0.0.1:<port>
NODE_USE_ENV_PROXY=1
```

`NO_PROXY` is deliberately **not** set: it is a bypass list, and a sandbox that
can name hosts in it can step around the policy.

Both listeners bind to `127.0.0.1` only and require a per-process token
(192 bits, generated at startup, never written to disk). Two agents on one
machine cannot borrow each other's allow list, and neither can another local
process running as the same user.

A refusal looks like this:

```
HTTP/1.1 403 Forbidden
X-Deny-Reason: not_allowed
Content-Type: text/plain; charset=utf-8

example.org:443 is not on the egress allow list
```

| `X-Deny-Reason` | Status | Meaning |
|---|---|---|
| `not_allowed` | 403 | No allow entry covers the destination |
| `denied_domain` | 403 | A `denied_domains` entry matched (the body carries `denied_domain_reasons`) |
| `denied_address` | 403 | The name is allowed, its resolved address is not (loopback, metadata, this host, a listed CIDR) |
| `proxy_auth_required` | 407 | No token, or the wrong one |
| `bad_request` | 400 | Not an absolute-form request / unparsable target |
| `upstream_unreachable` | 502 | Policy allowed it; the network did not |

SOCKS5 refusals answer `0x02` (connection not allowed by ruleset). Only
`CONNECT` is implemented: `BIND` and `UDP ASSOCIATE` are refused rather than
half-supported — a UDP relay would need its own allow-list semantics, and
`BIND` would let a sandbox accept inbound connections.

One SOCKS5 nuance worth knowing before it surprises you: a client that resolves
the name itself (`curl --proxy socks5://…`, and most SOCKS libraries by
default) sends the proxy an **IP literal**, and the allow list matches *names*.
Such a request is refused unless that literal is itself allow-listed. Use
remote resolution where the client supports it — `socks5h://` for curl,
`rdns = true` for PySocks — so the proxy sees the hostname it can judge.

---

## 6. Audit

Every refusal becomes an audit event with `source: "egress"`; so does every
allow if `Options.LogAllowed` is set (the agent leaves it off — at fleet scale
the allow stream is noise).

```json
{
  "event_id": "…", "occurred_at": "2026-10-24T12:00:00Z",
  "source": "egress", "decision": "deny", "caller": "connect",
  "host": "example.org", "port": 443,
  "code": "not_allowed", "rule": "",
  "reason": "example.org:443 is not on the egress allow list"
}
```

`address` appears only on a `denied_address` verdict. The model is told the
**class** ("resolves to a loopback address"); the audit log keeps the address.
That split is deliberate: an address is useful to an operator investigating
and useful to a model planning a way around the filter.

Denials land in the same NDJSON log as permission decisions and ship through
the same uploader (`-audit-upload-url`), so one cursor covers both.

---

## 7. What this does not do

Listed because a security boundary that overstates itself is worse than none:

- **A process that ignores the proxy variables can still connect.** The kernel
  floor (network namespace in containers, Seatbelt/bwrap on employee machines)
  is the next piece of work, and it is what makes this a wall rather than a
  filter.
- **No TLS interception.** A CONNECT tunnel is opaque: the destination is
  filtered, the bytes are not. That is why credential masking is not here —
  injecting a secret into a decrypted request needs a per-sandbox CA and the
  operational story for it (mTLS upstreams, certificate pinning, trust
  bundles), which is its own project.
- **No per-connection "ask the human".** Undecided means denied. A prompt per
  connection is a UX question with a rate-limit problem attached.
- **No UDP, no inbound.** SOCKS5 CONNECT only.
- **One proxy per agent.** Every sandbox on that agent shares the policy. Per
  sandbox policies need the control plane to carry the policy in the sandbox's
  job payload, which is the follow-up for container mode.
- **The policy is read at startup.** Editing the file has no effect until the
  agent restarts; a hot reload would need to be explicit about which changes
  take effect on in-flight tunnels (none of them do).
- **`git` over SSH is not covered.** `ssh` reads no proxy variable. The fix is
  the `GIT_SSH_COMMAND` + `socat` trick sandbox-runtime uses, which means
  requiring `socat` on every employee machine; not doing that yet.

---

## 8. Verifying it works

```bash
# allowed, through the proxy the sandbox was given
curl -x "$HTTP_PROXY" https://api.github.com/zen

# refused by policy
curl -x "$HTTP_PROXY" https://example.org/
# → 403, body: example.org:443 is not on the egress allow list

# refused by the address check even though the name is allowed
# (add "localhost:8080" to allowed_domains first)
curl -x "$HTTP_PROXY" http://localhost:8080/
# → 403, X-Deny-Reason: denied_address, body: … resolves to a loopback address

# the token is what stands between another local process and your allow list
curl -x "http://$(echo $HTTP_PROXY | sed 's|http://||;s|@.*||;' | cut -d: -f3)@127.0.0.1:PORT" …
```

The unit and end-to-end tests behind this live in `pkg/egress/`:
`policy_test.go` (entry grammar and precedence), `guard_test.go` (the address
classes, with an injected resolver), `proxy_test.go` (a real HTTP client, a
real CONNECT tunnel, a real SOCKS5 handshake, and the auth refusals).

---

## 9. Configuration reference

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-egress-mode` | `SANDRPOD_EGRESS_MODE` | `off` | `off` \| `enforce` |
| `-egress-policy` | `SANDRPOD_EGRESS_POLICY` | — | Path to the policy JSON; required with `enforce` |

See also: [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) for the consent
gate this complements, and
[SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md) for where the model
came from.