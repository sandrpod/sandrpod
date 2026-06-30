# Alibaba Cloud (Aliyun) Auto-Provisioning — Plan & Guide

How to bring the **Aliyun** provider to the same working state as AWS: create an
ECS instance on demand, bootstrap a Poder on it via Cloud Assist, and run
sandboxes there.

> **Status: NOT yet hardened or end-to-end validated.** The AWS path is fully
> working and tested; the Aliyun provider (`pkg/provider/aliyun/aliyun.go`) is
> real SDK code but carries the **same class of bugs the AWS provider had before
> we fixed them**. This document is both the fix plan and the deployment guide.
> See [AWS_PROVISIONING.md](AWS_PROVISIONING.md) for the working reference.

---

## 1. What Aliyun already inherits for free

Most of the work done while making AWS work lives **outside** the provider and is
provider-agnostic, so Aliyun gets it without changes:

| Layer | What it gives Aliyun |
|-------|----------------------|
| Scheduler (`pkg/sandpod/scheduler.go`) | forwards the API token, `PROVIDER_TYPE`, the request **region** (not AZ), `VM_INSTANCE_ID`, and the configurable poder/toolbox image to the Poder; builds VM networking (public IP / subnet / SG) via `NetworkConfig` |
| Poder (`pkg/poder/docker.go`) | `CreatePod` pulls the toolbox image before creating the container |
| Server (`cmd/server/main.go`) | delete-poder terminates the cloud VM (`isCloudProvider` includes `aliyun`); the reaper reclaims OFFLINE poders; both call `provider.DeleteVM(providerType, vmID)` |

So the remaining work is **(a)** the Aliyun-specific provider fixes and **(b)**
Aliyun deployment config. The control-plane / scheduler / poder plumbing is done.

---

## 2. Provider fixes needed (`pkg/provider/aliyun/aliyun.go`)

Apply the same fixes the AWS provider received. Mapping to the actual Aliyun
methods:

| Issue (fixed in AWS) | Aliyun location | Fix |
|----------------------|-----------------|-----|
| `CreateVM` returns no IP | `CreateVM` (~104): `CreateInstance` → `StartInstance` → caches a bare `VMInfo` | After the instance is **Running**, `DescribeInstances` and read `PublicIpAddress` / `InnerIpAddress`. Aliyun assigns the public IP **asynchronously** once `InternetMaxBandwidthOut > 0` and the instance starts, so it isn't known at create time. |
| `GetVM` returns stale cache | `GetVM` (~203): reads `p.vms` first | Always `DescribeInstances`; refresh the cache. The cached snapshot is `Pending` forever otherwise → health never ready. |
| `ExecuteCommand` false success on timeout, lost stderr, wrong exit code | `ExecuteCommand` (~254): `InvokeCommand` → poll `DescribeInvocationResults` | Honor the ctx deadline; on timeout return an **error** (not exit 0); read the real `ExitCode` and `ErrorInfo`/stderr; guard nil pointers. |
| SSM-registration race | Cloud Assist agent not ready right after boot | Retry `InvokeCommand` (or the invocation lookup) on the "instance not ready / agent offline" error until ready, bounded (~3 min). |
| `GetDefaultImage` not sorted, bad fallback | `GetDefaultImage` (~407): `DescribeImages` takes the first | Sort by creation time / pick the newest; on no match return an error rather than a hard-coded image. |
| Region vs AZ, zero `CreatedAt` | mapping helpers | Use real `RegionId` and `CreationTime`. |

> **VSwitch is effectively required.** Aliyun VPC instances need a `VSwitchId`
> (and a security group). There is no "default VPC" fast path like EC2-Classic —
> set `SANDRPOD_VM_SUBNET_ID` (→ `VSwitchId`) and `SANDRPOD_VM_SECURITY_GROUP`.

Add pure-function unit tests mirroring `pkg/provider/aws/aws_test.go`
(state mapping, instance→VMInfo mapping, image selection).

---

## 3. Deployment prerequisites

### Credentials (`pkg/provider/aliyun/config.go`)

Static AccessKey is the simplest:

```bash
ALIYUN_ACCESS_KEY_ID=LTAI...
ALIYUN_ACCESS_KEY_SECRET=...
ALIYUN_REGION=cn-hangzhou          # confirm the exact env var name in config.go
```

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
SANDRPOD_PODER_IMAGE=registry.<region>.aliyuncs.com/<ns>/poder:v0.3.1
SANDRPOD_TOOLBOX_IMAGE=registry.<region>.aliyuncs.com/<ns>/toolbox:v0.3.1
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
| `ALIYUN_ACCESS_KEY_ID` / `ALIYUN_ACCESS_KEY_SECRET` | **yes** | Aliyun API auth |
| `ALIYUN_REGION` | yes | target region (e.g. `cn-hangzhou`) |
| `SANDRPOD_VM_SUBNET_ID` | **yes** | VSwitch ID |
| `SANDRPOD_VM_SECURITY_GROUP` | **yes** | security group ID |
| `SANDRPOD_VM_PUBLIC_IP` | no (default true) | assign a public IP |
| `SANDRPOD_PODER_IMAGE` | **yes** | Poder image (ACR recommended) |
| `SANDRPOD_TOOLBOX_IMAGE` | **yes** | toolbox image, forwarded to the Poder |

Plus server flag `-public-url` reachable from the VMs. Reuse the systemd unit +
`service.d` drop-in pattern from [AWS_PROVISIONING.md](AWS_PROVISIONING.md).

---

## 5. Implementation & validation plan

Mirrors how AWS was done, but much smaller (only the provider file changes):

1. **Fix `aliyun.go`** — the six items in §2; wire region/credentials in
   `config.go` if missing.
2. **Build & test** — `go build/vet/test`; add provider unit tests.
3. **Publish images** — to ACR (recommended) or ensure GHCR is reachable; set
   the image envs.
4. **Aliyun setup** — VPC + VSwitch + security group; verify the chosen image
   has Cloud Assist.
5. **Deploy** — add the Aliyun envs to the server (systemd drop-in) and restart.
6. **End-to-end test** — reuse the AWS validation flow verbatim, only changing
   `provider_type=aliyun` and the region:
   ```bash
   curl -X POST http://<server>/api/v1/sandboxes -H "Authorization: Bearer <token>" \
     -d '{"name":"alitest","provider_type":"aliyun","region":"cn-hangzhou","instance_type":"ecs.t6-c1m1.large"}'
   ```
   Expect: ECS launches → Cloud Assist installs Docker → Poder starts → registers
   (with `vm_id`) → sandbox RUNNING → code executes.
7. **Verify reclamation** — delete the aliyun poder → ECS terminated
   (`vm_terminated:true`); confirm the reaper reclaims OFFLINE aliyun poders.
   These already support `aliyun` in the shared code; they only need `DeleteVM`
   to work (it does — `DeleteInstance`) and the Poder to report `vm_id` (shared,
   already working).
8. **Clean up** test resources.

---

## 6. Anticipated troubleshooting (from the AWS analogs)

| Symptom | Likely cause |
|---------|--------------|
| `CreateInstance` fails: VSwitch/security group required | set `SANDRPOD_VM_SUBNET_ID` + `SANDRPOD_VM_SECURITY_GROUP` |
| VM launches, command never runs | Cloud Assist agent not present/ready on the image; or the AK lacks `InvokeCommand` |
| Docker install / image pull slow or fails | mainland-China VM pulling `ghcr.io` — use ACR; check SG egress 443 |
| `poder registration timeout` | server not reachable from the VM (`-public-url`); token is forwarded automatically |
| poder ONLINE but scheduler keeps waiting | Poder registered under a different region/provider — ensure request region (not AZ) and `aliyun` |
| `No such image` for toolbox | image not pullable on the VM (private ACR without login, or wrong path) |

---

## 7. Known differences from AWS

- **Remote exec** is Cloud Assist `InvokeCommand`, not SSM — no instance RAM role
  needed, but the agent must be present.
- **No default-VPC shortcut** — a VSwitch is required.
- **Image pull** from `ghcr.io` is unreliable in mainland China — prefer ACR.
- Instance type / image identifiers differ (e.g. `ecs.t6-c1m1.large`, Aliyun
  Linux / Ubuntu image families).
- Effort is **much lower than AWS** because the scheduler, poder, server,
  delete-poder VM termination, and reaper are already provider-agnostic and done.
