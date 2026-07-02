# Upgrading SandrPod

How to upgrade an existing deployment (systemd binary + SQLite DB + env
drop-ins + already-running poders/VMs) in place. The general policy lives in
[ROADMAP.md](ROADMAP.md#upgrade-policy); the short version:

1. New behavior ships **opt-in or backward compatible**.
2. Schema changes are **additive and self-migrating** on startup.
3. **No poder protocol breaks** — already-deployed poder containers keep working.
4. Rollback = restore the previous binary; additive columns are ignored by
   older binaries, so a migrated SQLite DB still works after a rollback.

## Generic binary upgrade procedure

```bash
# 1. Build (or download) the new server binary
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/srv ./cmd/server

# 2. Copy to the host and VERIFY THE SIZE — a truncated scp produces a
#    SIGSEGV crash-loop that looks like a bad release.
scp /tmp/srv host:/tmp/srv-new
ssh host 'stat -c%s /tmp/srv-new'          # compare with the local size

# 3. Swap with a backup and restart
ssh host 'sudo systemctl stop sandrpod-server &&
          sudo cp -f /opt/sandrpod/sandrpod-server /opt/sandrpod/sandrpod-server.bak &&
          sudo mv /tmp/srv-new /opt/sandrpod/sandrpod-server &&
          sudo chmod +x /opt/sandrpod/sandrpod-server &&
          sudo systemctl start sandrpod-server'

# 4. Verify
ssh host 'systemctl is-active sandrpod-server &&
          sudo journalctl -u sandrpod-server -n 20 | grep "Registered providers"'
```

Rollback: `sudo systemctl stop sandrpod-server && sudo mv
/opt/sandrpod/sandrpod-server.bak /opt/sandrpod/sandrpod-server && sudo
systemctl start sandrpod-server`.

Poder containers on existing VMs are **not** touched by a server upgrade; they
reconnect automatically after the restart. New poder features require pushing a
new poder image and only apply to newly provisioned VMs (version skew is
expected and supported).

---

## Release notes by change

### Multi-tenancy slice: named tokens, ownership, quota

- **No action required.** The single `-token` keeps working (as an implicit
  admin token named `admin`); with no tokens configured, auth stays disabled.
- **SQLite migrates itself**: additive `owner` columns on `sandboxes` and
  `jobs` are added on first startup. Pre-existing records have no owner and
  remain visible to every authenticated caller.
- Opt in to named tokens with a JSON file and `-tokens-file` (or
  `SANDRPOD_TOKENS_FILE`):

  ```json
  [
    {"name": "alice", "token": "<random>", "role": "user"},
    {"name": "ops",   "token": "<random>", "role": "admin"}
  ]
  ```

  User-role tokens only see/manage their own sandboxes and cannot touch
  infrastructure endpoints (poder management, agent registration, job
  polling). Optionally cap user tokens with
  `-max-sandboxes-per-owner` / `SANDRPOD_MAX_SANDBOXES_PER_OWNER`.

### Async sandbox creation

- **No action required.** `POST /api/v1/sandboxes` stays synchronous unless
  the request carries `"async": true`; old CLIs/SDKs are unaffected.
- New: `GET /api/v1/jobs/{id}` returns job status/error for polling.
- The bundled CLI now requests async and polls to RUNNING by default
  (`--no-wait` to just get the job id). Against an **old server** the flag is
  ignored; if the connection drops mid-provision the CLI falls back to
  polling, so it works with both.

### Idle reclamation (cost safety)

- **Off by default** — enable explicitly:

  ```ini
  # /etc/systemd/system/sandrpod-server.service.d/reclaim.conf
  [Service]
  Environment=SANDRPOD_SANDBOX_IDLE_TIMEOUT=12h   # reap idle sandboxes
  Environment=SANDRPOD_PODER_IDLE_TIMEOUT=30m     # reclaim empty cloud poders (terminates the VM)
  ```

- Sandboxes created before the upgrade have no activity timestamp; their
  `created_at` is used as the baseline, so a long-forgotten sandbox is
  reaped on the first sweep after enabling. Direct-agent sandboxes (a user's
  own machine) are never reaped.

### Deleted-poder tombstones

- **No action required.** Deleting a poder now rejects its dying container's
  re-registration for 10 minutes, so the OFFLINE ghost records that needed a
  manual `poder delete --keep-vm` no longer appear. `keep_vm` deletions are
  exempt (a kept VM's poder legitimately reconnects).

### Auth hardening: hot-reload, rate limiting, TLS

- **Tokens file hot-reload** — no action required. If you use `-tokens-file`,
  edits are picked up within ~10 s (a bad edit keeps the previous set), so
  issuing/revoking a token no longer needs a restart.
- **Per-identity rate limiting** — off by default. Enable with
  `-rate-limit` / `SANDRPOD_RATE_LIMIT` (requests/second per user token;
  admins and poders exempt) → `429` when exceeded.
- **Built-in TLS** — off by default. Set `-tls-cert`/`-tls-key`
  (`SANDRPOD_TLS_CERT`/`KEY`) to serve HTTPS directly; the CLI/SDKs/console
  switch to `wss://` automatically. Plain HTTP behavior is unchanged when
  unset (front with a reverse proxy as before, or adopt this).

### Lifecycle webhooks

- **Off by default.** Set `SANDRPOD_WEBHOOK_URL` to receive fire-and-forget
  POSTs (`{event, time, data}`) for `sandbox.running|error|deleted|reaped`
  and `poder.registered|deleted|reclaimed`. Failures are logged and never
  affect the request flow.

### Preview URLs, snapshots, resource limits (need a fresh toolbox/poder image)

These features live in the **toolbox/poder image**, not the server binary, so
they only apply to sandboxes provisioned from an image built off this code:

- **Preview URLs** — `GET /api/v1/sandboxes/{name}/toolbox/proxy/{port}/...`
  (CLI `preview`) reverse-proxies to a service on `127.0.0.1:{port}` inside the
  sandbox. Sandboxes on an **older toolbox image return 404** for `/proxy` —
  rebuild/push the toolbox image and provision new sandboxes to get it.
- **Snapshots** — `POST /api/v1/sandboxes/{name}/snapshot` (CLI `snapshot`) does
  a `docker commit` on the poder; the poder must run this code.
- **Per-sandbox CPU/memory** — `cpu_cores`/`memory_mb` on create (CLI
  `--cpu`/`--memory`, SDK kwargs) apply to local/docker poders. Zero =
  unlimited, so old callers are unaffected; the poder must run this code to
  honor them.
- **To roll out:** `docker buildx ... -f docker/Dockerfile.toolbox` (and
  `Dockerfile.poder`), push, point `SANDRPOD_TOOLBOX_IMAGE` at the new tag.
  Existing sandboxes keep working on their current image; new ones pick up the
  features. No server change required.

### Interactive PTY shell

- **No action required** server-side (it proxies `/sandboxes/{name}/pty` over
  the existing tunnel). `sandrpod-cli shell <name>` needs the optional
  `websocket-client` dep: `pip install 'sandrpod-cli[shell]'`.

### SSH key persistence (DigitalOcean / Hetzner)

- **Opt-in, strongly recommended for these providers.** Set
  `SANDRPOD_SSH_KEY_DIR` to a writable dir (e.g. `/opt/sandrpod/ssh-keys`) so
  the per-VM ephemeral SSH key is persisted (0600) and survives a server
  restart. Without it, behavior is unchanged (memory-only): a restart orphans
  management of existing DO/Hetzner VMs. GCP is unaffected (metadata-injected).

### Observability: /metrics

- **No action required.** `GET /metrics` serves Prometheus text (admin-gated
  when auth is on; public like `/health` when auth is off). Point a scraper at
  it; see [SCALING.md](SCALING.md) for the signals to watch. CLI: `metrics`.

### Employee-PC mode: security fixes (rebuild the agent binary)

Applies only to `sandrpod-agent` deployments running `--permission-mode`.
These are **behavior changes in the local agent binary — rebuild and
redeploy `sandrpod-agent`** (and `sandrpod-tray`) to pick them up:

- **Path matching now folds case on macOS/Windows** — a hardlock on `~/.ssh`
  now correctly covers `~/.SSH/...` (case-insensitive filesystems). This
  *closes* a bypass; it only makes the gate stricter.
- **Rule paths are canonicalized/cleaned** — a trailing slash or symlinked
  home no longer makes a rule silently match nothing.
- **Command policy folds case on Windows** — `SCP.EXE` no longer slips past an
  `scp` deny.
- **First-run seeding** — `--permission-mode=strict|prompt` now seeds default
  hardlocks + command policy itself, so the gate is never a silent no-op just
  because `sandrpod-tray` was never launched. Existing `permissions.json`
  files are untouched (seeding only fires when empty).
- **Audit uploader** now commits its cursor only after a successful POST
  (at-least-once fix — no silent batch loss on a failed upload), and record
  errors are logged instead of swallowed.

### New client surfaces (no server action)

- CLI: `job get <id>` (inspect an async-create job) and `metrics`.
- Python `langchain-sandrpod`: `create_sandbox`/`sandbox()` accept
  `ttl_seconds`/`cpu_cores`/`memory_mb` (omitted when zero → old-server safe).
- New TypeScript SDK: `@sandrpod/sdk` (`pkg/sdk/typescript`).
- Web console at `/console` (static SPA; authenticates with a pasted token).
