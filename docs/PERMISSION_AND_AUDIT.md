# Permission gate and audit pipeline

> 中文版 / Chinese version: [PERMISSION_AND_AUDIT.zh.md](PERMISSION_AND_AUDIT.zh.md)
> — full design notes, IPC protocol, and per-package walkthrough.

Opt-in. Off by default (`-permission-mode=off`), which is the right default for
a sandbox in a container. This document is about the other deployment: the
sandbox **is** an employee's laptop.

---

## The problem

Once an agent runs on someone's own machine rather than in a disposable
container, a blacklist of system directories stops being a security model.
`~/.ssh`, `~/Documents`, `~/Library/Messages` are all in the home directory —
none of them are `/etc` or `/usr`.

Three things follow:

- **The person whose machine it is should know.** Even for a legitimate task,
  silent execution on personal hardware is not a defensible default.
- **Operators need to see what happened.** "The agent wanted X on machine Y and
  was allowed/denied" has to be recoverable after the fact.
- **Locking the agent into one directory makes it useless.** "Tidy my
  Downloads" and "run the tests in ~/code" are the actual value.

The model is macOS TCC — transparency, consent, control — applied to an agent.

## The decision

Five branches, first match wins, in `pkg/permission/manager.go`:

| | Branch | Outcome |
|---|---|---|
| 1 | Inside the agent's `work_dir` | allow, silently |
| 2 | **Hardlock** (`~/.ssh`, `~/.aws`, Keychain, IM databases) | **deny** |
| 3 | Standing permanent grant | allow |
| 4 | Live session grant (with TTL) | allow |
| 5 | Otherwise | ask the human |

The hardlock check runs **before** any allow-rule lookup. That ordering is the
point: an accidentally-permanent rule on `~/.ssh` can never take effect, and a
hardlock can only be lifted from the CLI (`sandrpod-tray unlock <path>
--i-understand-the-risk`), never from the GUI.

The prompt is a native dialog — `osascript` on macOS, `zenity`/`kdialog` on
Linux, PowerShell `MessageBox` on Windows — and it **fails closed**: a timeout
or an error is a denial.

## Two processes

`sandrpod-agent` runs as a daemon and holds the reverse tunnel.
`sandrpod-tray` runs in the user's GUI session and owns the dialogs, the tray
icon, and a local settings page. They talk over `~/.sandrpod/authz.sock`
(mode 0600).

They are separate because their lifecycles are: different privileges, different
startup, different context. Merging them breaks one or the other.

## Modes

```
-permission-mode  off | prompt | strict     (default: off)
```

- **off** — gate disabled entirely; only the pre-existing `resolveSafePath`
  blacklist applies. Backwards compatible.
- **prompt** — `work_dir` silent, everything else asks. Falls back to an
  in-process dialog if the tray is not running.
- **strict** — `work_dir` silent, everything else **silently denied**. For
  headless machines where nobody is there to answer.

## Audit

Every decision is written to a local NDJSON log (`~/.sandrpod/audit/`,
auto-rotating at 8 MiB). With `-audit-upload-url` set, a background goroutine
batches them to your endpoint every 30s with at-least-once delivery and
exponential backoff to 10 minutes; a cursor file makes the upload resumable
across restarts. Without it, the logs stay local.

```bash
sandrpod-agent -api-url=https://api.example.com -name=my-laptop \
  -permission-mode=prompt \
  -audit-upload-url=https://your-platform/api/audit/decisions/batch
```

## Rule storage

`~/.sandrpod/permissions.json`, written atomically (tmp + rename), mode 0600:

```json
{
  "version": 1,
  "work_dir": "/Users/alice/workspace",
  "rules": [
    { "path": "~/Documents", "mode": "rw",   "scope": "permanent" },
    { "path": "~/.ssh",      "mode": "deny", "scope": "hardlock", "note": "default seed" }
  ],
  "session_grants": [
    { "path": "…", "mode": "r", "session_id": "…", "expires_at": "…" }
  ],
  "command_policy": {
    "deny": ["scp", "rsync", "nc", "socat", "ssh-keygen", "crontab", "sudo", "dd", "mkfs"],
    "warn": ["curl", "wget", "osascript"]
  }
}
```

Managed from the tray CLI:

```bash
sandrpod-tray serve                                  # tray + IPC + settings page
sandrpod-tray rules ls                               # permanent rules and hardlocks
sandrpod-tray rules add ~/Documents --mode rw
sandrpod-tray policy ls                              # command deny/warn lists
sandrpod-tray unlock ~/.ssh --i-understand-the-risk  # CLI only, never from the GUI
sandrpod-tray seed                                   # install the default hardlocks
```

---

## What this stops, and what it does not

This is the part worth reading carefully, because the honest answer is narrower
than "the agent is sandboxed".

**It covers the paths that go through the gate**: the file API (`files/*`),
`/process`, `/procmgr/start`, `/process/session/*/exec`, and `/pty/create`.
On those it is a friction layer and an audit layer — the agent cannot read your
SSH key through the file API, cannot `scp` without the command policy seeing it,
cannot open a PTY without you agreeing.

**It is not an OS-level sandbox, and arbitrary code execution is not
containable by design.** A line of `open("~/.ssh/id_rsa")` inside
`/process` or `/code-interpreter` goes straight to a syscall. It never reaches
`pkg/permission`, and the hardlock does not apply. The real file boundary is OS
user isolation, seccomp, or landlock. SandrPod does not implement syscall-level
sandboxing and does not claim to.

Specifically not covered:

- **File I/O from interpreted code** — the above. This is what "run arbitrary
  code" means, not a bug.
- **`/code-interpreter/execute` audits but does not block.** Token-level denial
  on source code both false-positives (a bare `dd` in a string) and is trivially
  evaded (`__import__("o"+"s")`), so a hit is logged as a warning rather than
  enforced. The other execution entry points do enforce.
- **Encoded commands** — `echo c2NwIC4uLg== | base64 -d | sh` shows no `scp` to
  a token scanner.
- **Shell `eval` and interpolation**, and `subprocess.Popen(["s"+"cp", …])`.
- **Bytes typed into an already-open PTY.** Consent is per session, not per
  keystroke.
- **Network egress.** There is no allowlist yet — an agent that can run `curl`
  can exfiltrate. This is on the roadmap and is currently the largest gap.

### Known engineering limits

1. macOS `display dialog` allows three buttons, so there is no "allow for this
   session" button — that lives in the tray settings page instead.
2. Windows `MessageBox` cannot relabel buttons; the mapping is explained in the
   body text.
3. Linux has no standalone tray fallback — without `sandrpod-tray` you get
   `zenity` dialogs and no icon.
4. No IPC token rotation yet. The 0600 mode on `authz.sock` is the only
   protection; local root can forge messages.
5. The audit token is the SandrPod token, shared org-wide. A compromised
   employee machine could inject audit events attributed to another sandbox.
   Sandbox-scoped audit tokens are the intended fix.

---

Design notes, the IPC protocol, per-package internals, cross-compilation, and a
step-by-step bring-up walkthrough are in
[PERMISSION_AND_AUDIT.zh.md](PERMISSION_AND_AUDIT.zh.md).
