---
title: Isolation and placement
description: Who may run where, what a sandbox can reach, and what the platform refuses to do.
---

# Isolation and placement

Four separate controls, each doing one job. Confusing them is how a multi-tenant
deployment ends up with one tenant's sandbox on another tenant's hardware.

## 1. Placement is scoped to a tenant

Every worker carries a workspace. Every create carries a tenant, taken from the
credential that made the request — not from the request body.

```
sandbox(tenant=A) → worker(tenant=A)     ✓
sandbox(tenant=A) → worker(tenant=B)     ✗ never scheduled
sandbox(tenant=A) → worker(tenant="")    ✓ self-hosted fleet, unscoped on purpose
```

The rule lives in the scheduler, so it applies to every path that creates a
sandbox: the native API, the E2B surface, and a resumed delivery after a crash.

## 2. The worker role can do two things

A worker authenticates with a join token (`jn_…`) and gets a role that reaches
exactly two routes: the tunnel handshake, and its own heartbeat. It cannot list
the fleet, read a sandbox, or mint anything. Anything else answers `403`.

That matters because a join token ships to a machine you may not fully trust —
a customer's VM, a contractor's laptop. A compromised worker can run sandboxes;
it cannot see the rest of your deployment.

## 3. Network egress is a policy, not a hope

A sandbox can be created with an allow-list. On a Docker worker that is enforced
structurally: a per-sandbox internal network with no route out, plus an egress
proxy sidecar on a normal network. The sandbox is attached only to the internal
one, so it has no path except the proxy.

```json
{"allowed_domains": ["pypi.org", "github.com"]}
```

The policy is applied **when the container is created**. Setting it on a running
sandbox records it for the next creation; it cannot retrofit a network. Details
and the trade-offs are in [Egress policy](/egress).

## 4. The permission gate (agent and employee-PC mode)

In agent mode the sandbox is a real user's machine, so the question is different:
not "what can this container reach" but "what may this agent do to my files".
That is a six-branch decision engine — hardlocks, mandatory denials, the work
tree, permanent grants, session grants, then ask — with every decision written to
an audit log, and an optional OS sandbox underneath (`bwrap` on Linux, `seatbelt`
on macOS) that enforces the same answers at the kernel level.

It is opt-in (`-permission-mode prompt|strict`), because on a throwaway sandbox it
would only add prompts.

## What the platform refuses

Refusals are features. Each of these is deliberate:

| Request | Answer | Why |
|---|---|---|
| A policy on a fleet with no egress-capable worker | `409 worker capability unavailable` | the alternative is a sandbox reported as walled that runs unwalled |
| A create that would exceed the workspace's concurrent sandbox limit | `403` from the admission gate | the gate runs at the choke point, so no entry point can skip it |
| A member trying to delete a worker or mint a key | `403 owner_required` | spending and access decisions belong to the workspace owner |
| A sandbox from another tenant, by name | `404` | indistinguishable from "does not exist", on purpose |

## What isolation is not

- **Not a VM boundary.** Two sandboxes on one worker share a kernel. If you need
  hardware isolation, give a workspace its own worker (or its own cloud VM).
- **Not a resource guarantee.** `cpu_cores` and `memory_mb` are scheduler hints;
  the container limits are Docker's, and a busy worker degrades every sandbox on
  it.
- **Not protection from the worker.** The worker runs Docker: it can see every
  container it hosts. That is why the join token is scoped and short-lived, and
  why a customer's own hardware is the strongest tenancy boundary available.

## Next

- [Egress policy](/egress) — the policy format and its enforcement.
- [Workers and tunnels](/workers) — capabilities and the tenant field.