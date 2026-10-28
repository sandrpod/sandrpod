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