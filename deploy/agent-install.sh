#!/usr/bin/env bash
# Turn this machine into a SandrPod sandbox.
#
# Not the same thing as a worker: a worker hosts many sandbox containers and needs
# Docker; an agent IS the sandbox — the machine itself, its files, its tools, no
# container in between. Use it for a laptop, a build box, a GPU host, or a machine
# whose whole job is to be one agent's hands.
#
#   curl -fsSL https://docs.sandrpod.com/agent.sh | sudo bash -s -- \
#     --api-url=https://cloud.example.com --token=jn_… --name=my-laptop
#
# Installs the binary, writes a service, starts it, and prints what to look for.
set -euo pipefail

API_URL="${SANDRPOD_API_URL:-}"
TOKEN="${SANDRPOD_JOIN_TOKEN:-}"
NAME="${SANDRPOD_AGENT_NAME:-}"
VERSION="${SANDRPOD_VERSION:-latest}"
PREFIX="${SANDRPOD_PREFIX:-/usr/local/bin}"
DRY_RUN=0
ASSUME_YES=0

if [ -t 1 ]; then C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else C_OK=''; C_WARN=''; C_ERR=''; C_DIM=''; C_OFF=''; fi
step() { printf '\n%s==> %s%s\n' "$C_OK" "$*" "$C_OFF"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    %s! %s%s\n' "$C_WARN" "$*" "$C_OFF"; }
die()  { printf '\n%s✗ %s%s\n' "$C_ERR" "$*" "$C_OFF" >&2; exit 1; }
run()  { if [ "$DRY_RUN" = 1 ]; then printf '    %s[dry-run] %s%s\n' "$C_DIM" "$*" "$C_OFF"; else "$@"; fi; }

usage() {
  cat <<'EOF'
Turn this machine into a SandrPod sandbox (an agent, not a worker).

  --api-url URL     control plane to register with   (or SANDRPOD_API_URL)
  --token TOKEN     join token, jn_…                 (or SANDRPOD_JOIN_TOKEN)
  --name NAME       sandbox name for this machine    (default: hostname)
  --version TAG     release to install               (default: latest)
  --prefix DIR      where to install the binary      (default: /usr/local/bin)
  --yes             do not ask for confirmation
  --dry-run         print the plan, change nothing
  -h, --help        this text
EOF
}

while [ $# -gt 0 ]; do
  # Accept --flag=value as well as --flag value: the console prints the former, and
  # a script that only understands the latter dies on its own copy-paste.
  case "$1" in
    --*=*) key="${1%%=*}"; val="${1#*=}"; shift; set -- "$key" "$val" "$@" ;;
    *) break ;;
  esac
done

while [ $# -gt 0 ]; do
  case "$1" in
    --api-url) API_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --prefix) PREFIX="$2"; shift 2 ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    --*=*) key="${1%%=*}"; val="${1#*=}"; shift; set -- "$key" "$val" "$@" ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

step "Checking this machine"
[ "$(id -u)" = "0" ] || die "run as root (sudo bash -s -- …). The agent needs to write a service file and run as a system user."
OS="$(uname -s)"; ARCH="$(uname -m)"
case "$OS" in
  Linux) PLATFORM="linux" ;;
  Darwin) PLATFORM="darwin" ;;
  *) die "unsupported system ${OS}: this script covers Linux and macOS. On Windows use the .exe release and run it as a scheduled task." ;;
esac
case "$ARCH" in
  x86_64|amd64) GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  *) die "unsupported architecture ${ARCH}" ;;
esac
info "system: ${PLATFORM}/${GOARCH}"
[ -z "$NAME" ] && NAME="$(hostname -s 2>/dev/null || hostname)"
info "sandbox name: ${NAME}"

[ -n "$API_URL" ] || die "no control plane URL: pass --api-url or set SANDRPOD_API_URL"
[ -n "$TOKEN" ] || die "no join token: pass --token (Workers → Add worker, then copy the agent command)"
case "$TOKEN" in jn_*) ;; *) warn "a join token normally starts with jn_ — continuing anyway" ;; esac

if [ "$PLATFORM" = "darwin" ] && [ "$ASSUME_YES" = 0 ] && [ -t 0 ]; then
  warn "on macOS the agent needs Full Disk Access for the paths it will work in,"
  warn "granted by hand in System Settings → Privacy & Security. The service starts anyway."
fi

step "Installing the binary"
BIN="sandrpod-agent"
URL="https://github.com/sandrpod/sandrpod/releases/${VERSION}/download/${BIN}-${PLATFORM}-${GOARCH}"
[ "$VERSION" = "latest" ] && URL="https://github.com/sandrpod/sandrpod/releases/latest/download/${BIN}-${PLATFORM}-${GOARCH}"
info "from ${URL}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
if [ "$DRY_RUN" = 0 ]; then
  curl -fsSL --retry 3 --max-time 300 "$URL" -o "$TMP/$BIN" || die "download failed. Check the release tag, or build it: go build -o $PREFIX/$BIN ./cmd/agent"
  install -m 0755 "$TMP/$BIN" "$PREFIX/$BIN"
  info "installed $PREFIX/$BIN ($("$PREFIX/$BIN" --version 2>/dev/null | head -1 || echo 'version unknown'))"
else
  info "[dry-run] would download and install to $PREFIX/$BIN"
fi

step "Writing the service"
# Environment in the unit rather than a token file: the unit is 0600 and root-owned,
# and a separate file is one more thing to leave world-readable by accident.
if [ "$PLATFORM" = "linux" ]; then
  UNIT=/etc/systemd/system/sandrpod-agent.service
  if [ "$DRY_RUN" = 0 ]; then
    umask 077
    cat > "$UNIT" <<UNIT
[Unit]
Description=SandrPod agent (this machine as a sandbox)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${PREFIX}/${BIN} -api-url='${API_URL}' -token='${TOKEN}' -name='${NAME}'
Restart=always
RestartSec=5
# The agent runs the work the sandbox is asked for, as a fixed user, with the
# machine's own tools. It is not a container: be deliberate about what it can read.
User=root
Environment=SANDRPOD_AGENT_NAME=${NAME}

[Install]
WantedBy=multi-user.target
UNIT
    run systemctl daemon-reload
  else
    info "[dry-run] would write ${UNIT}"
  fi
  run systemctl enable --now sandrpod-agent
else
  PLIST=/Library/LaunchDaemons/com.sandrpod.agent.plist
  info "writes ${PLIST} and loads it with launchctl"
  if [ "$DRY_RUN" = 0 ]; then
    umask 077
    cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.sandrpod.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>${PREFIX}/${BIN}</string>
    <string>-api-url=${API_URL}</string>
    <string>-token=${TOKEN}</string>
    <string>-name=${NAME}</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
PLIST
    chmod 600 "$PLIST"
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load "$PLIST"
  fi
fi

step "Done"
cat <<EOF
    this machine is now the sandbox "${NAME}"
    control plane: ${API_URL}

    It dials out; nothing needs to be reachable from outside.
    Watch it:   $( [ "$PLATFORM" = linux ] && echo "journalctl -u sandrpod-agent -f" || echo "log stream --predicate 'process == \"sandrpod-agent\"'" )
    Remove it:  $( [ "$PLATFORM" = linux ] && echo "systemctl disable --now sandrpod-agent" || echo "launchctl unload ${PLIST}" )

    It appears in the console's sandbox list within a few seconds. Removing it from
    the console only drops the record; this service keeps reconnecting until it is
    stopped here.
EOF