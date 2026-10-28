---
title: Upgrading
description: The order, the compatibility promise, and why rollback is a tag change.
---

# Upgrading

## Order

1. **Workers first.** An older worker ignores the tenant field; a newer control
   plane refuses to place a tenant-scoped sandbox onto one — `409 worker
   capability unavailable` — rather than co-locating two tenants on one machine.
2. **Control plane.** Migrations are additive; the previous release still runs
   against the new schema.
3. **Agents** last. They only call the API.

Rolling in this order means no step requires downtime for the step before it.

## What "compatible" means

| Surface | Promise |
|---|---|
| Database schema | additive only (`ALTER TABLE … ADD COLUMN`); no renames, no drops. An older binary runs on a newer schema |
| HTTP API | fields are added, not removed. A client that ignores unknown fields keeps working |
| Go API (`pkg/server`, `pkg/sandpod`, `pkg/toolbox`) | semver; breaking changes are listed per release |
| Flags and environment variables | not removed within a major version |
| Python CLI and SDKs | server-version independent within a major version |

The one thing a rollback does not undo is a placement decision already made. If a
tenant-scoped sandbox was placed on a worker, moving it is an explicit action, not
a side effect of downgrading.

## Rollback

Point the image tag back. Because the schema only gains columns, there is no
migration to reverse.

```bash
docker rm -f sandrpod-cloud
docker run -d --name sandrpod-cloud … sandrpod-cloud:1.2.3   # the previous tag
```

## Before and after

```bash
# before: know what you are running
sandrpod-cli health
curl -s "$API/metrics" -H "X-Sandrpod-Token: $TOKEN" | grep sandrpod_version

# after: the same two answers, plus a real create
sandrpod-cli create canary --provider local && sandrpod-cli execute canary "echo ok"
sandrpod-cli delete canary
```

A create is the only health check that exercises the whole path — API, scheduler,
tunnel, container, toolbox — so it belongs in the deploy script, not in a
post-incident review.

## Per release

The breaking changes, who they affect, and how to migrate are recorded per release
in
[UPSTREAM_CHANGES.md](https://github.com/sandrpod/sandrpod/blob/main/docs/UPSTREAM_CHANGES.md).