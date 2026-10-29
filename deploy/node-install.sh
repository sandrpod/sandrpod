#!/usr/bin/env bash
# Turn a fresh machine into a SandrPod worker.
#
# Written for the machine you actually have: no Docker installed, one small
# system disk, one unformatted data disk, and a network where Docker Hub is
# slow or blocked. Every step is idempotent — running it twice is a repair, not
# a disaster.
#
#   ./node-install.sh --api-url https://cloud.example.com --token jn_… --yes
#   ./node-install.sh --dry-run          # print the plan, change nothing
#
# It does six things, in this order, because each depends on the one before:
#
#   1. a data disk, formatted and mounted where Docker will live (before Docker
#      is installed, so it is never moved afterwards);
#   2. swap, so a burst of sandboxes does not end in the OOM killer;
#   3. Docker itself, preferring the Aliyun mirror that works in China;
#   4. /etc/docker/daemon.json: data-root on the data disk, registry mirrors,
#      log rotation, and an address pool that cannot collide with an existing
#      172.17/16 bridge;
#   5. kernel and ulimit settings that sandboxes actually need (inotify watches,
#      open files, connection backlog);
#   6. the worker, joined with a join token, plus the two images it will need.
set -euo pipefail

# ─── defaults ───────────────────────────────────────────────────────────────
API_URL="${SANDRPOD_API_URL:-}"
JOIN_TOKEN="${SANDRPOD_JOIN_TOKEN:-}"
REGION="${SANDRPOD_WORKER_REGION:-local}"
DATA_DISK="${SANDRPOD_DATA_DISK:-}"
DATA_ROOT="${SANDRPOD_DOCKER_ROOT:-}"
WORKER_NAME="${SANDRPOD_WORKER_NAME:-sandrpod-worker}"
TOOLBOX_IMAGE="${SANDRPOD_TOOLBOX_IMAGE:-ghcr.io/sandrpod/toolbox:latest}"
EGRESS_IMAGE="${SANDRPOD_EGRESS_IMAGE:-ghcr.io/sandrpod/egress-proxy:latest}"
PODER_IMAGE="${SANDRPOD_PODER_IMAGE:-ghcr.io/sandrpod/poder:latest}"
SWAP_GB="${SANDRPOD_SWAP_GB:-4}"
ASSUME_YES=0
DRY_RUN=0

# ─── output ─────────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_ERR=$'\033[31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_OK=''; C_WARN=''; C_ERR=''; C_DIM=''; C_OFF=''
fi
step() { printf '\n%s==> %s%s\n' "$C_OK" "$*" "$C_OFF"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    %s! %s%s\n' "$C_WARN" "$*" "$C_OFF"; }
die()  { printf '\n%s✗ %s%s\n' "$C_ERR" "$*" "$C_OFF" >&2; exit 1; }
run() {
  if [ "$DRY_RUN" = "1" ]; then printf '    %s[dry-run] %s%s\n' "$C_DIM" "$*" "$C_OFF"; else "$@"; fi
}

usage() {
  cat <<'EOF'
Turn a fresh machine into a SandrPod worker.

  --api-url URL          control plane to join        (or SANDRPOD_API_URL)
  --token TOKEN          worker join token, jn_…      (or SANDRPOD_JOIN_TOKEN)
  --region NAME          region this worker serves    (default: local)
  --data-disk DEVICE     disk to dedicate to Docker   (default: the one unused disk, if exactly one)
  --data-root PATH       where Docker keeps its data  (default: the data disk's mount point)
  --worker-name NAME     container name               (default: sandrpod-worker)
  --swap-gb N            swap to create if none       (default: 4, 0 disables)
  --toolbox-image IMG    sandbox base image
  --poder-image IMG      worker image
  --egress-image IMG     egress proxy image
  --yes                  do not ask for confirmation
  --dry-run              print what would happen, change nothing
  -h, --help             this text
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --api-url) API_URL="$2"; shift 2 ;;
    --token) JOIN_TOKEN="$2"; shift 2 ;;
    --region) REGION="$2"; shift 2 ;;
    --data-disk) DATA_DISK="$2"; shift 2 ;;
    --data-root) DATA_ROOT="$2"; shift 2 ;;
    --worker-name) WORKER_NAME="$2"; shift 2 ;;
    --swap-gb) SWAP_GB="$2"; shift 2 ;;
    --toolbox-image) TOOLBOX_IMAGE="$2"; shift 2 ;;
    --poder-image) PODER_IMAGE="$2"; shift 2 ;;
    --egress-image) EGRESS_IMAGE="$2"; shift 2 ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

ask() { # ask <prompt> <default>; prints the answer
  local prompt="$1" def="${2:-}" reply
  if [ "$ASSUME_YES" = "1" ] || [ ! -t 0 ]; then printf '%s' "$def"; return; fi
  if [ -n "$def" ]; then printf '%s [%s]: ' "$prompt" "$def" >&2; else printf '%s: ' "$prompt" >&2; fi
  read -r reply </dev/tty || true
  printf '%s' "${reply:-$def}"
}

confirm() { # confirm <question>, default yes
  local q="$1" reply
  [ "$ASSUME_YES" = "1" ] && return 0
  [ ! -t 0 ] && return 0
  printf '%s [Y/n]: ' "$q" >&2
  read -r reply </dev/tty || true
  case "${reply:-y}" in [yY]*) return 0 ;; *) return 1 ;; esac
}

# ─── 0. preflight ───────────────────────────────────────────────────────────
step "Checking this machine"
[ "$(id -u)" = "0" ] || die "run as root (sudo -i, or sudo $0 …)"
[ "$(uname -s)" = "Linux" ] || die "this script is for Linux. On macOS the worker runs in Docker Desktop instead — see docs.sandrpod.com/workers"

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|aarch64|arm64) info "architecture ${ARCH}" ;;
  *) die "unsupported architecture ${ARCH}" ;;
esac

if [ -r /etc/os-release ]; then . /etc/os-release; OS_ID="${ID:-unknown}"; OS_VER="${VERSION_ID:-}"; OS_NAME="${PRETTY_NAME:-$OS_ID}"
else OS_ID=unknown; OS_VER=""; OS_NAME=unknown; fi
info "system ${OS_NAME}"

MEM_MB=$(( $(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0) / 1024 ))
info "memory ${MEM_MB} MiB"
[ "$MEM_MB" -lt 3500 ] && warn "under 4 GiB: a couple of sandboxes at most, and swap matters"

FREE_DISK_GB=$(df -BG --output=avail / 2>/dev/null | tail -1 | tr -dc '0-9' || true)
FREE_DISK_GB="${FREE_DISK_GB:-0}"
info "free on /: ${FREE_DISK_GB} GiB"
[ "$FREE_DISK_GB" -lt 15 ] && warn "under 15 GiB free: enough for the worker, not for many sandbox images"

HAS_DOCKER=0
if command -v docker >/dev/null 2>&1; then
  HAS_DOCKER=1
  info "docker already installed: $(docker --version 2>/dev/null | head -1)"
fi

# ─── 1. data disk ───────────────────────────────────────────────────────────
step "Data disk"

# Candidate = a whole disk (not a partition) with no mount, no filesystem
# signature, or not referenced by fstab, and not the one / lives on.
candidates=()
ROOT_DISK="$(lsblk -no PKNAME "$(findmnt -no SOURCE /)" 2>/dev/null || true)"
while read -r dev size type; do
  [ "$type" = "disk" ] || continue
  name="/dev/$dev"
  [ "$name" = "/dev/$ROOT_DISK" ] && continue
  # Detection must never abort the script: a minimal image without util-linux,
  # or a device that disappears mid-scan, is a machine we can still install on.
  mount="$(lsblk -no MOUNTPOINT "$name" 2>/dev/null | tr -d ' ' || true)"
  [ -n "$mount" ] && continue
  candidates+=("$name $size")
done < <(lsblk -dn -o NAME,SIZE,TYPE 2>/dev/null || true)

if [ -n "$DATA_DISK" ]; then
  info "using ${DATA_DISK} (given)"
elif [ "${#candidates[@]}" -eq 1 ]; then
  DATA_DISK="${candidates[0]%% *}"
  info "found one unattached disk: ${candidates[0]} — will use it for Docker"
elif [ "${#candidates[@]}" -gt 1 ]; then
  warn "several unattached disks:"
  for c in "${candidates[@]}"; do info "    $c"; done
  DATA_DISK="$(ask "Which one for Docker? (empty = use the system disk)" "")"
  [ -z "$DATA_DISK" ] && warn "no data disk chosen: Docker will live on /"
else
  warn "no spare disk found: Docker will live on / (fine for a test worker)"
fi

if [ -n "$DATA_DISK" ]; then
  [ -b "$DATA_DISK" ] || die "${DATA_DISK} is not a block device"
  [ "$DATA_ROOT" = "" ] && DATA_ROOT="/var/lib/docker"
  # Anything already on it? A partition table or a filesystem means somebody
  # put something there, and formatting it is not this script's call.
  if blkid "$DATA_DISK" >/dev/null 2>&1; then
    EXISTING_FS="$(blkid -o value -s TYPE "$DATA_DISK" 2>/dev/null || echo '?')"
    warn "${DATA_DISK} already has a ${EXISTING_FS} filesystem"
    info "the script will mount it, not format it"
  else
    info "formatting ${DATA_DISK} as ext4 and mounting at ${DATA_ROOT}"
    confirm "Erase everything on ${DATA_DISK}? this cannot be undone" || die "aborted at the data disk"
    run mkfs.ext4 -F -L sandrpod "$DATA_DISK"
  fi
  run mkdir -p "$DATA_ROOT"
fi

# ─── 2. swap ────────────────────────────────────────────────────────────────
step "Swap"
SWAP_MB=$(awk '/SwapTotal/ {print int($2/1024)}' /proc/meminfo)
if [ "$SWAP_MB" -gt 0 ]; then
  info "already ${SWAP_MB} MiB"
elif [ "${SWAP_GB:-0}" -eq 0 ]; then
  warn "disabled by --swap-gb 0; a burst of sandboxes will reach the OOM killer sooner"
else
  info "creating ${SWAP_GB} GiB at /swapfile"
  run fallocate -l "${SWAP_GB}G" /swapfile || run dd if=/dev/zero of=/swapfile bs=1M count=$((SWAP_GB * 1024)) status=none
  run chmod 600 /swapfile
  run mkswap /swapfile
  run swapon /swapfile
  if [ "$DRY_RUN" = "0" ] && ! grep -q '^/swapfile' /etc/fstab; then
    echo '/swapfile none swap sw 0 0' >> /etc/fstab
  fi
fi

# ─── 3. docker ──────────────────────────────────────────────────────────────
step "Docker"
install_docker_apt_mirror() {
  # Aliyun's docker-ce mirror: the official download.docker.com is unreachable
  # from a lot of networks in China, and this one is not.
  run apt-get update -qq
  run apt-get install -y -qq ca-certificates curl gnupg
  run install -m 0755 -d /etc/apt/keyrings
  local distro="ubuntu"
  [ "$OS_ID" = "debian" ] && distro="debian"
  if [ ! -f /etc/apt/keyrings/docker.asc ]; then
    run curl -fsSL "https://mirrors.aliyun.com/docker-ce/linux/${distro}/gpg" -o /tmp/docker.asc
    run install -m 0644 /tmp/docker.asc /etc/apt/keyrings/docker.asc
  fi
  if [ "$DRY_RUN" = "0" ]; then
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://mirrors.aliyun.com/docker-ce/linux/${distro} $(. /etc/os-release && echo "${VERSION_CODENAME}") stable" \
      > /etc/apt/sources.list.d/docker.list
  fi
  run apt-get update -qq
  run apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}
install_docker_yum_mirror() {
  run "$PKG" install -y -q yum-utils
  run yum-config-manager --add-repo https://mirrors.aliyun.com/docker-ce/linux/centos/docker-ce.repo
  run "$PKG" install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

if [ "$HAS_DOCKER" = "1" ]; then
  info "keeping the installed docker"
else
  case "$OS_ID" in
    ubuntu|debian)
      PKG=apt-get
      install_docker_apt_mirror || { warn "aliyun mirror failed; falling back to the distro package"; run apt-get install -y -qq docker.io; }
      ;;
    centos|rhel|rocky|almalinux|anolis|openeuler|kylin|uos)
      PKG="$(command -v dnf >/dev/null && echo dnf || echo yum)"
      install_docker_yum_mirror || { warn "aliyun mirror failed; falling back to the distro package"; run "$PKG" install -y -q docker; }
      ;;
    *)
      die "unsupported distribution ${OS_ID}; install Docker by hand and re-run (docs.sandrpod.com/deploy)"
      ;;
  esac
  run systemctl enable --now docker
fi

run docker info >/dev/null 2>&1 || [ "$DRY_RUN" = "1" ] || die "docker is not answering; check 'systemctl status docker'"

# ─── 4. daemon.json ─────────────────────────────────────────────────────────
step "Docker configuration"

if [ -z "$DATA_ROOT" ]; then
  DATA_ROOT="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)"
fi
info "data-root ${DATA_ROOT}"

# Registry mirrors: the first two are state-run/community mirrors that answer in
# China; the rest are fallbacks. Harmless elsewhere — Docker tries them in order
# and falls through to Docker Hub.
mkdir -p /etc/docker
DAEMON_JSON=/etc/docker/daemon.json
if [ -f "$DAEMON_JSON" ] && [ "$DRY_RUN" = "0" ]; then
  cp -a "$DAEMON_JSON" "${DAEMON_JSON}.bak-$(date +%Y%m%d%H%M%S)"
  info "existing daemon.json backed up"
fi
if [ "$DRY_RUN" = "0" ]; then
  cat > "$DAEMON_JSON" <<JSON
{
  "data-root": "${DATA_ROOT}",
  "exec-opts": ["native.cgroupdriver=systemd"],
  "log-driver": "json-file",
  "log-opts": { "max-size": "50m", "max-file": "3" },
  "live-restore": true,
  "registry-mirrors": [
    "https://docker.1ms.run",
    "https://docker.m.daocloud.io",
    "https://hub-mirror.c.163.com"
  ],
  "default-address-pools": [
    { "base": "172.30.0.0/16", "size": 24 },
    { "base": "10.201.0.0/16", "size": 24 }
  ]
}
JSON
  info "wrote ${DAEMON_JSON}"
else
  info "[dry-run] would write ${DAEMON_JSON} with data-root ${DATA_ROOT} and registry mirrors"
fi
run systemctl daemon-reload
run systemctl restart docker
sleep 2
run docker info >/dev/null 2>&1 || [ "$DRY_RUN" = "1" ] || die "docker did not come back after the configuration change"

# Mount the data disk *after* Docker knows about it, then move an existing
# data-root onto it. Doing it the other way round means a running daemon holds
# open files on the old path.
if [ -n "$DATA_DISK" ] && [ "$DRY_RUN" = "0" ]; then
  UUID="$(blkid -o value -s UUID "$DATA_DISK" || true)"
  if [ -n "$UUID" ] && ! grep -q "UUID=$UUID" /etc/fstab; then
    mkdir -p "$DATA_ROOT"
    cp -a "$DATA_ROOT/." "${DATA_ROOT}.seed/" 2>/dev/null || true
    echo "UUID=$UUID  ${DATA_ROOT}  ext4  defaults,nofail,x-systemd.device-timeout=30s  0 2" >> /etc/fstab
    systemctl daemon-reload
    mount "$DATA_ROOT"
    info "mounted ${DATA_DISK} at ${DATA_ROOT} and made it permanent"
  fi
fi

# ─── 5. kernel and limits ───────────────────────────────────────────────────
step "Kernel and limits"
cat > /etc/sysctl.d/99-sandrpod-worker.conf <<'SYSCTL'
# Sandboxes run development tools: watchers, servers, many small processes.
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 1024
net.core.somaxconn = 4096
net.ipv4.ip_forward = 1
vm.swappiness = 10
# A sandbox writing a big file should not evict everything else.
vm.max_map_count = 262144
SYSCTL
run sysctl --system >/dev/null
info "inotify, backlog and forwarding set for the sandbox workload"

mkdir -p /etc/systemd/system/docker.service.d
cat > /etc/systemd/system/docker.service.d/limits.conf <<'LIMITS'
[Service]
LimitNOFILE=1048576
LimitNPROC=infinity
LIMITS
run systemctl daemon-reload
info "docker may open a million files (builds are file-hungry)"

# ─── 6. join the fleet ──────────────────────────────────────────────────────
step "Joining the fleet"

if [ -z "$API_URL" ]; then
  API_URL="$(ask "Control plane URL (e.g. https://cloud.example.com)" "")"
fi
[ -n "$API_URL" ] || die "no control plane URL: pass --api-url or set SANDRPOD_API_URL"
if [ -z "$JOIN_TOKEN" ]; then
  JOIN_TOKEN="$(ask "Worker join token (jn_…, from the console: Workers → Add worker)" "")"
fi
[ -n "$JOIN_TOKEN" ] || die "no join token: pass --token or set SANDRPOD_JOIN_TOKEN"
case "$JOIN_TOKEN" in jn_*) ;; *) warn "a join token normally starts with jn_ — continuing anyway" ;; esac

info "pulling the images the worker needs (this is the slow part on a cold cache)"
run docker pull "$PODER_IMAGE"
run docker pull "$TOOLBOX_IMAGE"
# The egress proxy is only needed by sandboxes with a policy, and a worker that
# lacks it fails those creates *after* writing the record. Pull it now.
run docker pull "$EGRESS_IMAGE" || warn "egress-proxy not pulled: sandboxes with an egress policy will fail to create"

if [ "$DRY_RUN" = "0" ] && command -v docker >/dev/null 2>&1 && docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$WORKER_NAME"; then
  warn "a container named ${WORKER_NAME} already exists"
  confirm "Replace it? (the worker keeps its persisted identity)" || die "aborted before replacing the worker"
  run docker rm -f "$WORKER_NAME"
fi

run docker run -d --name "$WORKER_NAME" --restart=unless-stopped \
  --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v pode-state:/var/lib/sandrpod \
  -e SANDRPOD_TOOLBOX_IMAGE="$TOOLBOX_IMAGE" \
  "$PODER_IMAGE" \
  -api-url="$API_URL" -token="$JOIN_TOKEN" -region="$REGION"

if [ "$DRY_RUN" = "0" ]; then
  sleep 8
  docker logs --tail 5 "$WORKER_NAME" 2>&1 | sed 's/^/    /'
  if docker logs "$WORKER_NAME" 2>&1 | grep -q "Tunnel connected"; then
    PODER_ID="$(docker logs "$WORKER_NAME" 2>&1 | grep -o 'poder-[0-9a-f]*' | tail -1)"
    printf '\n%s✓ worker joined as %s%s\n' "$C_OK" "${PODER_ID:-unknown}" "$C_OFF"
  else
    printf '\n%s! the worker has not connected yet%s\n' "$C_WARN" "$C_OFF"
    printf '    check:  docker logs -f %s\n' "$WORKER_NAME"
  fi
fi

step "Done"
cat <<EOF
    worker:      ${WORKER_NAME}
    control:     ${API_URL}
    region:      ${REGION}
    data-root:   ${DATA_ROOT}
    images:      ${TOOLBOX_IMAGE}

    The worker dials out; nothing needs to be reachable from outside.
    Watch it:    docker logs -f ${WORKER_NAME}
    Remove it:   docker rm -f ${WORKER_NAME}
    Sandboxes:   the console's Workers page shows this machine within a minute.
EOF