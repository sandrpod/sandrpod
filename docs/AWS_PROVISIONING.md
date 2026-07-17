# AWS Auto-Provisioning Guide

How SandrPod creates EC2 VMs on demand, bootstraps a Poder on each, and runs
sandboxes there — and exactly what you must configure for it to work.

> **TL;DR:** an AWS access key is **necessary but not sufficient**. You also need
> (1) an IAM **instance profile** for SSM, (2) the API Server reachable from the
> VMs over the public internet, (3) networking that gives the VM outbound
> access, and (4) the `poder`/`toolbox` images published somewhere the VM can
> pull. Miss any one and provisioning stalls.

---

## What it does (and doesn't)

This is **lazy provision-on-demand**, not an autoscaler:

- Creating a sandbox with `provider_type=aws` when **no Poder is available** for
  that region triggers: launch one VM → install Docker → start a Poder → the
  Poder dials back and registers → the sandbox is created on it.
- Subsequent `aws` sandboxes in the same region **reuse** that Poder
  (`SelectBest`). One VM/Poder hosts **many** sandboxes (each sandbox is a
  Toolbox container).
- There is **no** continuous scale-out — one VM is launched only when a region
  has no usable Poder. Idle reclamation is **off by default**; enable via
  `SANDRPOD_PODER_IDLE_TIMEOUT` / `SANDRPOD_SANDBOX_IDLE_TIMEOUT`.

### Flow

```
POST /api/v1/sandboxes {provider_type: aws}
        │
        ▼  (no Poder for region?)
   EC2 RunInstances ──► wait running ──► DescribeInstances (get public IP)
        │
        ▼  SSM SendCommand  (needs the instance profile!)
   curl get.docker.com | sh   →   docker run … <poder image>
        │
        ▼  Poder dials API Server (-public-url) over WebSocket and registers
   sandbox created on the new Poder
```

---

## Prerequisites checklist

- [ ] AWS credentials for the **server** (key or instance role) with EC2 + SSM + `iam:PassRole`
- [ ] An **IAM instance profile** for launched VMs, granting `AmazonSSMManagedInstanceCore`
- [ ] API Server started with a **publicly reachable** `-public-url`
- [ ] A **security group** allowing the VM outbound to the API Server and to the internet (443)
- [ ] `poder` and `toolbox` images **published and pullable** by the VM (e.g. public GHCR)
- [ ] Server env vars set (see the [reference table](#environment-variable-reference))

---

## 1. Credentials for the API Server

The server calls EC2 and SSM. Provide credentials via env (read by
`pkg/provider/aws/config.go`) or the default AWS credential chain (IAM role,
`~/.aws`, etc.):

```bash
AWS_ACCESS_KEY_ID=AKIA...
AWS_SECRET_ACCESS_KEY=...
AWS_REGION=us-east-1
```

> **Instance role vs static keys.** If the server itself runs **on EC2**, prefer
> attaching an IAM role to that instance and omitting the static keys — the SDK
> picks up temporary credentials automatically (no long-lived secrets). If the
> server runs **off AWS** (a NAS, another cloud), there is no instance metadata,
> so you **must** set `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY`.

The associated IAM principal (role or user) needs at least:

```jsonc
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "ec2:RunInstances", "ec2:TerminateInstances",
        "ec2:DescribeInstances", "ec2:DescribeImages",
        "ec2:DescribeRegions", "ec2:CreateTags"
      ],
      "Resource": "*"
    },
    {
      // REQUIRED to attach the instance profile to launched VMs.
      "Effect": "Allow",
      "Action": "iam:PassRole",
      "Resource": "arn:aws:iam::<account-id>:role/<vm-instance-role>"
    },
    {
      "Effect": "Allow",
      "Action": ["ssm:SendCommand", "ssm:GetCommandInvocation"],
      "Resource": "*"
    }
  ]
}
```

> `iam:PassRole` is the most commonly missed permission — without it
> `RunInstances` is denied when an instance profile is specified.

Save that JSON as a customer-managed policy (e.g. `sandrpod-server-policy`),
then apply it one of two ways:

**Server on EC2 → attach a role to the server instance (no static keys):**

```bash
# Create the policy and an EC2 role that uses it, then attach to the instance.
aws iam create-policy --policy-name sandrpod-server-policy \
  --policy-document file:///tmp/server-policy.json
aws iam create-role --role-name sandrpod-server \
  --assume-role-policy-document file:///tmp/ec2-trust.json     # same trust as §2
aws iam attach-role-policy --role-name sandrpod-server \
  --policy-arn arn:aws:iam::<account-id>:policy/sandrpod-server-policy
aws iam create-instance-profile --instance-profile-name sandrpod-server
aws iam add-role-to-instance-profile --instance-profile-name sandrpod-server --role-name sandrpod-server
# Attach to the running server instance (console: EC2 → instance → Actions →
# Security → Modify IAM role):
aws iam ... # or: EC2 → Modify IAM role → sandrpod-server
```

Console equivalent: IAM → Roles → Create role (AWS service / EC2) → attach
`sandrpod-server-policy` → name `sandrpod-server`; then EC2 → select the server
instance → **Actions → Security → Modify IAM role** → choose `sandrpod-server`.

**Server off AWS → create an IAM user with the policy and use its access key:**

```bash
aws iam create-user --user-name sandrpod-server-user
aws iam attach-user-policy --user-name sandrpod-server-user \
  --policy-arn arn:aws:iam::<account-id>:policy/sandrpod-server-policy
aws iam create-access-key --user-name sandrpod-server-user   # set the keys as env
```

---

## 2. IAM instance profile for the VMs (SSM)

SandrPod bootstraps each VM via **SSM RunShellScript**, which only works if the
instance carries an IAM instance profile with the SSM core permissions.

**Console:** IAM → Roles → Create role → trusted entity **AWS service**, use case
**EC2** → attach **`AmazonSSMManagedInstanceCore`** → name it `sandrpod-vm-ssm`.
Creating an EC2 role in the console also creates the same-named instance profile.

**CLI:**

```bash
ROLE=sandrpod-vm-ssm
cat > /tmp/ec2-trust.json <<'EOF'
{ "Version": "2012-10-17", "Statement": [{
    "Effect": "Allow",
    "Principal": { "Service": "ec2.amazonaws.com" },
    "Action": "sts:AssumeRole" }] }
EOF
aws iam create-role --role-name "$ROLE" --assume-role-policy-document file:///tmp/ec2-trust.json
aws iam attach-role-policy --role-name "$ROLE" \
  --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
aws iam create-instance-profile --instance-profile-name "$ROLE"
aws iam add-role-to-instance-profile --instance-profile-name "$ROLE" --role-name "$ROLE"
```

Then tell the server its instance-profile **name** (not ARN):

```bash
AWS_IAM_INSTANCE_PROFILE=sandrpod-vm-ssm
```

Without this, `ExecuteCommand` has no managed instance to target and the
Docker-install / Poder-start steps never run.

---

## 3. Networking

The scheduler requests a VM with a **public IP by default** so it can reach the
API Server and pull images. Tune via env:

```bash
SANDRPOD_VM_PUBLIC_IP=true            # default; set false for NAT/private subnets
SANDRPOD_VM_SECURITY_GROUP=sg-0abc…   # optional but recommended
SANDRPOD_VM_SUBNET_ID=subnet-0abc…    # optional
```

Security group requirements (outbound):

- To the **API Server** host/port (the Poder dials it over WebSocket).
- To the internet on **443** (`get.docker.com`, the image registry).

If you set `SANDRPOD_VM_PUBLIC_IP=false`, the subnet must provide outbound via a
NAT gateway, and the API Server must be reachable on a private route.

> **Accounts with no default VPC must set `SANDRPOD_VM_SUBNET_ID`.** Without a
> subnet and without a default VPC, `RunInstances` fails with
> `VPCIdNotSpecified: No default VPC for this user`. The simplest choice is the
> same subnet + security group as the API Server's own instance — co-located,
> already public, already reachable.

---

## 4. API Server must be reachable from the VMs

The Poder running on the EC2 VM **dials back** to the API Server to register.
Start the server with a `-public-url` that the VM can actually reach:

```bash
go run ./cmd/server -port 8080 \
  -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token <token>
```

> **This is the most common reason cloud provisioning "configured the key but
> nothing happens".** If the server runs on a home NAS / localhost / behind NAT
> with no public address, the EC2 Poder cannot connect and
> `waitForPoderRegistration` times out. Put the server somewhere with a public
> endpoint, or expose it via a tunnel / port-forward.

---

## 5. Container images (poder + toolbox)

On a fresh VM, `docker run <poder image>` must be able to **pull** the image, and
the Poder in turn pulls the **toolbox** image. Point both at a registry the VM
can reach (e.g. public GHCR):

```bash
SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0
```

- `SANDRPOD_PODER_IMAGE` is used by the scheduler for the `docker run` on the VM.
- `SANDRPOD_TOOLBOX_IMAGE` is **forwarded** to the remote Poder so it spawns the
  matching toolbox.
- Publish images with `scripts/publish-images.sh` (local) or the
  `.github/workflows/release.yml` flow (if the repo is on GitHub). **Make the
  GHCR packages public**, or the VM needs `docker login` credentials.

---

## Environment variable reference

All set on the **API Server** process.

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | for static creds | (cred chain) | AWS API auth |
| `AWS_REGION` | no | `us-east-1` | target region |
| `AWS_IAM_INSTANCE_PROFILE` | **yes** | — | instance profile for SSM on VMs |
| `SANDRPOD_VM_PUBLIC_IP` | no | `true` | assign a public IP to VMs |
| `SANDRPOD_VM_SECURITY_GROUP` | recommended | — | security group for VMs |
| `SANDRPOD_VM_SUBNET_ID` | **yes if no default VPC** | — | subnet for VMs |
| `SANDRPOD_PODER_IMAGE` | **yes (cloud)** | `ghcr.io/sandrpod/poder:latest` | Poder image the VM runs |
| `SANDRPOD_TOOLBOX_IMAGE` | **yes (cloud)** | `ghcr.io/sandrpod/toolbox:latest` | toolbox image, forwarded to the Poder |

Server flag: `-public-url <url>` — reachable from the VMs (passed to the Poder as `API_URL`).

---

## End-to-end example

```bash
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1
export AWS_IAM_INSTANCE_PROFILE=sandrpod-vm-ssm
export SANDRPOD_VM_SECURITY_GROUP=sg-0abc123
export SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
export SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0

go run ./cmd/server -port 8080 \
  -public-url https://api.example.com \
  -db sqlite:./data/sandrpod.db -token "$SANDRPOD_TOKEN"

# Then create a sandbox on AWS:
sandrpod-cli create my-box --provider-type aws \
  --region us-east-1 --instance-type t3.medium
```

The first such request launches a VM (default AMI: latest Ubuntu 22.04 from
Canonical) and may take a few minutes (VM boot + Docker install + image pull).

---

## Running the server as a systemd service

A production deployment (the server on EC2, public on port 80) typically uses a
unit plus an environment drop-in so the AWS settings live in a root-only file:

`/etc/systemd/system/sandrpod-server.service`

```ini
[Unit]
Description=SandrPod API Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/opt/sandrpod
ExecStart=/opt/sandrpod/sandrpod-server -port 80 \
  -public-url http://<public-ip-or-dns> \
  -db sqlite:/opt/sandrpod/data/sandrpod.db -token <token>
Restart=always
RestartSec=3
# Lets a non-root unit bind port 80; harmless if you run as root.
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

`/etc/systemd/system/sandrpod-server.service.d/aws.conf` (chmod 600 — holds the
secret if you use static keys):

```ini
[Service]
Environment=AWS_REGION=ap-southeast-1
# Omit the two keys below if the server's EC2 instance has an IAM role.
Environment=AWS_ACCESS_KEY_ID=AKIA...
Environment=AWS_SECRET_ACCESS_KEY=...
Environment=AWS_IAM_INSTANCE_PROFILE=sandrpod-vm-ssm
Environment=SANDRPOD_VM_SUBNET_ID=subnet-0abc123
Environment=SANDRPOD_VM_SECURITY_GROUP=sg-0abc123
Environment=SANDRPOD_PODER_IMAGE=ghcr.io/sandrpod/poder:v0.4.0
Environment=SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:v0.4.0
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now sandrpod-server
sudo journalctl -u sandrpod-server -f      # watch provisioning
```

> Port 80 is plain HTTP and the token travels in the `Authorization` header —
> front it with a TLS reverse proxy (e.g. Caddy) or restrict the security group
> to known source IPs for anything beyond testing.

---

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| `RunInstances` denied | server creds missing `ec2:RunInstances` or **`iam:PassRole`** |
| `VPCIdNotSpecified: No default VPC` | account has no default VPC — set `SANDRPOD_VM_SUBNET_ID` |
| `no EC2 IMDS role found` (server) | server off-AWS or instance role not attached — set static `AWS_ACCESS_KEY_ID`/`SECRET` |
| `InvalidInstanceId: Instances not in a valid state` | SSM agent hasn't registered the just-booted VM yet — the provider retries this automatically for ~3 min; persistent failure means a missing instance profile / SSM agent |
| `websocket: bad handshake` (poder log) | the Poder reached the server but the handshake was rejected — usually a token mismatch (server has `-token`; ensure the scheduler forwards it, which it does automatically) |
| `poder registration timeout` while a Poder is **ONLINE** | the Poder registered under a different `(region, provider_type)` than requested — verify it shows the request's region (not the AZ) and `aws` |
| `No such image: …/toolbox:…` | the toolbox image isn't pullable on the VM — make the GHCR package **public** (or provide pull auth) and use a poder image new enough to pull on demand |
| `poder registration timeout` | API Server **not reachable** from the VM (`-public-url` wrong / behind NAT) |
| Docker install / image pull fails on VM | no outbound internet (no public IP and no NAT), or SG blocks 443 |

---

## Known limitations

- **No autoscaling.** Idle-VM reclamation is opt-in (`SANDRPOD_PODER_IDLE_TIMEOUT`; see [UPGRADING.md](UPGRADING.md)). One VM per "region has no Poder".
  Clean up VMs you no longer need (delete the sandbox/Poder; `Cleanup` terminates
  VMs tagged `sandrpod=true`).
- **SSM-only bootstrap.** Requires the instance profile and a working SSM agent.
- **Default AMI is Ubuntu 22.04 (Canonical).** Override per-request with
  `--image` / `ImageID`.
- The **Aliyun** provider follows the same shape and is likewise live-validated —
  see [ALIYUN_PROVISIONING.md](ALIYUN_PROVISIONING.md).
