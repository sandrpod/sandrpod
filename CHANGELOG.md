# Changelog

All notable changes to SandrPod are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Dates are month-granularity; the project moves in continuous small releases, so entries group by theme.

---

## [0.7.8] - 2026-10-29

### Fixed

- **`zsh: locking failed for ~/.zsh_history`** — 0.7.7 allowed writing the history
  file and the shell still complained, because the file it actually writes is the
  sibling temp file: zsh rewrites history by writing `.zsh_history.new` and renaming
  it over the original, and `compinit` writes `.zcompdump-<host>-<version>`. One
  regex now covers the shell state files at the top level of the user's own home
  (`.zsh_history`, `.zcompdump`, `.bash_history`, `.zsh_sessions`, and their
  suffixed forms) and nothing else.

Found by bisecting with a three-line probe rather than guessing: open+`fcntl` lock,
write `.new`, rename. Under the profile the first two passed and the third was
refused, which named the path precisely.

Verified through a real PTY session: three interactive starts, zero warnings of any
kind, and `execute` on the same sandbox returns empty stderr.

## [0.7.7] - 2026-10-29

### Fixed

- **`compdef:95: can't set tty pgrp: operation not permitted`** — with the preference
  noise gone, the next layer of the same problem was visible. Claiming a terminal is
  an `ioctl`, not a read or a write, so the profile needs
  `(allow file-ioctl (subpath "/dev"))`. Without it a login shell could not take the
  tty: zsh complained at every interactive start and job control (Ctrl-C, `fg`,
  `bg`) did not work inside a device sandbox.
- **`zsh: locking failed for ~/.zsh_history: operation not permitted`** — `rc` files
  set `HISTFILE` to `~/.zsh_history` regardless of the environment, so a shell in a
  read-only home warns on start and saves nothing. The profile now allows writes to
  exactly three files — `~/.zsh_history`, its lock, `~/.bash_history` — and to nothing
  else outside the work tree. The shell also gets `HISTFILE` inside the work tree, so
  a sandbox that keeps no host history keeps its own.

Verified through a real PTY session against an agent on macOS: three consecutive
interactive starts, zero warnings.

## [0.7.6] - 2026-10-29

### Fixed

- **`sandbox_extension_issue_file failed for …` on a launchd-installed agent.**
  0.7.5 allowed *reading* the preference plists, which is not what a daemon needs:
  it asks cfprefsd for settings over XPC and the kernel hands it a **sandbox
  extension** for the file. Without `file-issue-extension` on the preference roots
  and `cfprefsd` in `mach-lookup`, the request is refused and libsystem prints one
  line per file on the stderr of every command that reads a setting — seven lines
  ahead of an ordinary `curl`'s output.

  The earlier explanation (a sandbox created before the upgrade) was wrong: the
  variable is how the agent was started. Agents run in the foreground are clean;
  ones started by launchd — i.e. by the installer — are not.

  The profile now allows both, scoped to the preference roots and the two cfprefsd
  services, and `ProcessResult.Stderr` drops those lines if they still appear. Other
  sandbox refusals are kept: they are the evidence the sandbox works.

## [0.7.5] - 2026-10-29

### Fixed

- **A denied preference read printed an error into every command's output.** Under
  the seatbelt profile, any process that touches CFPreferences asks the kernel for a
  sandbox extension on `/Library/Preferences`, `~/Library/Preferences` and the managed
  variants; the refusal printed, on stderr, once per file:

  ```
  sandbox_extension_issue_file failed for /Library/Managed Preferences/…:
  1 (Operation not permitted)
  ```

  So an ordinary `curl` in a device sandbox came back with seven alarming lines
  before its output. Reading global preferences is not a persistence vector — writing
  them is, and that stays denied — so the profile allows the read now. Verified:
  `stderr` empty, no `sandbox_extension_issue_file` lines, `http=200`.

## [0.7.4] - 2026-10-29

The one-line cause of everything in 0.7.2 and 0.7.3: the resumer never looked at
`job.Status`.

`ListJobs` returns every row, and the only gate was age. So **every** create job ever
recorded was re-delivered every minute or so, forever — completed ones included. On a
worker that is one new container per minute per sandbox name, each with its own egress
network, until docker runs out of addresses; on a laptop worker it is the same, with
the added excitement of the machine filling up.

The fixes in 0.7.2 and 0.7.3 were real and are kept — the delivery window, the
completion write, the idempotent create, the liveness reapers — but they were treating
symptoms of this. With the status filter, a finished job is finished.

### Fixed

- The resumer skips jobs that are not `PENDING`. A test asserts a completed job is
  never re-delivered.

## [0.7.3] - 2026-10-29

The same evening, from watching 0.7.2 run for ten minutes.

### Fixed

- **A resumed delivery was never marked complete.** The resumer delivered the job,
  logged it, and left it PENDING — so the next pass delivered it again, once a
  minute, for as long as the record lived. On a worker that looked like
  "Creating sandbox: smoke6" every 60 seconds.
- **Creating a container was not idempotent.** Because delivery is at-least-once, a
  retry must land on the same sandbox; instead each attempt made a new `sp-…`
  container (and its egress network). `CreatePod` now removes any existing container
  labelled with that sandbox name first, which is what makes the retry a retry.
- **A worker that stopped heartbeating stayed ONLINE forever.** The liveness and
  reclamation reapers were started from `Main()` too, so an embedding build never
  ran them: a killed agent still showed "ONLINE, usage=70/10" and counted against
  placement for as long as the process was gone. `New` now starts them.

## [0.7.2] - 2026-10-29

Two hours after 0.7.1, from running it. The resumer told the truth about a lost
delivery and then kept telling it about every delivery ever lost.

### Fixed

- **A pending create with no sandbox record is delivered again.** The create writes
  the job first and the record second, so that state is a delivery that never got
  going — which is what this pass exists for. 0.7.1's parent commit had made it a
  permanent failure, and the multi-instance check had been red since.
- **Only deliveries inside the window are retried.** With that restored, the pending
  jobs of sandboxes deleted days earlier came back: each one recreated its sandbox,
  its egress network and its container, on a real worker until docker ran out of
  addresses (`all predefined address pools have been fully subnetted`) and every new
  create failed. A job older than 40 minutes is now failed with a reason.
- **Deleting a sandbox finishes its pending creates**, so the resumer cannot deliver
  one and resurrect what somebody removed.

## [0.7.1] - 2026-10-29

The release that made the cloud path real. Driving a live Aliyun deployment from the
console found five defects that between them made every `provider_type` other than
`local` fail — each one in a different layer, each one pointing somewhere else. Also
the lifecycle of a device agent, and an installer for a machine that has nothing on it.

### Added

- `deploy/node-install.sh`: a guided installer for a fresh worker — data disk, swap,
  Docker from a China-reachable mirror, registry mirrors, the sysctls development
  tooling needs, and the two images it will pull. `--dry-run` prints the plan.
- `deploy/agent-install.sh` (published at `docs.sandrpod.com/agent.sh`): turns a
  machine into a sandbox — installs the release binary, writes a systemd unit or
  launchd plist, and confines it by default.
- `/api/v1/capabilities` reports `providers`: which substrates this deployment can
  actually provision, so a client can stop offering the others.
- A device agent can be attached by a workspace, not only by an operator.
- [docs/UPSTREAM_CHANGES.md](docs/UPSTREAM_CHANGES.md): for each release, what
  breaks, who it breaks, and how to roll back.

### Fixed

- **An embedding build registered no cloud providers at all.** Registration lived in
  `Main()`, which a commercial build never runs, so every provider answered
  `provider aws not found` — after the customer had filled in a region and a size.
- **Provisioned VMs were told to dial `127.0.0.1`.** The address handed to a worker
  was the loopback fallback, so a cloud VM connected to itself and the create timed
  out waiting for a worker that could never arrive.
- **Cloud Assistant's 60-second default killed the Docker install, and a timeout
  counted as success.** The bootstrap then ran `docker run` on a machine with no
  Docker and reported `docker: command not found` after it had been terminated. The
  command timeout is 10 minutes now and `Timeout`/`Stopped` are failures.
- **The first create pulled a 550 MB image inside a two-minute budget.** The container
  appeared after the caller was told the create had failed, with no record pointing at
  it. The image is pre-pulled during bootstrap.
- **The idle reaper never ran in an embedding build**, so every cloud VM provisioned
  for a sandbox stayed up and billed after the sandbox was deleted. It also judged
  "empty" by container count, so an orphan container pinned a machine alive.
- **A delete leaked the egress sidecar, on every delete.** Create named the sidecar
  after the container's name; delete looked it up by the container's ID and treated
  "not found" as success. One container and one bridge network per egress sandbox.
- **Deleting a device sandbox did not stick.** The delete dropped a pointer instead of
  the connection, so the agent kept the socket, believed it was still serving, and a
  reconnect recreated the record. Deleted names are tombstoned, the tunnel is closed
  for real, and an agent told `410` stops and exits.
- **A provisioned VM's worker belonged to nobody.** It registered with the platform
  token, so it was invisible in the workspace that paid for it and eligible for every
  other tenant's sandboxes. It inherits the requesting workspace now.
- A closed tunnel left the peer blocked on a read: `Close` shut the yamux session and
  not the transport.
- `agent.sh` exited silently on macOS — `getent` does not exist there, and `set -e`
  turned that into a script that printed two lines and stopped.
- The images could not be built without buildx: `FROM --platform=$BUILDPLATFORM` with
  the variable unset is a parse error on its first line.

### Changed

- The project positions itself as a **production service**, not a prototype: version
  support policy, upgrade guarantees and support channels are in the README.
- A device agent is confined by default: it runs as the invoking user, writes only
  under `~/sandrpod-work` (plus the system temp dir), gates the rest, and uses the
  kernel sandbox where the platform has one. `--full-machine` opens all of it and
  says so.
- Both installers accept `--flag=value`, the form the console prints.

## [0.7.0] - 2026-10-27

The control plane becomes a library, and tenant isolation reaches the paths it
had missed. Release notes (what changed, the upgrade order, the breaking Go API
changes, the limits) are in [docs/RELEASE_NOTES.md](docs/RELEASE_NOTES.md).

### Added

- `pkg/server`: `New`, `Main`, `NewE2BGateway` — the API server as an importable
  package, with its flags bound to a private FlagSet.
- `Config.Authenticator`, `Config.Admission`, `Config.Events`: three seams an
  embedding build supplies.
- `RoleWorker` and `workerRoute`: a join token can handshake and poll, nothing else.
- `Config.E2BAPIKeys`: the E2B surface resolves its own keys and carries the tenant.

### Fixed

- Async create dropped the tenant, so a sandbox could land on another tenant's
  worker; the grace-period fallback used the unscoped selector.
- A refusal at the create choke point left the sandbox `PENDING` forever.
- `sandbox.deleted` events carried no owner or resources, so metering lost them.
- An embedding build's `-port`/`-db` panicked with `flag redefined`.
- `poder` heartbeats from join-token workers were rejected (403), leaving the
  worker online but stale.
- Sandbox creation pulled the toolbox image unconditionally (4.2s → 230ms).
- `poder` reported `0.5` as host CPU/memory when `/proc` was unreadable; it now
  reports `-1`, and reads `/proc` directly instead of gating on a docker call.
- A browser terminal's resize frame was written into the shell.
- The playground's fleet phase skipped itself on CI instead of failing.

## [0.6.0] - 2026-10-25

The release notes for this version — what it adds, the upgrade order, the breaking
Go API changes and the known limits — are in [docs/RELEASE_NOTES.md](docs/RELEASE_NOTES.md).

### Upgrade notes

Read this before tagging a release: everything below is what an *existing*
deployment can notice.

- **Defaults are unchanged.** Every feature added in this cycle is opt-in
  (`-os-sandbox`, `-egress-mode`, `-credential-mask`, `-mandatory-deny-depth`
  is inert without a gate). An upgrade that changes no flags behaves exactly as
  before, including on the agent path.
- **Storage and wire format are additive.** `sandboxes.egress_policy` is an
  `ADD COLUMN … DEFAULT ''` migration, and every new JSON field is
  `omitempty`/ignored-if-unknown, so an old binary can still read a database and
  a payload produced by a new one. Rolling either direction is safe.
- **Mixed-version fleets are now detected, not trusted.** A worker advertises
  what it can enforce on the tunnel handshake (`X-Poder-Capabilities`), and the
  control plane refuses to schedule a sandbox that asks for a policy onto one
  that does not advertise `egress` — `409` with "worker capability unavailable",
  rather than a sandbox reported as walled that runs with no route policy. A
  worker from before this change advertises nothing, so it is refused; upgrade
  poders *or* drop the policy. (The refusal is per-capability, so a plain
  sandbox still schedules onto an old worker.)
- **New agent with an old server**: `-egress-mode=server` polls
  `GET /api/v1/sandboxes/{name}/egress`, and `decider` calls
  `POST /api/v1/policy/decide`. Both are new endpoints; against an old server
  they 404, and both modes then **deny all egress** (fail-closed by design).
  Upgrade the server first if you intend to use them.
- **Go API (only for importers of `pkg/...`)**: `permission.AuditSink.Record`
  gained a `modelReason` parameter — an interface implementer will fail to
  compile until it adds one. `egress.Server.SOCKSURL()` now returns `""` when the
  proxy listens on a unix socket instead of a malformed URL. Everything else in
  this cycle is additive at the Go level.
- **Image tags**: a release publishes `<version>` **and moves `latest`**. Running
  containers keep the image they were started with (Docker does not re-pull on
  restart); anything that pulls `latest` explicitly, or starts on a machine
  without the image, gets the new one. Pin tags in production if that timing
  matters.

### Added

- **The E2B surface scopes placement like every other surface.** `e2bcompat.Config`
  gained an optional `Authz func(key) (Identity, bool)` — the richer form of
  `Auth`, which can also report the caller's tenant — and `NewSandbox` carries
  that tenant to the backend on a field that is not part of E2B's wire format.
  A hosted build that issues workspace-scoped keys now gets tenant-scoped
  placement on `/v2/sandboxes` too, instead of sandboxes landing wherever there
  was capacity. `Auth` keeps working unchanged for hosts that only have names;
  `Authz` wins when both are set.

### Added

- **Sandbox create latency: 4.2s → 230ms (p50, measured).** The pode called
  `ImagePull` on every create, which costs a registry round-trip even when the
  image is already local (1.8s here) — the single largest part of the cold
  start. It now inspects first and pulls only when the image is missing, so a
  warm host creates a sandbox in about 230ms and the nightly playground asserts
  a 1.5s budget. The GTM target of "under 500ms" was reached without the
  snapshot/warm-pool work that was planned for it.

- **The mixed-version refusals are re-proven every night.** The playground's
  legacy phase starts a second control plane with `-emulate-legacy` (404 on
  `/api/v1/capabilities`, as an older build does) and a worker container that
  advertises nothing, then asserts all four facts: the agent refuses that control
  plane with the upgrade message, a policy on that worker is refused `409`, the
  message names the capability, and a sandbox *without* a policy still runs.
  Both flags are TEST ONLY and only remove what the binary claims — they cannot
  widen anything — and both log a warning at startup. The failure they guard
  against is silent (a sandbox reported RUNNING with no route policy), which is
  what makes it worth a nightly rather than a paragraph.

### Added

- **The control plane states its capabilities, and the agent refuses a server it
  cannot rely on** (`GET /api/v1/capabilities` -> `egress_policy`,
  `egress_decider`). `-egress-mode=server|decider` probe it at startup: an older
  server has no such route, and the agent now exits with
  `the control plane at ... has no /api/v1/capabilities - it predates "egress_policy"...`
  instead of starting deny-all and making every connection in the sandbox fail as
  if the network were down. A 404 is the honest signal here: an absent route
  cannot mean "not registered yet" the way a missing record can. Verified against
  a server without the route and against a current one, where both modes start
  and register.

### Added

- **The TypeScript SDK can manage egress**: `createSandbox(..., { network })` and
  `getEgress`/`setEgress`/`clearEgress`, mirroring the Python client. Verified
  against a live server from Node (create with a policy, read it back, replace,
  execute, clear, delete).
- **The CLI takes the common case as flags**: `--allow-domain` / `--deny-domain`
  (repeatable) on `create` and `egress set`, merged into a `--egress-policy` file
  when both are given. `egress set` now requires only one of the two.
- **The playground proves the fleet path too**: a policy created through the CLI,
  enforced by the real sidecar in a real container (allowed host 200, denied host
  403 with `X-Deny-Reason: denied_domain`). It builds the worker and the egress
  sidecar from the tree under test — its first version used the published
  `poder:latest`, which predates the egress work, ignored the `network` field and
  ran the sandbox completely unwalled while reporting `RUNNING`. That is the
  mixed-version hazard in the upgrade notes, reproduced: the phase now builds
  from source and skips loudly when an image is unavailable.

- **The CLI can manage egress.** `create --egress-policy file.json` pins the
  allow-list at creation, and `egress get` / `egress set` / `egress clear` read
  and edit it afterwards (`set` takes a file, `-` for stdin, or inline JSON; the
  server's validation errors surface as-is). `egress clear` needed a new
  `DELETE /api/v1/sandboxes/{name}/egress`, because `PUT {}` is not "unset" — an
  empty policy is a wall with an empty allow-list. Both distinctions are in the
  help text, and `CLIClient` grew `get_egress`/`set_egress`/`clear_egress` plus
  a `network=` argument on both create paths for anyone using it as a SDK.

### Fixed

- **The langchain backend did not follow the sandbox protocol in three places.**
  Checked against langchain's own `SandboxIntegrationTests` (86 tests) and
  against deepagents' `LocalShellBackend` as a control: `write` overwrote an
  existing file where the protocol says "error if file exists" (and `edit` is the
  path for changing one); `read(limit=0)` returned the whole file instead of
  nothing; and `upload_files` did not create the parent directory, which the
  protocol makes it responsible for — that single omission accounted for seven
  failing conformance cases. 69/86 pass now, and every remaining failure is one
  deepagents' own backend fails too, with 18 the other way (we pass where it
  does not).

### Fixed

A review of everything borrowed from sandbox-runtime found and fixed these.
Each one has a regression test; the two marked “playground” were found by
running the assembled product.

- **The interactive PTY path never went through the sandbox.**
  `ProcManager.Start` returned from its PTY branch before `wrapSandbox`, so
  `POST /procmgr/start` with `pty: true` ran a shell completely outside
  bubblewrap/Seatbelt — the one path an interactive session uses.
- **A SigV4 request was never re-signed when masking was on.** Header
  substitution rewrote the credential scope to the real access key *before*
  `ResignAWS` looked for the sentinel, so the lookup missed and the request went
  upstream with a signature over a key id it did not match: every masked AWS
  call failed with `SignatureDoesNotMatch`. Re-signing now recognises both
  spellings, and the canonical query is built from the raw bytes the client
  signed instead of re-encoded parsed values.
- **The terminated path forwarded the proxy's own headers upstream**, including
  `Proxy-Authorization`, which carries the sandbox token. Hop-by-hop headers are
  stripped before forwarding, and the `Connection:` token list is now honoured
  (the HTTP path deleted `Connection` before reading it, so marked headers
  travelled anyway).
- **A masked credentials file inside a hardlocked directory was invisible.**
  The substitution was mounted before the hardlock masks, and the default seeds
  hardlock `~/.aws` wholesale — so the documented `-credential-mask-files
  ~/.aws/credentials` produced a sandbox with no credentials file at all.
  Substitutions are mounted last, and a missing sentinel copy now refuses the
  spawn instead of falling back to the real file.
- **An inherited `NO_PROXY` silently disabled the proxy.** The child environment
  is the agent's environment plus ours, so a `NO_PROXY` from the launching shell
  survived and every proxy-aware client went direct. It is set empty now, and
  `ALL_PROXY` is omitted entirely when the proxy listens on a unix socket.
- **The SOCKS tunnel dropped bytes the client sent with the handshake.** The
  handshake parser buffers up to 4 KiB; splicing the raw connection discarded
  whatever a pipelining client had already sent. The buffered reader is now
  re-attached, as the HTTP CONNECT path already did.
- **A transient accept error permanently killed a listener** (SOCKS, and the
  relay's equivalent), and the relay wrapper ran the command even when the relay
  never started — with no proxy and no diagnostic. Both retry or fail loudly
  now, and the relay's own log goes to a file instead of the command's stderr.
- **`.github/config` was shaded read-only as if it were git's config** in the
  kernel plan (a `contains ".git"` test matches `.github`), while the gate
  allowed it: the two layers disagreed about the same path.
- **A sandbox could unlink its own proxy socket.** The socket directory is in
  `/tmp`, which is bound writable; the read-only bind over it now comes after
  the writable binds, so it wins.
- **Path lists were not symlink-resolved.** bubblewrap refuses to mount on a
  symlink destination (dotfile-managed `~/.aws` is a symlink), and Seatbelt
  matches resolved paths (`/var` vs `/private/var` on macOS): every plan now
  carries resolved paths.
- **`CreatePod` could leave an orphaned container** when a step after
  `ContainerCreate` failed, which also blocked removal of the per-pod egress
  network.
- Smaller: the truncated-upstream-body case now aborts instead of delivering a
  well-formed short response; token/argument refusals are counted and audited
  like every other refusal (the stats used to stay at zero); the decider cache is
  bounded; a trailing-dot allow entry (`example.com.`) matches; an interface
  enumeration failure denies instead of allowing for 30 seconds; an out-of-range
  CONNECT port is a 400 rather than a silent 443; the 502 body no longer carries
  the vetted IP.
- **The sandbox policy is rebuilt for every spawn** (`SetSandboxPolicyFunc`), so
  a `.git` created inside the sandbox, a hardlock unlocked in the tray, or a
  rotated credentials file reaches the kernel and not only the gate. It also
  drops `--cap-drop ALL` and `--unshare-ipc`/`--unshare-uts` into the plan, and
  the plan is what the probe now exercises.

### Added

- **The playground as a script, and a nightly job that runs it**
  (`hack/playground/run.sh`, `.github/workflows/playground.yml`). The manual
  walkthrough in `docs/PLAYGROUND.md` §B is now one command: it builds the agent
  for Linux, starts a control plane and an upstream, runs the agent in a
  privileged container with every layer on, and asserts each outcome — the
  hard-locked key is unreadable, a git hook cannot be written, `.git/config`
  stays readable, an allow-listed host answers, an unlisted one is refused with
  `X-Deny-Reason`, the sandbox holds a sentinel while the upstream logs the real
  key, a cleartext credential is refused, and the audit records the termination
  with the credential name. It exits non-zero with the failing assertion named,
  keeps the containers on failure, and cleans up otherwise. Nightly plus on PRs
  that touch egress, sandbox, the gate or the agent — it needs `--privileged`
  and two containers, and its first run found four bugs that only appear once
  the layers are assembled.

- **`docs/APP_INTEGRATION.md` and `docs/PLAYGROUND.md`** (both with Chinese
  versions). The first answers "what does upgrading do to my application": the
  API diff is empty, three things are visible inside the sandbox, four kinds of
  tool are expected to break, and the three deployment profiles are spelled out.
  The second is a manual walkthrough that verifies every layer end to end — the
  gate, the kernel floor, the network namespace, the egress policy and
  credential masking — with the real output of each check, plus
  `hack/playground/echo`, a sixty-line TLS upstream that reports the
  `Authorization` header it received so masking can be verified without a third
  party.

### Fixed

- **The OS sandbox never actually ran through the toolbox.** `Resolve` returned
  `bwrap` as a bare name, and `wrapSandbox` assigned it to `exec.Cmd.Path`, which
  `execve` takes verbatim: every `Execute` with `-os-sandbox` failed with
  `fork/exec bwrap: no such file or directory`. The path is now resolved by
  `exec.LookPath` in `Resolve` and again defensively in `wrapSandbox`. The
  automated sandbox tests missed it because they call `Command()` and hand the
  result to `exec.Command`, which does the lookup themselves; the playground
  found it in the first command it ran.
- **Masking silently dropped the proxy environment.** `applyEgressChildEnv` set
  the child environment, then the credential path set it again with only the
  sentinels and the trust bundle, so `HTTP_PROXY` vanished and every request in
  the sandbox failed to connect. The two now build one list.
- **Only port 443 was ever terminated**, so a masked credential sent to any other
  HTTPS port arrived upstream as a sentinel. `-credential-terminate-ports`
  (default `443`) makes the set explicit, and an unparsable value fails startup.
- **Credential use did not reach the audit trail.** The egress event carried
  `Terminated`/`Credentials` but neither `audit.Event` nor the agent's adapter
  mapped them, and allowed events were dropped unless `LogAllowed` was on.
  "Which secret went to which host" is recorded now even when allowed-logging is
  off — names only, never values.
- **A terminated request dropped a non-443 port from the Host header**, which
  made a terminated tunnel behave differently from an opaque one for upstreams
  that build absolute links.

- **Credentials the agent can use but not read** (`-credential-mask`,
  `docs/CREDENTIALS.md`). A key in the sandbox's environment is a key the model
  can print: masked credentials are replaced with `fake_value_<uuid4>` sentinels
  and substituted back by the egress proxy on the way out — in headers, in
  bodies (length-preserving, so `Content-Length` stays valid), in credentials
  files bound over the originals, and in AWS SigV4 signatures, which are
  re-signed with the real secret and checked against AWS's published example.
  This requires terminating TLS for allow-listed hosts, which is a real loss:
  the sandbox's certificate is ours, pinning breaks, and mTLS cannot work. That
  is why it is opt-in, per-host excludable (`-credential-terminate-exclude`
  keeps mTLS and pinned hosts opaque, without masking), and why a masked
  credential over plain HTTP is refused rather than leaked. The proxy records
  which credential went where, and marks the connection `terminated` — breaking
  end-to-end TLS should be a visible event, not a footnote. Verified end to end
  in a real network namespace: a sandboxed process with no route fetches HTTPS
  through the relay, the upstream reads the real key, and the sandbox's own
  output never contains it.

### Added

- **Decider mode: per-connection verdicts from the control plane**
  (`-egress-mode=decider`, `POST /api/v1/policy/decide`). `-egress-mode=server`
  polls a copy of the policy; this one holds no rules at all and asks for every
  destination, so a tightened rule applies to the next connection instead of the
  next sync, and the server sees each decision as it happens. A decider that
  cannot answer is a denial (`X-Deny-Reason: decider_unavailable`), never an
  allow — an unreachable policy server must not become an open proxy — and a
  10-second per-destination cache keeps a page load from becoming a round trip
  per socket. The address check (loopback, link-local, cloud metadata, own
  addresses) stays local: name resolution is not a policy question and should
  not depend on the network.

- **Every refusal now carries two explanations.** `Reason` stays the localized
  string the person sees in the consent dialog; `ModelReason` is one line of
  English with what was refused and what to do instead, and it is what the tool
  result hands back to the agent (`Decision.Explanation()`). Both land in the
  audit record (`reason`, `model_reason`), so "why did the agent keep retrying"
  has an answer. A localized UI string surfaced to a model is noise it cannot
  act on; an English imperative in a dialog reads as a bug to the human.

- **macOS egress is narrowed to the proxy.** No network namespace exists there,
  so with egress enforcing the Seatbelt profile allows outbound to exactly one
  destination, `localhost:<proxy port>`. A process that ignores `HTTP_PROXY` can
  still open a socket — to the proxy, and nowhere else.

- **Mandatory deny: the persistence paths inside a work tree stay write-only
  protected, whatever was granted.** Granting `work_dir` used to mean "write
  anything under it", which included `.git/hooks/pre-commit` — a file that
  survives every later sandbox and runs on the host at the next `git commit` —
  plus `.bashrc`, `.mcp.json` (which configures what the agent itself may run)
  and IDE launch configs. Those are now refused on write even with an explicit
  rw grant, sit above the `work_dir` shortcut in the gate, and stay readable
  (a build may need to read `.git/config`). The same list is handed to the OS
  sandbox: `--ro-bind` islands over the writable tree on Linux, a trailing
  `deny file-write*` on macOS, found by a bounded walk of the tree
  (`-mandatory-deny-depth`, default 3). Verified against a real sandboxed
  process: the hook reads, the write fails, the host's copy is untouched, and
  an ordinary file in the same tree is still writable.


- **The network namespace on Linux, with a relay so the proxy survives it.**
  With `-os-sandbox=bwrap|auto` and egress enforcing, a sandboxed process now
  runs in its own network namespace (`--unshare-net`): loopback and nothing
  else — no route, no DNS, no host services. The egress proxy moves to a unix
  socket for that mode (`egress.Options.UnixSocketPath`), the socket directory
  and the agent binary (which doubles as the relay, `sandrpod-agent relay …`)
  are bound into the sandbox read-only, and a shell wrapper starts the relay,
  waits for its ready file, then `exec "$@"` — the argv travels through `$@` so
  arbitrary code is never re-parsed. Proxy variables inside point at
  `127.0.0.1:3128`, the relay. A process that ignores them now reaches nothing
  at all: on Linux with this on, the laptop's egress is a wall, not a filter.
  Verified end to end in a privileged container
  (`TestNetnsRelay_ProxyIsTheOnlyWayOut`): no route without the proxy, an
  allow-listed host through relay → proxy, an unlisted host refused. macOS has
  no equivalent and stays a filter for proxy-unaware processes.

- **An OS sandbox for spawned processes** (`pkg/sandbox`, `-os-sandbox`,
  `SANDRPOD_OS_SANDBOX`). The gate covers the file API and the egress proxy
  covers proxy-aware clients; neither sees a process that calls `open(2)` or
  `connect(2)` directly, which is what `Execute` and the PTY are. With this on,
  every process the toolbox spawns runs under bubblewrap (Linux) or Seatbelt
  (macOS) with a read-only root, a writable `work_dir` and the permission
  store's hardlocks masked — `~/.ssh` is unreadable to `open()`, not just to the
  file API. One list of sensitive paths, enforced in two places. `auto`
  degrades loudly when the backend cannot run; `bwrap`/`seatbelt` refuse to
  start rather than run unprotected. Linux is verified against a real sandboxed
  process (`pkg/sandbox/bwrap_linux_test.go`, run in a privileged container);
  macOS is experimental — `sandbox-exec` cannot be exercised inside an
  already-sandboxed shell, which is where it was written — and is gated behind
  its own probe. The network namespace is implemented but off by default: the
  egress proxy lives on the host's loopback, so taking the network away needs
  the relay that has not been built yet.

- **Egress policy for agent mode** — `pkg/egress`, `-egress-mode=enforce`,
  `-egress-policy <file>`. The agent starts an HTTP and a SOCKS5 proxy on
  loopback behind a per-process token, and hands every process the toolbox
  spawns (one-shot `Execute`, session shells, PTY, the code-interpreter kernel,
  procmgr starts) the proxy address through the new
  `toolbox.SetChildEnv`/`ChildEnv` plumbing. A connection is allowed only when
  an allow-list entry covers the destination **and** the address check passes:
  an allowed hostname is resolved once and refused if it lands in loopback,
  link-local, a cloud metadata endpoint or one of this host's own addresses, so
  allowing `*.example.com` does not also allow `169.254.169.254`. Refusals are
  a 403 with `X-Deny-Reason` plus a sentence the model can act on, and are
  audited as `source: "egress"` with the destination, the deciding rule and the
  resolved address. A policy file that fails to parse, or a typo'd key, is a
  startup error rather than an empty allow-list. This is a filter over tools
  that honour `HTTP_PROXY`, not a kernel boundary: `docs/EGRESS_POLICY.md`
  states where the line is, what is still missing (a network namespace /
  Seatbelt for processes that ignore the environment), and why TLS
  interception and credential masking are not in it yet.

- **Container sandboxes enforce the egress policy with Docker's network
  isolation, not with environment variables alone.** A pod created with a policy
  gets a per-pod `Internal: true` bridge network (no masquerade rule, so no route
  off the machine and no DNS beyond the embedded resolver), a `cmd/egress-proxy`
  sidecar joined to that network and to an ordinary one, and an environment
  pointing at it on fixed ports. The sandbox container joins the internal network
  only, so the proxy is the single way out — `--noproxy '*'` no longer reaches
  anything, which is the difference between this and the laptop case. A poder
  running as a container joins each pod's internal network so the toolbox stays
  reachable; a poder on a Linux host needs nothing. If the proxy cannot be stood
  up, `CreatePod` fails rather than starting a sandbox whose policy nobody
  enforces. New image: `docker/Dockerfile.egress` / `make docker-egress`
  (`ghcr.io/sandrpod/egress-proxy`, override with `SANDRPOD_EGRESS_IMAGE`).

- **The egress policy now travels with the sandbox instead of living on the
  machine.** `POST /api/v1/sandboxes` accepts a `network` block, validated at
  create time (a policy that does not parse is a 400, before any provisioning),
  stored on the sandbox record (`sandboxes.egress_policy`, an additive column
  migration) and served at `GET /api/v1/sandboxes/{name}/egress` — the endpoint
  the sandbox itself polls. `PUT` replaces the policy of a running sandbox and
  applies to its next connection, which is what makes "tighten this sandbox
  now" a call rather than a redeploy. The agent gains `-egress-mode=server`: it
  starts its proxy with an empty policy (deny all) and hot-swaps the policy it
  fetches every `-egress-sync`, so the window between "the sandbox is
  reachable" and "the policy has arrived" is closed rather than open. Both the
  fetch and the swap are fail-closed: a control plane that never answers leaves
  the sandbox without egress and says so in the log, and a 500 keeps the last
  policy that parsed rather than widening to "no policy".

### Fixed

- **A broad `work_dir` silently re-opened every hardlock.** The permission gate
  tested `work_dir` before the hardlock list, and `work_dir` defaults to the
  directory `sandrpod-agent` was started from (`-work-dir` unset →
  `os.Getwd()`). Launched from a home directory — the ordinary double-click or
  launchd case — `work_dir` was `$HOME`, so `~/.ssh`, `~/.aws` and
  `~/.sandrpod/permissions.json` all matched the silent-allow branch first: none
  of the seeded hardlocks ever ran, and the gate could rewrite its own rules
  through the file API. The hardlock branch now runs before every allow path,
  and the agent logs a warning when its `work_dir` contains hardlocked paths
  (with the advice to scope it with `-work-dir`). Regression tests pin the
  order: `TestCheck_HardlockBeatsWorkDir`,
  `TestCheck_HardlockBeatsWorkDir_PolicyFileNotWritable` and
  `TestHardlocksUnder`. Found while comparing our boundary with Anthropic's
  [sandbox-runtime](https://github.com/anthropics/sandbox-runtime), which makes
  the same invariant explicit — a mandatory deny applies inside an allowed
  root. See `docs/SANDBOX_RUNTIME_ANALYSIS.md` §4.5.

## [0.5.5] — 2026-08

The second half of the leak 0.5.4 started on: same mistake, different resource,
and this one shows up in none of the places that caught the first.

### Fixed
- **The tunnel leaked a yamux stream per completed streaming request.**
  `StreamClient()` built a fresh `http.Transport` on every call and both call
  sites discarded it — the same mistake as the tray's in 0.5.4, on a different
  resource. A response that finishes cleanly is read to EOF, which returns its
  stream to the discarded Transport's idle pool instead of closing it, and
  nothing ever collects it. One stream stranded at each end per successful
  `/stream` or MCP proxy request, for the life of the tunnel — invisible to
  `lsof`, so the signals that caught the tray bug would never have shown this
  one. Watch goroutine count and RSS on a long-uptime server instead. Reported
  in #28.

### Changed
- CI checks `gofmt`. Four files had been unformatted for an unknown length of
  time — `vet`, `build` and `test` have no opinion about formatting, so nothing
  was going to report it.

## [0.5.4] — 2026-07

### Fixed
- **The tray leaked a file descriptor pair every ten seconds.** Its MCP admin
  client built a fresh `http.Transport` per call, and a discarded Transport does
  not close the idle connections left in its pool — the garbage collector will
  not do it for you. The menu refreshes on a ten-second ticker, so this cost
  ~8,600 descriptors a day on each side. Observed in the wild after 6 days 19
  hours: 34,315 descriptors held by the tray and 34,314 by the agent, 57% of the
  machine's file table between them, which starved unrelated software — Docker
  could no longer open files for bind mounts, and containers reported imports as
  missing rather than as failures. The client is now a single instance for the
  process, with a bounded idle pool. The agent side was never at fault: it
  releases its half as soon as the peer goes away.

## [0.5.3] — 2026-07

Everything here was found by running real clients against a real deployment —
the unmodified E2B SDK over wildcard TLS, `sandrpod-cli` end to end, and
deepagents through both of its integration paths. None of it showed up in unit
tests, and three of the four failed *silently*, returning something that looked
like an answer.

### Added
- **`scripts/cli-sweep.py`** — an acceptance sweep for `sandrpod-cli` against a
  live deployment, shelling out to the real binary so what is tested is what a
  user types. Six commands are
  39 checks covering **all 55 commands** — server, tokens, lifecycle, exec, the
  stateful kernel, sessions, the filesystem, port previews, async jobs, the
  interactive PTY, directory watching, snapshots, MCP and local config. Each
  check declares which commands it exercises, and the run reconciles that
  against `sandrpod-cli --help`, so a command added later shows up as uncovered
  instead of quietly never being run. The sandbox it creates is torn down in a `finally`, so
  an interrupted run — Ctrl-C, or piping the output through `head` — does not
  leave one running, and `snapshot` writes a fixed tag so repeated runs
  overwrite one image instead of accumulating them on the worker.

### Fixed
- **The sandbox image carries GNU grep and findutils**, not just BusyBox's.
  Agent frameworks build GNU-flavoured commands: deepagents' default sandbox
  backend greps with `grep -rHnFZ`, BusyBox rejects `-Z`, and the command is
  wrapped in `2>/dev/null || true` — so the failure was silent and the agent was
  told the file contained nothing rather than that grep had broken. A wrong
  answer delivered confidently is worse than an error.
- **A missing working directory says so.** `os/exec` reports an absent `Dir` as
  ENOENT against the *binary*, so starting a process with a `cwd` that does not
  exist failed with `fork/exec /bin/bash: no such file or directory` — sending
  you to look for a shell that was right there. Now: `working directory
  "/home/user" is not usable: …`. Reached easily: an agent framework whose
  default workdir is `/home/user` hits it on its first call.
- **`sandrpod-cli fs ls` takes its path positionally**, like every other `fs`
  subcommand (`fs cat NAME PATH`, `fs mkdir NAME PATH`, …). It was the one that
  required `--path`, so the obvious `fs ls my-sandbox /workspace` failed with a
  usage error. `--path` still works. Released to PyPI as `sandrpod-cli` 0.2.5.
- **Enabling the E2B surface no longer takes the whole domain.** The host router
  matched `*.<domain>` and handed all of it to the compatibility gateway, so
  with `SANDRPOD_E2B_DOMAIN` set there was no hostname left for the native API —
  `sandrpod-cli`, the REST API and the SDKs 404'd everywhere under the domain
  and could only be reached over an SSH tunnel to the loopback port. The gateway
  now takes the sandbox hosts `<port>-<sandboxID>.<domain>` whole, and on
  `api.<domain>` only its own paths (`/sandboxes`, `/templates`, `/snapshots`,
  `/volumes`) — which do not overlap the native `/api/v1/*`, `/health`,
  `/metrics` and `/ws/*`. Both surfaces therefore share the one hostname, and
  `sandrpod-cli` and the SDKs keep working exactly as before. Compatibility for
  existing E2B users no longer costs you your own API, or a second hostname.

## [0.5.2] — 2026-07

Three E2B-compatibility fixes, all of them only reachable in **domain mode**
(`SANDRPOD_E2B_DOMAIN`). The earlier verification ran against the plain-HTTP
debug listener, where requests never traverse the `<port>-<sandboxID>.<domain>`
host router — so none of these could show up there.

### Fixed
- **`Sandbox.is_running()` always returned `False`.** The SDK probes `GET
  /health` on the envd host and reads 502 as "not running". `/health` is not an
  envd RPC path, so it fell through to the generic port proxy, which dialled
  `127.0.0.1:49983` inside the container, found nothing listening and returned
  502 — for a perfectly healthy sandbox. After `kill()` the probe now returns
  502 rather than 401, so `is_running()` returns `False` instead of raising:
  the per-sandbox envd token dies with the sandbox it names.
- **Code-interpreter contexts were unusable** (`create_code_context`,
  `list_code_contexts`, `restart_code_context`, `remove_code_context` all 401).
  The SDK sends `X-Access-Token` on `/execute` but no credential at all on
  `/contexts`. Accepted now on the code-interpreter port only, for `/contexts`
  only, and only when the sandbox is named by the Host — never by the
  caller-supplied `E2b-Sandbox-Id` header.

### Changed
- **`get_host(port)` URLs are fetchable without an API key**, matching E2B:
  possession of the unguessable `<port>-<sandboxID>.<domain>` hostname is the
  capability. Requiring a header made the common case — opening the dev server
  running in your sandbox, pointing a webhook at it — impossible rather than
  inconvenient, since a browser cannot attach one. `SANDRPOD_E2B_PRIVATE_PORTS=1`
  restores the previous behaviour. The envd RPC surface (filesystem, process,
  PTY) and the code interpreter authenticate either way.

## [0.5.1] — 2026-07

### Fixed
- **The Poder now negotiates the Docker API version with the daemon**
  instead of demanding whatever version its SDK was built against. Against
  any daemon older than the SDK (Docker 25.x and earlier report a maximum
  of API 1.45, while the vendored SDK speaks 1.51) every call failed with
  `client version 1.51 is too new`, so the Poder connected and heartbeated
  but could never list or create a sandbox. `DOCKER_API_VERSION` still
  overrides the negotiated value when it is set.

## [0.5.0] — 2026-07

### Security
- **Sandbox ownership is enforced on the E2B data plane** (envd, code
  interpreter, and the generic port proxy) — previously only the control
  plane checked ownership, so a valid key holder who learned another
  tenant's sandbox ID could reach it. Agent-connect and admin-create now
  stamp `Owner` so no sandbox is world-visible.
- **Platform credentials are stripped before proxying to workers**
  (`X-Sandrpod-Token` always; `Authorization` except on the MCP path,
  which carries the per-sandbox `mcp_token`) — a malicious worker can no
  longer capture the platform admin token from forwarded requests.
- **The tray settings server is token-gated** (per-session token via
  launch URL, constant-time compare) — closes a localhost port-proxy
  pivot from inside a sandbox.
- **`/procmgr/start` and session exec go through the command gate**
  (deny-scan + audit, same as `/process`); `/code-interpreter/execute` is
  audit-only by design (token scanning of arbitrary source both
  false-positives and is trivially bypassed — documented honestly in
  PERMISSION_AND_AUDIT.md).
- **Loud startup warning when authentication is disabled** — a token-less
  server runs as anonymous admin; that mode is now impossible to miss.
- Hardening bundle: permission-gate session-grant leak, `session_id` path
  traversal, OAuth-callback reflected XSS + state-TTL, seeded hardlocks
  for sandrpod's own security state, widened MCP sensitive-tool keywords,
  `/files/find` result caps, loopback-only OAuth callback, Aliyun
  instance-ID JSON escaping.

### Added
- **E2B MCP Gateway compatibility**: in-sandbox `mcp-gateway` shim
  (`:50005`, Streamable-HTTP + Bearer token) plus generic per-port
  subdomain routing (`<port>-<sandbox-id>.<domain>`).
- **Native OAuth for remote MCP servers** (`"auth": "oauth"` in mcp.json):
  child parks in `waiting_auth`, agent opens the system browser, loopback
  callback exchanges the code (PKCE + dynamic client registration), token
  persisted 0600 and auto-refreshed. Verified end-to-end against Notion's
  hosted MCP.
- `sandrpod-cli mcp` command group (`ls` / `add` / `rm` / `url` / `tools`)
  and matching `mcp_*` methods in the Python SDK.
- MCP permission gate improvements: `-mcp-grant-scope server|tool`
  (server-wide grants by default), real session grants, `server:*`
  wildcards, and grants hot-reload — hand edits and revocations apply
  without an agent restart.
- `SANDRPOD_BRAND` env to white-label the tray and consent-dialog strings.

### Fixed
- Agents on non-UTF-8 Windows (GBK `cmd /c ver` output) failed to persist
  on PostgreSQL and appeared OFFLINE; the server now sanitizes runtime
  strings and no longer swallows store errors.
- Windows tray icon rendered empty (placeholder image + Windows' ICO
  requirement); real per-platform icons are embedded now.
- A corrupt `mcp_grants.json` disabled the permission gate open (allow-all);
  it now degrades to prompt-for-everything.
- Release binaries could not report their version (`-X main.version`
  injected into a nonexistent variable); server/agent/poder now carry it,
  and container images receive it as a build arg.
- Chinese Windows agents (GBK `ver` output) failed to persist on
  PostgreSQL and appeared OFFLINE (also listed above — shipped in this
  release).

### Changed
- **LICENSE is now the canonical Apache-2.0 text** (the previous file was
  a paraphrase that license detectors could not recognize); the Python
  packages' metadata says Apache-2.0 accordingly.
- Default container images point at the published `ghcr.io/sandrpod/*`
  names everywhere (code defaults, compose, docs); the release workflow
  also publishes the `server` image, and image builds cross-compile
  instead of running under QEMU.
- Docs accuracy sweep for open-sourcing: E2B compatibility documented as
  shipped/verified, provisioning guides reflect idle reclamation and
  SSH-key persistence, architecture doc brought to v0.4 reality.

## [0.4.0] — 2026-07

### Added
- **E2B wire-protocol compatibility**: the unmodified E2B SDK works as a
  drop-in — control-plane REST, envd filesystem/process surface, stateful
  code-interpreter contexts with chart extraction, PTY, commands
  (background/stream/stdin/kill), snapshots, pause/resume, metrics,
  watch_dir — behind a config-driven provider and per-sandbox subdomain
  routing.
- **PostgreSQL backend + multi-instance LOAD mode**: one dialect-neutral
  SQL store targets SQLite or PostgreSQL from the same code; N active
  server instances share a database, claim jobs via
  `FOR UPDATE SKIP LOCKED`, and forward requests cross-node to the
  instance holding a sandbox's tunnel.
- API token issuance and management (`sandrpod-cli token create/list/rm`,
  hash-at-rest, hot reload), Prometheus `/metrics`.

## [0.3.0] — 2026-06

### Added
- Cloud coverage grew to **8 providers** — AWS, Aliyun, Azure, GCP,
  Tencent, DigitalOcean, Hetzner, Oracle — over two remote-exec backends
  (managed run-command APIs, or SSH with per-VM ephemeral keys).
- Sandbox lifecycle: idle-TTL reclamation, async create with queryable
  jobs, per-sandbox CPU/memory limits, snapshots (`docker commit`),
  preview port proxy, interactive PTY shell through the tunnel.
- TypeScript SDK MVP and web console MVP.

## [0.2.0] — 2026-05

### Added
- **Employee-PC mode**: opt-in permission gate (work_dir → hardlock →
  permanent → session → ask), native consent dialogs on macOS / Linux /
  Windows, NDJSON audit log with at-least-once central upload, and the
  `sandrpod-tray` companion (menu-bar UI, local settings page, IPC over a
  unix socket).
- **MCP transport bridge**: aggregate N stdio/remote MCP servers from a
  standard `mcp.json` into one Streamable-HTTP `/mcp` endpoint — locally
  (`--mcp-only`) or through the sandbox tunnel; hot reload, per-tool
  allow/deny lists, and two-layer auth (platform header + optional
  per-sandbox `mcp_token`).

## [0.1.0] — 2026-04

### Added
- Initial release: API Server control plane; Poder worker managing Docker
  sandbox lifecycles; Toolbox in-sandbox execution service (exec, PTY,
  files, sessions); WebSocket + yamux reverse tunnel (zero inbound ports
  on workers); `sandrpod-agent` direct-machine mode; AWS and Aliyun
  providers; Python SDK (`langchain-sandrpod`) and `sandrpod-cli`.
