---
title: Limits
description: Plan caps, idle reaping, and the two limits that surprise people.
---

# Limits

## Plan caps

Concurrent sandboxes per workspace:

| Plan | Concurrent sandboxes |
|---|---|
| trial | **1** |
| starter | 5 |
| team | 20 |
| enterprise / unlimited | no cap |

A stopped sandbox still occupies a slot until it is deleted — stopping pauses the
container, it does not free the plan.

An unknown or unset plan is treated as **trial**. A limit that fails closed is
better than a plan that silently grants twenty sandboxes because a field was empty.

## Idle reaping

A sandbox with no activity for its TTL is stopped and its slot freed. Activity —
execution, a terminal session, a file operation — resets the clock. The default is
a few hours; `--ttl` sets it per sandbox, up to the plan's maximum.

If you need a long-lived sandbox, set the TTL explicitly rather than relying on
activity: an agent that thinks for an hour between calls looks idle.

## The two that surprise people

**1. The plan cap counts sandboxes, not work.** One sandbox can run a hundred
sequential commands; ten parallel agents need ten sandboxes and are refused on
trial. Parallelism is what you are buying.

**2. Disk is not metered, but it is finite.** A workspace that writes 40 GB into
`/workspace` fills the worker and every sandbox on it degrades. The console shows
per-sandbox disk in **Metrics**; nothing stops you from writing that much.

## Rate limits

Sign-in, password reset and registration are rate limited per account and per
address (5 and 30 per 15 minutes). A refusal is `429` with `Retry-After`. In a
multi-instance deployment the limit is per instance, so N instances mean N times
the budget — documented rather than pretended away.

## Next

- [Billing and usage](/console-billing) — what the plan costs.
- [Troubleshooting](/console-troubleshooting) — when a create is refused.
