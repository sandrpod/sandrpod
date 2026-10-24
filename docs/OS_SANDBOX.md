# OS sandbox (the kernel floor)

> Opt-in. `-os-sandbox=off | auto | bwrap | seatbelt` — off by default, like the
> permission gate: a sandbox that breaks a toolchain is worse than none, so this
> one has to be chosen.

Three layers protect an agent running on someone's machine, and they cover
different things:

| Layer | What it sees | What it stops |
|---|---|---|
| **Permission gate** (`pkg/permission`) | file API calls, exec/PTY launches | an agent using the *gated* API to read `~/.ssh` |
| **Egress proxy** (`pkg/egress`) | proxy-aware network clients | `curl`/`npm`/`pip` reaching a host nobody allow-listed |
| **OS sandbox** (`pkg/sandbox`) — this doc | every syscall from a spawned process | `open("~/.ssh/id_rsa")` inside `Execute`, `curl` with the proxy env unset |

The first two are **filters**: they see what goes through them. A line of Python
that calls `open()` or `connect()` directly goes around both — that is what
"run arbitrary code" means. This layer wraps the process in the platform's own
sandbox, so those syscalls fail no matter which code path issues them.

---

## 1. What it enforces

One model on both platforms, deliberately:

- **Read-only root.** The disk is readable (toolchains live there) and nothing
  is writable.
- **Writable work tree.** `work_dir` plus the temp dirs tools assume.
- **Hardlocks masked.** Every path the permission gate hard-locked — `~/.ssh`,
  `~/.aws`, `~/.sandrpod/permissions.json`, the keychain, … — is made
  **unreadable to the process**, not merely off-limits to the file API.

On Linux that is bubblewrap:

```
bwrap --ro-bind / / --bind <work_dir> <work_dir> --bind /tmp /tmp
      --ro-bind /dev/null <hardlocked file> --tmpfs <hardlocked dir>
      --dev /dev --proc /proc --die-with-parent --unshare-pid --new-session -- <cmd>
```

On macOS it is a generated Seatbelt profile under `sandbox-exec`, with the same
shape: `(deny default)`, `(allow file-read*)` then `(deny file-read* …)` for the
hardlocks (later rules win in SBPL), and `(allow file-write* …)` for the work
tree.

Every process the toolbox spawns goes through it — `Execute`, the session shell,
the PTY, the code-interpreter kernel, procmgr starts — because the seam lives
next to `ChildEnv` in `pkg/toolbox/env.go`'s sibling `sandbox.go`, and a spawn
site that forgot to call it fails `TestSandbox_*`.

---

## 2. Platform support, honestly

| Platform | Backend | Status |
|---|---|---|
| Linux | `bwrap` | **Verified.** The package's own test runs a real sandboxed command (`pkg/sandbox/bwrap_linux_test.go`) and is exercised in a privileged container. |
| macOS | `sandbox-exec` | **Experimental.** The profile could not be exercised where it was written: `sandbox-exec` refuses to nest inside an already-sandboxed process (`sandbox_apply: Operation not permitted`), which is what a developer shell under MDM turns out to be. Gate it with the probe below and widen the allow list from real workloads, not guesses. |
| Windows | — | Not implemented. |

`auto` uses the backend when it works and logs why not; `bwrap`/`seatbelt` are
refusals — the agent will not start if the backend cannot run. An OS sandbox
that silently does not start is indistinguishable from one that is protecting
you, which is the one thing this layer must never be.

The probe is not theoretical. On the machine this was written on:

```
os sandbox: auto requested but not usable on this host — processes run unwrapped
# and, when forced:
os sandbox setup failed: sandbox-exec cannot apply a profile here (exit status 71):
    sandbox_apply: Operation not permitted
```

---

## 3. Configuration

```bash
sandrpod-agent -api-url=https://api.example.com -name=my-laptop -token=… \
  -permission-mode=prompt \
  -os-sandbox=auto          # or bwrap / seatbelt to force, off to disable
```

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-os-sandbox` | `SANDRPOD_OS_SANDBOX` | `off` | `off` \| `auto` \| `bwrap` \| `seatbelt` |

The policy is not configured separately: it is read from the permission store —
`work_dir` becomes the writable tree and every `scope: "hardlock"` entry becomes
a masked path. That is deliberate. Two lists of "sensitive paths" that can drift
apart is how a hardlock stops being a hardlock; there is one list, and this layer
enforces the same one where the kernel can.

Startup is loud about what it did:

```
os sandbox: bwrap active — work_dir=/home/alice/AgentSandbox writable, 17 hard-locked path(s) masked
os sandbox:   masked /home/alice/.ssh
os sandbox:   masked /home/alice/.sandrpod/permissions.json
…
```

---

## 4. What it does not do

- **It does not take the network away.** On Linux `--unshare-net` is implemented
  (`Policy.NetworkNone`) but off: the egress proxy listens on the host's
  loopback, so a sandbox without a bridge cannot reach the proxy either. Closing
  that needs the relay sandbox-runtime builds — a unix socket bind-mounted into
  the sandbox plus a tiny relay inside — and that is its own change. Until then,
  egress on a laptop stays a **filter** (see
  [EGRESS_POLICY.md](EGRESS_POLICY.md)); in a container it is already a wall.
- **It does not cover the toolbox's own file API.** `fs/*` calls are Go code in
  the toolbox process, not a spawned command; the permission gate is their
  boundary, and it still applies.
- **It does not survive a hostile toolchain.** A sandbox profile is a policy, not
  a proof: a missing mask or an allow that is broader than intended is a hole.
  The hardlocked paths are masked explicitly rather than kept out by a rule, so
  the failure mode is visible (a path is either mounted over or it is not).
- **macOS semantics are unverified.** See the table above.

---

## 5. Verifying it yourself

Linux, without installing anything (the test skips if bubblewrap is unusable):

```bash
GOOS=linux GOARCH=arm64 go test -c ./pkg/sandbox -o /tmp/sandbox.test
docker run --rm --privileged -v /tmp:/mnt alpine:3.20 \
  sh -c 'apk add -q bubblewrap && /mnt/sandbox.test -test.v -test.run Bwrap'
```

It asserts, against a real sandboxed process: an allowed file reads, a
hard-locked file does not (and leaks nothing), a hard-locked directory does not
list, a write inside `work_dir` works, a write outside it never reaches the
host, and `--unshare-net` really removes the network.

On macOS, on a machine that is not already sandboxed:

```bash
sandrpod-agent -os-sandbox=seatbelt …     # refuses to start if the profile cannot be applied
```

See also: [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) (the gate this
enforces), [EGRESS_POLICY.md](EGRESS_POLICY.md) (the network half), and
[SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md) (where the model came
from).