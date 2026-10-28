---
title: Console troubleshooting
description: The refusals and odd states the console produces, and what each one means.
---

# Console troubleshooting

## "You have reached your plan limit"

The workspace already holds as many concurrent sandboxes as its plan allows —
**including stopped ones**. Delete the sandboxes you are not using; deleting frees
the slot, stopping does not. See [Limits](/console-limits).

## "Insufficient balance"

The wallet is at or below zero. Top up, or delete what is running. The mail and the
banner appear before this: a refusal is the last step, not the first warning.

## Paid, but the credit did not arrive

Three things to check, in order:

1. **The order status in Billing.** `pending` means the provider has not told us
   yet; it expires after the session lifetime and is not a charge.
2. **The provider's dashboard.** If it shows the payment as completed, the webhook
   did not reach us — a deployment problem, not a money problem. Support can
   reconcile it.
3. **The amount.** Top-ups credit the amount you paid, converted to cents.

A failed card leaves the order `pending` and no mail is sent. That is the
provider's contract, not a bug: there is no failure webhook for a one-time order.

## The terminal connects but shows nothing

The sandbox is running but its worker is not connected to the instance serving the
request — the console shows `tunnel not available` in that case. Reload once; if it
persists, the worker is down. Workers page → state `OFFLINE`.

## A sandbox is stuck on "provisioning"

The create never reached a worker. Open the sandbox's menu → **Job** to see the
reason. `no available poder found` means the fleet cannot place it: check
**Workers** (online? at its container limit? in the right region?).

The engine retries a lost delivery for about a minute; if it is still provisioning
after that, the job row has the reason.

## The API key works in the CLI but not in the E2B SDK

The SDK wants `e2b_<hex>` and validates it client-side. Mint a key with
`format: e2b` — see [API keys](/console-keys).

## Mail never arrives

Verification and reset links come from the deployment's relay. Check spam first,
then support: a misconfigured sender domain is the usual cause, and it affects every
recipient at once.

## Where to look

| Question | Where |
|---|---|
| Is the platform healthy? | `/healthz` on the deployment |
| What is this workspace doing? | Workspaces → Usage |
| Who did what? | Audit |
| Why was this create refused? | Sandbox menu → Job |
| Are the workers up? | Workers |
