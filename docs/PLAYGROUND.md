# Playground: verifying every layer by hand

A runnable walkthrough of what SandrPod now enforces — the permission gate, the
OS sandbox, the network namespace, the egress policy, and credential masking —
with the output each step produces.

It is deliberately manual. The automated suites (`go test ./pkg/egress`,
`./pkg/sandbox`, and the Docker-backed tests below) already prove the mechanisms;
this page is for the other question: **does the assembled product do what the
docs say, on my machine, end to end?** Every command here has been run: the
"expected" snippets are real output, not intentions.

Two tracks:

- **A — container sandbox (poder)**: fleet behaviour, egress in a real Docker
  network.
- **B — employee machine (agent)**: the full stack (gate, kernel, netns, masking)
  inside a privileged Linux container, which is the closest thing to a laptop
  without being one.

> Everything runs locally and cleans up after itself. Nothing here touches your
> real `~/.sandrpod`, because the agent in track B uses a container's `/root`.

---

## 0. One command instead of §B

Track B is scripted — same checks, same expectations, exits non-zero with the
failing assertion named:

```bash
./hack/playground/run.sh          # PORT=18080 by default; KEEP=1 leaves it up
```

It runs nightly and on pull requests that touch this area
(`.github/workflows/playground.yml`), because it needs `--privileged` and two
containers while the unit suite needs neither. It is slower and it is worth it:
its first run found four bugs the unit tests had passed through, all of which
only appear once the layers are wired together.

The rest of this page is the same walkthrough by hand, which is what you want
when a check fails: the script tells you *that* something broke, this tells you
*what it should look like*.

## 0. Prerequisites

```bash
docker --version          # track A and B
go version                # to build the binaries you will run
```

Build the pieces once:

```bash
mkdir -p /tmp/pg
# the agent and the playground upstream, built for the Linux container
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/pg/pg-agent-linux ./cmd/agent
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/pg/pg-echo-linux  ./hack/playground/echo
# on amd64 machines, use GOARCH=amd64 above and below
```

The upstream in `hack/playground/echo` is a 100-line TLS server that prints the
`Authorization` header it received. It exists so the masking check does not
depend on some third party's echo service.

---

## A. Container sandbox: the egress wall

### A.1 Start the control plane

```bash
go run ./cmd/server -port 18080 -db sqlite:/tmp/pg/sandrpod.db
```

Expected: `API server listening on :18080`.

### A.2 Start a worker

```bash
docker run -d --name pg-poder --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e API_URL=http://host.docker.internal:18080 \
  -e SANDRPOD_TOOLBOX_IMAGE=ghcr.io/sandrpod/toolbox:latest \
  --add-host=host.docker.internal:host-gateway \
  ghcr.io/sandrpod/poder:latest
```

Expected: it registers and polls for jobs.

**Start it in Docker, not with `go run`, on macOS.** A host-run poder cannot
reach a container's bridge IP (Docker Desktop runs the daemon inside a VM), so
every `execute` sits for the whole timeout and then answers 500/503 while the
sandbox looks perfectly RUNNING. On Linux either form works.

### A.3 Create a sandbox with a policy

```bash
curl -s -X POST http://127.0.0.1:18080/api/v1/sandboxes \
  -H 'Content-Type: application/json' -d '{
    "name": "walled", "provider": "local",
    "network": {
      "allowed_domains": ["example.com"],
      "denied_domains": ["*.blocked.test"],
      "denied_domain_reasons": {"*.blocked.test": "blocked on purpose by the playground"}
    }
  }'
```

Expected: a job, then `walled` reaches `RUNNING`. Behind it, poder created

- a docker network `sandrpod-egress-walled` with `Internal: true`,
- an `egress-proxy` sidecar on it,
- the sandbox attached **only** to that internal network.

### A.4 Check the three answers

```bash
curl -s -X POST "http://127.0.0.1:18080/api/v1/sandboxes/execute?sandbox=walled" \
  -H 'Content-Type: application/json' \
  -d '{"language":"bash","code":"curl -s -o /dev/null -w \"allow=%{http_code}\\n\" https://example.com/; curl -s -o /dev/null -w \"deny=%{http_code}\\n\" https://blocked.test/; curl -s --noproxy \"*\" -o /dev/null -w \"direct=%{http_code}\\n\" https://example.com/","timeout":30}'
```

Expected, in order:

```
allow=200
deny=000          # the proxy answered 403 to the CONNECT; curl reports a failed tunnel
direct=000        # no route at all: the container is on an internal network
```

The deny is easier to read with `-v`, which shows the proxy's answer:

```
< HTTP/1.1 403 Forbidden
< X-Deny-Reason: denied_domain
```

and the body carries the operator's sentence from `denied_domain_reasons`.

### A.5 Clean up

```bash
curl -s -X DELETE http://127.0.0.1:18080/api/v1/sandboxes/walled
docker network ls | grep sandrpod            # nothing left
docker ps -a | grep sandrpod-egress          # nothing left
```

---

## B. Employee machine: the full stack in a container

This runs the real `sandrpod-agent` inside a privileged Linux container, with
bubblewrap, so all five layers are active: permission gate, kernel sandbox,
network namespace, egress policy, credential masking.

### B.1 The upstream

```bash
/tmp/pg/pg-echo-linux -gen-ca /tmp/pg/certs          # if you have not already
docker network create pg-net
docker run -d --name pg-echo --network pg-net \
  -v /tmp/pg:/pg -w /pg alpine:3.20 \
  sh -c '/pg/pg-echo-linux -addr :8443 -cert /pg/cert.pem -key /pg/key.pem'
docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' pg-echo
# note the address, e.g. 172.22.0.2 — it goes in /etc/hosts below
```

### B.2 Prepare the agent's world

```bash
mkdir -p /tmp/pg/work /tmp/pg/audit
printf 'PRIVATE-KEY-SECRET\n' > /dev/null   # (created inside the container, next step)

cat > /tmp/pg/egress.json <<'EOF'
{
  "allowed_domains": ["echo.local:8443", "example.com"],
  "denied_domains": ["*.blocked.test"],
  "denied_domain_reasons": {"*.blocked.test": "blocked on purpose by the playground"}
}
EOF

cat > /tmp/pg/agent-run.sh <<'EOF'
#!/bin/sh
set -e
apk add -q bubblewrap ca-certificates bash curl >/dev/null
cp /pg/ca.pem /usr/local/share/ca-certificates/playground.crt
update-ca-certificates >/dev/null 2>&1          # the proxy must trust our upstream
echo "<ECHO_IP> echo.local" >> /etc/hosts
mkdir -p /root/.ssh /pg/work/.git/hooks
printf 'PRIVATE-KEY-SECRET\n' > /root/.ssh/id_rsa
printf '#!/bin/sh\necho ok\n' > /pg/work/.git/hooks/pre-commit
printf 'TOP-SECRET\n'         > /pg/work/.git/config
cd /pg/work
exec /pg/pg-agent-linux \
  -api-url=http://host.docker.internal:18080 -name=playground \
  -permission-mode=strict -permission-file=/pg/permissions.json \
  -work-dir=/pg/work \
  -os-sandbox=bwrap \
  -egress-mode=enforce -egress-policy=/pg/egress.json \
  -credential-mask=OPENAI_API_KEY \
  -credential-terminate-ports=443,8443 \
  -audit-dir=/pg/audit
EOF
sed -i '' "s/<ECHO_IP>/$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' pg-echo)/" /tmp/pg/agent-run.sh
# on Linux: use `sed -i` without the empty -i argument
```

`-permission-mode=strict` is used because there is nobody to answer a consent
dialog in a container; the gate then denies anything not in `work_dir`.

### B.3 Start it

```bash
docker run -d --name pg-agent --privileged --network pg-net \
  --add-host=host.docker.internal:host-gateway \
  -e OPENAI_API_KEY=sk-real-playground-key \
  -v /tmp/pg:/pg alpine:3.20 sh /pg/agent-run.sh
sleep 15 && docker logs pg-agent | tail -20
```

Expected — the four layers announce themselves:

```
egress: credential masking on for [OPENAI_API_KEY]; TLS is terminated for allowed hosts except []
egress: enforcing from /pg/egress.json (unix=/tmp/sandrpod-egress-…/proxy.sock relay=127.0.0.1:3128)
os sandbox: network namespace removed — the relay at 127.0.0.1:3128 is the only route out
os sandbox: bwrap active — work_dir=/pg/work writable, 15 hard-locked path(s) masked, 2 persistence path(s) read-only
os sandbox:   masked ~/.ssh
…
Connected — sandbox "playground" is online
```

`2 persistence path(s)` are `.git/hooks` and `.git/config`, found by the
mandatory-deny walk.

### B.4 Run the checks

A helper, so each check is one line:

```bash
cat > /tmp/pg/exec.sh <<'EOF'
#!/bin/sh
curl -s -X POST "http://127.0.0.1:18080/api/v1/sandboxes/execute?sandbox=playground" \
  -H 'Content-Type: application/json' \
  -d "$(python3 -c 'import json,sys; print(json.dumps({"language":"bash","code":sys.argv[1],"timeout":30}))' "$1")" \
| python3 -c 'import json,sys; d=json.load(sys.stdin); print("exit:",d["exit_code"]); print(d["stdout"]); print(d["stderr"],end="")'
EOF
chmod +x /tmp/pg/exec.sh
```

#### B.4.1 Network: allowed, denied, and absent

```bash
/tmp/pg/exec.sh 'curl -s -o /dev/null -w "allow=%{http_code}\n" https://example.com/;
                 curl -s -o /dev/null -w "deny=%{http_code}\n"  https://blocked.test/;
                 curl -s --noproxy "*" https://example.com/'
```

Observed:

```
allow=200
deny=000
curl: (6) Could not resolve host: example.com
```

The last line is the network namespace: with `--noproxy`, there is no DNS server
and no route, so the failure is resolution, not a refused connection.

#### B.4.2 Kernel floor: hardlocks and persistence

```bash
/tmp/pg/exec.sh 'cat ~/.ssh/id_rsa; echo "---"; echo pwned >> .git/hooks/pre-commit; echo "---"; wc -c < .git/config'
```

Observed:

```
cat: /root/.ssh/id_rsa: No such file or directory
---
bash: .git/hooks/pre-commit: Read-only file system
---
11
```

Read it carefully:

- `~/.ssh` is replaced by an **empty tmpfs**, so the file is not merely
  unreadable, it is not there;
- `.git/hooks` is read-only, so the write fails;
- `.git/config` still reads (11 bytes) — mandatory deny protects against
  *writing* persistence, not against reading a repository's config.

#### B.4.3 Permission gate: a structured refusal

The gate is what the file API uses (as opposed to a shell working around it):

```bash
curl -s "http://127.0.0.1:18080/api/v1/sandboxes/playground/toolbox/files?path=/root/.ssh"
```

Observed (HTTP 403):

```
access denied: /root/.ssh is hard-locked by the operator's policy and cannot be
read or written; do not retry, and ask the user to unlock it in the tray if this
is genuinely needed
```

That sentence is `ModelReason` — what the agent is told. The person at the
machine sees the localized `Reason` in the dialog and in the audit record.

#### B.4.4 Credential masking: the real value arrives upstream

```bash
/tmp/pg/exec.sh 'echo "sentinel: $OPENAI_API_KEY";
                 curl -s https://echo.local:8443/v1/chat -H "Authorization: Bearer $OPENAI_API_KEY"'
```

Observed:

```
sentinel: fake_value_3281d413-bb82-44b1-a4b0-99e5f3b956be
{"authorization":"Bearer sk-real-playground-key","host":"echo.local:8443","path":"/v1/chat"}
```

Two lines, two facts: the sandbox only ever held a sentinel, and the request that
reached the upstream carried the real key. The upstream's own log says the same:

```bash
docker logs pg-echo | tail -1
# upstream: GET /v1/chat Authorization="Bearer sk-real-playground-key"
```

Nothing in the sandbox's output contains `sk-real-playground-key`.

#### B.4.5 Cleartext refusal

```bash
/tmp/pg/exec.sh 'curl -s -D - -o /dev/null http://example.com/ -H "Authorization: Bearer $OPENAI_API_KEY" | head -3'
```

Observed:

```
HTTP/1.1 403 Forbidden
X-Deny-Reason: masked_credential_cleartext
```

#### B.4.6 The audit trail

```bash
python3 - <<'PY'
import json
for line in open('/tmp/pg/audit/active.log'):
    d = json.loads(line)
    if d.get("source") == "egress":
        print(d.get("host"), d.get("decision"), "terminated=", d.get("terminated"), d.get("credentials"))
PY
```

Observed:

```
blocked.test deny terminated= None None
echo.local allow terminated= None None
echo.local allow terminated= True ['OPENAI_API_KEY']
```

The first `echo.local` line is the tunnel being opened; the second records the
substitution: the connection was terminated and `OPENAI_API_KEY` was placed in
it. Only the name is recorded — never the value.

> Credential use is logged even when allowed-logging is off: "which secret went
> to which host" is the question this feature exists to answer.

### B.5 Clean up

```bash
docker rm -f pg-agent pg-echo
docker network rm pg-net
rm -rf /tmp/pg
```

---

## B'. Fleet check: a CLI-created policy, enforced in a container

The last section of `hack/playground/run.sh` covers the other deployment: a
container sandbox whose policy was set through the CLI. It builds the worker and
the egress sidecar **from the tree under test**, creates a sandbox with
`--allow-domain example.com --deny-domain blocked.test`, and asserts from inside
the container that the allowed host answers 200 while the denied one is refused
403 with `X-Deny-Reason: denied_domain`.

It also asserts a **create-latency budget** (under 1.5s): the number every agent
loop waits for, and one that regressed to 4.2s without anyone noticing because
the pode pulled the container image on every create. A timing that is not
asserted is a timing that drifts.

It is worth its own paragraph because its first version failed, and the failure
was the interesting part: with the *published* `poder:latest` image (2026-08-30,
predating the egress work) the sandbox came up `RUNNING`, the policy read back
correctly from the API, and the container ran **completely unwalled** — no proxy
env, no internal network, no sidecar. That is the mixed-version hazard from the
release notes, demonstrated on a real machine. The phase now builds both images
from source for exactly that reason, and skips (with a message) if the toolbox
image is not pullable.

## B''. Mixed-version guard

The last section emulates the *older* builds and asserts the two refusals that
keep a mixed fleet honest:

```
== legacy: the two refusals that guard a mixed-version fleet ==
PASS an agent refuses a control plane without the capability route
PASS a policy on a legacy worker is refused with 409
PASS …and the message names the capability
PASS a sandbox without a policy still runs on a legacy worker
```

It starts a second server with `-emulate-legacy` (which answers 404 to
`/api/v1/capabilities`, exactly as a build from before it does) and a worker
container with `-emulate-legacy` (which advertises nothing). Both flags only
*remove* what the binary claims; they cannot widen anything, and both log a
warning at startup. The point is that the guard is re-proven every night without
anyone keeping an old binary around — the failure they prevent is silent (a
sandbox reported `RUNNING` with no route policy), which is exactly the kind that
a test has to hold down.

## C. What this walkthrough proved

| Layer | Check | Evidence |
|---|---|---|
| Permission gate | B.4.3 | 403 + model-facing reason |
| Mandatory deny | B.4.2 | `Read-only file system` on `.git/hooks` |
| Hardlocks (kernel) | B.4.2 | `~/.ssh` is an empty tmpfs |
| Network namespace | B.4.1 | `Could not resolve host` without the proxy |
| Egress policy | B.4.1 / A.4 | 200 / 403 + `X-Deny-Reason` |
| Container wall | A.4 | no route at all outside the sidecar |
| Credential masking | B.4.4 | sentinel in the sandbox, real key upstream |
| Cleartext refusal | B.4.5 | `masked_credential_cleartext` |
| Audit | B.4.6 | `terminated=True`, credential names only |

## D. If something does not match

- **`fork/exec bwrap: no such file or directory`** — the sandbox binary was not
  resolved to an absolute path (fixed in `pkg/sandbox.Resolve`); rebuild the
  agent.
- **`curl: (7) Failed to connect to 127.0.0.1:3128`** — the relay is not up; it
  is started by the sandbox wrapper and waits for a ready file. Check the
  command's stderr for `relay:` lines.
- **The upstream sees a sentinel** — the destination's port is not terminated.
  Add it to `-credential-terminate-ports`, or the host is in
  `-credential-terminate-exclude`.
- **`x509: certificate signed by unknown authority` inside the sandbox** — the
  trust bundle. Check `SSL_CERT_FILE` points at
  `~/.sandrpod/egress-ca/trust-bundle.pem` and that the file exists.
- **Everything allowed, no denials** — the policy file was not loaded. Look for
  `egress: enforcing from …` at startup; without it, egress is off.

For the automated equivalents: `go test ./pkg/egress -run 'Masking|ResignAWS'`,
`go test ./pkg/permission -run 'Mandatory'`, and the Docker-backed sandbox tests
in `pkg/sandbox` (`Bwrap|Netns`), which need `--privileged`.