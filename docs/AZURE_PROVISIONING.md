# Azure Auto-Provisioning Guide

How SandrPod creates Azure VMs on demand, bootstraps a Poder on each via **Azure
Run Command**, and runs sandboxes there — and exactly what you must configure.

> **Status: implemented, not yet end-to-end validated on a live subscription.**
> The provider (`pkg/provider/azure/azure.go`) is real SDK code, builds, and has
> unit tests, but has not been run against a real Azure account. Treat the
> live-cloud steps below as "should work"; smoke-test before relying on it. The
> **AWS** path ([AWS_PROVISIONING.md](AWS_PROVISIONING.md)) is the hardened
> reference for the provider-agnostic plumbing.

> **TL;DR:** a service principal is **necessary but not sufficient**. You also
> need (1) a **pre-existing resource group**, (2) a **subnet** the VMs bind to,
> (3) the API Server reachable from the VMs, and (4) the `poder`/`toolbox` images
> pullable by the VM. Miss any one and provisioning stalls.

---

## What it does (and doesn't)

This is **lazy provision-on-demand**, not an autoscaler — identical lifecycle to
the AWS path:

- Creating a sandbox with `provider_type=azure` when **no Poder is available**
  for that region triggers: create a public IP + NIC + VM → install Docker →
  start a Poder → the Poder dials back and registers → the sandbox is created.
- Subsequent `azure` sandboxes in the same region **reuse** that Poder. One
  VM/Poder hosts **many** sandboxes.
- **No** continuous scale-out and **no** idle-VM reclamation.

### Flow

```
POST /api/v1/sandboxes {provider_type: azure}
        │
        ▼  (no Poder for region?)
   create public IP ──► create NIC (binds subnet) ──► create VM ──► read public IP
        │
        ▼  Run Command (RunShellScript, via the VM agent)
   curl get.docker.com | sh   →   docker run … <poder image>
        │
        ▼  Poder dials API Server (-public-url) over WebSocket and registers
   sandbox created on the new Poder
```

Unlike AWS SSM, Azure **Run Command** needs no instance role — it rides the Azure
VM agent (walinuxagent), present in the marketplace Ubuntu images. Run Command
reports stdout/stderr but **no structured exit code**, so the provider wraps each
command to echo its real exit code on stdout and parses it back — giving the same
exit-code semantics as the other clouds.

---

## Prerequisites checklist

- [ ] An **Azure subscription** and a **service principal** (tenant/client/secret)
- [ ] A **pre-existing resource group** new VMs (and their NIC/public IP) land in
- [ ] A **VNet + subnet**; the subnet's **resource ID** is required
- [ ] The service principal has **Contributor** (or granular VM+Network) on the RG
- [ ] API Server started with a **publicly reachable** `-public-url`
- [ ] Subnet/NSG allows the VM **outbound** to the API Server and to the internet (443)
- [ ] `poder` and `toolbox` images **published and pullable** by the VM
- [ ] Server env vars set (see the [reference table](#environment-variable-reference))

---

## 1. Credentials for the API Server (service principal)

The server calls the Compute and Network ARM APIs with a **service principal**.
Create one and note the three IDs + secret:

```bash
az ad sp create-for-rbac --name sandrpod-server \
  --role Contributor \
  --scopes /subscriptions/<sub-id>/resourceGroups/<resource-group>
# Output: appId (=CLIENT_ID), password (=CLIENT_SECRET), tenant (=TENANT_ID)
```

Set them for the server (read by `pkg/provider/azure/config.go`):

```bash
AZURE_SUBSCRIPTION_ID=<sub-id>
AZURE_TENANT_ID=<tenant>
AZURE_CLIENT_ID=<appId>
AZURE_CLIENT_SECRET=<password>
AZURE_RESOURCE_GROUP=sandrpod-rg
AZURE_LOCATION=eastus
```

> Setting these is what **enables** the provider — `azure.Register()` skips
> registration when subscription / tenant / client / secret / resource group is
> empty (`pkg/provider/azure/register.go`).

**Scope matters.** Scoping the role to the single resource group (above) is the
least-privilege choice; the SP can only touch that RG. Contributor covers VM,
NIC, public IP, and Run Command. For tighter control, combine
**Virtual Machine Contributor** + **Network Contributor** on the RG instead.

---

## 2. Resource group, VNet, and subnet

Azure has no "default VPC" fast path — every VM's NIC must bind to an existing
subnet, so **`CreateVM` fails fast if no subnet is provided.**

```bash
az group create -n sandrpod-rg -l eastus
az network vnet create -g sandrpod-rg -n sandrpod-vnet \
  --address-prefix 10.0.0.0/16 \
  --subnet-name default --subnet-prefix 10.0.0.0/24
# Get the subnet resource ID for SANDRPOD_VM_SUBNET_ID_AZURE:
az network vnet subnet show -g sandrpod-rg --vnet-name sandrpod-vnet -n default --query id -o tsv
# /subscriptions/<sub>/resourceGroups/sandrpod-rg/providers/Microsoft.Network/virtualNetworks/sandrpod-vnet/subnets/default
```

The provider creates a **Standard-SKU static public IP** and a **NIC** per VM
(named `<vm>-pip` / `<vm>-nic`), and `DeleteVM` tears down VM → NIC → public IP
in that order (Azure does not cascade-delete them).

> **NSG egress.** If the subnet has an NSG, it must allow **outbound** to the API
> Server host/port and to the internet on **443** (`get.docker.com`, the image
> registry). Azure's default outbound is permissive, but locked-down subnets need
> an explicit rule.

---

## 3. API Server must be reachable from the VMs

The Poder on the Azure VM **dials back** to register. Start the server with a
`-public-url` the VM can reach:

```bash
go run ./cmd/server -port 8080 \
  -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token <token>
```

> Same failure mode as every cloud: a server on localhost / behind NAT means the
> Azure Poder can't connect and `waitForPoderRegistration` times out.

---

## 4. Container images (poder + toolbox)

On a fresh VM, `docker run <poder image>` must **pull**, and the Poder then pulls
the **toolbox** image. Point both at a registry the VM can reach (public GHCR, or
**Azure Container Registry** for lower latency / private pulls):

```bash
SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0
```

If using a private ACR, the bootstrap does not `docker login` — keep the repo
public or extend the bootstrap (same trade-off as GHCR/ACR on other clouds).

---

## 5. VM login auth (optional)

Azure requires *some* Linux auth config at VM creation even though SandrPod never
logs in over SSH (all remote exec goes through the VM agent). By default the
provider generates a throwaway strong password to satisfy the API — it is never
returned or used. To instead install your own SSH key (and disable password
auth):

```bash
AZURE_SSH_PUBLIC_KEY="ssh-ed25519 AAAA... you@host"
AZURE_ADMIN_USERNAME=sandrpod           # default
```

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `AZURE_SUBSCRIPTION_ID` | **yes** | — | subscription for all API calls |
| `AZURE_TENANT_ID` | **yes** | — | service principal tenant |
| `AZURE_CLIENT_ID` | **yes** | — | service principal app ID |
| `AZURE_CLIENT_SECRET` | **yes** | — | service principal secret |
| `AZURE_RESOURCE_GROUP` | **yes** | — | pre-existing RG new VMs land in |
| `AZURE_LOCATION` | no | `eastus` | target region |
| `AZURE_ADMIN_USERNAME` | no | `sandrpod` | Linux admin user |
| `AZURE_SSH_PUBLIC_KEY` | no | — | install a key + disable password auth |
| `SANDRPOD_VM_SUBNET_ID` (`_AZURE`) | **yes** | — | subnet **resource ID** the NIC binds to |
| `SANDRPOD_VM_PUBLIC_IP` (`_AZURE`) | no | `true` | assign a public IP |
| `SANDRPOD_PODER_IMAGE` (`_AZURE`) | **yes (cloud)** | `ghcr.io/sandrpod/poder:latest` | Poder image the VM runs |
| `SANDRPOD_TOOLBOX_IMAGE` (`_AZURE`) | **yes (cloud)** | `ghcr.io/sandrpod/toolbox:latest` | toolbox image, forwarded to the Poder |

The `SANDRPOD_VM_*` and image vars accept a **provider-scoped** `_AZURE` suffix
that overrides the unscoped default, so one server can drive Azure alongside
AWS/Aliyun/GCP without value collisions (see
[ALIYUN_PROVISIONING.md](ALIYUN_PROVISIONING.md#running-aws-and-aliyun-on-one-server-per-provider-env-vars)).
Note `SANDRPOD_VM_SECURITY_GROUP` is **not** used by Azure — the NIC's subnet/NSG
governs traffic.

Server flag: `-public-url <url>` — reachable from the VMs (passed to the Poder as `API_URL`).

---

## End-to-end example

```bash
export AZURE_SUBSCRIPTION_ID=<sub-id>
export AZURE_TENANT_ID=<tenant>
export AZURE_CLIENT_ID=<appId>
export AZURE_CLIENT_SECRET=<password>
export AZURE_RESOURCE_GROUP=sandrpod-rg
export AZURE_LOCATION=eastus
export SANDRPOD_VM_SUBNET_ID_AZURE=/subscriptions/<sub>/resourceGroups/sandrpod-rg/providers/Microsoft.Network/virtualNetworks/sandrpod-vnet/subnets/default
export SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
export SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0

go run ./cmd/server -port 8080 \
  -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token "$SANDRPOD_TOKEN"

# Then create a sandbox on Azure:
sandrpod-cli create my-box --provider-type azure \
  --region eastus --instance-type Standard_D2s_v5
```

The first such request creates a VM (default image: Ubuntu 22.04 LTS gen2 from
Canonical) and may take a few minutes (VM boot + Docker install + image pull).

---

## Running the server as a systemd service

Reuse the unit + `service.d` drop-in pattern from
[AWS_PROVISIONING.md](AWS_PROVISIONING.md#running-the-server-as-a-systemd-service),
swapping the environment drop-in for Azure (chmod 600 — holds the SP secret):

`/etc/systemd/system/sandrpod-server.service.d/azure.conf`

```ini
[Service]
Environment=AZURE_SUBSCRIPTION_ID=<sub-id>
Environment=AZURE_TENANT_ID=<tenant>
Environment=AZURE_CLIENT_ID=<appId>
Environment=AZURE_CLIENT_SECRET=<password>
Environment=AZURE_RESOURCE_GROUP=sandrpod-rg
Environment=AZURE_LOCATION=eastus
Environment=SANDRPOD_VM_SUBNET_ID_AZURE=/subscriptions/<sub>/resourceGroups/sandrpod-rg/providers/Microsoft.Network/virtualNetworks/sandrpod-vnet/subnets/default
Environment=SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
Environment=SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0
```

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| provider not registered at startup | one of subscription/tenant/client/secret/**resource group** is unset |
| `azure requires a subnet resource ID` | set `SANDRPOD_VM_SUBNET_ID_AZURE` to the full subnet resource ID |
| `AuthorizationFailed` on create | the SP lacks Contributor (or VM+Network Contributor) on the RG |
| VM created, command never runs | Azure VM agent not ready yet, or image lacks it — retry; use a marketplace Ubuntu image |
| `poder registration timeout` | API Server not reachable from the VM (`-public-url` wrong / behind NAT) |
| Docker install / image pull fails | subnet NSG blocks egress 443, or image not pullable |
| `No such image` for toolbox | image not pullable on the VM (private ACR without login, or wrong path) |
| leftover NIC / public IP after delete | `DeleteVM` cleans them, but a failed/partial create can orphan `<vm>-nic` / `<vm>-pip` — delete by name in the RG |

---

## Known limitations & caveats

- **Not end-to-end validated on a live subscription.** The most likely things to
  need real-cloud verification: the Run Command StdOut/StdErr status codes, the
  default image URN's gen2 availability in your region, and power-state parsing.
- **No autoscaling / no idle reclamation.** One VM per "region has no Poder".
  `Cleanup` deletes VMs tagged `sandrpod=true` (VM + its NIC + public IP).
- **Subnet is required** (no default-VNet shortcut); `SANDRPOD_VM_SECURITY_GROUP`
  is ignored (subnet/NSG governs traffic).
- **Default image is Ubuntu 22.04 LTS gen2.** Override per-request with `--image`
  as a `publisher:offer:sku:version` URN or a full image resource ID.
- **Run Command has no native exit code** — recovered via a stdout sentinel the
  provider injects and strips.
