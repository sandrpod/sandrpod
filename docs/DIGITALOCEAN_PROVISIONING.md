# DigitalOcean Auto-Provisioning Guide

How SandrPod creates DigitalOcean droplets on demand, bootstraps a Poder on each
**over SSH**, and runs sandboxes there.

> **Status: implemented, not yet end-to-end validated on a live account.** The
> provider (`pkg/provider/digitalocean/`) is real SDK code, builds, and has unit
> tests, but has not been run against a real DigitalOcean account. Smoke-test
> before relying on it. The **GCP** path ([GCP_PROVISIONING.md](GCP_PROVISIONING.md))
> is the validated reference for the SSH-executor mechanics.

> **DigitalOcean has no managed run-command API**, so SandrPod bootstraps over
> **SSH**: CreateVM injects a per-VM ephemeral ed25519 key via cloud-init (root
> login) and ExecuteCommand connects with the shared `pkg/provider/sshexec`
> helper. Unlike GCP, droplets log in as **root** — no sudo, and no firewall rule
> is needed by default (droplets are open unless you attach a Cloud Firewall).

> **TL;DR:** an API token is **necessary but not sufficient**. You also need
> (1) the API Server reachable from the droplets, (2) the server able to reach
> the droplet's port 22 (open by default; if a **Cloud Firewall** is attached it
> must allow 22 from the server), and (3) `poder`/`toolbox` images pullable.

---

## What it does (and doesn't)

Lazy provision-on-demand, identical lifecycle to the AWS/GCP path:

- Creating a sandbox with `provider_type=digitalocean` when **no Poder is
  available** for that region triggers: create a droplet (public IP + ephemeral
  SSH key via cloud-init) → SSH in to install Docker → start a Poder → register
  → the sandbox is created.
- Subsequent droplets in the same region **reuse** that Poder.
- **No** autoscaling and **no** idle reclamation.

### Flow

```
POST /api/v1/sandboxes {provider_type: digitalocean}
        │
        ▼  (no Poder for region?)
   Droplets.Create (public IP + cloud-init ssh key + tag sandrpod) ──► poll public IP
        │
        ▼  SSH to <public-ip>:22 as root with the ephemeral key
   curl get.docker.com | sh   →   docker run … <poder image>
        │
        ▼  Poder dials API Server (-public-url) over WebSocket and registers
   sandbox created on the new Poder
```

**SSH executor specifics** (`pkg/provider/sshexec`):

- `CreateVM` generates a one-time **ed25519** key; the public half is injected
  into the droplet's `root` `authorized_keys` via cloud-init user-data, the
  private half is held **in-process** for `ExecuteCommand`. It never touches disk
  and dies with the droplet.
- Because the key lives only in memory, `ExecuteCommand` works within the same
  server process that created the droplet — not across restarts.
- Host-key verification uses `InsecureIgnoreHostKey` (first-boot ephemeral host).
- A non-zero command exit surfaces via `ExitCode`.

---

## Prerequisites checklist

- [ ] A DigitalOcean **API token** with write scope
- [ ] API Server started with a **publicly reachable** `-public-url`
- [ ] The server can reach droplet **tcp:22** (open by default; allow it if a Cloud Firewall is attached)
- [ ] Droplets have **outbound** to the API Server and to the internet (443)
- [ ] `poder` and `toolbox` images **pullable** by the droplet
- [ ] Server env vars set (see the [reference table](#environment-variable-reference))

---

## 1. Credentials

```bash
DIGITALOCEAN_TOKEN=dop_v1_...   # or DO_TOKEN
DO_REGION=nyc3                  # default region slug
```

> Setting the token is what **enables** the provider — `digitalocean.Register()`
> skips registration when it is empty. Create the token in the DO console under
> **API → Tokens**, with **Write** scope.

---

## 2. Networking & SSH reachability

Droplets get a **public IP by default**, and by default **no firewall** is
attached, so the server can SSH to port 22 out of the box. Two things to know:

- If you attach a **DigitalOcean Cloud Firewall** to the droplets, add an inbound
  rule allowing **tcp:22** from the API Server's IP (scope it — avoid `0.0.0.0/0`).
- Droplets need **outbound** to the API Server and to **443** (Docker install +
  image pulls); DO's default egress is open.
- A private network can be selected with `SANDRPOD_VM_SUBNET_ID_DIGITALOCEAN`
  (interpreted as a **VPC UUID**); otherwise the region's default VPC is used.
  `SANDRPOD_VM_SECURITY_GROUP` is **not** used (DO firewalls are separate resources).

---

## 3. API Server reachability & container images

The Poder dials `-public-url` back, so the server must be reachable from the
droplet. Point the images at a registry the droplet can reach (public GHCR works
well from DO regions):

```bash
SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.3.1
SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.3.1
```

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `DIGITALOCEAN_TOKEN` (or `DO_TOKEN`) | **yes** | — | API token; enables the provider |
| `DO_REGION` | no | `nyc3` | default region slug |
| `SANDRPOD_VM_SUBNET_ID` (`_DIGITALOCEAN`) | no | — | VPC UUID (optional) |
| `SANDRPOD_PODER_IMAGE` (`_DIGITALOCEAN`) | **yes (cloud)** | `sandrpod/poder:latest` | Poder image the droplet runs |
| `SANDRPOD_TOOLBOX_IMAGE` (`_DIGITALOCEAN`) | **yes (cloud)** | `sandrpod/toolbox:test` | toolbox image, forwarded to the Poder |

`SANDRPOD_VM_SECURITY_GROUP` is unused (DO Cloud Firewalls are separate). The
`SANDRPOD_VM_*`/image vars accept a provider-scoped `_DIGITALOCEAN` suffix.
Server flag: `-public-url <url>` — reachable from the droplets.

---

## End-to-end example

```bash
export DIGITALOCEAN_TOKEN=dop_v1_...
export DO_REGION=nyc3
export SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.3.1
export SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.3.1

go run ./cmd/server -port 8080 -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token "$SANDRPOD_TOKEN"

sandrpod-cli create my-box --provider digitalocean \
  --region nyc3 --instance-type s-2vcpu-4gb
```

The first request creates a droplet (default image: `ubuntu-22-04-x64`) and may
take a couple of minutes. Reuse the systemd pattern from
[AWS_PROVISIONING.md](AWS_PROVISIONING.md#running-the-server-as-a-systemd-service).

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| provider not registered | `DIGITALOCEAN_TOKEN`/`DO_TOKEN` unset |
| `401 Unauthorized` on create | token invalid or lacks write scope |
| `could not SSH to <ip>:22 before timeout` | a Cloud Firewall blocks 22 from the server, or the droplet got no public IP |
| `no ssh credential for droplet <id>` | `ExecuteCommand` for a droplet created by a different/earlier process (in-memory key gone) |
| Docker install / image pull fails | egress blocked, or image not pullable |
| `poder registration timeout` | API Server not reachable from the droplet (`-public-url`) |

---

## Known limitations & caveats

- **Not validated on a live account.** Most likely to need verification: cloud-init
  root-key injection timing and SSH first-connect vs the 3-minute dial-retry.
- **SSH executor, in-process key.** `ExecuteCommand` only works within the
  process that created the droplet; reclaim orphans via the reaper / poder delete.
- **Public IP is mandatory** (SSH needs it) — DO assigns one by default.
- **No autoscaling / no idle reclamation.** `Cleanup` deletes droplets tagged
  `sandrpod`.
- **Default image is `ubuntu-22-04-x64`.** Override per-request with `--image`.
