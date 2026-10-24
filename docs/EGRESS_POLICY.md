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
| **Kernel floor** (network namespace, Seatbelt, bwrap, WFP) | Stops a process that ignores the proxy variables | ✅ for container sandboxes (Docker's internal network, §2.3) · ❌ for laptops — [roadmap](#7-what-this-does-not-do) |

That last row is the honest part. The proxy is a **filter**: it covers every
tool that honours `HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`, which is most of
what an agent actually runs (curl, wget, npm, pip, cargo, go, git over http,
API SDKs, package managers). It does not cover a process that opens a socket
directly and ignores the environment. Treat it as "the agent cannot casually
walk data out", not as "the machine is airgapped".

---

## 2. Quick start

### 2.1 From a file (one machine, hands on)

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

### 2.2 From the control plane (the fleet case)

The policy belongs to the sandbox, not to the machine it runs on: it is set
when the sandbox is created, it is returned with the sandbox record, and it can
be tightened while the sandbox runs.

```bash
# Create a sandbox that may reach npm and GitHub, and nothing else.
curl -sS -X POST https://api.example.com/api/v1/sandboxes \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "sb-1", "provider_type": "local", "region": "local",
        "network": { "allowed_domains": ["registry.npmjs.org:443", "api.github.com"] }
      }'

# What this sandbox is allowed to reach (the sandbox itself polls this).
curl -sS https://api.example.com/api/v1/sandboxes/sb-1/egress \
  -H "X-Sandrpod-Token: $TOKEN"

# Tighten it while it runs. Takes effect on the sandbox's next connection.
curl -sS -X PUT https://api.example.com/api/v1/sandboxes/sb-1/egress \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"allowed_domains": ["registry.npmjs.org:443"]}'
```

The agent then runs with `-egress-mode=server` and no policy file:

```bash
sandrpod-agent -api-url=https://api.example.com -name=sb-1 -token=… \
  -egress-mode=server -egress-sync=60s
```

Ordering is deliberate: the proxy starts with an **empty** policy and only
widens when the control plane answers. The agent is reachable through the
tunnel from the moment it registers, so a proxy that started permissive "for a
second" would be a hole with a schedule. A control plane that never answers
leaves the sandbox with no egress and a log line saying why.

The first seconds are worth spelling out, because the sandbox record is created
*by* the agent's own registration: a 404 from the endpoint means "not
registered yet", not "no policy". The agent retries that case every 2s instead
of waiting out the sync interval, so a restart costs a couple of seconds of no
egress rather than a minute. A 200 with a `null` policy is the other thing
entirely — a registered sandbox nobody configured — and it means deny all.

Two storage-level notes, because they are the parts that surprise people:

- A create request with a policy that does not parse is **400**, before any
  provisioning: the operator sees the typo while the call is still in front of
  them. The same validation runs on PUT, so a rejected edit leaves the running
  policy untouched.
- A PUT changes the policy, not the connections already open. The proxy filters
  destinations at connection time; an established CONNECT tunnel keeps
  carrying bytes until it closes. For "stop everything now", delete the
  sandbox — that closes the tunnel and the container with it.

### 2.3 In a container (poder)

A container sandbox enforces the same policy, and Docker supplies the part a
proxy cannot: the container's route out is **removed**.

```
sandbox ── (internal network) ── sidecar proxy ── (bridge) ── internet
```

- a per-pod bridge network created with `Internal: true`. Docker installs no
  masquerade rule for it, so a container on it has no route off the machine —
  not even DNS beyond the embedded resolver;
- the sandbox container joins that network and nothing else;
- a small sidecar runs the proxy (`cmd/egress-proxy`, image
  `ghcr.io/sandrpod/egress-proxy`, built with `make docker-egress`), joined to
  the internal network so the sandbox reaches it by name, and to an ordinary
  network so it can reach out. It listens on fixed ports (8080 HTTP / 1080
  SOCKS) because the sandbox's environment is written before the proxy starts,
  and its password is minted per pod.

Nothing changes at the API: the same `network` block on create. The policy
reaches the poder in the pod request, and if the proxy cannot be stood up the
create fails — a sandbox that asked for a policy and silently did not get one
is worse than one that was never created.

Two deployment notes worth knowing before you see them:

- **A poder running as a container joins each pod's internal network**, which is
  what keeps the toolbox (`http://<pod IP>:8080`) reachable. A poder on a Linux
  host reaches the bridge itself and needs nothing. This is why the proxy is a
  sidecar and not a listener in the poder: on Docker Desktop the bridge address
  lives inside the Docker VM, and the host cannot bind it at all.
- `SANDRPOD_EGRESS_IMAGE` overrides the sidecar image, for an air-gapped mirror.

Verified against a real daemon: with a policy allowing `example.com` only, a
container reaches it through the proxy (200), is refused `example.org` and a
`denied_domains` entry (403), and cannot reach `example.com` at all with
`--noproxy '*'` — the last one is the difference between this and the
laptop case.

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
- **A container can still reach what else joins its network.** The sandbox's
  only route is the per-pod internal network, whose only other member is the
  proxy — but on a Linux host the host's own services on the bridge subnet are
  reachable too. Closing that needs host-level `iptables` (a `DOCKER-USER`
  chain, root) and is not in this build.
- **On a laptop it is a wall only with the OS sandbox on.** With
  `-os-sandbox=bwrap|auto` on Linux, the sandbox gets its own network namespace
  and reaches the proxy through a relay on loopback — a process that ignores the
  proxy variables then reaches nothing (see
  [OS_SANDBOX.md](OS_SANDBOX.md) §4a). Without that flag, or on macOS, a process
  that ignores the proxy variables still gets out: the kernel floor is what
  turns this filter into a wall, and on macOS it has no network half yet.
- **`-egress-mode=server` refreshes, `-egress-mode=enforce` does not.** In
  server mode the agent re-reads the policy every `-egress-sync` (60s by
  default) and swaps it into the running proxy; a file-based policy is read
  once at startup. Both apply to the *next* connection — in-flight tunnels are
  not cut (see 2.2).
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
| `-egress-mode` | `SANDRPOD_EGRESS_MODE` | `off` | `off` \| `enforce` (local file) \| `server` (control plane) |
| `-egress-policy` | `SANDRPOD_EGRESS_POLICY` | — | Path to the policy JSON; required with `enforce`, ignored with `server` |
| `-egress-sync` | `SANDRPOD_EGRESS_SYNC` | `60s` | How often `server` re-reads the policy (floor 5s) |

See also: [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) for the consent
gate this complements, and
[SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md) for where the model
came from.