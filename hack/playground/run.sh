#!/usr/bin/env bash
# Copyright 2026 SandrPod Contributors
#
# The employee-machine playground, as one command: start a control plane, run a
# real agent inside a privileged Linux container, and assert every layer.
#
# It is the scripted form of docs/PLAYGROUND.md §B. The manual page explains
# each check and what its output means; this file only decides pass or fail, so
# it can run unattended (nightly) and catch the class of bug that only appears
# when the layers are assembled — the first run of the manual version found four.
#
# Usage:  hack/playground/run.sh
#   PORT=18080            API port (default 18080; must be free)
#   KEEP=1                leave containers and network behind for inspection
#   SANDRPOD_PG_IMAGE     base image (default alpine:3.20)
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT=$(pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/sandrpod-pg.XXXXXX")
PORT=${PORT:-18080}
IMAGE=${SANDRPOD_PG_IMAGE:-alpine:3.20}
API="http://127.0.0.1:${PORT}"
NET=sandrpod-pg-net
AGENT=sandrpod-pg-agent
ECHO=sandrpod-pg-echo
SERVER_PID=""
FAILED=0

log()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
pass() { printf '   \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '   \033[31mFAIL\033[0m %s\n' "$*"; FAILED=1; }

# check <description> <expected> <actual>
check() {
	if [ "$2" = "$3" ]; then
		pass "$1"
	else
		fail "$1 (expected [$2], got [$3])"
	fi
}

contains() {
	case "$3" in
	*"$2"*) pass "$1" ;;
	*) fail "$1 (expected to contain [$2], got [$3])" ;;
	esac
}

cleanup() {
	local code=$?
	if [ "${KEEP:-0}" = "1" ]; then
		printf '\nKEEP=1: leaving %s %s and network %s up; logs in %s\n' "$AGENT" "$ECHO" "$NET" "$WORK"
		return
	fi
	docker rm -f "$AGENT" "$ECHO" >/dev/null 2>&1
	docker network rm "$NET" >/dev/null 2>&1
	[ -n "$SERVER_PID" ] && kill "$SERVER_PID" >/dev/null 2>&1
	rm -rf "$WORK"
	exit "$code"
}
trap cleanup EXIT INT TERM

# ── 0. prerequisites ────────────────────────────────────────────────────────
log "prerequisites"
command -v docker >/dev/null || { echo "docker is required"; exit 1; }
command -v go >/dev/null || { echo "go is required"; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required"; exit 1; }
docker info >/dev/null 2>&1 || { echo "the docker daemon is not reachable"; exit 1; }

case "$(docker version --format '{{.Server.Arch}}' 2>/dev/null)" in
arm64 | aarch64) GOARCH=arm64 ;;
amd64 | x86_64) GOARCH=amd64 ;;
*) GOARCH=$(go env GOARCH) ;;
esac
info "docker arch: $GOARCH, work dir: $WORK"

if curl -s -o /dev/null --max-time 1 "$API/api/v1/sandboxes"; then
	echo "port $PORT is already in use (set PORT=...)"
	exit 1
fi

# ── 1. build ────────────────────────────────────────────────────────────────
log "build (linux/$GOARCH)"
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o "$WORK/agent" ./cmd/agent || exit 1
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o "$WORK/echo" ./hack/playground/echo || exit 1
go build -o "$WORK/server" ./cmd/server || exit 1
# A host build for -gen-ca: the linux binaries cannot run here when the host is
# not linux.
go build -o "$WORK/echo-host" ./hack/playground/echo || exit 1
"$WORK/echo-host" -gen-ca "$WORK/certs" >/dev/null || exit 1
chmod +x "$WORK/agent" "$WORK/echo"

# ── 2. control plane + upstream ─────────────────────────────────────────────
log "start the control plane and the upstream"
"$WORK/server" -port "$PORT" -db "sqlite:$WORK/sandrpod.db" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 60); do
	curl -s -o /dev/null "$API/api/v1/sandboxes" && break
	sleep 0.5
done
curl -s -o /dev/null "$API/api/v1/sandboxes" || { cat "$WORK/server.log"; exit 1; }
info "control plane up on :$PORT"

docker network create "$NET" >/dev/null || exit 1
docker run -d --name "$ECHO" --network "$NET" -v "$WORK:/pg" -w /pg "$IMAGE" \
	sh -c '/pg/echo -addr :8443 -cert /pg/certs/cert.pem -key /pg/certs/key.pem' >/dev/null || exit 1
ECHO_IP=$(docker inspect -f "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}" "$ECHO")
[ -n "$ECHO_IP" ] || { echo "the upstream has no address"; exit 1; }
info "upstream at $ECHO_IP:8443"

# ── 3. the agent, with every layer on ───────────────────────────────────────
log "start the agent (gate + kernel + netns + egress + masking)"
mkdir -p "$WORK/work" "$WORK/audit"
cat >"$WORK/egress.json" <<'JSON'
{
  "allowed_domains": ["echo.local:8443", "example.com"],
  "denied_domains": ["*.blocked.test"],
  "denied_domain_reasons": {"*.blocked.test": "blocked on purpose by the playground"}
}
JSON
cat >"$WORK/agent-run.sh" <<'SH'
#!/bin/sh
set -e
apk add -q bubblewrap ca-certificates bash curl >/dev/null
cp /pg/certs/ca.pem /usr/local/share/ca-certificates/playground.crt
update-ca-certificates >/dev/null 2>&1
echo "$ECHO_IP echo.local" >> /etc/hosts
mkdir -p /root/.ssh /pg/work/.git/hooks
printf 'PRIVATE-KEY-SECRET\n' > /root/.ssh/id_rsa
printf '#!/bin/sh\necho ok\n' > /pg/work/.git/hooks/pre-commit
printf 'TOP-SECRET\n' > /pg/work/.git/config
cd /pg/work
echo "agent: pwd=$(pwd) HOME=${HOME:-unset} workdir=$(pwd)" >&2
exec /pg/agent \
  -api-url="http://host.docker.internal:$PORT" -name=playground \
  -permission-mode=strict -permission-file=/pg/permissions.json \
  -work-dir=/pg/work \
  -os-sandbox=bwrap \
  -egress-mode=enforce -egress-policy=/pg/egress.json \
  -credential-mask=OPENAI_API_KEY \
  -credential-terminate-ports=443,8443 \
  -audit-dir=/pg/audit
SH
chmod +x "$WORK/agent-run.sh"

docker run -d --name "$AGENT" --privileged --network "$NET" \
	--add-host=host.docker.internal:host-gateway \
	-e OPENAI_API_KEY=sk-real-playground-key \
	-e ECHO_IP="$ECHO_IP" -e PORT="$PORT" \
	-v "$WORK:/pg" "$IMAGE" sh /pg/agent-run.sh >/dev/null || exit 1

for _ in $(seq 1 90); do
	docker logs "$AGENT" 2>&1 | grep -q "is online" && break
	sleep 1
done
AGENT_LOG=$(docker logs "$AGENT" 2>&1)
if ! printf '%s' "$AGENT_LOG" | grep -q "is online"; then
	echo "$AGENT_LOG" | tail -25
	exit 1
fi

log "the four layers announce themselves"
for want in \
	"credential masking on for" \
	"network namespace removed" \
	"bwrap active" \
	"2 persistence path(s) read-only"; do
	contains "startup reports: $want" "$want" "$AGENT_LOG"
done
contains "startup masks ~/.ssh" "masked ~/.ssh" "$AGENT_LOG"

# ── 4. run code in the sandbox ──────────────────────────────────────────────
run_code() { # run_code <code> -> prints stdout, then stderr on failure
	python3 - "$API" "$1" <<'PY'
import json, sys, urllib.request
api, code = sys.argv[1], sys.argv[2]
req = urllib.request.Request(
    api + "/api/v1/sandboxes/execute?sandbox=playground",
    data=json.dumps({"language": "bash", "code": code, "timeout": 60}).encode(),
    headers={"Content-Type": "application/json"}, method="POST")
with urllib.request.urlopen(req, timeout=120) as r:
    d = json.load(r)
sys.stdout.write(d.get("stdout") or "")
if d.get("exit_code") not in (0, None):
    sys.stdout.write("\n[stderr] " + (d.get("stderr") or "") + "\n")
PY
}

log "network: allowed, denied, absent"
NET_OUT=$(run_code '
printf "allow=%s\n" "$(curl -s -o /dev/null -w "%{http_code}" https://example.com/)"
printf "deny=%s\n" "$(curl -s -o /dev/null -w "%{http_code}" https://blocked.test/)"
printf "deny_code="
curl -s -D - -o /dev/null http://blocked.test/ | tr -d "\r" | sed -n "s/^X-Deny-Reason: //p"
printf "direct="
curl -sS --noproxy "*" https://example.com/ 2>&1 | head -1
')
printf '%s\n' "$NET_OUT"
contains "an allow-listed host is reachable" "allow=200" "$NET_OUT"
contains "an unlisted host is refused" "deny_code=not_allowed" "$NET_OUT"
contains "without the proxy there is no DNS" "Could not resolve host" "$NET_OUT"

log "kernel floor: hardlocks masked, persistence read-only"
KERNEL_OUT=$(run_code '
echo "cwd=$(pwd) HOME=${HOME:-unset}"; ls -a 2>&1 | head -4; ls -d /workspace 2>&1 | head -1
if cat ~/.ssh/id_rsa >/dev/null 2>&1; then echo "hardlock=READABLE"; else echo "hardlock=DENIED"; fi
if echo pwned >> .git/hooks/pre-commit 2>/dev/null; then echo "hook=WRITABLE"; else echo "hook=DENIED"; fi
if echo pwned >> .git/config 2>/dev/null; then echo "config=WRITABLE"; else echo "config=DENIED"; fi
wc -c < .git/config
')
printf '%s\n' "$KERNEL_OUT"
contains "the hard-locked key cannot be read" "hardlock=DENIED" "$KERNEL_OUT"
contains "a git hook cannot be written" "hook=DENIED" "$KERNEL_OUT"
contains ".git/config cannot be written either" "config=DENIED" "$KERNEL_OUT"
contains "but .git/config stays readable" "11" "$KERNEL_OUT"

log "permission gate: a structured refusal"
GATE_OUT=$(curl -s -w '\n%{http_code}' "$API/api/v1/sandboxes/playground/toolbox/files?path=/root/.ssh")
printf '%s\n' "$GATE_OUT" | head -3
contains "the file API refuses a hardlock" "hard-locked by the operator" "$GATE_OUT"
contains "with a 403" "403" "$GATE_OUT"

log "credential masking: sentinel inside, real value upstream"
MASK_OUT=$(run_code '
echo "sentinel: $OPENAI_API_KEY"
curl -s https://echo.local:8443/v1/chat -H "Authorization: Bearer $OPENAI_API_KEY"
')
printf '%s\n' "$MASK_OUT"
contains "the sandbox only holds a sentinel" "sentinel: fake_value_" "$MASK_OUT"
contains "the upstream received the real key" "Bearer sk-real-playground-key" "$MASK_OUT"
case "$MASK_OUT" in
*sentinel:*) : ;;
*) fail "no sentinel line at all" ;;
esac

UPSTREAM_SEEN=$(docker logs "$ECHO" 2>&1 | grep -c 'Authorization="Bearer sk-real-playground-key"')
check "the upstream logged the real key exactly once" "1" "$UPSTREAM_SEEN"

log "cleartext refusal"
CLEAR_OUT=$(run_code 'curl -s -D - -o /dev/null http://example.com/ -H "Authorization: Bearer $OPENAI_API_KEY"')
contains "a masked credential is refused over HTTP" "masked_credential_cleartext" "$CLEAR_OUT"

log "audit: which secret went where"
# Read it from inside the container: on Linux the file is written by root and
# is 0600, so the host-side runner cannot open it (and a glob over an
# unreadable directory fails silently — which is how this check passed on
# macOS and failed on CI).
AUDIT_OUT=$(docker exec "$AGENT" sh -c 'cat /pg/audit/*.log' 2>/dev/null | python3 -c '
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        d = json.loads(line)
    except json.JSONDecodeError:
        continue
    if d.get("source") == "egress" and d.get("terminated"):
        print("terminated=yes credentials=%s host=%s" % (d.get("credentials"), d.get("host")))
')
printf '%s\n' "$AUDIT_OUT"
contains "the termination is recorded with the credential name" "credentials=['OPENAI_API_KEY']" "$AUDIT_OUT"

log "result"
if [ "$FAILED" -ne 0 ]; then
	printf '\033[31mplayground FAILED\033[0m — logs kept in %s\n' "$WORK"
	KEEP=1
	exit 1
fi
printf '\033[32mplayground passed\033[0m: gate, kernel, netns, egress, masking, audit\n'