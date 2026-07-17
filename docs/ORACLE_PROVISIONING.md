# Oracle Cloud Infrastructure (OCI) Auto-Provisioning Guide

How SandrPod creates OCI compute instances on demand, bootstraps a Poder on each
via the **Compute Instance Agent** run-command, and runs sandboxes there. OCI's
**Always Free** tier (Ampere A1 / E2.1.Micro) makes it attractive for **zero-cost
testing** of the multi-cloud path.

> **Status: implemented, not yet end-to-end validated on a live account — and the
> one most likely to need it.** OCI is the heaviest provider: the public IP is
> resolved by walking VNIC attachments, auth goes through an OCI
> ConfigurationProvider, and the run-command uses polymorphic content types. It
> compiles and has unit tests, but smoke-test carefully. The **AWS** path
> ([AWS_PROVISIONING.md](AWS_PROVISIONING.md)) is the hardened reference for the
> provider-agnostic plumbing.

> **TL;DR:** OCI config is **necessary but not sufficient**. You also need
> (1) a **compartment** + **availability domain** + a **public subnet OCID**,
> (2) an IAM policy allowing compute + instance-agent-command + VNIC reads,
> (3) the API Server reachable from the VMs, and (4) `poder`/`toolbox` images
> pullable by the VM.

---

## What it does (and doesn't)

Lazy provision-on-demand, identical lifecycle to the AWS path:

- Creating a sandbox with `provider_type=oracle` when **no Poder is available**
  triggers: `LaunchInstance` (with `assignPublicIp`) → wait running → resolve the
  public IP via VNIC → install Docker via the instance agent → start a Poder →
  register → the sandbox is created.
- Subsequent instances **reuse** that Poder.
- **No** autoscaling. Idle reclamation is **off by default** — enable via `SANDRPOD_PODER_IDLE_TIMEOUT` / `SANDRPOD_SANDBOX_IDLE_TIMEOUT`.

### Flow

```
POST /api/v1/sandboxes {provider_type: oracle}
        │
        ▼  (no Poder?)
   LaunchInstance (subnet + assignPublicIp + freeform tag sandrpod)
        │  ──► GetInstance (running) ──► ListVnicAttachments ──► GetVnic (public IP)
        ▼  CreateInstanceAgentCommand (run-command)   (needs the agent + Run Command plugin)
   curl get.docker.com | sh   →   docker run … <poder image>
        │
        ▼  Poder dials API Server (-public-url) over WebSocket and registers
   sandbox created on the new Poder
```

Remote exec uses the **Compute Instance Agent** run-command (agent-based, like
SSM/CloudAssist) — no SSH. Platform Ubuntu images ship the agent with the **Run
Command** plugin enabled. The public IP is **not** on the Instance object, so the
provider lists the instance's VNIC attachments and reads the VNIC's `publicIp`.

---

## Prerequisites checklist

- [ ] An OCI tenancy with an **API-key user** and `~/.oci/config` (or OCI_* env)
- [ ] A **compartment OCID**, an **availability domain**, and a **public subnet OCID**
- [ ] An IAM policy allowing compute + instance-agent-command + VNIC reads
- [ ] The subnet is **public** (route to an internet gateway) so `assignPublicIp` works
- [ ] API Server started with a **publicly reachable** `-public-url`
- [ ] `poder` and `toolbox` images **pullable** by the VM
- [ ] Server env vars set (see the [reference table](#environment-variable-reference))

---

## 1. Credentials (OCI ConfigurationProvider)

The provider authenticates with a standard OCI **ConfigurationProvider**: either
the default `~/.oci/config` (+ OCI_* env), or an explicit file via
`OCI_CONFIG_FILE`. Generate an API signing key and config in the OCI console
(**Identity → Users → your user → API Keys → Add API Key**), which produces the
`~/.oci/config` stanza (tenancy/user/fingerprint/key_file/region).

Then set the placement env (read by `pkg/provider/oracle/config.go`):

```bash
OCI_COMPARTMENT_OCID=ocid1.compartment.oc1..aaaa...
OCI_AVAILABILITY_DOMAIN="Uocm:PHX-AD-1"    # exact AD name, quote it
OCI_CONFIG_FILE=/opt/sandrpod/oci-config    # optional; omit for ~/.oci/config
```

> Setting `OCI_COMPARTMENT_OCID` is what **enables** the provider —
> `oracle.Register()` skips registration when it is empty. The **region** comes
> from the OCI config, not from env.

**IAM policy** for the group your API user belongs to (in the compartment):

```text
Allow group SandrPod to manage instance-family in compartment <name>
Allow group SandrPod to use virtual-network-family in compartment <name>
Allow group SandrPod to read app-catalog-listing in compartment <name>
Allow group SandrPod to use instance-agent-command-family in compartment <name>
```

---

## 2. Networking

You must provide a **public subnet OCID** — OCI has no default-VPC fallback and
`CreateVM` fails fast without it:

```bash
SANDRPOD_VM_SUBNET_ID_ORACLE=ocid1.subnet.oc1.phx.aaaa...
```

- The subnet must be **public** (its route table has an internet gateway and its
  security list allows egress), so `assignPublicIp=true` yields a reachable IP.
- Egress must reach the API Server host/port and **443** (Docker install + image
  pulls). `SANDRPOD_VM_SECURITY_GROUP` is not used — OCI traffic is governed by
  the subnet's security lists / network security groups.

---

## 3. API Server reachability & container images

The Poder dials `-public-url` back, so the server must be reachable from the OCI
VM. Point the images at a registry the VM can reach (public GHCR, or **OCIR** for
low-latency/private pulls):

```bash
SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.5.0
SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.5.0
```

---

## Shapes (and the free tier)

- **Flex shapes** (`VM.Standard.A1.Flex`, `VM.Standard.E4.Flex`, …) require a
  shape config; the provider auto-applies a default **1 OCPU / 6 GB** when the
  shape name contains "flex". To size differently, adjust `flexDefaultOcpus` /
  `flexDefaultGB` in `pkg/provider/oracle/oracle.go` (per-request sizing isn't
  plumbed yet).
- **Always Free** options: `VM.Standard.A1.Flex` (Ampere ARM, up to 4 OCPU / 24
  GB across the tenancy) and `VM.Standard.E2.1.Micro` (x86). Great for zero-cost
  smoke tests.

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `OCI_COMPARTMENT_OCID` | **yes** | — | compartment for instances; enables the provider |
| `OCI_AVAILABILITY_DOMAIN` | **yes** | — | AD to launch into (e.g. `Uocm:PHX-AD-1`) |
| `OCI_CONFIG_FILE` | no | `~/.oci/config` | OCI config path |
| `SANDRPOD_VM_SUBNET_ID` (`_ORACLE`) | **yes** | — | public subnet OCID |
| `SANDRPOD_PODER_IMAGE` (`_ORACLE`) | **yes (cloud)** | `ghcr.io/sandrpod/poder:latest` | Poder image the VM runs |
| `SANDRPOD_TOOLBOX_IMAGE` (`_ORACLE`) | **yes (cloud)** | `ghcr.io/sandrpod/toolbox:latest` | toolbox image, forwarded to the Poder |

`SANDRPOD_VM_SECURITY_GROUP` is unused (subnet security lists govern traffic). The
`SANDRPOD_VM_*`/image vars accept a provider-scoped `_ORACLE` suffix. Server flag:
`-public-url <url>` — reachable from the VMs.

---

## End-to-end example

```bash
export OCI_COMPARTMENT_OCID=ocid1.compartment.oc1..aaaa...
export OCI_AVAILABILITY_DOMAIN="Uocm:PHX-AD-1"
export SANDRPOD_VM_SUBNET_ID_ORACLE=ocid1.subnet.oc1.phx.aaaa...
export SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.5.0
export SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.5.0

go run ./cmd/server -port 8080 -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token "$SANDRPOD_TOKEN"

# region = the AD's region, from ~/.oci/config; the request region is unused by OCI
sandrpod-cli create my-box --provider oracle --instance-type VM.Standard.A1.Flex
```

The first request launches an instance (default image: newest Canonical Ubuntu
22.04) and may take a few minutes. Reuse the systemd pattern from
[AWS_PROVISIONING.md](AWS_PROVISIONING.md#running-the-server-as-a-systemd-service);
keep the OCI key file root-readable (`chmod 600`).

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| provider not registered | `OCI_COMPARTMENT_OCID` unset |
| `can not create client, bad configuration` | `~/.oci/config` / `OCI_CONFIG_FILE` missing or malformed |
| `NotAuthorizedOrNotFound` on launch | IAM policy missing manage instance-family / use virtual-network-family |
| `oracle requires a subnet OCID` | set `SANDRPOD_VM_SUBNET_ID_ORACLE` |
| instance runs but no public IP | subnet is private (no internet gateway), or `assignPublicIp` blocked |
| launch fails on a Flex shape | shape needs a shape config — the provider sets 1 OCPU/6 GB; adjust if the shape rejects it |
| command never runs | Compute agent / Run Command plugin disabled on the image; the provider retries ~3 min |
| `poder registration timeout` | API Server not reachable from the VM (`-public-url`) |

---

## Known limitations & caveats

- **Heaviest, least-validated provider.** Verify on a live tenancy: the VNIC
  public-IP walk, the run-command polymorphic output extraction, Flex shape
  configs, and the ListImages Ubuntu filter.
- **Public subnet is required** (no default-VPC fallback); the public IP comes
  from a VNIC, not the Instance object.
- **Flex-shape sizing is a fixed default** (1 OCPU / 6 GB) — per-request sizing
  isn't plumbed.
- **The OCI SDK is large** (one monolithic module); only the compute / agent /
  network packages are compiled in.
- **No autoscaling.** Idle-VM reclamation is opt-in (`SANDRPOD_PODER_IDLE_TIMEOUT`; see [UPGRADING.md](UPGRADING.md)). `Cleanup` deletes instances with the
  freeform tag `sandrpod=true`.
- **Default image is the newest Canonical Ubuntu 22.04.** Override per-request
  with `--image ocid1.image...`.
