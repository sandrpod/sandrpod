---
title: Troubleshooting
description: The failures this system actually produces, and what each one means.
---

# Troubleshooting

Every entry here is a real failure with its cause, ordered by how often it is
mistaken for something else.

## `no available local poder found`

A create could not find a worker. The message names `local` because that is the
provider, not because the fleet is broken.

| Cause | Check |
|---|---|
| No worker has registered | `sandrpod-cli poder list` |
| **Region mismatch** — the client defaults to `local`, the worker registered as `hk` | `select id, region from poders` — the values must match |
| Worker at its container limit | `usage_containers` / `max_containers` on the worker row; delete stopped sandboxes |
| Worker offline after a restart | its heartbeat stopped; `docker logs sandrpod-worker` |
| Tenant mismatch | a join token minted for one workspace cannot host another's sandbox |

The last one is deliberate: refusing is better than co-locating two tenants.

## `sandbox tunnel not available` (503)

The sandbox exists, but the worker holding its tunnel is not connected to the
instance that received the request.

- Single instance: the worker is offline, or its tunnel dropped and is
  reconnecting (it retries every few seconds).
- Multiple instances: the owner claim is stale. Check that the owning instance has
  a non-empty `-node-url` and is receiving heartbeats; `tunnel_owners.updated_at`
  should be seconds old, not minutes.

## `worker capability unavailable` (409)

The request needs something no eligible worker advertises — an egress policy needs
a worker with the `egress` capability. Deploy such a worker, or drop the policy.

## A sandbox is stuck `PENDING`

The create never reached a worker. The job row says why:

```bash
sandrpod-cli job get <job-id>
```

`no available local poder found` → see above. Nothing at all → the process died
mid-delivery; the resumer picks it up within about a minute, and a job older than
that with no error is worth investigating.

## `create egress proxy container: No such image`

The worker does not have `ghcr.io/sandrpod/egress-proxy`. Pull it and recreate —
the failed attempt already wrote a record, so delete that sandbox first.

## A created sandbox comes back after being deleted

Deleting removes the record and the container; a create job that was still
`PENDING` used to be re-delivered by the resumer. Fixed in 0.7.1: a job whose
sandbox record is gone, or terminal, is failed instead of replayed. If you see it
on an older build, upgrade.

## `commands.run` returns 401 with the E2B SDK

The SDK does not send the API key to envd. It sends `envdAccessToken` from the
create response as `X-Access-Token`, and a hosting deployment must accept that
token, not only its own workspace keys. Fixed in 0.7.1; on older builds only
`create` and `kill` work.

## The E2B SDK refuses the key before sending anything

`Invalid API key format: expected "e2b_" followed by hex characters`. The key must
be minted with that spelling — same store, same revocation, different prefix. See
[E2B drop-in](/e2b#keys).

## An envd host answers `200` with a health document that is not envd's

The host is not routed to the E2B surface. Path-based mounting (`/sandboxes`) is
enough for create and kill and not for `<port>-<id>.<domain>`; the request falls
through to the control plane. Fixed in 0.7.1 for the hosted build; check your
reverse proxy passes the `Host` header unchanged.

## TLS fails on a self-signed deployment

The certificate is usually issued for one name. A worker or peer that connects by
IP while the certificate names a hostname fails verification even though the CA is
trusted. Connect by the name the certificate carries, or add the CA to the
process's trust store (`SSL_CERT_FILE`).

## Everything is slow, or the host stops answering

Look at memory before CPU. A `go build` on the same machine has taken whole hosts
down; so has a burst of sandboxes on a box that also serves the portal.

```bash
free -h; uptime; ps aux --sort=-%mem | head
```

The fix is almost always to stop co-locating: workers on their own machines, the
control plane small, the database managed. See
[Self-host in production](/deploy).

## Where to look

| Question | Command |
|---|---|
| Is the platform healthy? | `curl -s https://…/healthz \| jq` |
| What is the fleet doing? | `sandrpod-cli poder list` |
| What did this create do? | `sandrpod-cli job get <job-id>` |
| What is the container doing? | `sandrpod-cli logs <name> --tail 100` |
| What did the worker do? | `docker logs sandrpod-worker` |