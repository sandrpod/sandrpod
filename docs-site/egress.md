---
title: Egress policy
description: An allow-list the sandbox cannot walk around — how it is expressed, enforced, and when it applies.
---

# Egress policy

By default a sandbox has the network access its container has. An egress policy
narrows it to a list, and on a Docker worker the narrowing is structural rather
than advisory.

## The policy

```json
{
  "allowed_domains": ["pypi.org", "github.com", "*.example.com"]
}
```

Set it at creation, or replace it later:

```bash
sandrpod-cli create agent-01 --provider local --allow-domain pypi.org --allow-domain github.com
sandrpod-cli egress get agent-01
sandrpod-cli egress set agent-01 policy.json
sandrpod-cli egress clear agent-01
```

`egress clear` removes the policy (back to "unconfigured"). It is **not** the same
as setting `{}`, which allows nothing.

## How it is enforced on a Docker worker

```
sandbox container ──► internal docker network (no route out)
                              │
                              ▼
                     egress-proxy sidecar ──► the internet
                     (also on a normal network)
```

The container is attached only to the internal network, so its single path is the
proxy, which allows or denies by hostname. It cannot be defeated by connecting to
an IP directly, because there is nothing to connect from.

Two consequences:

- The worker must have the `ghcr.io/sandrpod/egress-proxy` image **before** the
  first such sandbox; creation fails after the record is written otherwise.
- Only workers advertising the `egress` capability can host these sandboxes. On a
  fleet without one, creation answers `409 worker capability unavailable` instead
  of starting an unenforced sandbox.

## When it applies

**At container creation.** The network is built with the container, so a policy
set on a running sandbox is stored and takes effect the next time that sandbox is
created. It is not a live firewall.

This is deliberate — rebuilding a container's network in place is a restart with
extra steps — but it means `create --allow-domain` is the path that matters, and
`PUT /egress` is for the next generation of that sandbox.

## Verifying it

```bash
sandrpod-cli create box --provider local --allow-domain example.com
sandrpod-cli execute box "curl -s -o /dev/null -w '%{http_code}' https://example.com"   # 200
sandrpod-cli execute box "curl -s -o /dev/null -w '%{http_code}' https://pypi.org"      # 000
```

`000` is curl's "could not connect": the connection never reached the network, so
there was nothing to answer.

## Agent mode

On a real machine (`sandrpod-agent`) enforcement is different, because there is no
namespace to remove:

| Mode | Behaviour |
|---|---|
| `off` | no policy |
| `enforce` | read a local policy file and proxy through it |
| `server` | start deny-all, poll the control plane for the policy |
| `decider` | ask the control plane for a verdict per connection, fail-closed, 10 s cache |

The proxy sits on loopback and the child processes get its environment, so a
sandboxed process's traffic routes through it. Denials are audited with a reason
the model can read.

## Decider mode

`POST /api/v1/policy/decide` answers one destination at a time:

```json
{"sandbox":"agent-01","host":"pypi.org","port":443}
```

```json
{"allow":true,"rule":"allowed_domains","reason":"matched pypi.org"}
```

A denial is a `200` with `allow:false`, not an HTTP error: the agent must be able
to distinguish "denied" from "the decider is down", and the second must fail
closed.

## Credential masking is separate

An allow-list decides *where* traffic may go. Credential masking decides *what the
sandbox can see* inside that traffic: sentinel values in the sandbox are replaced
with real credentials at the proxy, TLS is terminated with a per-host certificate,
and AWS SigV4 requests are re-signed. The sandbox never holds the secret. See
[CREDENTIALS.md](https://github.com/sandrpod/sandrpod/blob/main/docs/CREDENTIALS.md).