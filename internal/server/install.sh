#!/bin/sh
# Orchestrator agent installer. Served by the control plane at /install.sh.
#   curl -fsSL {{SERVER}}/install.sh | sudo sh -s -- --token <join-token> [--name pi-1] [--labels pi,edge]
set -eu

SERVER="{{SERVER}}"
# SHA-256 of each agent binary this server holds, so a download that was swapped or damaged in
# transit is refused. Empty when the server has no binaries to checksum.
SUMS="{{SUMS}}"
TOKEN=""
NAME=""
LABELS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --token) TOKEN="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --labels) LABELS="$2"; shift 2 ;;
    --server) SERVER="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 1 ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "please run as root (sudo)" >&2; exit 1; }
[ -n "$TOKEN" ] || { echo "--token is required (Devices -> Add device)" >&2; exit 1; }

# Install Docker (and git, which GitHub builds need) when missing, so a bare
# Raspberry Pi OS / Debian / Ubuntu install works from scratch.
need_pkgs=""
command -v curl >/dev/null 2>&1 || need_pkgs="$need_pkgs curl"
command -v git >/dev/null 2>&1 || need_pkgs="$need_pkgs git"
if [ -n "$need_pkgs" ]; then
  if command -v apt-get >/dev/null 2>&1; then
    echo "Installing:$need_pkgs"
    apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates $need_pkgs
  else
    echo "warning: please install:$need_pkgs" >&2
  fi
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "Docker not found; installing it with get.docker.com"
  curl -fsSL https://get.docker.com -o /tmp/get-docker.sh || { echo "could not download the Docker installer" >&2; exit 1; }
  sh /tmp/get-docker.sh || { echo "Docker installation failed; see https://docs.docker.com/engine/install/" >&2; exit 1; }
  rm -f /tmp/get-docker.sh
fi
if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now docker >/dev/null 2>&1 || true
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv7*) ARCH=armv7 ;;
  armv6l|armv6*) ARCH=armv6 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

echo "Downloading orchestrator-agent ($ARCH) from $SERVER"
curl -fsSL "$SERVER/downloads/orchestrator-agent-linux-$ARCH" -o /usr/local/bin/orchestrator-agent.new
case "$SERVER" in
  http://*) echo "warning: $SERVER is plain HTTP; anyone on the path could alter the agent. Use the HTTPS dashboard URL where you can." >&2 ;;
esac
WANT=$(printf '%s\n' "$SUMS" | awk -v f="orchestrator-agent-linux-$ARCH" '$2 == f { print $1 }')
if [ -n "$WANT" ] && command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum /usr/local/bin/orchestrator-agent.new | awk '{ print $1 }')
  if [ "$GOT" != "$WANT" ]; then
    rm -f /usr/local/bin/orchestrator-agent.new
    echo "the downloaded agent does not match the checksum this server published; refusing to install it" >&2
    exit 1
  fi
fi
chmod +x /usr/local/bin/orchestrator-agent.new
mv /usr/local/bin/orchestrator-agent.new /usr/local/bin/orchestrator-agent

mkdir -p /etc/orchestrator-agent /var/lib/orchestrator-agent
umask 077
cat > /etc/orchestrator-agent/env <<CONF
ORCH_SERVER=$SERVER
ORCH_JOIN_TOKEN=$TOKEN
ORCH_NAME=$NAME
ORCH_LABELS=$LABELS
ORCH_STATE=/var/lib/orchestrator-agent/state.json
CONF

cat > /etc/systemd/system/orchestrator-agent.service <<'UNIT'
[Unit]
Description=Orchestrator agent
After=network-online.target docker.service
Wants=network-online.target

[Service]
EnvironmentFile=/etc/orchestrator-agent/env
ExecStart=/usr/local/bin/orchestrator-agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now orchestrator-agent
systemctl restart orchestrator-agent

# Repair: workers deployed without a restart policy would not come back after a reboot.
for id in $(docker ps -aq --filter label=orchestrator.project 2>/dev/null); do
  docker update --restart unless-stopped "$id" >/dev/null 2>&1 || true
done

# Verify it will survive logout and reboot.
fail=0
check() { # description, command...
  desc="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "  ok   $desc"; else echo "  FAIL $desc"; fail=1; fi
}
echo "Checking the install:"
check "Docker starts at boot" systemctl is-enabled docker
check "Docker is running" docker info
check "agent starts at boot" systemctl is-enabled orchestrator-agent
check "agent is running" systemctl is-active orchestrator-agent
[ "$fail" -eq 0 ] || { echo "Some checks failed; see 'journalctl -u orchestrator-agent -n 50'." >&2; exit 1; }
echo "Orchestrator agent installed. It should appear in the dashboard within a few seconds."
