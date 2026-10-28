---
title: API keys
description: sk_ and e2b_ keys, worker join tokens, and what each one can reach.
---

# API keys

Three kinds of credential, three jobs.

| Credential | Format | Reaches |
|---|---|---|
| Workspace key (CLI/API) | `sk_<hex>` | one workspace: sandboxes, files, execution |
| Workspace key (E2B) | `e2b_<hex>` | the same, on the E2B-compatible surface |
| Worker join token | `jn_<hex>` | the tunnel handshake and its own heartbeat — nothing else |
| Platform token | 32 hex chars | everything, every workspace (operators only) |

## The two spellings of a workspace key

`sk_` and `e2b_` are the same credential with different prefixes, because the
official E2B SDK validates the format **before** it sends a request and refuses
anything that is not `e2b_<hex>`.

Create one in **Settings → API keys**, choosing the format:

```bash
curl -s -X POST https://cloud.sandrpod.com/portal/v1/api-keys \
  -H "Cookie: sandrpod_session=…" -H 'Content-Type: application/json' \
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
worker role reaches the tunnel handshake and its own heartbeat, and nothing else.
It cannot list sandboxes, read files, or mint keys.

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

<｜DSML｜ calls>
<｜DSML｜ invoke name="bash">
<｜DSML｜ parameter name="command" string="true">cd /Users/cjzhao/goworkspace/sandrpod/docs-site && cat > console-billing.md <<'MD'
---
title: Billing and usage
description: A prepaid wallet, what a sandbox costs, and what happens when it runs out.
---

# Billing and usage

The console is prepaid: you top up a wallet, sandboxes debit it as they run, and a
create is refused before the balance would go negative.

## What a sandbox costs

Priced from **what it reserved**, not from what the code inside did:

```
cost = (vCPU × 5 cents/hour + GiB × 1 cent/hour) × duration
```

A sandbox that reserves 1 vCPU and 1 GiB costs **6 cents per hour** — about ¥0.43
per day if it ran around the clock, which it should not. Pricing idle time is
deliberate: a sandbox holds its cores and memory whether or not the agent is
thinking, and "what you reserved" is the only number a customer can predict before
pressing create.

Billing is per second, rounded on the interval, not on the request.

## Top-ups

**Billing → Add credit.** Payment runs through a merchant of record, which means no
card data touches SandrPod — the checkout page is the provider's, and the browser
comes back to the console when it finishes.

Available top-ups are set per deployment (¥50 / ¥100 / ¥500 by default), in CNY.
The checkout session expires after an hour; an unpaid order is not a charge, and it
is swept rather than left pending forever.

## Refunds

A refund reverses credit on the wallet. If the credit has already been spent, the
balance can go negative — that is a debt, and the next top-up clears it before new
sandboxes can run.

## Running out

Three signals before the wall:

| Signal | When |
|---|---|
| Low-balance mail | balance falls below a threshold (¥2 in the reference deployment) |
| Banner in the console | same threshold, on every page |
| `403` on create | balance is at or below zero |

The refusal comes from the admission gate inside the control plane, so it applies
to the CLI, the API and the E2B surface identically. A refused create writes a job
with the reason and does not leave a sandbox record behind.

## Usage

**Usage** shows the current month: sandbox count, vCPU·hours, GiB·hours and the
money spent. It is the same data the wallet was debited from, not a separate
estimate — if the numbers ever disagree, the wallet is right.

## Taxes and invoices

Payment records are kept for as long as tax law requires, detached from the
workspace if the workspace is deleted. An invoice is issued by the merchant of
record, not by SandrPod.

## Next

- [Limits](/console-limits) — plan caps and idle reaping.
- [Workspaces and roles](/console-workspaces) — who may spend.
