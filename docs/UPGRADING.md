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
