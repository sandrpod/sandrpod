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


### What a device agent can touch

An agent is not a container: there is no filesystem boundary between it and the
machine. That is the point — it uses the host's tools, GPU and files — and it is also
the risk. `agent.sh` therefore starts it confined, and every part of that is a flag
you can see:

| Default | What it does | How to change it |
|---|---|---|
| runs as the **user who ran sudo**, not root | a laptop is not a build server | `--user` |
| `--work-dir ~/sandrpod-work` | writes are confined here — plus the system temp dir, which build tools expect; created `0700` | `--work-dir` |
| `--permission-mode prompt` | anything outside the work dir is asked about, and denied when nobody answers | `--permission-mode strict\|off` |
| `--os-sandbox auto` | kernel-enforced: root filesystem read-only, writes only under the work dir (seatbelt on macOS, bwrap on Linux) | `--os-sandbox off` |

`--full-machine` opens all of it — `/`, no gate, no kernel sandbox — and asks first.
It is the right choice for a box dedicated to one agent and the wrong one for the
laptop you are typing on.

Measured with the defaults on macOS: `$HOME`, `~/Documents`, `/usr/local/bin` and
`/private/var` refuse writes; `~/.ssh`, `~/Library/Mail`, `~/Library/Messages` and
`~/Library/Cookies` refuse reads; `/tmp` stays writable, because `pip` and `npm` fail
in ways that look like bugs when it does not. Everything else is read-only.

Running as root is the part people skip. As root, `/` and `./` are the same place and
a single bad command has no boundary to hit; the default now avoids it, and the
service runs under your user with the machine's own tools.


### Two credentials, not one

The command a customer pastes carries a **join token**: one hour, one workspace, meant
to be used once. The machine exchanges it for its own **worker key** on the first
connection, and keeps that — 180 days, bound to that machine's id.

| | join token (`jn_…`) | worker key (`wk_…`) |
|---|---|---|
| lives | 1 hour | 180 days |
| handed to | a human, to paste once | the machine, which stores it `0600` |
| travels in | the install command | the tunnel handshake, every reconnect |
| revoked | with the workspace's invitations | **per machine** |
| if it leaks | an hour to enrol a machine | access as that one worker |

The split exists because the two roles are different. A credential that lives long
enough to survive a reboot is one worth stealing, and it was issued to somebody who
has long since forgotten it; a credential short enough to be safe cannot survive a
reboot, and the first restart after it expires takes the node out of the fleet —
silently, until someone notices the console is empty.

```bash
# what the machine keeps, inside its data volume
$ docker exec sandrpod-worker cat /var/lib/sandrpod/worker-key
{"token":"wk_…","expires_at":"2027-04-27T15:01:40Z"}
```

Enrolment happens automatically. A worker that finds a stored key uses it and ignores
the token on its command line; one whose key expired says so in its log and enrols
again. If enrolment fails — older control plane, expired invitation — it falls back to
the invitation and keeps retrying, because refusing to start turns a missing feature
into an outage.

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