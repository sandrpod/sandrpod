#!/usr/bin/env bash
# One command that proves the multi-instance behaviour still holds.
#
# Four mechanisms have to agree for this to pass, and none of them is visible on
# a single instance or on SQLite:
#
#   1. placement on PostgreSQL              (a SQLite-only SQL function broke it once)
#   2. tunnel ownership and its refresh      (expired 90 s after a worker connected)
#   3. delivery to the peer holding the tunnel (create used to fail on the wrong node)
#   4. the outbox resume + claim             (a lost delivery is re-sent, exactly once)
#
# Usage:
#   ./hack/multi-instance-check.sh                  # starts a throwaway PostgreSQL
#   TEST_POSTGRES_DSN=postgres://… ./hack/…         # use an existing one
#   MI_KEEP=1 ./hack/…                              # leave the stack running for poking
set -euo pipefail
cd "$(dirname "$0")/.."

PORT_A=${PORT_A:-18193}
PORT_B=${PORT_B:-18194}
TOKEN=${TOKEN:-mi-check-token}
WORK=$(mktemp -d /tmp/sandrpod-mi.XXXXXX)
LOG_A="$WORK/a.log"
LOG_B="$WORK/b.log"
PG_CONTAINER="sandrpod-mi-pg"
PODE_CONTAINER="sandrpod-mi-poder"
DSN="${TEST_POSTGRES_DSN:-}"
CREATED_PG=0

pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
info() { printf '\n== %s\n' "$*"; }

cleanup() {
  local rc=$?
  if [ "${MI_KEEP:-0}" = "1" ]; then
    printf '\nkept: logs in %s, servers on %s/%s, container %s\n' "$WORK" "$PORT_A" "$PORT_B" "$PODE_CONTAINER"
    return
  fi
  # By flag value: the binary lives in a per-run temp directory, so a pattern on
  # its name leaves the previous run's instances holding the ports.
  pkill -f -- "-port $PORT_A" 2>/dev/null || true
  pkill -f -- "-port $PORT_B" 2>/dev/null || true
  docker rm -f "$PODE_CONTAINER" >/dev/null 2>&1 || true
  if [ "$CREATED_PG" = "1" ]; then
    docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT INT TERM

psql_run() { psql "$DSN" -tAc "$1"; }

info "PostgreSQL"
if [ -z "$DSN" ]; then
  command -v docker >/dev/null || fail "no TEST_POSTGRES_DSN and no docker to start one"
  docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
  docker run -d --name "$PG_CONTAINER" -e POSTGRES_PASSWORD=mi -e POSTGRES_DB=sandrpod_mi \
    -p 15432:5432 postgres:16-alpine >/dev/null
  CREATED_PG=1
  DSN="postgres://postgres:mi@127.0.0.1:15432/sandrpod_mi?sslmode=disable"
  for _ in $(seq 1 40); do
    psql_run "select 1" >/dev/null 2>&1 && break
    sleep 1
  done
fi
psql_run "select 1" >/dev/null || fail "cannot reach PostgreSQL ($DSN)"
# Start from nothing. A database left over from an earlier run has stale workers,
# and placement would then pick one whose tunnel is on neither instance — the
# check would still pass, for the wrong reason, through the resumer instead of the
# peer delivery it is meant to exercise.
psql "$DSN" -c "TRUNCATE sandboxes, poders, jobs, api_tokens, tunnel_owners" >/dev/null 2>&1   || psql "$DSN" -c "DROP TABLE IF EXISTS sandboxes, poders, jobs, api_tokens, tunnel_owners" >/dev/null 2>&1 \
  || fail "cannot reset the database"
pass "database reset and reachable"

info "build"
go build -o "$WORK/server" ./cmd/server || fail "server build"
pass "server built"

info "two instances, one database"
for p in "$PORT_A" "$PORT_B"; do
  if curl -fsS -o /dev/null --max-time 1 "http://127.0.0.1:$p/health" 2>/dev/null; then
    fail "port $p already answers /health — a stale instance would be tested instead of this build (pkill -f -- \"-port $p\")"
  fi
done
"$WORK/server" -port "$PORT_A" -db "$DSN" -token "$TOKEN" -node-url "http://127.0.0.1:$PORT_A" >"$LOG_A" 2>&1 &
"$WORK/server" -port "$PORT_B" -db "$DSN" -token "$TOKEN" -node-url "http://127.0.0.1:$PORT_B" >"$LOG_B" 2>&1 &
for p in "$PORT_A" "$PORT_B"; do
  for _ in $(seq 1 40); do
    curl -fsS -o /dev/null "http://127.0.0.1:$p/health" 2>/dev/null && break
    sleep 0.5
  done
  curl -fsS -o /dev/null "http://127.0.0.1:$p/health" || fail "instance :$p did not start (see $WORK)"
done
pass "A=:$PORT_A  B=:$PORT_B"

info "worker attached to A"
docker rm -f "$PODE_CONTAINER" >/dev/null 2>&1 || true
docker run -d --name "$PODE_CONTAINER" --network bridge \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e SANDRPOD_TOOLBOX_IMAGE="${SANDRPOD_TOOLBOX_IMAGE:-ghcr.io/sandrpod/toolbox:latest}" \
  --add-host=host.docker.internal:host-gateway \
  "${PODER_IMAGE:-ghcr.io/sandrpod/poder:latest}" \
  -api-url="http://host.docker.internal:$PORT_A" -token="$TOKEN" -region=local >/dev/null 2>&1 \
  || fail "could not start the worker container"
for _ in $(seq 1 40); do
  online=$(curl -fsS -H "X-Sandrpod-Token: $TOKEN" "http://127.0.0.1:$PORT_A/api/v1/poders" 2>/dev/null \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); print(sum(1 for p in (d.get("poders") or []) if p.get("state")=="ONLINE"))' 2>/dev/null || echo 0)
  [ "${online:-0}" -ge 1 ] && break
  sleep 1
done
[ "${online:-0}" -ge 1 ] || fail "the worker never came online on A"
PODER_ID=$(psql_run "select id from poders where state='ONLINE' limit 1")
pass "worker $PODER_ID online on A"

info "create on B (the instance WITHOUT the tunnel)"
code=$(curl -s -o "$WORK/create.json" -w '%{http_code}' -X POST "http://127.0.0.1:$PORT_B/api/v1/sandboxes" \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"mi-check-box","provider_type":"local","async":true}')
[ "$code" = "202" ] || fail "create on B answered $code ($(cat "$WORK/create.json"))"
for _ in $(seq 1 60); do
  state=$(psql_run "select state from sandboxes where name='mi-check-box'")
  [ "$state" = "RUNNING" ] && break
  sleep 2
done
[ "$state" = "RUNNING" ] || fail "sandbox never reached RUNNING (last state: ${state:-none}); see $LOG_A $LOG_B"
pass "created through B, running on A's tunnel"
# The access log has two shapes depending on the build (plain line, or structured
# with method=/path=/status=); match either rather than pinning one.
grep -Eq "path=/internal/provision status=200|POST /internal/provision 200" "$LOG_A" \
  || fail "A never received the provisioning call"
pass "delivered by the peer that holds the tunnel"

info "execute through B"
out=$(curl -fsS -X POST "http://127.0.0.1:$PORT_B/api/v1/sandboxes/execute?sandbox=mi-check-box" \
  -H "X-Sandrpod-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"language":"bash","code":"echo mi-check-ok","timeout":20}')
echo "$out" | grep -q "mi-check-ok" || fail "execute through B: $out"
pass "forwarded to A and ran"

info "outbox: a delivery that never happened is resumed, exactly once"
psql "$DSN" -c "
INSERT INTO jobs (id, type, status, sandbox_name, poder_id, request, created_at, updated_at)
VALUES ('job-mi-lost', 'CREATE_SANDBOX', 'PENDING', 'mi-lost-box', '$PODER_ID',
        '{\"name\":\"mi-lost-box\",\"provider_type\":\"local\"}',
        to_char((now() at time zone 'utc') - interval '3 minutes','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
        to_char((now() at time zone 'utc') - interval '3 minutes','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'))" >/dev/null
pass "planted a PENDING job that was never delivered"

for _ in $(seq 1 40); do
  status=$(psql_run "select status from jobs where id='job-mi-lost'")
  [ "$status" = "COMPLETED" ] && break
  sleep 2
done
[ "$status" = "COMPLETED" ] || fail "the resumer never completed the job (status: ${status:-none})"
pass "resumed and completed"

delivered=$(grep -ch "resume: delivered job job-mi-lost" "$LOG_A" "$LOG_B" | paste -sd+ - | bc)
[ "$delivered" = "1" ] || fail "delivered $delivered times across two instances, want exactly 1"
pass "claimed by exactly one instance"

printf '\n\033[32mmulti-instance check passed\033[0m\n'
