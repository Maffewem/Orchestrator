#!/bin/sh
# Installs Docker if needed, then runs the Orchestrator control plane so that it keeps
# running after you log out and comes back after a reboot. Safe to re-run: it recreates
# the container (data lives in the orchestrator-data volume) and repairs restart policies.
#   curl -fsSL https://raw.githubusercontent.com/Maffewem/Orchestrator/main/scripts/install-server.sh | sudo sh
# Back from Beta to Stable (data is kept): ... | sudo sh -s -- --channel stable
#   add --restore-snapshot if Stable can't read a database a Beta build changed.
# The dashboard can update the server, which needs the Docker socket inside the container
# (full control of Docker on this host). Opt out with: ... | sudo sh -s -- --no-web-update
set -eu

CHANNEL="${ORCH_CHANNEL:-stable}"   # stable (main) or beta (dev branch); see docs/RELEASING.md
IMAGE="${ORCH_IMAGE:-}"
PORT="${ORCH_PORT:-8080}"
NAME="${ORCH_NAME:-orchestrator}"      # container name; change it (and port) to run a second server
VOLUME="${ORCH_VOLUME:-$NAME-data}"
RESTORE=0
WEB_UPDATE=1
[ "${ORCH_WEB_UPDATE:-1}" = "0" ] && WEB_UPDATE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --no-web-update) WEB_UPDATE=0; shift ;;
    --channel) CHANNEL="${2:-}"; shift 2 ;;
    --channel=*) CHANNEL="${1#*=}"; shift ;;
    --restore-snapshot) RESTORE=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 1 ;;
  esac
done

[ "$CHANNEL" = beta ] && CHANNEL=dev   # Beta is the dashboard's name for the dev image tag
case "$CHANNEL" in stable|dev) ;; *) echo "channel must be stable or beta" >&2; exit 1 ;; esac
[ -n "$IMAGE" ] || IMAGE="ghcr.io/maffewem/orchestrator:$CHANNEL"

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
docker rm -f "$NAME" >/dev/null 2>&1 || true
if [ "$RESTORE" = 1 ]; then
  # The way back when a Beta build changed the database and Stable can't read it: the server
  # saves a copy before every channel switch; put the newest one back (the server swaps it in
  # on start and keeps the current database next to it as .pre-restore).
  docker run --rm --user 10001 --entrypoint sh -v "$VOLUME:/data" "$IMAGE" -c \
    'f=$(ls -1 /data/channel-snapshots/before-*.db 2>/dev/null | sort | tail -n 1); [ -n "$f" ] || { echo "no snapshot found in the data volume" >&2; exit 1; }; cp "$f" /data/orchestrator.db.restore && echo "restoring $f"' \
    || { echo "could not stage a snapshot" >&2; exit 1; }
fi
if [ "$WEB_UPDATE" = 1 ]; then
  # The server runs as an unprivileged user, so it joins the socket's group.
  SOCK_GID="$(stat -c %g /var/run/docker.sock 2>/dev/null || echo 0)"
  set -- -v /var/run/docker.sock:/var/run/docker.sock --group-add "$SOCK_GID"
else
  set --
fi
docker run -d --name "$NAME" --restart unless-stopped -p "$PORT:8080" \
  -v "$VOLUME:/data" "$@" "$IMAGE" >/dev/null

# Repair: containers (workers) created without a restart policy stay down after a reboot.
for id in $(docker ps -aq --filter label=orchestrator.project 2>/dev/null); do
  docker update --restart unless-stopped "$id" >/dev/null 2>&1 || true
done

fail=0
check() { # description, command...
  desc="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "  ok   $desc"; else echo "  FAIL $desc"; fail=1; fi
}
policy() { [ "$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$NAME")" = "unless-stopped" ]; }
running() { [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" = "true" ]; }
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
if [ "$fail" -ne 0 ]; then
  echo "Some checks failed; see 'docker logs $NAME'." >&2
  [ "$CHANNEL" = stable ] && [ "$RESTORE" = 0 ] && echo "If the Beta build changed the database and Stable can't read it, re-run with --channel stable --restore-snapshot to put back the copy saved before the switch." >&2
  exit 1
fi

if [ "$WEB_UPDATE" = 1 ]; then
  echo "Dashboard updates are on (Docker socket mounted). Re-run with --no-web-update to turn them off."
else
  echo "Dashboard updates are off. Re-run without --no-web-update to turn them on."
fi
echo "Orchestrator is running on port $PORT and will restart on boot. Admin token (first install only):"
docker logs $NAME 2>&1 | grep -i -A2 "admin token" | head -5 || true
