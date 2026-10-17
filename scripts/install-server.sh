#!/bin/sh
# Installs Docker if needed, then runs the Orchestrator control plane so that it keeps
# running after you log out and comes back after a reboot. Safe to re-run: it recreates
# the container (data lives in the orchestrator-data volume) and repairs restart policies.
#   curl -fsSL https://raw.githubusercontent.com/Maffewem/Orchestrator/main/scripts/install-server.sh | sudo sh
set -eu

IMAGE="${ORCH_IMAGE:-ghcr.io/maffewem/orchestrator:latest}"
PORT="${ORCH_PORT:-8080}"

[ "$(id -u)" -eq 0 ] || { echo "please run as root (sudo)" >&2; exit 1; }

if ! command -v curl >/dev/null 2>&1 && command -v apt-get >/dev/null 2>&1; then
  apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates curl
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "Docker not found; installing it with get.docker.com"
  curl -fsSL https://get.docker.com | sh
fi
# Docker must start at boot, or nothing below survives a reboot.
if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now docker || { echo "could not enable the Docker service" >&2; exit 1; }
fi

docker pull "$IMAGE"
docker rm -f orchestrator >/dev/null 2>&1 || true
docker run -d --name orchestrator --restart unless-stopped -p "$PORT:8080" \
  -v orchestrator-data:/data "$IMAGE" >/dev/null

# Repair: containers (workers) created without a restart policy stay down after a reboot.
for id in $(docker ps -aq --filter label=orchestrator.project 2>/dev/null); do
  docker update --restart unless-stopped "$id" >/dev/null 2>&1 || true
done

fail=0
check() { # description, command...
  desc="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "  ok   $desc"; else echo "  FAIL $desc"; fail=1; fi
}
policy() { [ "$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' orchestrator)" = "unless-stopped" ]; }
running() { [ "$(docker inspect -f '{{.State.Running}}' orchestrator)" = "true" ]; }
healthy() {
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    curl -fsS "http://localhost:$PORT/api/health" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}
echo "Checking the install:"
if command -v systemctl >/dev/null 2>&1; then check "Docker starts at boot" systemctl is-enabled docker; fi
check "restart policy is unless-stopped" policy
check "container is running" running
check "answers on port $PORT" healthy
[ "$fail" -eq 0 ] || { echo "Some checks failed; see 'docker logs orchestrator'." >&2; exit 1; }

echo "Orchestrator is running on port $PORT and will restart on boot. Admin token (first install only):"
docker logs orchestrator 2>&1 | grep -i -A2 "admin token" | head -5 || true
