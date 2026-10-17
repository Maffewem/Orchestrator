# Orchestrator

A self-hosted manager for a fleet of Raspberry Pis, VMs and servers. Think Portainer
across many machines, with a Cloudflare Pages-style dashboard: projects, step-by-step
deployments with live logs, GitHub push-to-deploy, and Server Manager-style health for
every device.

Everything is done from the web dashboard. The only command you run is the one-line
agent install on each new device.

## How it works

```
                 ┌──────────────────────────────────────────────┐
  Browser ──────▶│  orchestrator-server                         │◀── GitHub push webhook
                 │  dashboard (React) · REST API · SQLite       │
                 └──────────────▲───────────────────────────────┘
                                │ HTTPS, outbound from each device
          ┌─────────────────────┼─────────────────────┐
   ┌──────┴───────┐      ┌──────┴───────┐      ┌──────┴───────┐
   │ agent (Pi 5) │      │ agent (Pi 3) │      │ agent (VM)   │
   │ Docker       │      │ Docker       │      │ Docker       │
   └──────────────┘      └──────────────┘      └──────────────┘
```

- **Control plane** (`cmd/orchestrator-server`): a single Go binary that serves the
  dashboard, the admin API, the agent API and the GitHub webhook. It stores state in SQLite.
- **Agent** (`cmd/orchestrator-agent`): a small, static Go binary for each device. It
  registers once with a join token and then heartbeats every 5 seconds. Each heartbeat
  reports host info, CPU/memory/disk and the containers on the device, and picks up any
  queued commands (deploy, start, stop, restart, logs). The agent only makes outbound
  connections, so devices behind NAT work with no port forwarding. It talks to the local
  Docker engine over its socket and uses only the Go standard library.
- **Deployments** run on the device in four steps that the dashboard shows as a timeline:
  **Initializing → Building / Pulling image → Deploying → Verifying**. Logs stream back
  with timestamps. Each rollout is a blue/green swap: the previous container is kept and
  restored automatically if the new one fails to start or doesn't stay up.
- **GitHub projects** are built on the device itself. The Docker engine clones the repo at
  the exact commit and builds the Dockerfile, so images are native to the Pi's
  architecture and no registry is needed. Pushes to the production branch deploy
  automatically through the webhook.

### Supported devices

The agent is built for `linux/amd64`, `linux/arm64` (Pi 3/4/5 on 64-bit OS),
`linux/armv7` (32-bit Raspberry Pi OS) and `linux/armv6` (Pi Zero / Pi 1). The installer
picks the right one automatically. Devices need Docker. Building from GitHub also needs
`git` on the device, because the Docker engine uses it to clone.

## Quick start

Pre-built multi-arch images (amd64 and arm64) are published to GitHub Container Registry by
CI on every push to `main`, so on a server or a Pi 4/5 you can run the control plane with one
command:

```sh
docker run -d --name orchestrator --restart unless-stopped -p 8080:8080 \
  -v orchestrator-data:/data ghcr.io/maffewem/orchestrator:latest
docker logs orchestrator   # copy the admin token printed on first start
```

Or build it yourself from a clone:

```sh
docker compose up -d
docker compose logs orchestrator   # copy the admin token printed on first start
```

Open http://localhost:8080 and sign in with that token. Then:

1. **Devices → Add device.** Copy the install command and run it on a Pi or VM. The device
   appears within a few seconds.
2. **Settings → GitHub.** Paste a fine-grained token with read-only *Contents* and
   *Metadata* access, and add the webhook URL and secret shown there to your repo.
3. **Projects → Create project.** Pick a repository (or a container image), a device,
   ports and environment variables. Save to start the first deployment.

### Without Docker

```sh
make all                         # dashboard, server, and agents for every architecture
./bin/orchestrator-server -downloads bin/downloads
```

## Development

```sh
make dev-server     # API on :8080 (Go)
make dev-web        # dashboard on :5173 with hot reload, proxying /api
make test           # go vet, go test, TypeScript typecheck
```

Layout:

| Path | What |
| --- | --- |
| `cmd/orchestrator-server` | control plane entrypoint |
| `cmd/orchestrator-agent` | device agent entrypoint |
| `internal/api` | wire types shared by server and agent |
| `internal/store` | SQLite persistence: devices, projects, deployments, steps, events |
| `internal/server` | HTTP API, GitHub webhook, health rules, metrics history, `install.sh` |
| `internal/agent` | heartbeat loop, Docker client, deployment executor, host metrics |
| `web/` | React + TypeScript + Tailwind dashboard, embedded into the server binary |

### API overview

Admin endpoints need `Authorization: Bearer <admin token>`.

| | |
| --- | --- |
| `GET /api/overview` | fleet counts and recent activity |
| `GET /api/devices`, `GET/PATCH/DELETE /api/devices/{id}` | devices with health and status |
| `GET /api/devices/{id}/metrics` | the last hour of CPU/memory/disk samples |
| `POST /api/devices/{id}/commands` | `container.start/stop/restart/logs` |
| `GET /api/containers` | every container in the fleet |
| `GET/POST /api/projects`, `GET/PATCH/DELETE /api/projects/{id}` | projects |
| `POST /api/projects/{id}/deployments` | deploy (GitHub projects use the branch head) |
| `GET /api/deployments/{id}`, `POST /api/deployments/{id}/retry` | deployment with steps and logs |
| `GET /api/events` | activity feed |
| `GET /api/settings`, `PUT /api/settings/github`, `POST /api/settings/join-token/rotate` | settings |

The agent uses `POST /api/agent/register`, `/heartbeat`, `/commands/{id}/result` and
`/deployments/{id}/steps/{n}`. GitHub calls `POST /api/github/webhook`, which is verified
with HMAC-SHA256.

## Security notes

- The admin token, agent tokens and the GitHub token are random secrets. Agent and admin
  tokens are stored only as SHA-256 hashes.
- The GitHub token is never stored with deployments. It is added to the clone URL only
  when a command is handed to an agent, and agents redact credentials from all logs.
- Put the dashboard behind HTTPS (for example Caddy or a Cloudflare Tunnel) before exposing
  it to the internet. That is also what GitHub webhooks need to reach it.

## Roadmap

- Live log streaming over WebSocket, instead of 1s step updates and 5s heartbeats
- Deploying one project to several devices or groups (by role label), with rolling rollouts
- Preview deployments for pull requests and branches, and one-click rollback to any earlier deployment
- Docker Compose stacks as a project source
- GitHub App installation instead of a personal access token
- Multiple users with roles, and an audit log
- Long-term metrics storage and alerts (email, webhook, push)
- Agent self-update from the dashboard
