# GCP Auto-Provisioning Guide

How SandrPod creates Google Compute Engine VMs on demand, bootstraps a Poder on
each **over SSH**, and runs sandboxes there — and exactly what you must configure.

> **Status: validated end-to-end on a live project** (zone `asia-east1-a`,
> `e2-medium`): create → SSH bootstrap → Docker install →
> Poder registers over the cross-cloud tunnel → sandbox RUNNING → code executes →
> poder delete terminates the VM with no leak. Poder **reuse** was also confirmed
> (2nd/3rd sandbox in the same zone reuses the VM, no new instance). The three
> issues that surfaced during that first live run — SSH must run as root, a GCE
> first-boot apt race, and a delete-context bug — are fixed and folded into the
> guidance below.

> **GCP is the odd one out.** AWS/Aliyun/Azure use a managed run-command API for
> bootstrap. GCP has **no equivalent**, so SandrPod bootstraps over **SSH** using
> a per-VM ephemeral key. That means: the VM needs a **public IP** and a
> **firewall rule allowing the server to reach port 22**. Without the firewall
> rule, provisioning stalls at "install Docker".

> **TL;DR:** a service account is **necessary but not sufficient**. You also need
> (1) a **firewall rule opening tcp:22** to the VM (network tag `sandrpod`), (2)
> the API Server reachable from the VMs, and (3) the `poder`/`toolbox` images
> pullable by the VM.

---

## What it does (and doesn't)

Lazy provision-on-demand, identical lifecycle to the AWS path:

- Creating a sandbox with `provider_type=gcp` when **no Poder is available** for
  that zone triggers: create a VM (with a public IP + an ephemeral SSH key in
  metadata) → SSH in to install Docker → start a Poder → the Poder registers →
  the sandbox is created.
- Subsequent `gcp` sandboxes in the same zone **reuse** that Poder. One VM/Poder
  hosts **many** sandboxes.
- **No** continuous scale-out. Idle reclamation is **off by default** — enable via `SANDRPOD_PODER_IDLE_TIMEOUT` / `SANDRPOD_SANDBOX_IDLE_TIMEOUT`.

### Flow

```
POST /api/v1/sandboxes {provider_type: gcp}
        │
        ▼  (no Poder for zone?)
   Insert instance (public IP + ssh-keys metadata + tag "sandrpod") ──► read NAT IP
        │
        ▼  SSH to <public-ip>:22 with the ephemeral key   (needs the firewall rule!)
   curl get.docker.com | sh   →   docker run … <poder image>
        │
        ▼  Poder dials API Server (-public-url) over WebSocket and registers
   sandbox created on the new Poder
```

**SSH executor specifics** (`pkg/provider/gcp/ssh.go`):

- `CreateVM` generates a one-time **ed25519** key; the public half goes into the
  instance's `ssh-keys` metadata, the private half is held **in-process** and
  used by `ExecuteCommand`. It is never written to disk and dies with the VM.
- Because the key lives only in memory, `ExecuteCommand` works within the same
  server process that created the VM (the normal provisioning flow). It cannot
  drive a VM created by a different/earlier process.
- Host-key verification uses `InsecureIgnoreHostKey` — a first-boot ephemeral VM
  has no host key to pin (same posture as `gcloud compute ssh` on first connect).
- A non-zero command exit surfaces via `*ssh.ExitError`, so `ExitCode` matches
  the managed-agent providers' semantics.
- Commands run **as root** (piped into `sudo -n bash`): the metadata-provisioned
  login user isn't in the `docker` group, and the managed-agent providers run as
  root, so this keeps `docker …` bootstrap working and behavior consistent. It
  relies on the user being a passwordless sudoer — which GCE's metadata SSH-key
  flow guarantees via `google-sudoers`.

---

## Prerequisites checklist

- [ ] A **GCP project** and a **service account** (JSON key or ADC)
- [ ] The SA has **Compute Instance Admin (v1)** on the project
- [ ] A **firewall rule allowing tcp:22** from the server to VMs tagged `sandrpod`
- [ ] API Server started with a **publicly reachable** `-public-url`
- [ ] VMs have **outbound** to the API Server and to the internet (443)
- [ ] `poder` and `toolbox` images **published and pullable** by the VM
- [ ] Server env vars set (see the [reference table](#environment-variable-reference))

---

## 1. Credentials for the API Server (service account)

First ensure the project has **billing enabled** and the **Compute Engine API**
enabled (`gcloud services enable compute.googleapis.com`, or the Console prompt on
first visit to Compute Engine). Then the server authenticates to the Compute API
with **Application Default Credentials** or an explicit service-account JSON key.

**gcloud path** — create the SA, grant it instance admin, download a key:

```bash
gcloud iam service-accounts create sandrpod-server \
  --display-name "SandrPod server"
gcloud projects add-iam-policy-binding <project> \
  --member "serviceAccount:sandrpod-server@<project>.iam.gserviceaccount.com" \
  --role roles/compute.instanceAdmin.v1
# If launched VMs run as a service account, the server SA also needs:
gcloud projects add-iam-policy-binding <project> \
  --member "serviceAccount:sandrpod-server@<project>.iam.gserviceaccount.com" \
  --role roles/iam.serviceAccountUser
gcloud iam service-accounts keys create /opt/sandrpod/gcp-sa.json \
  --iam-account sandrpod-server@<project>.iam.gserviceaccount.com
```

**Console path** (no gcloud needed) — the JSON key is a file you download:

1. **IAM & Admin → Service Accounts → Create service account**, name it
   `sandrpod-server`.
2. Grant the role **Compute Instance Admin (v1)** → Done.
3. Open the account → **Keys → Add key → Create new key → JSON** → the browser
   downloads a `*.json` file. **That file is the service-account key.**
4. Copy it to the server (e.g. `/opt/sandrpod/gcp-sa.json`, `chmod 600`) and
   point `GCP_CREDENTIALS_FILE` at it. Don't commit it or paste it anywhere.

Point the server at it (read by `pkg/provider/gcp/config.go`):

```bash
GCP_PROJECT=<project>
GCP_ZONE=us-central1-a          # GCP is zonal; the request "region" is a zone here
GCP_CREDENTIALS_FILE=/opt/sandrpod/gcp-sa.json   # or use ADC (GOOGLE_APPLICATION_CREDENTIALS)
```

> Setting `GCP_PROJECT` is what **enables** the provider — `gcp.Register()` skips
> registration when it is empty (`pkg/provider/gcp/register.go`). When
> `GCP_CREDENTIALS_FILE` is unset, the client uses ADC
> (`GOOGLE_APPLICATION_CREDENTIALS`, `gcloud auth application-default`, or the
> metadata server if the server runs on GCE).

---

## 2. Firewall rule for SSH (required)

This is the GCP-specific gotcha. Every VM gets the network tag **`sandrpod`**;
you must add a firewall rule letting the API Server reach **tcp:22** on tagged
VMs, or the SSH bootstrap can't connect.

```bash
gcloud compute firewall-rules create sandrpod-allow-ssh \
  --network default \
  --direction INGRESS --action ALLOW --rules tcp:22 \
  --target-tags sandrpod \
  --source-ranges <SERVER_PUBLIC_IP>/32     # restrict to the server; avoid 0.0.0.0/0
```

> Scope `--source-ranges` to the server's IP. Opening 22 to `0.0.0.0/0` works but
> exposes SSH to the internet — only the ephemeral key is accepted, but tight
> source ranges are strongly preferred.

VMs also need **egress** to the API Server and to the internet on 443 (Docker
install + image pulls). GCP's default egress is permissive; locked-down VPCs need
an explicit egress rule.

---

## 3. Networking

The provider always attaches a **public IP** (a `ONE_TO_ONE_NAT` access config) —
SSH and image pulls need it. Subnet is optional:

```bash
SANDRPOD_VM_SUBNET_ID_GCP=projects/<project>/regions/us-central1/subnetworks/default  # optional
SANDRPOD_VM_PUBLIC_IP=true    # default; the SSH bootstrap requires a public IP
```

When no subnet is given, the VM uses the fallback network (`GCP_NETWORK`, default
`global/networks/default`). `SANDRPOD_VM_SECURITY_GROUP` is **not** used by GCP —
firewall rules keyed on the `sandrpod` network tag govern traffic.

> **Zone, not region.** GCP is zonal. The request's `region` field is treated as
> a **zone**, and `GCP_ZONE` must be one too. Use `asia-east1-a`, **not**
> `asia-east1` — a bare region name fails the instance insert ("invalid value for
> field zone"). Also keep the sandbox request's `region` equal to `GCP_ZONE`: the
> provider looks up / deletes the VM in `GCP_ZONE`, so a mismatched request zone
> would leave `GetVM`/`DeleteVM` searching the wrong zone.

---

## 4. API Server must be reachable from the VMs

Same hard requirement as every cloud — the Poder dials `-public-url` back:

```bash
go run ./cmd/server -port 8080 \
  -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token <token>
```

---

## 5. Container images (poder + toolbox)

The VM must **pull** the poder image, and the Poder then pulls the toolbox image.
Public GHCR works; **Artifact Registry** is the low-latency / private option:

```bash
SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.5.0
SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.5.0
```

A private Artifact Registry needs the VM to authenticate; the bootstrap does not
`docker login` — keep the repo public or extend the bootstrap.

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `GCP_PROJECT` | **yes** | — | project for all API calls; enables the provider |
| `GCP_ZONE` | no | `us-central1-a` | default zone (GCP is zonal) |
| `GCP_CREDENTIALS_FILE` | no | (ADC) | service-account JSON path; else ADC |
| `GCP_NETWORK` | no | `global/networks/default` | fallback network when no subnet |
| `GCP_ADMIN_USERNAME` | no | `sandrpod` | Linux user created via SSH-key metadata |
| `SANDRPOD_VM_SUBNET_ID` (`_GCP`) | no | — | subnetwork URL the NIC uses |
| `SANDRPOD_VM_PUBLIC_IP` (`_GCP`) | no | `true` | must stay true — SSH needs a public IP |
| `SANDRPOD_PODER_IMAGE` (`_GCP`) | **yes (cloud)** | `ghcr.io/sandrpod/poder:latest` | Poder image the VM runs |
| `SANDRPOD_TOOLBOX_IMAGE` (`_GCP`) | **yes (cloud)** | `ghcr.io/sandrpod/toolbox:latest` | toolbox image, forwarded to the Poder |

The `SANDRPOD_VM_*` and image vars accept a **provider-scoped** `_GCP` suffix
that overrides the unscoped default, so one server can drive GCP alongside the
other clouds (see
[ALIYUN_PROVISIONING.md](ALIYUN_PROVISIONING.md#running-aws-and-aliyun-on-one-server-per-provider-env-vars)).

Server flag: `-public-url <url>` — reachable from the VMs (passed to the Poder as `API_URL`).

---

## End-to-end example

```bash
export GCP_PROJECT=my-project
export GCP_ZONE=us-central1-a
export GCP_CREDENTIALS_FILE=/opt/sandrpod/gcp-sa.json
export SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.5.0
export SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.5.0

# One-time: allow the server to SSH to provisioned VMs.
gcloud compute firewall-rules create sandrpod-allow-ssh \
  --network default --direction INGRESS --action ALLOW --rules tcp:22 \
  --target-tags sandrpod --source-ranges <SERVER_PUBLIC_IP>/32

go run ./cmd/server -port 8080 \
  -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token "$SANDRPOD_TOKEN"

# Then create a sandbox on GCP (region = zone):
sandrpod-cli create my-box --provider-type gcp \
  --region us-central1-a --instance-type e2-medium
```

The first such request creates a VM (default image: latest Ubuntu 22.04 LTS from
`ubuntu-os-cloud`) and may take a few minutes (VM boot + sshd up + Docker install
+ image pull).

---

## Running the server as a systemd service

Reuse the unit + `service.d` drop-in pattern from
[AWS_PROVISIONING.md](AWS_PROVISIONING.md#running-the-server-as-a-systemd-service),
swapping the drop-in for GCP:

`/etc/systemd/system/sandrpod-server.service.d/gcp.conf`

```ini
[Service]
Environment=GCP_PROJECT=my-project
Environment=GCP_ZONE=us-central1-a
Environment=GCP_CREDENTIALS_FILE=/opt/sandrpod/gcp-sa.json
Environment=SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.5.0
Environment=SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.5.0
```

Keep the SA JSON root-readable only (`chmod 600`), owned by the service user.

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| provider not registered at startup | `GCP_PROJECT` unset |
| `could not find default credentials` | no `GCP_CREDENTIALS_FILE` and no ADC — set one |
| `Required 'compute.instances.create' permission` | SA lacks `roles/compute.instanceAdmin.v1` (and `serviceAccountUser` if VMs run as an SA) |
| `could not SSH to <ip>:22 before timeout` | **firewall rule missing** (tcp:22 to tag `sandrpod`), or the VM never got a public IP |
| `no ssh credential for <vm>` | `ExecuteCommand` called for a VM created by a different/earlier process (the in-memory key is gone) |
| `VM has no public IP to SSH to` | `SANDRPOD_VM_PUBLIC_IP` set false, or IP not yet assigned — GCP requires the public IP for SSH |
| `poder registration timeout` | API Server not reachable from the VM (`-public-url` wrong / behind NAT) |
| Docker install / image pull fails | VPC egress blocks 443, or image not pullable |
| `permission denied ... /var/run/docker.sock` | login user not root/`docker` group — fixed: the SSH executor runs commands via `sudo -n bash`. Recurs only if the user lacks passwordless sudo |
| `apt-get update` fails: `Type '...archive.ubuntu.com...' is not known` | GCE first-boot apt-mirror rewrite race — fixed: bootstrap runs `cloud-init status --wait` before installing Docker |
| poder delete logs `context canceled` / `vm_terminated:false` | client disconnected before the ~90 s GCP delete finished — fixed: the handler now uses a detached context. The VM still gets deleted; use a longer client timeout to see `vm_terminated:true` |
| wrong zone / "resource not found" | `GCP_ZONE`/request `region` must be a **zone** (`asia-east1-a`), not a region (`asia-east1`) |

---

## Known limitations & caveats

- **Validated end-to-end** (`asia-east1-a`, `e2-medium`):
  create → SSH bootstrap → Poder registers → sandbox RUNNING → code runs → delete
  terminates the VM (no leak); poder reuse confirmed (2nd/3rd sandbox shares the
  VM). SSH first-connect timing, `ssh-keys` propagation, and `InsecureIgnoreHostKey`
  all worked in practice — the 3-minute dial-retry window absorbs sshd startup.
- **SSH executor, in-process key.** `ExecuteCommand` only works within the
  process that created the VM (ephemeral key held in memory). Fine for the
  scheduler's create→bootstrap flow; not for cross-process control. A restart of
  the API Server orphans the SSH key for existing GCP VMs — reclaim them via the
  reaper / poder delete rather than expecting further `ExecuteCommand` on them.
- **Public IP + a firewall rule opening 22 are mandatory** — this is the biggest
  operational difference from the other clouds.
- **No autoscaling.** Idle-VM reclamation is opt-in (`SANDRPOD_PODER_IDLE_TIMEOUT`; see [UPGRADING.md](UPGRADING.md)). `Cleanup` deletes VMs labeled
  `sandrpod=true` (the boot disk auto-deletes with the instance).
- **Default image is Ubuntu 22.04 LTS** from `ubuntu-os-cloud`. Override
  per-request with `--image` (a source-image URL).
- **`region` means zone**, and `SANDRPOD_VM_SECURITY_GROUP` is ignored (firewall
  rules on the `sandrpod` tag govern traffic).
