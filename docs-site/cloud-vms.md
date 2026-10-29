---
title: Cloud VMs as the substrate
description: A sandbox can provision its own machine — what it needs, and what the failure modes look like.
---

# Cloud VMs as the substrate

`provider_type: local` runs a sandbox on a worker you already joined. Any other
provider provisions **a VM for that sandbox**, installs Docker on it, starts a
worker there, and places the sandbox on it.

```bash
sandrpod-cli create agent-01 --provider aliyun --region cn-hongkong \
  --instance-type ecs.e-c1m2.large --ttl 1800
```

## What the deployment needs

| Piece | Aliyun | AWS |
|---|---|---|
| Credentials | `ALIYUN_ACCESS_KEY`, `ALIYUN_SECRET_KEY` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |
| Region default | `ALIYUN_REGION` | — (per request) |
| Subnet | `SANDRPOD_VM_SUBNET_ID_ALIYUN` | `SANDRPOD_VM_SUBNET_ID_AWS` |
| Security group | `SANDRPOD_VM_SECURITY_GROUP_ALIYUN` | `SANDRPOD_VM_SECURITY_GROUP_AWS` |
| Public IP | on by default; `SANDRPOD_VM_PUBLIC_IP_ALIYUN=false` for NAT | same |

A provider only appears in `/api/v1/capabilities` once it registers, and the console
disables the rest — offering a substrate the server cannot provision produces a form
that fails after it is filled in.

The VM needs **outbound** network: it installs Docker, pulls the worker image, and
dials the control plane. No inbound port is required, but the worker reaches the
control plane at its public URL, so `SANDRPOD_PUBLIC_URL` must be the address a
stranger's machine can use.

## What happens on create

```
create → provider API (RunInstances) → wait for running → public IP
       → Cloud Assistant: install Docker (10-minute budget)
       → Cloud Assistant: pre-pull the sandbox image
       → start the worker container → it dials the control plane
       → wait for registration (5 minutes) → create the sandbox
```

Every failure up to registration gives the machine back, and the error names the
step. After registration the VM is a worker in the fleet; deleting the sandbox does
not delete it, because the worker may be used again.

## Reclaiming the worker

A VM that has held no sandboxes for `SANDRPOD_PODER_IDLE_TIMEOUT` is terminated and
its record removed. This is not optional on a metered cloud — without it every
create leaves a machine billing for as long as nobody looks.

```bash
SANDRPOD_PODER_IDLE_TIMEOUT=20m
```

Emptiness is decided by **sandbox records**, not by a container counter: a container
whose record is gone is unreachable, and letting it pin the VM keeps paying for
nothing.

## Four things that were wrong, and are now fixed

Each of these turned a working feature into a machine that was created and thrown
away. They are recorded because the symptoms point everywhere except the cause.

| Symptom | Cause |
|---|---|
| `provider aws not found` | provider registration lived in `Main()`; an embedding build never runs it |
| VM created, then "docker: command not found" | Cloud Assistant's 60 s default timeout killed the Docker install, and a timeout was reported as success |
| Worker started, never dialled | the VM was told to reach the control plane at `127.0.0.1` |
| First create timed out; container appeared afterwards | the 550 MB sandbox image was pulled inside a two-minute budget. It is pre-pulled during bootstrap now |
| **VM outlived every sandbox** | the idle reaper was also only started from `Main()`, and an orphan container counted as busy |

## Verifying a deployment

```bash
sandrpod-cli create canary --provider aliyun --region cn-hongkong \
  --instance-type ecs.e-c1m2.large --ttl 900
sandrpod-cli execute canary "uname -srm; curl -s -o /dev/null -w '%{http_code}' https://example.com"
sandrpod-cli delete canary
# then: the VM must disappear once the idle timeout passes
```

A create is the only test that exercises the provider API, the bootstrap, the tunnel
and the container together. The last line is the one people forget, and it is the one
that costs money.
