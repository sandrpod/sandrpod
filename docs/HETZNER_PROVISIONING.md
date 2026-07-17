# Hetzner Cloud Auto-Provisioning Guide

How SandrPod creates Hetzner Cloud servers on demand, bootstraps a Poder on each
**over SSH**, and runs sandboxes there. Hetzner is the **cheapest** of the
supported clouds, which makes it a good fit for large or cost-sensitive sandbox
fleets.

> **Status: implemented, not yet end-to-end validated on a live account.** The
> provider (`pkg/provider/hetzner/`) is real SDK code, builds, and has unit
> tests, but has not been run against a real Hetzner account. Smoke-test before
> relying on it. The **GCP** path ([GCP_PROVISIONING.md](GCP_PROVISIONING.md)) is
> the validated reference for the SSH-executor mechanics.

> **Hetzner has no managed run-command API**, so bootstrap runs over **SSH**:
> CreateVM injects a per-VM ephemeral ed25519 key via cloud-init (root login) and
> ExecuteCommand connects with the shared `pkg/provider/sshexec` helper. Servers
> log in as **root**; no firewall rule is needed by default (servers are open
> unless you attach a Hetzner Cloud Firewall).

> **TL;DR:** an API token is **necessary but not sufficient**. You also need
> (1) the API Server reachable from the servers, (2) the server able to reach the
> server's port 22 (open by default; if a Cloud Firewall is attached it must
> allow 22 from the API Server), and (3) `poder`/`toolbox` images pullable.

---

## What it does (and doesn't)

Lazy provision-on-demand, identical lifecycle to the GCP path:

- Creating a sandbox with `provider_type=hetzner` when **no Poder is available**
  for that location triggers: create a server (public IPv4 + ephemeral SSH key
  via cloud-init) → SSH in to install Docker → start a Poder → register → the
  sandbox is created.
- Subsequent servers in the same location **reuse** that Poder.
- **No** autoscaling. Idle reclamation is **off by default** — enable via `SANDRPOD_PODER_IDLE_TIMEOUT` / `SANDRPOD_SANDBOX_IDLE_TIMEOUT`.

### Flow

```
POST /api/v1/sandboxes {provider_type: hetzner}
        │
        ▼  (no Poder for location?)
   Server.Create (public IPv4 + cloud-init ssh key + label sandrpod) ──► poll public IP
        │
        ▼  SSH to <public-ip>:22 as root with the ephemeral key
   curl get.docker.com | sh   →   docker run … <poder image>
        │
        ▼  Poder dials API Server (-public-url) over WebSocket and registers
   sandbox created on the new Poder
```

SSH-executor specifics are the same as
[DigitalOcean](DIGITALOCEAN_PROVISIONING.md): a per-VM ephemeral ed25519 key
injected into `root` `authorized_keys` via cloud-init (held in-process by
default; set `SANDRPOD_SSH_KEY_DIR` to persist across server restarts),
`InsecureIgnoreHostKey`, real exit codes.

---

## Prerequisites checklist

- [ ] A Hetzner Cloud **API token** (project-scoped, read+write)
- [ ] API Server started with a **publicly reachable** `-public-url`
- [ ] The server can reach the server's **tcp:22** (open by default; allow it if a Cloud Firewall is attached)
- [ ] Servers have **outbound** to the API Server and to the internet (443)
- [ ] `poder` and `toolbox` images **pullable** by the server
- [ ] Server env vars set (see the [reference table](#environment-variable-reference))

---

## 1. Credentials

```bash
HCLOUD_TOKEN=...          # Hetzner Cloud project API token
HCLOUD_LOCATION=fsn1      # default location (fsn1/nbg1/hel1/ash/hil/sin)
```

> Setting the token is what **enables** the provider — `hetzner.Register()` skips
> registration when it is empty. Create the token in the Hetzner Cloud Console
> under **Project → Security → API Tokens** with **Read & Write** permission.

---

## 2. Networking & SSH reachability

Servers get a **public IPv4 by default**, and by default **no firewall** is
attached, so the API Server can SSH to port 22 out of the box. Notes:

- If you attach a **Hetzner Cloud Firewall**, add an inbound rule allowing
  **tcp:22** from the API Server's IP (scope it, avoid `0.0.0.0/0`).
- Servers need **outbound** to the API Server and to **443**; Hetzner's default
  egress is open.
- `SANDRPOD_VM_SUBNET_ID` / `SANDRPOD_VM_SECURITY_GROUP` are **not** used by the
  Hetzner provider — a public IPv4 is always attached and firewalls are separate
  resources.

---

## 3. API Server reachability & container images

The Poder dials `-public-url` back, so the server must be reachable from the
Hetzner server. Point the images at a registry the server can reach (public GHCR
works well from Hetzner's EU/US locations):

```bash
SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0
```

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `HCLOUD_TOKEN` | **yes** | — | API token; enables the provider |
| `HCLOUD_LOCATION` | no | `fsn1` | default location |
| `SANDRPOD_SSH_KEY_DIR` | recommended | — (in-memory) | persist per-VM SSH keys across server restarts |
| `SANDRPOD_PODER_IMAGE` (`_HETZNER`) | **yes (cloud)** | `ghcr.io/sandrpod/poder:latest` | Poder image the server runs |
| `SANDRPOD_TOOLBOX_IMAGE` (`_HETZNER`) | **yes (cloud)** | `ghcr.io/sandrpod/toolbox:latest` | toolbox image, forwarded to the Poder |

`SANDRPOD_VM_SUBNET_ID` / `SANDRPOD_VM_SECURITY_GROUP` are unused. The image vars
accept a provider-scoped `_HETZNER` suffix. Server flag: `-public-url <url>` —
reachable from the servers.

---

## End-to-end example

```bash
export HCLOUD_TOKEN=...
export HCLOUD_LOCATION=fsn1
export SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
export SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0

go run ./cmd/server -port 8080 -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token "$SANDRPOD_TOKEN"

sandrpod-cli create my-box --provider hetzner \
  --region fsn1 --instance-type cx22
```

The first request creates a server (default image: `ubuntu-22.04`) and may take a
minute or two. Reuse the systemd pattern from
[AWS_PROVISIONING.md](AWS_PROVISIONING.md#running-the-server-as-a-systemd-service).

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| provider not registered | `HCLOUD_TOKEN` unset |
| `unauthorized` / `forbidden` on create | token invalid or read-only |
| `invalid input` on `server_type`/`location`/`image` | bad name (use e.g. `cx22`, `fsn1`, `ubuntu-22.04`) |
| `could not SSH to <ip>:22 before timeout` | a Cloud Firewall blocks 22 from the server, or no public IP |
| `no ssh credential for server <id>` | `ExecuteCommand` for a server created by a different/earlier process |
| `poder registration timeout` | API Server not reachable from the server (`-public-url`) |

---

## Known limitations & caveats

- **Not validated on a live account.** Most likely to need verification: cloud-init
  root-key injection and SSH first-connect vs the 3-minute dial-retry.
- **SSH executor.** Keys are in-process by default — set `SANDRPOD_SSH_KEY_DIR`
  to persist them across server restarts; reclaim orphans via the reaper /
  poder delete.
- **Public IPv4 is always attached** (SSH needs it).
- **No subnet/security-group plumbing** — attach a Cloud Firewall out-of-band if
  you need to restrict traffic.
- **No autoscaling.** Idle-VM reclamation is opt-in (`SANDRPOD_PODER_IDLE_TIMEOUT`; see [UPGRADING.md](UPGRADING.md)). `Cleanup` deletes servers labeled
  `sandrpod=true`.
- **Default image is `ubuntu-22.04`.** Override per-request with `--image`.
