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
# Confinement. An agent is the machine, not a container, so the defaults have to be
# the safe ones: a work directory it is confined to, a permission gate, and the
# kernel sandbox where the platform has one. Opening the whole machine is a flag
# somebody has to type, and it says what it does.
WORK_DIR="${SANDRPOD_WORK_DIR:-}"
PERMISSION_MODE="${SANDRPOD_PERMISSION_MODE:-prompt}"
OS_SANDBOX="${SANDRPOD_OS_SANDBOX:-auto}"
RUN_USER="${SANDRPOD_AGENT_USER:-}"
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
  --work-dir DIR    writes are confined here (plus the system temp dir);
                    default: ~/sandrpod-work
  --permission-mode MODE  prompt | strict | off      (default: prompt)
  --os-sandbox MODE off | auto | bwrap | seatbelt    (default: auto: read-only root,
                    writes only under the work dir, enforced by the kernel)
  --user NAME       run the service as this user     (default: the sudo invoking user)
  --full-machine    the whole filesystem, no gate, no kernel sandbox — for a
                    machine you have dedicated to one agent and nothing else
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
    --work-dir) WORK_DIR="$2"; shift 2 ;;
    --permission-mode) PERMISSION_MODE="$2"; shift 2 ;;
    --os-sandbox) OS_SANDBOX="$2"; shift 2 ;;
    --user) RUN_USER="$2"; shift 2 ;;
    --full-machine) WORK_DIR="/"; PERMISSION_MODE="off"; OS_SANDBOX="off"; shift ;;
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

# Who the service runs as. Not root by default: an agent that can be driven by a
# remote model as root is one prompt injection away from the whole machine, and
# ./ and / are then the same thing.
if [ -z "$RUN_USER" ]; then
  RUN_USER="${SUDO_USER:-}"
  [ -z "$RUN_USER" ] && RUN_USER="$(id -un)"
  # sudo from a normal login: run as that person, whose home and tools are the
  # point of attaching their machine.
  [ "$RUN_USER" = "root" ] && [ -n "${SUDO_USER:-}" ] && RUN_USER="$SUDO_USER"
fi
RUN_HOME="$(getent passwd "$RUN_USER" 2>/dev/null | cut -d: -f6)"
[ -z "$RUN_HOME" ] && RUN_HOME="/root"
[ -z "$WORK_DIR" ] && WORK_DIR="${RUN_HOME}/sandrpod-work"
info "runs as: ${RUN_USER} (${RUN_HOME})"
info "writes confined to: ${WORK_DIR} (+ the system temp dir)"
info "  measured with -os-sandbox=auto on macOS: \$HOME, /usr/local/bin, /private/var"
info "  and ~/Documents refuse writes; ~/.ssh and ~/Library/{Mail,Messages,Cookies}"
info "  refuse reads; /tmp stays writable because build tools expect it"
info "permission gate: ${PERMISSION_MODE}, kernel sandbox: ${OS_SANDBOX}"

case "$PERMISSION_MODE" in off|prompt|strict) ;; *) die "--permission-mode must be off, prompt or strict" ;; esac
case "$OS_SANDBOX" in off|auto|bwrap|seatbelt) ;; *) die "--os-sandbox must be off, auto, bwrap or seatbelt" ;; esac
if [ "$WORK_DIR" = "/" ] || [ "$PERMISSION_MODE" = "off" ]; then
  warn "this agent can write anywhere on the machine and is not gated."
  warn "that is right for a dedicated box and wrong for a laptop; Ctrl-C to stop."
  if [ "$ASSUME_YES" = 0 ] && [ -t 0 ]; then
    printf '    continue? [y/N]: ' >&2
    read -r reply </dev/tty || reply=""
    case "$reply" in [yY]*) ;; *) die "aborted" ;; esac
  fi
fi

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

step "Creating the work directory"
if [ "$DRY_RUN" = 0 ] && [ "$WORK_DIR" != "/" ]; then
  mkdir -p "$WORK_DIR"
  chown "$RUN_USER" "$WORK_DIR" 2>/dev/null || true
  chmod 700 "$WORK_DIR"
  info "created ${WORK_DIR} (0700, owned by ${RUN_USER})"
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
ExecStart=${PREFIX}/${BIN} -api-url='${API_URL}' -token='${TOKEN}' -name='${NAME}' \
  -work-dir='${WORK_DIR}' -permission-mode='${PERMISSION_MODE}' -os-sandbox='${OS_SANDBOX}'
# on-failure, not always: the agent exits 0 when its sandbox is deleted from the
# platform, and restarting it would re-register what somebody just removed.
Restart=on-failure
RestartSec=5
# Not root, and confined: this is a machine somebody sits at, not a container.
User=${RUN_USER}
Environment=SANDRPOD_AGENT_NAME=${NAME}
WorkingDirectory=${WORK_DIR}

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
    <string>-work-dir=${WORK_DIR}</string>
    <string>-permission-mode=${PERMISSION_MODE}</string>
    <string>-os-sandbox=${OS_SANDBOX}</string>
  </array>
  <key>UserName</key><string>${RUN_USER}</string>
  <key>WorkingDirectory</key><string>${WORK_DIR}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key><false/>
  </dict>
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