---
title: Your first sandbox
description: Create, run, inspect, and delete a sandbox, with the pieces named.
---

# Your first sandbox

A sandbox is one container with a toolbox inside it: an HTTP service that runs
code, keeps a PTY, and owns the filesystem under `/workspace`. Creating one is
four steps, and each is visible if something goes wrong.

## Create

```bash
sandrpod-cli create demo --provider local
```

Under the hood:

```
POST /api/v1/sandboxes {"name":"demo","provider_type":"local"}
  → scheduler picks a worker (region, provider, capabilities, tenant)
  → a sandbox record is written PENDING
  → a job is recorded  ────────────────────►  this is what makes the create
  → the worker is told to create the container   recoverable if a process dies
  → container up, toolbox answering, record RUNNING
```

States: `PENDING → STARTING → RUNNING → STOPPING → STOPPED`, plus `ERROR`
(creation failed) and `TERMINATED`.

## Run code

```bash
sandrpod-cli execute demo "python3 -c 'print(2**10)'"
sandrpod-cli stream  demo "for i in 1 2 3; do echo \$i; sleep 1; done"
sandrpod-cli shell   demo          # interactive PTY, Ctrl-] to leave
```

`execute` is stateless: each call is a fresh process in the same filesystem.
`stream` keeps the output flowing as it is produced.
`shell` is a real PTY, so prompts, colours and `Ctrl-C` behave.

## Files

```bash
sandrpod-cli fs mkdir demo /workspace/data
sandrpod-cli fs write demo /workspace/data/hello.txt "written from outside"
sandrpod-cli fs ls    demo /workspace/data
sandrpod-cli fs cat   demo /workspace/data/hello.txt
sandrpod-cli fs upload   demo ./local.bin /workspace/local.bin
sandrpod-cli fs download demo /workspace/local.bin ./back.bin
```

## Inspect

```bash
sandrpod-cli get   demo      # state, worker, IP, architecture
sandrpod-cli env   demo      # arch, OS, shell inside the sandbox
sandrpod-cli stats demo      # live CPU / memory / disk for this sandbox
sandrpod-cli logs  demo --tail 50
```

## Stop, start, delete

```bash
sandrpod-cli stop demo       # container paused; the record and filesystem stay
sandrpod-cli start demo      # back to RUNNING
sandrpod-cli delete demo     # container and record removed
```

A stopped sandbox still counts against the worker's container limit until it is
deleted — stopping is for a pause, not for cleanup.

## Snapshot

```bash
sandrpod-cli snapshot demo --image registry.example.com/tuned-agent:2026-10
```

Commits the container as an image, so a prepared environment (dependencies,
checked-out repos, warmed caches) becomes the starting point for the next
sandbox. Requires Docker on the worker, which the local provider already has.

## Next

- [Egress policy](/egress) — decide what the sandbox may reach.
- [Workers and tunnels](/workers) — what the scheduler chose, and why.
- [CLI](/cli) — the full command surface.