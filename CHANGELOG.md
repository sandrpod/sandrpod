# Changelog

All notable changes to SandrPod are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added
- **Permission gate & decision audit pipeline** for employee-PC mode
  (`cmd/agent`). New packages: `pkg/permission` (5-branch decision engine
  with persisted rules + per-user IPC), `pkg/notify` (cross-platform
  consent prompters: osascript / zenity / kdialog / PowerShell MessageBox),
  `pkg/audit` (NDJSON local recorder + at-least-once HTTP batch
  uploader). New flags on `cmd/agent`:
  `--permission-mode=off|prompt|strict`, `--permission-file`,
  `--audit-dir`, `--audit-upload-url`, `--audit-upload-token`. See
  [docs/PERMISSION_AND_AUDIT.md](docs/PERMISSION_AND_AUDIT.md) for the
  full design.
- **`cmd/sandrpod-tray`** — new user-session GUI binary running a tray
  icon, an IPC consent server (`~/.sandrpod/authz.sock`), and a local-only
  HTTP settings page. Subcommands: `serve`, `unlock`, `lock`,
  `rules ls/add/rm`, `policy ls/deny/warn/rm`, `seed`. Cross-compiled for
  darwin/{amd64,arm64}, windows/{amd64,arm64} (mingw-w64),
  linux/{amd64,arm64} (Docker-built).
- **PTY session-level consent** — `ptyCreateHandler` now calls
  `mgr.CheckPTY()` before spawning the shell; denied requests return 403
  without forking the child.
- **Default hardlock seeds** — first run installs ~13 platform-aware
  hardlock rules (`~/.ssh`, `~/.aws`, browser profiles, Keychain, Mail,
  Messages, etc.) that can only be removed via an explicit
  `--i-understand-the-risk` CLI flag.
- **Default command policy** — built-in deny list
  (scp/rsync/nc/socat/launchctl/crontab/sudo/dd/mkfs/...) and warn list
  (curl/wget/osascript), tokenized at exec time.
- `Makefile` `build-all` now produces 6 agent binaries + 5 tray binaries
  with availability checks for `mingw-w64` and `docker`. New
  `tray-linux-amd64` / `tray-linux-arm64` targets build inside
  `golang:1.25` Linux containers, avoiding the need for a native
  GTK + libayatana-appindicator cross-toolchain on the build host.

### Changed
- `pkg/toolbox/files.go` — every file API now takes `context.Context` as
  its first parameter so the permission manager can attach a deadline
  and a sandbox-session id. HTTP handlers in `pkg/toolbox/api.go` were
  updated accordingly.
- `pkg/toolbox/Executor` gained `permMgr *permission.Manager` and a
  `resolveAndAuthorize()` chokepoint that runs the existing
  `resolveSafePath` blacklist AND (if installed) the new permission
  manager. Without a manager installed, behavior is unchanged.

### Security
- Removed `generateSandboxPassword` (used insecure `math/rand`); ID generation now uses `crypto/rand`
- Added Bearer token authentication to Toolbox HTTP server (`-token` / `TOOLBOX_TOKEN`)
- Added session ID and command ID validation to prevent shell injection via file paths
- Added path traversal guard (`resolveSafePath`) to all Toolbox file operations
- Shell-quoted dynamic values in scheduler `docker run` command to prevent argument injection
- Replaced `math/rand` with `crypto/rand` for all random string generation

### Fixed
- Data race in `PATCH /api/v1/jobs/{id}`: sandbox state is now updated via `sandboxStore.Update()` (under the sandbox store lock) instead of being mutated directly on the pointer returned by `Get()`
- Double `WriteHeader` call in `sandboxTunnel()` helper
- Dead `processAsyncHandler` route removed; dead `sessionLogsHandler` removed
- `cleanupOfflinePoders` goroutine now respects context cancellation on server shutdown
- `ExecWithPty` in Docker Poder: removed hardcoded `echo started && cat` shell command and all `fmt.Printf` debug output; now launches a clean `/bin/bash` PTY session

### Added
- `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout` on both the API Server and Toolbox HTTP servers
- `http.MaxBytesReader` (1 MiB) on all POST/PATCH request bodies in the API Server
- `-token` flag and `TOOLBOX_TOKEN` env var for Toolbox authentication
- `LICENSE` file (Apache 2.0)
- `CONTRIBUTING.md`, `Makefile`, `CHANGELOG.md`
- GitHub Actions CI workflow (`.github/workflows/ci.yml`)
- GitHub issue templates and PR template

---

## [0.3.0] — 2024-Q4

### Added
- SQLite persistence backend (`-db sqlite:<path>`) via Repository pattern
- `pkg/store/` with in-memory adapter and `pkg/store/sqlite/` SQLite implementation
- `sandrpod-agent`: registers local machine directly as a sandbox (no Docker required)
- `DELETE /api/v1/poders/{id}` endpoint
- `pkg/sandpod/repo.go` — Repository interfaces (`SandboxRepository`, `PoderRepository`, `JobRepository`)

### Changed
- `Scheduler` now depends on `store.PoderRepository` interface instead of concrete `*PoderStore`
- `cmd/server/main.go` accepts `-db` flag for persistence backend selection

---

## [0.2.0] — 2024-Q3

### Added
- WebSocket + yamux reverse tunnel architecture
- Poder worker node service (`cmd/poder`)
- Toolbox code execution service (`cmd/toolbox`)
- Session-based persistent shell execution
- PTY support via `creack/pty`
- Python SDK (`langchain-sandrpod`) and CLI (`sandrpod-cli`)

---

## [0.1.0] — 2024-Q2

### Added
- Initial API Server with in-memory sandbox and job stores
- Docker Poder implementation
- AWS and Aliyun provider integrations
