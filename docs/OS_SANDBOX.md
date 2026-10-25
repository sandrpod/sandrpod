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
- **Persistence paths read-only.** The mandatory-deny set — `.git/hooks`,
  `.git/config`, `.bashrc`, `.mcp.json`, `.vscode`, `.claude/commands`, … —
  stays readable and loses its write permission (`--ro-bind` over the writable
  tree on Linux, a trailing `deny file-write*` on macOS). It is a walk of the
  work tree bounded by `-mandatory-deny-depth` (default 3).

On Linux that is bubblewrap:

```
bwrap --ro-bind / / --bind <work_dir> <work_dir> --bind /tmp /tmp
      --ro-bind /dev/null <hardlocked file> --tmpfs <hardlocked dir>
      --dev /dev --proc /proc --die-with-parent
      --unshare-pid --unshare-ipc --unshare-uts --new-session --cap-drop ALL
      -- <cmd>
```

`--cap-drop ALL` matters when the agent runs as root (a systemd system service is
the natural deployment): bubblewrap only creates a user namespace for an
unprivileged caller, so without it the sandbox would hold real root and a single
`umount` would lift every mask above. It is a no-op for an unprivileged agent.

A root agent adds two capabilities back, `CAP_DAC_OVERRIDE` and
`CAP_DAC_READ_SEARCH`, because it is usually serving a tree owned by the person
at the machine: without them a command in the sandbox gets EACCES on the user's
own work directory. They are the narrowest pair that reads those files; the one
that would undo the mounts, `CAP_SYS_ADMIN`, stays dropped.

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

- **On macOS the network is narrowed, not removed.** There is no network
  namespace to take away, so the profile restricts outbound to the proxy's
  loopback port instead: `(allow network-outbound (remote ip "localhost:3128"))`.
  A process that ignores `HTTP_PROXY` can still open a socket — to the proxy,
  which is the point; it cannot reach anything else. This only runs when the
  probe passes (§2), and the profile itself is unverified here.
- **The network namespace only exists with egress enforcing.** `--unshare-net`
  without a way back to the proxy would leave a sandbox that cannot resolve or
  fetch anything, so the two are switched on together (§4a).
- **It does not cover the toolbox's own file API.** `fs/*` calls are Go code in
  the toolbox process, not a spawned command; the permission gate is their
  boundary, and it still applies.
- **It does not survive a hostile toolchain.** A sandbox profile is a policy, not
  a proof: a missing mask or an allow that is broader than intended is a hole.
  The hardlocked paths are masked explicitly rather than kept out by a rule, so
  the failure mode is visible (a path is either mounted over or it is not).
- **Two residuals of the mount model, stated rather than implied.** The
  write-protected paths (`.git/hooks`, `.bashrc`, …) are read-only *mounts*, so:
  (a) a **hardlink** to one of them, created inside the writable tree, writes the
  same inode — directories cannot be linked, so `.git/hooks` itself is safe, but
  a protected *file* is not; (b) the list is rebuilt for every spawn, so a command
  that creates `.git/hooks` and writes into it *in the same command* wins — the
  protection applies from the next spawn, and a `git init` followed by a commit
  in one shell is the case to keep in mind. Closing both needs Landlock, not
  bind mounts.
- **macOS semantics are unverified.** See the table above.

### 4a. The network, on Linux

When the egress proxy is enforcing, the sandbox also gets its own network
namespace and the proxy moves to a unix socket:

```
sandbox netns (loopback only)
   127.0.0.1:3128  ←  relay  ──→  host: /tmp/sandrpod-egress-*/proxy.sock
                                     (the egress proxy, with the policy)
```

- `--unshare-net` gives the sandbox a loopback and nothing else — no route, no
  DNS server, no host services;
- the proxy listens on a **unix socket** instead of a port (`egress.Options.UnixSocketPath`);
- the socket directory and the agent binary (which doubles as the relay,
  `sandrpod-agent relay …`) are bind-mounted read-only into the sandbox;
- the wrapper starts the relay, waits for its ready file, then `exec "$@"` — the
  real argv travels through `$@`, never through string interpolation, because
  the argument may be arbitrary code;
- proxy variables inside point at `127.0.0.1:3128`, the relay.

A process that ignores the proxy variables now reaches nothing at all. Verified
end to end in a privileged container (`pkg/sandbox/netns_linux_test.go`): no
route without the proxy, an allow-listed host fetched through relay → proxy,
and an unlisted host refused through the same path.

Why a relay at all: a sandbox with no network cannot reach a TCP listener on the
host, and a unix socket is not something `curl` speaks. The relay is the
smallest thing that bridges the two, and it has no policy of its own — every
byte goes to the proxy, which decides.

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
list, a write inside `work_dir` works, and a write outside it never reaches the
host. `-test.run NetnsRelay` adds the network case: no route at all without the
proxy, an allowed host through relay → proxy, an unlisted host refused.

On macOS, on a machine that is not already sandboxed:

```bash
sandrpod-agent -os-sandbox=seatbelt …     # refuses to start if the profile cannot be applied
```

Credential masking adds one more kind of substitution to this layer: a masked
file is bound over its real path inside the sandbox (`Policy.ReadSubstitute`),
so the sandbox reads a file full of sentinels. See
[CREDENTIALS.md](CREDENTIALS.md).

See also: [PERMISSION_AND_AUDIT.md](PERMISSION_AND_AUDIT.md) (the gate this
enforces), [EGRESS_POLICY.md](EGRESS_POLICY.md) (the network half), and
[SANDBOX_RUNTIME_ANALYSIS.md](SANDBOX_RUNTIME_ANALYSIS.md) (where the model came
from).