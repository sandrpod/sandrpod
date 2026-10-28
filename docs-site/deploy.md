---
title: Self-host in production
description: The deployment shape to copy when SandrPod is in front of real traffic.
---

# Self-host in production

One control plane, PostgreSQL, a real hostname, TLS, backups and alerts. Everything
below was run start to finish; the measurements are from a live deployment.

## What you need

| Piece | Sizing that works |
|---|---|
| control plane | 2 vCPU / 4 GB — the process idles at ~60 MB |
| database | PostgreSQL 14+, TLS on; managed is worth it for backups and PITR |
| worker(s) | 4 vCPU / 8 GB / 100 GB per host; sandboxes are the load, not the control plane |
| TLS | a reverse proxy that can obtain certificates (Caddy does it automatically) |
| DNS | one name for the API/console, optionally a wildcard for the E2B surface |

## Shape

```
                ┌──────────────┐
   internet ───►│  TLS proxy   │  Caddy / nginx / cloud LB
                └──────┬───────┘
                       ▼ 127.0.0.1:8090
                ┌──────────────┐
                │control plane │
                └──────┬───────┘
                       ▼ PostgreSQL (TLS)
                ┌──────────────┐        ┌──────────────┐
                │   worker     │  …     │   worker     │
                └──────────────┘        └──────────────┘
```

Bind the control plane to loopback and let the proxy own the public port. Nothing
else needs to be reachable.

## Environment that matters

```bash
SANDRPOD_TOKEN=…                  # operator credential, 32 hex chars
SANDRPOD_PUBLIC_URL=https://cloud.example.com
CLOUD_DB=postgres://user:pass@host:5432/db?sslmode=require
CLOUD_REGISTRATION=open           # or invite / closed
CLOUD_BOOTSTRAP_ADMIN=admin@example.com
CLOUD_BOOTSTRAP_PASSWORD=…        # applied only while no account exists
CLOUD_SMTP_HOST=…                 # without it, mail prints to the log
```

`SANDRPOD_PUBLIC_URL` is not cosmetic: mail links and payment return URLs are
built from it, and a wrong value fails at the worst moment (when a customer clicks
"reset password").

## Three things to prove before the first customer

1. **A backup restores.** A backup nobody restored is a hypothesis.
   ```bash
   pg_dump "$DSN" -Fc -f /backup/drill.dump
   psql "$DSN" -c "CREATE DATABASE drill"
   pg_restore -d "${DSN%/*}/drill" --no-owner /backup/drill.dump
   # start a second instance against it, sign in with a production password,
   # compare the wallet/audit numbers, then drop the database
   ```
2. **The alerts fire.** Stop the service on purpose and confirm a mail arrives —
   the log line saying it was sent is not the same fact.
3. **`/healthz` says ok** with a worker online:
   ```bash
   curl -s https://cloud.example.com/healthz | jq
   ```

## Maintenance that is not optional

| Task | Cadence |
|---|---|
| database dump + verify | nightly (`pg_dump -Fc`, then `pg_restore -l`) |
| restore drill | quarterly, and after any schema change |
| image and container prune on workers | weekly — disk is the resource that fills first |
| dependency and base-image updates | monthly |
| certificate renewal | automatic with Caddy or ACME; check it once a quarter |

## Where this connects

- [More than one instance](/multi-instance) — when one control plane is not enough.
- [Upgrading](/upgrading) — the order, and why rollback is a tag change.
- [Troubleshooting](/troubleshooting) — the failures this shape actually produces.

A reference deployment with every command and its output is in
[PRODUCTION_DEPLOYMENT.md](https://github.com/sandrpod/sandrpod/blob/main/docs/PRODUCTION_DEPLOYMENT.md).