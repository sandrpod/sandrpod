# DigitalOcean Auto-Provisioning Guide

How SandrPod creates DigitalOcean droplets on demand, bootstraps a Poder on each
**over SSH**, and runs sandboxes there.

> **Status: validated end-to-end on a live account** (region `sgp1`,
> `s-2vcpu-2gb`, default VPC, no Cloud Firewall): droplet + public IP in ~40 s,
> SSH bootstrap as root, Docker install ~56 s, Poder registers over the
> cross-cloud tunnel, sandbox RUNNING, code executes, poder reuse in ~3 s, and
> poder delete removes the droplet with no leak. The first live run surfaced
> two now-fixed issues worth knowing about: DO's first-boot vendor tasks can
> keep cloud-init "running" indefinitely (the bootstrap's cloud-init wait is
> now bounded at 180 s), and droplets created without a provider-registered
> SSH key get a root password with **forced expiry that PAM enforces even for
> pubkey logins** — the cloud-init user-data now clears it (`chage`).

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
- **No** autoscaling. Idle reclamation is **off by default** — enable via `SANDRPOD_PODER_IDLE_TIMEOUT` / `SANDRPOD_SANDBOX_IDLE_TIMEOUT`.

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
  private half is used by `ExecuteCommand`. By default it is held **in-process**
  (never touches disk, dies with the droplet).
- Set `SANDRPOD_SSH_KEY_DIR=/var/lib/sandrpod/ssh-keys` to persist per-VM keys
  (PKCS#8 PEM, `0600`) so a server restart doesn't orphan existing droplets;
  without it, `ExecuteCommand` only works within the process that created the
  droplet.
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
SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.5.0
SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.5.0
```

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `DIGITALOCEAN_TOKEN` (or `DO_TOKEN`) | **yes** | — | API token; enables the provider |
| `DO_REGION` | no | `nyc3` | default region slug |
| `SANDRPOD_VM_SUBNET_ID` (`_DIGITALOCEAN`) | no | — | VPC UUID (optional) |
| `SANDRPOD_SSH_KEY_DIR` | recommended | — (in-memory) | persist per-VM SSH keys across server restarts |
| `SANDRPOD_PODER_IMAGE` (`_DIGITALOCEAN`) | **yes (cloud)** | `ghcr.io/sandrpod/poder:latest` | Poder image the droplet runs |
| `SANDRPOD_TOOLBOX_IMAGE` (`_DIGITALOCEAN`) | **yes (cloud)** | `ghcr.io/sandrpod/toolbox:latest` | toolbox image, forwarded to the Poder |

`SANDRPOD_VM_SECURITY_GROUP` is unused (DO Cloud Firewalls are separate). The
`SANDRPOD_VM_*`/image vars accept a provider-scoped `_DIGITALOCEAN` suffix.
Server flag: `-public-url <url>` — reachable from the droplets.

---

## End-to-end example

```bash
export DIGITALOCEAN_TOKEN=dop_v1_...
export DO_REGION=nyc3
export SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.5.0
export SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.5.0

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

- **Validated end-to-end** (`sgp1`, `s-2vcpu-2gb`): cloud-init root-key
  injection, SSH first-connect timing, Docker bootstrap, reuse, and droplet
  reclamation all confirmed live. Two DO first-boot behaviors are handled in
  code: cloud-init can stay "running" indefinitely (bounded wait), and the
  forced root-password expiry is cleared via cloud-init (PAM would otherwise
  kill pubkey command sessions with "Your password has expired").
- **SSH executor.** Keys are in-process by default — set `SANDRPOD_SSH_KEY_DIR`
  to persist them across server restarts; reclaim orphans via the reaper /
  poder delete.
- **Public IP is mandatory** (SSH needs it) — DO assigns one by default.
- **No autoscaling.** Idle-VM reclamation is opt-in (`SANDRPOD_PODER_IDLE_TIMEOUT`; see [UPGRADING.md](UPGRADING.md)). `Cleanup` deletes droplets tagged
  `sandrpod`.
- **Default image is `ubuntu-22-04-x64`.** Override per-request with `--image`.
