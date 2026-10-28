---
title: Workspaces and roles
description: Two roles, what each may do, and how work is separated inside one account.
---

# Workspaces and roles

A workspace owns sandboxes, members, API keys, workers and money. An account can
belong to several and switch between them; nothing crosses a workspace boundary.

## The two roles

| Action | Owner | Member |
|---|---|---|
| Create, stop, delete a sandbox | ✓ | ✓ |
| Open a terminal, run code, move files | ✓ | ✓ |
| Read usage, audit, settings | ✓ | ✓ |
| Invite or remove members | ✓ | ✗ |
| Create or revoke API keys | ✓ | ✗ |
| Top up, change the plan, delete the workspace | ✓ | ✗ |
| Mint a worker join token | ✓ | ✗ |

A member is a collaborator, not a co-owner. The distinction is enforced on the
server — a member calling an owner-only endpoint gets `403 owner_required`, not a
hidden button.

**Why two roles and not four.** Every extra role is a decision someone has to make
at invite time, and the common case is "someone who uses the sandboxes" versus
"someone who pays for them". If your team needs finer control, give each group its
own workspace; the boundary is already there and costs nothing to add.

## Invitations

**Settings → Members → Invite** sends a link. The recipient either signs up with it
(invite-only deployments) or uses it to join an existing account. Registration mode
is a deployment setting:

| Mode | Who can create an account |
|---|---|
| `open` | anyone with the URL |
| `invite` | only through an invitation |
| `closed` | nobody; an administrator creates accounts |

## Deleting things

| What | Effect |
|---|---|
| A sandbox | container and record gone; usage already metered stays metered |
| A workspace | sandboxes, keys, members, audit trail and join tokens removed; payment records are kept for tax, detached from the workspace |
| An account | same as a workspace, plus sessions and verification tokens |

Deletion is immediate and audited. There is no soft-delete window to recover from —
if you need the data, export or snapshot before deleting.

## Audit trail

Every lifecycle event a workspace can see about itself lands in **Audit**: who
created what, when it started, when it was deleted, how much it reserved. Kept for
**180 days**, then pruned; a promise with a pruner behind it.

## Next

- [API keys](/console-keys) — the credential a script carries.
- [Billing and usage](/console-billing) — what a sandbox costs.
