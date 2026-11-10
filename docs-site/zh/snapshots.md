# Snapshots and forks

> A snapshot freezes a sandbox — its `/workspace` data and its image layers — and a
> fork starts a new sandbox from it.

## Create one

```bash
curl -s -X POST "$API/api/v1/sandboxes/box/snapshot?image=sandrpod-snapshot/box:v1" \
  -H "Authorization: Bearer $TOKEN"
# → {"image":"registry.example.com/sandrpod-snapshots/box:v1","sandbox":"box"}
```

In the console: the row's ⋯ menu → **快照成镜像**. List them with the **快照** button,
fork with **从快照创建** in the create drawer.

## What a snapshot contains

- the **image layers** (what `docker commit` captures: installed packages, files
  outside `/workspace`)
- the **`/workspace` volume**, which `docker commit` does *not* capture — the engine
  copies it when the fork is created

## Where it lives

With `SANDRPOD_REGISTRY` set on the workers, a snapshot is tagged
`<registry>/sandrpod-snapshots/<sandbox>:<tag>` and **pushed**, so any worker can fork
from it. Without a registry it stays on the worker that committed it and a fork only
works there — the console pins the node in that case.

The platform's own deployment runs a small `registry:2` behind Caddy at
`registry.sandbox.sandrpod.com`; see [deploy](deploy.md) for the wiring.

## Names

The default tag is `latest`, so two snapshots of the same sandbox on the same worker
replace each other. Pass a distinct tag (a timestamp, a label) when the history
matters.

## Deleting

```bash
curl -s -X DELETE "$API/api/v1/snapshots?image=sandrpod-snapshot/box:v1" \
  -H "Authorization: Bearer $TOKEN"
# → {"removed":1}
```

Deletes every worker's copy of that tag. The worker refuses an image it did not
commit, and docker refuses one still in use.
