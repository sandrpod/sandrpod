# Production deployment

The reference deployment: one host, PostgreSQL, a wildcard TLS certificate, and
the E2B-compatible surface turned on. This is the shape to copy when you put
SandrPod in front of real traffic.

Everything below was run start to finish on a fresh CentOS Stream 10 VM and
verified with the **unmodified** `e2b` 2.35.0 and `e2b-code-interpreter` 2.9.0
SDKs — 50 checks, 48 passing (§7). Command output is verbatim from that run with
the domain replaced by `example.com`.

Scaling past one host: [MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md).
Capacity model: [SCALING.md](SCALING.md). Upgrades:
[UPGRADING.md](UPGRADING.md).

---

## What you need

- A host with a public IP and ports 80/443 free. The reference used 8 vCPU /
  15 GB / 50 GB, which is generous for a first deployment — sandboxes are the
  thing that consumes it, not the four service containers.
- A domain whose nameservers are at a provider with an API.
  [lego](https://go-acme.github.io/lego/dns/) supports ~150 of them; you need
  credentials for yours.
- Nothing else. Docker is step 1.

Files referenced here live in the repo:
[`docker/docker-compose.prod.yml`](../docker/docker-compose.prod.yml) and
[`docker/Caddyfile`](../docker/Caddyfile).

---

## 1. Docker

```bash
dnf -y install dnf-plugins-core
curl -fsSL https://download.docker.com/linux/centos/docker-ce.repo \
  -o /etc/yum.repos.d/docker-ce.repo
dnf -y install docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker
```

Two failures are common on minimal cloud images and cost time if you have not
seen them:

**`Extension addrtype revision 0 not supported, missing kernel module?`** —
`xt_addrtype` lives in `kernel-modules-extra`, and on a rolling distro `dnf`
often cannot find a build matching the *running* kernel. Rather than chase
kernel packages, use Docker's nftables firewall backend, which does not need it:

```bash
cat > /etc/docker/daemon.json <<'JSON'
{ "firewall-backend": "nftables" }
JSON
```

**`IPv4 forwarding is disabled`** — the bridge network needs it:

```bash
echo 'net.ipv4.ip_forward = 1' > /etc/sysctl.d/99-docker.conf
sysctl -p /etc/sysctl.d/99-docker.conf
systemctl restart docker
```

Confirm, and confirm containers can reach the network — sandboxes install
packages:

```
$ docker version --format 'server={{.Server.Version}} api={{.Server.APIVersion}}'
server=29.6.2 api=1.55
$ docker run --rm alpine:3.20 wget -qO- -T5 https://example.com >/dev/null && echo ok
ok
```

---

## 2. DNS: two records, one of them a wildcard

```
api.example.com   A   203.0.113.10
*.example.com     A   203.0.113.10
```

**The wildcard is not optional.** The E2B surface addresses a service inside a
sandbox as `<port>-<sandboxID>.<domain>`, and the sandbox ID does not exist
until the sandbox does. You cannot enumerate those names ahead of time, so you
cannot create records — or certificates — for them ahead of time.

Verify from the host, and verify a name that does not exist yet. That is the one
that matters:

```
$ getent ahostsv4 8000-abc.example.com | head -1
203.0.113.10
```

If you skip this step the symptom is confusing rather than obvious: the control
plane works, sandboxes get created, and every call that touches a sandbox host
fails to resolve.

---

## 3. A wildcard certificate

Let's Encrypt issues wildcards **only** through the DNS-01 challenge, so the
ACME client needs API credentials for your DNS provider — not just port 80.

```bash
export <PROVIDER>_SECRET_ID=... <PROVIDER>_SECRET_KEY=...     # see lego's docs
lego --email you@example.com --accept-tos \
     --dns <provider> --dns.propagation-wait 90s \
     -d example.com -d '*.example.com' \
     --path /etc/lego run
```

Set `--dns.propagation-wait` generously. lego polls the authoritative
nameservers for the TXT record it just wrote; ~100 s per domain is normal.

```
$ openssl x509 -in /etc/lego/certificates/example.com.crt -noout -ext subjectAltName
    DNS:*.example.com, DNS:example.com
```

lego names the bundle after the **first** `-d`, not after the wildcard — so with
the invocation above the files are `example.com.crt` / `example.com.key`.

### Renewal

A daily timer that no-ops until fewer than 30 days remain, and reloads the proxy
when it does renew:

```ini
# /etc/systemd/system/lego-renew.service
[Unit]
Description=Renew the wildcard certificate
After=network-online.target

[Service]
Type=oneshot
# DNS-provider credentials plus SANDRPOD_DOMAIN / ACME_EMAIL
EnvironmentFile=/etc/sandrpod/renew.env
ExecStart=/usr/local/bin/lego --email ${ACME_EMAIL} --accept-tos \
  --dns <provider> --dns.propagation-wait 90s \
  -d ${SANDRPOD_DOMAIN} -d *.${SANDRPOD_DOMAIN} \
  --path /etc/lego renew --days 30
ExecStartPost=-/usr/bin/docker kill --signal=SIGUSR1 sandrpod-caddy
```

```ini
# /etc/systemd/system/lego-renew.timer
[Timer]
OnCalendar=daily
RandomizedDelaySec=6h
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl daemon-reload && systemctl enable --now lego-renew.timer
```

---

## 4. The stack

```bash
export SANDRPOD_DOMAIN=example.com
umask 077
cat > .env <<ENV
SANDRPOD_DOMAIN=$SANDRPOD_DOMAIN
SANDRPOD_TOKEN=$(openssl rand -hex 32)
POSTGRES_PASSWORD=$(openssl rand -hex 16)
ENV

docker compose -f docker/docker-compose.prod.yml up -d --wait
```

```
 Container sandrpod-postgres  Healthy
 Container sandrpod-server    Healthy
 Container sandrpod-poder     Healthy
 Container sandrpod-caddy     Healthy
```

Four settings in that file are load-bearing. Each has a failure mode worth
recognising.

| Setting | Why |
|---|---|
| `SANDRPOD_TOKEN: ${SANDRPOD_TOKEN:?}` | An empty token means every request runs as an anonymous admin. `:?` makes Compose refuse to start rather than boot wrong. |
| `SANDRPOD_NETWORK: sandrpod` + `name: sandrpod` on the network | The worker hands that string to the Docker API when creating sandbox containers, so it must be the network's **real** name — and Compose prefixes network names with the project name unless pinned. Get it wrong and sandboxes are created successfully, land on another network, and every exec hangs until it times out. |
| `REGION: local` | The E2B gateway schedules the local substrate as `provider=local` / `region=local`. A worker registered under a datacentre name is filtered out by the scheduler: `Sandbox.create()` returns `500: no available local poder found` while `/api/v1/poders` shows it ONLINE. Set `SANDRPOD_E2B_PROVIDER` instead to provision real cloud VMs. |
| `ports: ["127.0.0.1:8080:8080"]` | See §5. |

### The reverse proxy

```caddyfile
:443 {
	tls /certs/{$SANDRPOD_DOMAIN}.crt /certs/{$SANDRPOD_DOMAIN}.key
	reverse_proxy sandrpod-server:8080 {
		flush_interval -1
	}
}
```

One `:443` block serves every hostname: a wildcard certificate is valid for all
of them and the backend routes on `Host` itself, so there is nothing to
enumerate.

**Caddy forwards the original `Host` header by default, and that header is the
routing mechanism** — it is how the backend distinguishes the control plane from
"port 8000 of sandbox X". On nginx you must write `proxy_set_header Host $host;`
explicitly, or every sandbox URL silently stops resolving to a sandbox.

`flush_interval -1` disables response buffering. Without it, streamed command
output and incremental `run_code` results arrive as one blob at the end.

---

## 5. The admin API moves to loopback

Once `SANDRPOD_E2B_DOMAIN` is set, **every** host under that domain belongs to
the E2B gateway — including `api.<domain>`. The native API returns 404 there:

```
$ curl -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $SANDRPOD_TOKEN" \
    https://api.example.com/api/v1/poders
404
```

So the production compose binds it to `127.0.0.1:8080` and you reach it from the
host or over SSH:

```bash
ssh -L 8080:127.0.0.1:8080 root@<host>
```

The side effect is desirable: the admin surface is not on the internet at all.

Issue an API key — the E2B SDK validates the shape client-side, so keys are
`e2b_<hex>` and drop in as `E2B_API_KEY`. Only the hash is stored; the bare key
is shown once. See [AUTH_AND_KEYS.md](AUTH_AND_KEYS.md).

```bash
curl -s -X POST -H "Authorization: Bearer $SANDRPOD_TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"demo","role":"user"}' \
  http://127.0.0.1:8080/api/v1/tokens
```

```json
{"key":"e2b_…","prefix":"e2b_…","name":"demo","role":"user"}
```

---

## 6. Point a client at it

Two environment variables, no code change:

```bash
pip install e2b e2b-code-interpreter     # official packages, unmodified
export E2B_DOMAIN=example.com
export E2B_API_KEY=e2b_…
```

```python
from e2b import Sandbox

sbx = Sandbox.create()
print(sbx.sandbox_id, sbx.is_running())
print(sbx.commands.run("echo hello from $(hostname)").stdout)
sbx.kill()
```

Protocol detail, config, and the debug (no-DNS, no-TLS) alternative:
[E2B_COMPAT.md](E2B_COMPAT.md).

---

## 7. Verify

Two sweeps are the acceptance test for a deployment. Both are in the repo's
companion material and take about a minute.

| Group | Result |
|---|---|
| Lifecycle — create, connect, list, metadata filter, timeout, pause/resume, kill | 11/11 |
| Filesystem — read/write, batch write, rename, remove, `watch_dir` | 11/11 |
| Commands — foreground, background pid, streaming, stdin, kill | 7/7 |
| PTY — create/send/resize/kill | 1/1 |
| Metrics | 1/1 |
| In-sandbox port preview | 1/1 |
| Code interpreter — `run_code`, stateful kernel, contexts, matplotlib charts | 14/14 |
| Newer SDK surface — `create_snapshot`, `fork` | 2/4 (both 405, not implemented) |

**50 checks, 48 passing.**

Latency measured from a laptop over the public internet, so these include real
round-trip time:

| | median |
|---|---|
| `Sandbox.create()` → usable | 2.7 s |
| `commands.run` round trip | 217 ms |
| `run_code` round trip | 194 ms |

The first `create` after a cold image pull is much slower — budget ~20 s once.

### Not implemented

| Call | Status |
|---|---|
| `create_snapshot()` | `405` |
| `fork()` | `405` |
| `envVars` on create | accepted, not yet exercised by the real SDK |

---

## 8. In-sandbox ports are public by default

`get_host(port)` URLs are fetchable with **no credential**, matching E2B:
possession of the unguessable `<port>-<sandboxID>.<domain>` hostname is the
capability. A browser cannot attach an API key, so requiring one would not make
the common case — open the dev server running in your sandbox, point a webhook
at it — inconvenient; it would make it impossible.

```
$ curl https://8000-<sandboxID>.example.com/index.html
<h1>served from the sandbox</h1>
```

The envd RPC surface on the same wildcard domain is **not** part of that and
authenticates regardless:

```
$ curl -o /dev/null -w '%{http_code}\n' https://49983-<sandboxID>.example.com/files
401
```

If every consumer in your deployment is a program that can carry a key, set
`SANDRPOD_E2B_PRIVATE_PORTS=1` on the server and the preview URLs require one
too.

---

## 9. Operating it

**Backup.** PostgreSQL holds sandboxes, jobs, workers, and API-token hashes.
Back that up and everything else rebuilds from the Compose file and the
certificate.

**Reclamation.** Sandboxes are reclaimed on the idle TTL set per sandbox
(`set_timeout`, or `-ttl` at create time). Nothing else accumulates.

**Certificates.** The timer in §3. After a renewal, check that the proxy picked
it up: `openssl s_client -connect api.<domain>:443 -servername api.<domain>`.

**Adding capacity.** Run another worker — anywhere. Each `poder` dials the
control plane over an outbound tunnel, so a second machine (another cloud,
another region, a box behind NAT) joins with no inbound firewall changes:

```bash
docker run -d --restart=unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=https://api.example.com -e SANDRPOD_TOKEN=… -e REGION=local \
  ghcr.io/sandrpod/poder:latest
```

Multiple *server* instances behind a load balancer is a different exercise —
[MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md).

---

## Troubleshooting

| Symptom | Cause |
|---|---|
| Sandbox creates, every exec hangs | `SANDRPOD_NETWORK` does not match the real Docker network name (Compose project prefix). Check `docker network ls`. |
| `500: no available local poder found`, worker shows ONLINE | Worker's `REGION` is not `local`. |
| Sandbox hostnames do not resolve | Missing wildcard A record. Test a name that does not exist: `getent ahostsv4 8000-abc.<domain>`. |
| TLS works on `api.<domain>`, fails on sandbox hosts | Certificate lacks the wildcard SAN, or the proxy is serving a per-host cert. |
| Everything 404s on `api.<domain>/api/v1/*` | Expected — that host is the E2B gateway. Use the loopback port (§5). |
| Streamed output arrives all at once | Reverse proxy is buffering. `flush_interval -1` on Caddy; `proxy_buffering off` on nginx. |
| Sandbox routing broke after switching to nginx | `Host` header not forwarded. `proxy_set_header Host $host;`. |
