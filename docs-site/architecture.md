---
title: Architecture
description: Three processes, one reverse tunnel, and where a request goes.
---

# Architecture

```
        your agent / SDK / CLI
                 │  HTTPS
                 ▼
   ┌──────────────────────────────┐
   │  control plane (pkg/server)  │  scheduler · tunnels · policy · API
   └──────────────┬───────────────┘
                  │  reverse WebSocket + yamux   ← the worker dials out
                  ▼
   ┌──────────────────────────────┐
   │  worker (cmd/poder)          │  Docker lifecycle on one machine
   └──────────────┬───────────────┘
                  │  container network
                  ▼
   ┌──────────────────────────────┐
   │  toolbox (in the sandbox)    │  code execution · PTY · files
   └──────────────────────────────┘
```

## The control plane

`pkg/server` is the whole API: it schedules a create onto a worker, keeps the
tunnel objects, enforces auth and quotas, calls the admission gate, and emits
lifecycle events. It is importable as a library — `server.New(...)` returns the
handler the command-line binary serves — which is how a commercial build adds
accounts and billing without forking anything.

Persistent state is either SQLite (one instance) or PostgreSQL (a fleet). The
schema is additive: an older binary runs against a newer schema, which is what
makes a rollback a tag change rather than a migration.

## The worker

`cmd/poder` runs on the machine that has the Docker socket. It dials the control
plane once and keeps that connection: **no inbound port is opened on the
worker**, which is the property that makes it usable on a laptop behind NAT, a
customer's VM, or an employee workstation.

Over that tunnel it receives "create this container", "delete that one", and
forwards execution requests to the toolbox inside each sandbox.

## The toolbox

`cmd/toolbox` runs *inside* every sandbox. It is the thing that runs code:
language runtimes, a PTY, file operations, streaming output, and a port preview
proxy. It listens on `8080` inside the container; nothing outside reaches it
directly.

## The agent (no Docker)

`cmd/agent` registers the local machine itself as the sandbox, embedding the
toolbox. That is how SandrPod runs on a macOS laptop or a bare VM with no Docker:
the machine is the sandbox. The same binary carries the permission gate, the
decision audit, egress enforcement, and OS-level sandboxing (`bwrap`, `seatbelt`).

## What a request actually does

`POST /sandboxes/execute` on a running sandbox:

1. control plane authenticates, resolves the sandbox, checks ownership;
2. finds the tunnel for the sandbox's worker — locally, or on a peer instance
   (it forwards, see [More than one instance](/multi-instance));
3. opens a yamux stream to the worker and asks for the toolbox;
4. the worker dials the container's toolbox over the container network;
5. the toolbox runs the process and streams the result back along the same path.

Latency is dominated by the process, not the hops: the tunnel is a warm TCP
connection, not a new one per request.

## Eight providers, two mechanisms

Creating a worker *in the cloud* is the same shape for all eight providers, with
two remote-exec mechanisms underneath:

| Mechanism | Providers |
|---|---|
| managed run-command | AWS (SSM), Aliyun (Cloud Assistant), Azure, Tencent (TAT), Oracle |
| SSH with an ephemeral key | GCP, DigitalOcean, Hetzner |

Either way the VM boots, Docker is installed, a worker is started with a join
token, and the control plane waits for it to come online. If it never does, the
VM is terminated rather than left billing — every failure path in that flow gives
the machine back.

## Where to read next

- [Workers and tunnels](/workers) — the connection, its identity, its lifecycle.
- [Isolation and placement](/isolation) — who may run where.
- [Self-host in production](/deploy) — the deployment shape recommended for real traffic.