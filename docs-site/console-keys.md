---
title: API keys
description: sk_ and e2b_ keys, worker join tokens, and what each one can reach.
---

# API keys

Four kinds of credential, four jobs.

| Credential | Format | Reaches |
|---|---|---|
| Workspace key (CLI/API) | `sk_<hex>` | one workspace: sandboxes, files, execution |
| Workspace key (E2B) | `e2b_<hex>` | the same, on the E2B-compatible surface |
| Worker join token | `jn_<hex>` | the tunnel handshake, its job poll, and its own heartbeat — nothing else |
| Platform token | 32 hex chars | everything, every workspace (operators only) |

## The two spellings of a workspace key

`sk_` and `e2b_` are the same credential with different prefixes, because the
official E2B SDK validates the format **before** it sends a request and refuses
anything that is not `e2b_<hex>`.

Create one in **Settings → API keys**, choosing the format:

```bash
curl -s -X POST https://cloud.sandrpod.com/portal/v1/api-keys \
  -H "Cookie: sd_cloud_session=…" -H 'Content-Type: application/json' \
  -d '{"name":"agent-prod","format":"e2b"}'
```

```json
{"key":{"id":"…","name":"agent-prod","prefix":"e2b_1a2b3c","role":"member"},
 "secret":"e2b_1a2b3c…"}
```

The secret is returned **once**. The list shows the prefix, never the secret:
storage is a hash, so a lost key is replaced, not recovered.

## Revocation

Deleting a key revokes every use of it, including the `sk_`/`e2b_` twin — there is
one record, two spellings. A revoked key answers `401` on every surface.

## Worker join tokens

A join token lets a machine you own join the fleet. It is deliberately weak: the
worker role reaches the tunnel handshake, its job poll, and its own heartbeat, and
nothing else. It cannot list sandboxes, read files, or mint keys.

Mint one in **Workers → Add worker**; the console prints a `docker run` command
with the token in it. Treat it as a credential — it is one — and delete it when the
machine is decommissioned.

## Platform token

The operator credential, used by the deployment itself. It is not issued by the
console and should never appear in an agent. If it leaks, every workspace is
exposed; rotate it in the deployment environment.

## What to give an agent

| Situation | Key |
|---|---|
| One agent, one workspace | `sk_…` scoped to that workspace, revoked when the agent is retired |
| An E2B-based agent | `e2b_…`, same rules |
| Many agents in one workspace | one key per agent, so revocation is per agent |
| An agent that should never spend | a `member` key, in a workspace whose owner controls the wallet |

## Next

- [E2B drop-in](/e2b) — the surface an `e2b_` key unlocks.
- [Limits](/console-limits) — what a key can create.
