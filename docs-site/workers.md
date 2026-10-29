---
title: Workers and tunnels
description: How a machine joins the fleet, what it advertises, and what it may do.
---

# Workers and tunnels

A worker is a machine that runs sandboxes. It joins by **dialing out**, so it
never needs an inbound port, a public address, or a firewall exception.

## Joining

There are two shapes, and they answer different questions.

```bash
# on the control plane: mint a join token for one workspace
curl -s -X POST "$API/portal/v1/join-tokens" \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"worker-1"}'
```

```json
{"secret":"jn_…",
 "command":"curl -fsSL https://docs.sandrpod.com/install.sh | sudo bash -s -- --api-url=… --token=jn_… --yes",
 "docker_run":"docker run -d --name sandrpod-worker …",
 "agent_command":"curl -fsSL https://docs.sandrpod.com/agent.sh | sudo bash -s -- … --name=device-worker-1"}
```

| This machine | Use | Why |
|---|---|---|
| a server with Docker | `install.sh` (the worker) | it hosts **many** sandbox containers; the installer handles Docker, the data disk, mirrors, swap and the ulimits a sandbox needs |
| a laptop, a build box, a GPU host | `agent.sh` (the device) | the machine **is** the sandbox: its files, its tools, its GPU, no container in between |
| Docker already installed | `docker_run` | the same worker, without the steps it would skip |

Both scripts are idempotent and both end by printing how to watch and remove the
service. Same credential, because a worker and an agent are both worker-role.

**The difference matters.** A worker isolates every sandbox in a container and can
hold a dozen of them; an agent has no isolation at all beyond the OS itself, which is
what you want for a machine whose whole job is to be one agent's hands — and exactly
what you do not want for a shared fleet.

**Region is the field that bites.** The CLI and the E2B surface both default to
`local`; a worker registered with `-region=hk` is invisible to them, and creation
fails with `no available local poder found` while the fleet looks healthy.

## What a worker advertises

At handshake the worker sends its identity, resources, **capabilities**, and
**tenant**:

| Field | Effect |
|---|---|
| `X-Poder-Capabilities: egress` | only this worker may host a sandbox that asks for an egress policy |
| `X-Poder-Workspace: …` | placement scoped: a sandbox from workspace A is not scheduled onto workspace B's worker |
| resources (arch, OS, cores, memory) | recorded on the sandbox so the API can report it |

A sandbox that asks for a policy on a fleet with no capable worker gets **409**,
not a sandbox that silently runs unwalled.

## Heartbeats and state

Workers heartbeat every ten seconds with their container list. That does three
things: keeps the record `ONLINE`, lets the control plane reconcile sandbox states
against the containers that actually exist, and (in a multi-instance deployment)
renews which node holds the tunnel.

A worker that stops heartbeating goes `OFFLINE`; its sandboxes stop being
reachable, and a create that lands on it fails with a reason rather than hanging.

## Prerequisites on the machine

| Requirement | Why |
|---|---|
| Docker socket | the worker creates containers |
| `SANDRPOD_TOOLBOX_IMAGE` | the sandbox base image, pulled on first use |
| `ghcr.io/sandrpod/egress-proxy` pre-pulled | only for sandboxes with an egress policy; without it creation fails **after** the record is written |
| Ports: none inbound; outbound HTTPS + DNS | the worker dials the control plane and pulls images |

Disk is the resource that fills first: images plus per-sandbox writable layers.
A 4 vCPU / 8 GB / 100 GB machine carries several concurrent sandboxes; the
default sandbox is 1 core and 1 GiB.

## Pausing versus deleting

Stopping a sandbox pauses the container; it still occupies a container slot on
the worker until it is deleted. A worker at its `max-containers` limit refuses new
placement (`no available poder found`), which is a capacity answer, not a bug.

## Seeing the fleet

```bash
sandrpod-cli poder list          # id, state, provider, arch, region
curl -s "$API/api/v1/poders" -H "X-Sandrpod-Token: $TOKEN" | jq
```

`poder delete <id>` removes the record and drops the tunnel; `--keep-vm` leaves a
cloud VM running for debugging.

## Next

- [Isolation and placement](/isolation) — the rules that decide which worker gets which sandbox.
- [More than one instance](/multi-instance) — when the control plane itself is a fleet.