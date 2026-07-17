# Authentication & API Keys

SandrPod has **one token system**, shared by the native REST API and the
E2B-compatible gateway. A token that authenticates on one authenticates on the
other — they go through the same resolver. This doc covers the three ways to
provision tokens, how they combine, and how a token is used as a drop-in
`E2B_API_KEY`.

## How a request authenticates

A caller presents a token; it resolves to an **identity** (`name` + `role`).

| Surface | Header the caller sends |
|---------|-------------------------|
| Native REST API | `X-Sandrpod-Token: <token>` or `Authorization: Bearer <token>` |
| E2B gateway (control plane) | `X-API-KEY: <token>` (the E2B SDK sets this from `E2B_API_KEY`) |
| E2B gateway (envd/code) | `Authorization: Bearer <per-sandbox envd token>` (issued automatically) |

Resolution order (first match wins), in `resolveToken`:

1. the single **`-token`** (legacy admin), then
2. **tokens-file** entries + hot-reload registry, then
3. **DB-issued** keys (matched by hash via an in-memory index).

If **none** of these are configured, auth is **disabled** and every request runs
as an anonymous admin (legacy dev behaviour). Issuing even one DB key counts as
"configured", so a key-only server is not left open.

## The three ways to provision tokens

### 1. Single admin token — `-token` (simplest)

```bash
sandrpod-server -token "$(openssl rand -hex 24)" ...
# env: SANDRPOD_TOKEN
```

- One shared secret, role **admin** (identity name `admin`), full access.
- Best for a single operator / quick start. No per-client identity, no revocation
  short of rotating the token.

### 2. Static named tokens file — `-tokens-file` (declarative, hot-reloaded)

A JSON file of named tokens, re-read every 10 s (revocations apply without a
restart). Adds to `-token`.

```json
[
  { "name": "alice",  "token": "e2b_1a2b…", "role": "user"  },
  { "name": "ops-ci", "token": "s3cr3t…",   "role": "admin" }
]
```

```bash
sandrpod-server -token <admin> -tokens-file /etc/sandrpod/tokens.json ...
# env: SANDRPOD_TOKENS_FILE ; role defaults to "user"; also accepts {"tokens": [...]}
```

- Per-client identities + roles, revoke by editing the file.
- Tokens are stored **in plaintext** — protect the file (chmod 600).
- Good for a small, hand-managed set. No API to mint keys.

### 3. DB-backed issuance API — `/api/v1/tokens` (dynamic, persisted) — recommended

Admin endpoints that mint / list / revoke tokens persisted in the store
(requires a persistent `-db` backend — `sqlite:<path>` or `postgres://…`).
Issued keys use E2B's canonical `e2b_<hex>`
shape, so they double as a drop-in `E2B_API_KEY`. **Only the SHA-256 hash is
stored** — the raw key is returned once at creation and is never retrievable.

```bash
# Issue (admin). role: "user" (default) | "admin"
curl -X POST https://api.example.com/api/v1/tokens \
  -H "X-Sandrpod-Token: <admin>" \
  -d '{"name":"customer-A","role":"user"}'
# → {"name":"customer-A","role":"user","prefix":"e2b_1a2b3c4d5e6f",
#    "key":"e2b_1a2b3c4d5e6f…","created_at":"…"}      # key shown ONCE

# List (no raw keys — only name / prefix / role / created_at)
curl https://api.example.com/api/v1/tokens -H "X-Sandrpod-Token: <admin>"

# Revoke by display prefix (takes effect immediately)
curl -X DELETE https://api.example.com/api/v1/tokens/e2b_1a2b3c4d5e6f \
  -H "X-Sandrpod-Token: <admin>"
```

Or via the CLI (`SANDRPOD_API_URL` + an admin `SANDRPOD_API_TOKEN`):

```bash
sandrpod-cli token create customer-A --role user   # prints the key ONCE
sandrpod-cli token list                            # prefix / name / role / created
sandrpod-cli token rm e2b_1a2b3c4d5e6f             # revoke by prefix
```

- Self-serve issuance + revocation over the API; survives restart (loaded into
  the in-memory auth index at startup, so the hot path never hits the DB).
- Hash-only storage: a leaked database yields no usable keys.
- Best for multi-client / programmatic key management. Needs a persistent
  backend — SQLite or PostgreSQL (with the in-memory store, issued keys are
  ephemeral and lost on restart). On PostgreSQL, revocation propagates to all
  instances instantly via `LISTEN/NOTIFY`.

### Auth disabled (no credentials) — dev only

Start with no `-token`, no `-tokens-file`, no DB keys → auth is off and
everything runs as an anonymous admin. Never do this on a reachable server.

## Roles, ownership & quota

| Role | Can do |
|------|--------|
| `admin` | Everything: infra endpoints (poders, jobs), all sandboxes, token issuance |
| `user`  | The sandbox API only, scoped to sandboxes **it created** (`owner` = token name) |

- `-max-sandboxes-per-owner N` caps concurrent sandboxes per **user** token
  (admins exempt). Env: `SANDRPOD_MAX_SANDBOXES_PER_OWNER`.
- `-rate-limit R` throttles requests/second per **user** token (admins exempt).
  Env: `SANDRPOD_RATE_LIMIT`.
- Ownerless records (created before multi-token auth, or with auth off) stay
  visible to everyone, for upgrade smoothness.

## Using a token as an E2B client

The token **is** the `E2B_API_KEY`. Point the SDK at your domain with
`E2B_DOMAIN` (production, TLS) — nothing else.

```bash
# Key minted via /api/v1/tokens is already e2b_<hex>, so no extra client env:
E2B_DOMAIN=sandrpod.example.com  E2B_API_KEY=e2b_1a2b3c4d…  python your_app.py
```

- **`e2b_<hex>`-shaped** token (what the issuance API produces) → works as-is.
- A token that is **not** `e2b_`-shaped (e.g. a hand-picked `-token` string) →
  the client must also set `E2B_VALIDATE_API_KEY=false` to skip the SDK's
  client-side format check. Prefer issuing `e2b_` keys to avoid this.
- HTTP-only debug harness (no domain/TLS): set `E2B_API_URL` + `E2B_SANDBOX_URL`
  to the debug port and `E2B_VALIDATE_API_KEY=false`. See
  [E2B_COMPAT.md](E2B_COMPAT.md).

## Which should I use?

| Need | Use |
|------|-----|
| Quick start, one operator | `-token` |
| A few hand-managed clients, no DB | `-tokens-file` |
| Many clients / programmatic issuance / revocation, persisted | **DB issuance API** (`-db sqlite:` + `/api/v1/tokens`) |
| Local dev, no security | no credentials (auth off) |

They compose freely: e.g. an admin `-token` for operations **plus** DB-issued
`user` keys for customers is the typical production setup.

## Security notes

- **Storage**: `-token` and `-tokens-file` hold **plaintext** secrets — protect
  the flag/env/file. DB-issued keys store **only a SHA-256 hash**; the raw key is
  shown once and cannot be recovered (rotate by revoke + re-issue).
- **Comparison**: `-token` and tokens-file use constant-time comparison; DB keys
  are looked up by hash (preimage resistance means the map lookup leaks nothing
  usable).
- **Transport**: always terminate TLS in front of the API in production (built-in
  `-tls-cert`/`-tls-key`, or the Caddy wildcard-TLS walkthrough in
  [MULTI_INSTANCE_DEPLOYMENT.md](MULTI_INSTANCE_DEPLOYMENT.md) Part 4, referenced
  from [E2B_COMPAT.md](E2B_COMPAT.md) §2); tokens are bearer credentials.
- **Blast radius**: give customers `user` keys, keep `admin` for operators. Use
  `-rate-limit` and `-max-sandboxes-per-owner` to bound abuse.
