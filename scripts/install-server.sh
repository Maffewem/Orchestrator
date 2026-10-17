#!/bin/sh
# Installs Docker if needed, then runs the Orchestrator control plane.
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
if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now docker >/dev/null 2>&1 || true
fi

docker pull "$IMAGE"
docker rm -f orchestrator >/dev/null 2>&1 || true
docker run -d --name orchestrator --restart unless-stopped -p "$PORT:8080" \
  -v orchestrator-data:/data "$IMAGE" >/dev/null

echo "Orchestrator is starting on port $PORT. Admin token:"
for _ in 1 2 3 4 5 6 7 8 9 10; do
  sleep 1
  docker logs orchestrator 2>&1 | grep -i token && break || true
done
