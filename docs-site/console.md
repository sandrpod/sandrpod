---
title: Console quickstart
description: The hosted console — sign up, get a workspace, and run a sandbox in a browser.
---

# Console quickstart

[docs.sandrpod.com](https://docs.sandrpod.com) documents the engine. This section
documents the **hosted console** — the build with accounts, workspaces, wallets
and a browser terminal, at [cloud.sandrpod.com](https://cloud.sandrpod.com).

The engine underneath is the open-source one. Anything in
[CLI](/cli) or [REST API](/api) works against the console too; this section covers
what the console adds.

## 1. An account and a workspace

Signing up creates two things at once: an account, and a **workspace** — the
container for sandboxes, members, keys and money. Most people never need a second
one; a second workspace is how you separate two projects (or two customers) that
should not share a wallet or an audit trail.

New workspaces start on the **trial** plan with **¥5 of credit**, which is a few
hours of a small sandbox. Enough to finish this page, not enough to run a
business by accident.

## 2. A key

Every non-browser client needs one. Two spellings of the same credential:

| Key | Used by |
|---|---|
| `sk_…` | the CLI, `curl`, the Python client |
| `e2b_…` | the unmodified E2B SDK (it validates the prefix client-side) |

Create one in **Settings → API keys**. It is shown once; the list keeps only a
prefix afterwards. See [API keys](/console-keys).

## 3. A sandbox

The portal's **Sandboxes → New** creates one and opens its page:

- **Terminal** — a real PTY in the browser
- **Files** — browse, upload, download
- **Logs** — the container's output
- **Metrics** — live CPU and memory for this sandbox
- **Execute** — one-shot commands with their exit code

## 4. The same thing from a terminal

```bash
pip install sandrpod-cli
export SANDRPOD_API_URL=https://cloud.sandrpod.com
export SANDRPOD_API_TOKEN=sk_...

sandrpod-cli create demo --provider local
sandrpod-cli execute demo "python3 --version"
sandrpod-cli delete demo
```

## What the console adds over the engine

| | |
|---|---|
| Accounts | email verification, password reset, sessions with per-device revocation |
| Workspaces | two roles, invitations, switching, deletion with an audit trail |
| Money | prepaid wallet, top-ups, refunds, low-balance and arrears warnings |
| Audit | every lifecycle event, 180 days |
| Workers | join tokens, so a customer can bring their own machines |
| Support | `support@gatemux.ai` |

## Next

- [Workspaces and roles](/console-workspaces)
- [API keys](/console-keys)
- [Billing and usage](/console-billing)
- [Limits](/console-limits)
