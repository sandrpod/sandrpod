# Alibaba Cloud (Aliyun) Auto-Provisioning Guide

How to bring the **Aliyun** provider to the same working state as AWS: create an
ECS instance on demand, bootstrap a Poder on it via Cloud Assist, and run
sandboxes there.

> **Status: live-validated end-to-end**, twice — `cn-beijing` originally, and
> `cn-hongkong` on 2026-10-29 after the fixes in §2: create → ECS launch → Cloud
> Assist installs Docker → image pre-pulled → Poder registers over the tunnel →
> sandbox RUNNING → code executes → sandbox deleted → **VM reclaimed by the idle
> reaper**. No machine and no container left behind, which is what "no leak" has to
> mean on a metered cloud.
>
> The second validation found five defects that the first could not expose, because
> they only appear on a deployment that the *commercial* build drives (no `Main()`).
> They are listed in §6 with their symptoms. See
> [AWS_PROVISIONING.md](AWS_PROVISIONING.md) for the shared mechanics, and
> [docs.sandrpod.com/cloud-vms](https://docs.sandrpod.com/cloud-vms) for the short
> version.

---

## 1. What Aliyun already inherits for free

Most of the work done while making AWS work lives **outside** the provider and is
provider-agnostic, so Aliyun gets it without changes:

| Layer | What it gives Aliyun |
|-------|----------------------|
| Scheduler (`pkg/sandpod/scheduler.go`) | forwards the API token, `PROVIDER_TYPE`, the request **region** (not AZ), `VM_INSTANCE_ID`, and the configurable poder/toolbox image to the Poder; builds VM networking (public IP / subnet / SG) via `NetworkConfig` |
| Poder (`pkg/poder/docker.go`) | `CreatePod` pulls the toolbox image before creating the container |
| Server (`pkg/server/main.go`) | delete-poder terminates the cloud VM (`isCloudProvider` includes `aliyun`); the reaper reclaims OFFLINE poders; both call `provider.DeleteVM(providerType, vmID)` |

So the remaining work is **(a)** the Aliyun-specific provider fixes and **(b)**
Aliyun deployment config. The control-plane / scheduler / poder plumbing is done.

---

## 2. Hardening applied (`pkg/provider/aliyun/aliyun.go`)

The provider received the same hardening as the AWS path. What shipped, mapped
to the Aliyun methods:

| Issue class (first seen in AWS) | Aliyun method | Shipped behavior |
|---------------------------------|---------------|------------------|
| `CreateVM` returns no IP | `CreateVM` (~104): `RunInstances` (creates *and* starts in one call) | After the instance is **Running**, `DescribeInstances` and read `PublicIpAddress` / `InnerIpAddress`. Aliyun assigns the public IP **asynchronously** once `InternetMaxBandwidthOut > 0` and the instance starts, so it isn't known at create time. `RunInstances` replaced `CreateInstance`+`StartInstance` because the separate start races the instance's initialisation (`IncorrectInstanceStatus`). |
| `GetVM` returns stale cache | `GetVM` (~203): reads `p.vms` first | Always `DescribeInstances`; refresh the cache. The cached snapshot is `Pending` forever otherwise → health never ready. |
| `ExecuteCommand` false success on timeout | `ExecuteCommand`: `RunCommand` → poll `DescribeInvocationResults` | **This row used to describe an intention, not the code.** `Timeout` and `Stopped` were in the terminal-status set, and for those Cloud Assistant reports exit code 0 with no output — so a command killed at the 60-second default returned success. That is how a truncated `get.docker.com` install became "docker install failed" one step later, on a VM already being deleted. Fixed in 0.7.1: an explicit 10-minute command timeout, `Timeout`/`Stopped` are errors that carry the status and partial output, and the wait matches the timeout. |
| Unreadable diagnostics | `ExecuteCommand` result mapping | Cloud Assistant returns the command's output **base64-encoded**. It was passed through untouched, so every error from a VM was base64 exactly when it mattered. Decoded now, with a plain-text fallback. |
| SSM-registration race | Cloud Assist agent not ready right after boot | Retry `InvokeCommand` (or the invocation lookup) on the "instance not ready / agent offline" error until ready, bounded (~3 min). |
| `GetDefaultImage` not sorted, bad fallback | `GetDefaultImage` (~407): `DescribeImages` takes the first | Sort by creation time / pick the newest; on no match return an error rather than a hard-coded image. |
| Region vs AZ, zero `CreatedAt` | mapping helpers | Use real `RegionId` and `CreationTime`. |

> **VSwitch is effectively required.** Aliyun VPC instances need a `VSwitchId`
> (and a security group). There is no "default VPC" fast path like EC2-Classic —
> set `SANDRPOD_VM_SUBNET_ID` (→ `VSwitchId`) and `SANDRPOD_VM_SECURITY_GROUP`.

Pure-function unit tests mirroring `pkg/provider/aws/aws_test.go` ship in
`pkg/provider/aliyun/aliyun_test.go` (state mapping, instance→VMInfo mapping,
image selection).

---

## 3. Deployment prerequisites

### Credentials (`pkg/provider/aliyun/config.go`)

Static AccessKey is the simplest:

```bash
ALIYUN_ACCESS_KEY=LTAI...          # RAM user AccessKey ID
ALIYUN_SECRET_KEY=...              # RAM user AccessKey Secret
ALIYUN_REGION=cn-hangzhou          # default cn-hangzhou
```

> Setting `ALIYUN_ACCESS_KEY` + `ALIYUN_SECRET_KEY` is what **enables** the
> provider — `aliyun.Register()` skips registration when either is empty
> (`pkg/provider/aliyun/register.go`).

The RAM principal needs at least: `ecs:RunInstances`/`CreateInstance`,
`ecs:StartInstance`, `ecs:DeleteInstance`, `ecs:DescribeInstances`,
`ecs:DescribeImages`, and Cloud Assist `ecs:RunCommand`/`InvokeCommand`,
`ecs:DescribeInvocationResults`.

> Unlike AWS SSM, Cloud Assist does **not** require an instance RAM role on the
> launched VM — only the agent. So there is no `AWS_IAM_INSTANCE_PROFILE`
> equivalent; the server's own AccessKey is what matters.

### Networking

- Create a VPC + **VSwitch** + **security group** in the target region.
- Security group must allow **outbound** to the API Server (your port) and to
  **443** (Docker install + image registry pulls).
- Public IP: the provider sets `InternetMaxBandwidthOut` (currently 10 Mbps),
  which auto-assigns a public IP. Toggle via `SANDRPOD_VM_PUBLIC_IP`.

### Cloud Assist agent

Bootstrap runs via Cloud Assist `InvokeCommand`. Most official Aliyun images
ship the Cloud Assist agent; confirm the chosen image has it, or the bootstrap
commands never run.

### Container images

The VM pulls the `poder` and `toolbox` images. GHCR works, but **pulling
`ghcr.io` from inside mainland-China ECS is often slow or unreliable** —
strongly consider pushing to **Alibaba Cloud Container Registry (ACR)** and
pointing the image envs there:

```bash
SANDRPOD_PODER_IMAGE=registry.<region>.aliyuncs.com/<ns>/poder:v0.7.1
SANDRPOD_TOOLBOX_IMAGE=registry.<region>.aliyuncs.com/<ns>/toolbox:v0.7.1
```

If the ACR repos are private the VM needs `docker login` to ACR (the current
bootstrap does not log in — keep the repos public or extend the bootstrap, same
trade-off as GHCR).

### API Server reachability

Same hard requirement as AWS: the Poder dials `-public-url` back to the server,
so the server must be reachable from the ECS VM.

---

## 4. Environment variable reference (server process)

| Variable | Required | Purpose |
|----------|----------|---------|
| `ALIYUN_ACCESS_KEY` / `ALIYUN_SECRET_KEY` | **yes** | Aliyun API auth (RAM user AccessKey) |
| `ALIYUN_REGION` | yes | target region (e.g. `cn-hangzhou`) |
| `SANDRPOD_VM_SUBNET_ID` | **yes** | VSwitch ID — Aliyun has no default-VPC fallback |
| `SANDRPOD_VM_SECURITY_GROUP` | **yes** | security group ID; omitting it fails with `InvalidSecurityGroupId` |
| `SANDRPOD_VM_PUBLIC_IP` | no (default true) | assign a public IP; `false` for a NAT'd subnet |
| `SANDRPOD_PODER_IDLE_TIMEOUT` | **effectively yes** | reclaim an empty cloud worker after this; **without it every create leaves a VM billing** |
| `SANDRPOD_SANDBOX_IDLE_TIMEOUT` | no | default idle TTL for sandboxes that do not set their own |
| `SANDRPOD_PODER_IMAGE` | no | worker image; defaults to `ghcr.io/sandrpod/poder:latest`, set it for ACR |
| `SANDRPOD_TOOLBOX_IMAGE` | no | sandbox image, forwarded to the worker; pre-pulled during bootstrap |

Plus `SANDRPOD_PUBLIC_URL` (or `-public-url`), which must be an address the VM can
reach — the worker dials it to come home. Reuse the systemd unit + `service.d`
drop-in pattern from [AWS_PROVISIONING.md](AWS_PROVISIONING.md).

### Reclamation is not optional on a metered cloud

A VM provisioned for a sandbox becomes a worker afterwards, and deleting the
sandbox does **not** delete it — that is deliberate, so the next sandbox in the
region can reuse it. What deletes it is the idle reaper:

```ini
Environment=SANDRPOD_PODER_IDLE_TIMEOUT=20m
```

The reaper terminates a cloud worker that has held no sandboxes for the TTL and
removes its record. "Empty" is decided by **sandbox records**, not by a container
count: a container whose record is gone cannot be reached by anyone, so it must not
keep a machine alive.

Verify it once, by hand, before trusting it:

```bash
sandrpod-cli create canary --provider aliyun --region cn-hongkong --ttl 900
sandrpod-cli execute canary "echo ok"
sandrpod-cli delete canary
# wait out the TTL, then: the ECS instance must be gone from the console
aliyun ecs DescribeInstances --RegionId cn-hongkong
```

### Confirm the provider registered

Registration is what makes the substrate available, and it fails silently when the
credentials are missing:

```bash
curl -s "$API/api/v1/capabilities" -H "X-Sandrpod-Token: $TOKEN" | jq .providers
# ["local","aliyun"]
```

If `aliyun` is not in that list, the deployment has no usable Aliyun credentials (or
is running a build older than 0.7.1, which never registered providers in an embedded
build at all). The console reads the same list and disables what is missing.

### Running AWS and Aliyun on one server (per-provider env vars)

`SANDRPOD_VM_SUBNET_ID`, `SANDRPOD_VM_SECURITY_GROUP`, `SANDRPOD_VM_PUBLIC_IP`,
`SANDRPOD_PODER_IMAGE`, and `SANDRPOD_TOOLBOX_IMAGE` each accept a
**provider-scoped** form with a `_<PROVIDER>` suffix that overrides the unscoped
value for that cloud, so one server can drive both clouds without their values
colliding:

```ini
# AWS
Environment=SANDRPOD_VM_SUBNET_ID_AWS=subnet-xxxx
Environment=SANDRPOD_VM_SECURITY_GROUP_AWS=sg-xxxx
# Aliyun
Environment=SANDRPOD_VM_SUBNET_ID_ALIYUN=vsw-xxxx
Environment=SANDRPOD_VM_SECURITY_GROUP_ALIYUN=sg-xxxx
# (a region-local ACR image just for Aliyun)
Environment=SANDRPOD_PODER_IMAGE_ALIYUN=registry.<region>.aliyuncs.com/<ns>/poder:v0.7.1
Environment=SANDRPOD_TOOLBOX_IMAGE_ALIYUN=registry.<region>.aliyuncs.com/<ns>/toolbox:v0.7.1
```

The unscoped `SANDRPOD_VM_SUBNET_ID` etc. still work as a shared default when no
`_<PROVIDER>` form is set.

---

## 5. End-to-end validation (how to reproduce)

This is the flow that was run against a live deployment, in order. Each step has a
visible symptom when it breaks, which is why they are listed separately.

1. **Credentials and network** — the `ALIYUN_*` envs, a VSwitch and a security group.
   Confirm the provider registered (see above); if `/api/v1/capabilities` does not
   list `aliyun`, nothing else will work.
2. **Create** — this provisions the VM, not just the sandbox:
   ```bash
   sandrpod-cli create alitest --provider aliyun --region cn-hongkong \
     --instance-type ecs.e-c1m2.large --ttl 1800
   ```
   What the server does, in the log:
   ```
   [Scheduler] No available poder for aliyun, creating new VM
   [Scheduler] VM i-… created with IP 47.x.x.x
   [Scheduler] Installing Docker on VM i-…          (10-minute budget)
   [Scheduler] Pre-pulling ghcr.io/sandrpod/toolbox:latest on VM i-…
   [Scheduler] Poder container started on VM i-…
   [Scheduler] Poder poder-… registered successfully
   ```
   `Pre-pulling` is deliberate: the sandbox image is 500+ MB, and the first create
   only waits two minutes for the container. Pulling it during bootstrap is the
   difference between a create that times out and one that returns in seconds.
3. **Execute** on the sandbox — proves the tunnel, not just the API:
   ```bash
   sandrpod-cli execute alitest "uname -srm; curl -s -o /dev/null -w '%{http_code}' https://example.com"
   ```
4. **Delete the sandbox** — the sandbox goes away; **the VM does not**. It is a
   worker now, reusable by the next sandbox in that region.
5. **Watch the VM disappear** — this is the step people skip and the only one that
   costs money if it is broken:
   ```bash
   # after SANDRPOD_PODER_IDLE_TIMEOUT passes
   aliyun ecs DescribeInstances --RegionId cn-hongkong
   # with the reaper working, the instance is gone and the log says:
   # idle reaper: reclaimed poder poder-… (empty 3m0s > 2m0s), VM i-… terminated
   ```
   Every failed create before registration gives the machine back on its own; this
   step covers the ones that succeeded.

## 6. Troubleshooting

The first four rows are failures this project hit on a real deployment, with the
symptom as it appeared in the log. They point at the wrong thing, which is why they
are recorded verbatim.

| Symptom | Cause, and what it means |
|---------|--------------------------|
| `failed to create VM: provider aliyun not found` | the provider never registered — no usable `ALIYUN_ACCESS_KEY`/`SECRET`, or a build older than 0.7.1, where registration only happened in `Main()` and an embedding build never ran it |
| VM launches, then `docker: command not found` | Cloud Assistant's 60-second default killed the Docker install, and a `Timeout` invocation was read as success. Fixed in 0.7.1 (10-minute budget, and a timeout is a failure) |
| `poder registration timeout`, and the VM's worker logs show `dial tcp 127.0.0.1:8090` | the worker was told to reach the control plane on loopback. Fixed in 0.7.1: `SANDRPOD_PUBLIC_URL` is what a VM is given |
| First create times out, then a container appears on the VM anyway | the sandbox image was being pulled inside the 2-minute create budget. Fixed in 0.7.1 by pre-pulling during bootstrap |
| VM still running long after every sandbox is deleted | no `SANDRPOD_PODER_IDLE_TIMEOUT`, or a build older than 0.7.1 where the reaper only started from `Main()` |
| `InvalidSecurityGroupId` | `SANDRPOD_VM_SECURITY_GROUP_ALIYUN` (or the unscoped form) is missing; Aliyun has no default VPC to fall back to |
| VM launches, command never runs | Cloud Assist agent not present on the image, or the AK lacks `RunCommand`/`DescribeInvocationResults` |
| Docker install or image pull slow, fails | mainland-China VM pulling `ghcr.io` — use ACR; check SG egress 443 |
| `poder registration timeout` for other reasons | the server is not reachable from the VM at `SANDRPOD_PUBLIC_URL`; the join token is forwarded automatically |
| Worker ONLINE but the scheduler keeps waiting | the request region (not AZ) and `aliyun` must match what the worker registered with |
| `No such image` for toolbox | the image is not pullable from the VM (private ACR without a login, or a wrong path) |

Every one of the first five now fails loudly instead of leaving a machine behind, but
a deployment running an older build still has the old behaviour — check
`/api/v1/capabilities` for the version.

## 7. Known differences from AWS

- **Remote exec** is Cloud Assist `InvokeCommand`, not SSM — no instance RAM role
  needed, but the agent must be present.
- **No default-VPC shortcut** — a VSwitch is required.
- **Image pull** from `ghcr.io` is unreliable in mainland China — prefer ACR.
- Instance type / image identifiers differ (e.g. `ecs.t6-c1m1.large`, Aliyun
  Linux / Ubuntu image families).
- Effort is **much lower than AWS** because the scheduler, poder, server,
  delete-poder VM termination, and reaper are already provider-agnostic and done.
- **Billing shape**: the VM exists from the moment the sandbox is created until the
  idle reaper reclaims it, and Aliyun bills the machine, not the sandbox. Set the
  timeout deliberately; a generous one is a standing cost.
