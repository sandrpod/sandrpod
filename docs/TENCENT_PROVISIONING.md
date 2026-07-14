# Tencent Cloud Auto-Provisioning Guide

How SandrPod creates Tencent Cloud (CVM) instances on demand, bootstraps a Poder
on each via **TAT (TencentCloud Automation Tools)**, and runs sandboxes there.

> **Status: validated end-to-end on a live account** (region `ap-singapore`,
> zone `ap-singapore-1`, `S5.MEDIUM2`, default VPC): create → TAT installs
> Docker (~50 s, real exit codes over base64) → Poder registers over the
> cross-cloud tunnel → sandbox RUNNING in under 2 minutes → bash/python code
> executes → poder reuse confirmed (2nd sandbox in ~3 s, no new VM) → poder
> delete terminates the CVM (`TerminateInstances` is async — the instance shows
> STOPPING briefly, then is gone) with no leak. Pulling the poder/toolbox
> images from **GHCR worked directly in the Singapore region**; the TCR advice
> below applies to mainland-China regions.

> **TL;DR:** an API key (SecretId/SecretKey) is **necessary but not sufficient**.
> You also need (1) the **TAT agent** present on the image, (2) the API Server
> reachable from the VMs, and (3) the `poder`/`toolbox` images pullable by the VM
> (prefer TCR in mainland China — GHCR is slow/unreliable there).

---

## What it does (and doesn't)

Lazy provision-on-demand, identical lifecycle to the AWS path:

- Creating a sandbox with `provider_type=tencent` when **no Poder is available**
  for that region triggers: `RunInstances` → wait running + public IP → install
  Docker via TAT → start a Poder → the Poder registers → the sandbox is created.
- Subsequent `tencent` sandboxes in the same region **reuse** that Poder. One
  VM/Poder hosts **many** sandboxes.
- **No** autoscaling and **no** idle-VM reclamation.

### Flow

```
POST /api/v1/sandboxes {provider_type: tencent}
        │
        ▼  (no Poder for region?)
   RunInstances (public IP + tag sandrpod) ──► DescribeInstances (get public IP)
        │
        ▼  TAT RunCommand (SHELL, base64)   (needs the TAT agent on the image)
   curl get.docker.com | sh   →   docker run … <poder image>
        │
        ▼  Poder dials API Server (-public-url) over WebSocket and registers
   sandbox created on the new Poder
```

Remote exec uses **TAT** (`RunCommand` + `DescribeInvocationTasks`), an
agent-based managed API like AWS SSM / Aliyun CloudAssist — so **no instance
role is needed**, only the TAT agent (present on Tencent public images). Command
content and output cross the wire base64-encoded; the provider handles the
encoding and recovers the real exit code.

---

## Prerequisites checklist

- [ ] A Tencent Cloud **API key** (SecretId + SecretKey) for a CAM user/role
- [ ] That principal can run CVM + TAT actions (see [permissions](#1-credentials))
- [ ] A **public Ubuntu image** with the TAT agent (Tencent public images qualify)
- [ ] API Server started with a **publicly reachable** `-public-url`
- [ ] Security group / network allows the VM **outbound** to the API Server and 443
- [ ] `poder` and `toolbox` images **pullable** by the VM (TCR recommended in CN)
- [ ] Server env vars set (see the [reference table](#environment-variable-reference))

---

## 1. Credentials

Static API key is simplest (read by `pkg/provider/tencent/config.go`):

```bash
TENCENTCLOUD_SECRET_ID=AKID...
TENCENTCLOUD_SECRET_KEY=...
TENCENTCLOUD_REGION=ap-guangzhou      # the client is bound to this region
TENCENTCLOUD_ZONE=ap-guangzhou-3      # default availability zone for placement
```

> Setting SecretId + SecretKey is what **enables** the provider — `tencent.Register()`
> skips registration when either is empty.

The CAM principal needs at least: `cvm:RunInstances`, `cvm:TerminateInstances`,
`cvm:DescribeInstances`, `cvm:DescribeImages`, and TAT `tat:RunCommand`,
`tat:DescribeInvocationTasks`.

> **Region vs zone.** The client is bound to `TENCENTCLOUD_REGION`
> (`ap-guangzhou`). The sandbox request's `region` is treated as the **zone**
> (`ap-guangzhou-3`); if empty it falls back to `TENCENTCLOUD_ZONE`. Keep the
> request zone inside the client's region.

---

## 2. Networking

The provider assigns a **public IP** (`InternetAccessible`, 10 Mbps) so the
instance can reach the API Server and pull images.

> **Leave the subnet env unset to use the default VPC.** Tencent VPC placement
> needs **both** a VPC ID and a subnet ID; the scheduler's network plumbing only
> carries a subnet. So if you set `SANDRPOD_VM_SUBNET_ID_TENCENT` without a VPC,
> `RunInstances` will fail. For now, leave it unset and Tencent places the
> instance in the zone's default VPC. A security group can still be set via
> `SANDRPOD_VM_SECURITY_GROUP_TENCENT`.

The security group must allow **outbound** to the API Server host/port and to
**443** (Docker install + image pulls).

---

## 3. API Server reachability & container images

Same hard requirement as every cloud: the Poder dials `-public-url` back, so the
server must be reachable from the CVM.

The VM pulls the `poder` and `toolbox` images. **Pulling `ghcr.io` from inside
mainland-China CVMs is often slow or unreliable** — strongly prefer **Tencent
Container Registry (TCR)**:

```bash
SANDRPOD_PODER_IMAGE_TENCENT=ccr.ccs.tencentyun.com/<ns>/poder:v0.4.0
SANDRPOD_TOOLBOX_IMAGE_TENCENT=ccr.ccs.tencentyun.com/<ns>/toolbox:v0.4.0
```

Private TCR repos need the VM to `docker login`; the current bootstrap does not,
so keep the repos public or extend the bootstrap.

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `TENCENTCLOUD_SECRET_ID` / `TENCENTCLOUD_SECRET_KEY` | **yes** | — | API auth; enables the provider |
| `TENCENTCLOUD_REGION` | no | `ap-guangzhou` | client region |
| `TENCENTCLOUD_ZONE` | no | `ap-guangzhou-3` | default availability zone |
| `SANDRPOD_VM_SECURITY_GROUP` (`_TENCENT`) | recommended | — | security group ID |
| `SANDRPOD_VM_SUBNET_ID` (`_TENCENT`) | **leave unset** | — | see the VPC caveat above |
| `SANDRPOD_PODER_IMAGE` (`_TENCENT`) | **yes (cloud)** | `ghcr.io/sandrpod/poder:latest` | Poder image (TCR recommended) |
| `SANDRPOD_TOOLBOX_IMAGE` (`_TENCENT`) | **yes (cloud)** | `ghcr.io/sandrpod/toolbox:latest` | toolbox image, forwarded to the Poder |

The `SANDRPOD_VM_*` and image vars accept a provider-scoped `_TENCENT` suffix
that overrides the unscoped default (see
[ALIYUN_PROVISIONING.md](ALIYUN_PROVISIONING.md#running-aws-and-aliyun-on-one-server-per-provider-env-vars)).
Server flag: `-public-url <url>` — reachable from the VMs.

---

## End-to-end example

```bash
export TENCENTCLOUD_SECRET_ID=AKID...
export TENCENTCLOUD_SECRET_KEY=...
export TENCENTCLOUD_REGION=ap-guangzhou
export TENCENTCLOUD_ZONE=ap-guangzhou-3
export SANDRPOD_PODER_IMAGE_TENCENT=ccr.ccs.tencentyun.com/<ns>/poder:v0.4.0
export SANDRPOD_TOOLBOX_IMAGE_TENCENT=ccr.ccs.tencentyun.com/<ns>/toolbox:v0.4.0

go run ./cmd/server -port 8080 -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token "$SANDRPOD_TOKEN"

sandrpod-cli create my-box --provider tencent \
  --region ap-guangzhou-3 --instance-type S5.MEDIUM4
```

The first request launches a CVM (default image: newest public Ubuntu) and may
take a few minutes. Reuse the systemd unit + `service.d` drop-in pattern from
[AWS_PROVISIONING.md](AWS_PROVISIONING.md#running-the-server-as-a-systemd-service).

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| provider not registered | `TENCENTCLOUD_SECRET_ID`/`SECRET_KEY` unset |
| `AuthFailure` / `UnauthorizedOperation` | key lacks the CVM/TAT actions |
| `tencent VPC placement needs both a VPC and a subnet` | you set a subnet without a VPC — unset `SANDRPOD_VM_SUBNET_ID_TENCENT` (the provider fails fast locally before calling the API) |
| VM launches, command never runs | TAT agent not present/ready on the image; the provider retries agent-not-ready ~3 min |
| Docker install / image pull slow or fails | mainland-CN VM pulling `ghcr.io` — use TCR; check SG egress 443 |
| `poder registration timeout` | API Server not reachable from the VM (`-public-url`) |

---

## Known limitations & caveats

- **Validated end-to-end** (`ap-singapore-1`, `S5.MEDIUM2`): TAT agent
  readiness, base64 command/output round-trip, the default-image
  DescribeImages filter, poder reuse, and VM reclamation all worked in
  practice on the first attempt — VM+IP in ~20 s, Docker install ~50 s,
  sandbox RUNNING in under 2 minutes. Mainland-China regions remain untested
  (expect the TCR image-registry caveat to matter there).
- **Subnet plumbing is VPC-incomplete** — leave the subnet env unset (default
  VPC); setting a subnet without a VPC now fails fast with a clear error
  instead of an opaque API rejection.
- **No autoscaling / no idle reclamation.** `Cleanup` deletes instances tagged
  `sandrpod=true`.
- **Default image is the newest public Ubuntu.** Override per-request with
  `--image img-xxxxxxxx`.
