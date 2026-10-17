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

- **Auto-deploy** happens two ways. A GitHub push webhook deploys immediately. As a
  fallback for servers GitHub can't reach, the control plane also polls each auto-deploy
  project's branch every 60 seconds (`-poll`, `0` disables) and deploys when the head moves.
- **Environment variables** are edited as a table (secret-looking values are masked), as one
  raw box, or imported from a `.env` file, which merges into what is already there.
- **Backups** (Workers page, archive button) stream a container's configuration and the data
  in its writable volumes and bind mounts to the control plane as one `.tar.gz`, with nothing
  staged on the device. Download them from the Backups page, or restore one: the data is
  written back into the container of the same name on the original device, which is stopped
  while files are copied and then started again. Files are copied live, so databases may want
  to be stopped first. Archives are stored unencrypted in `-backups` (default: beside the
  database), so protect that directory like the database itself.

- **Backups from a worker's page** have a schedule (daily or weekly, with a number of
  backups to keep) under the worker's Backups tab, as well as "Back up now". Agents that are
  too old to back up are named in the error, with the fix (Updates page, or re-run the
  install command).
- **Domains** (a worker's Domains tab): set a base domain under Settings and every project
  gets `name.<base domain>`, with an optional `<branch>-name.<base domain>` URL for each
  preview (see below; create a wildcard `*.<base domain>` DNS record so they resolve). Add custom domains, or a hostname plus path route such as `example.com/api/*`. Each
  one shows the DNS record to create (an A record to the device's IPv4 address) and whether
  DNS already points there. The control plane renders a Caddyfile per device and the agent
  runs a `caddy:2-alpine` container (`orchestrator-proxy`, host network, restarts with
  Docker) with it, so there is no central proxy. Caddy gets certificates itself, which needs
  ports 80 and 443 reachable from the internet (or use a Cloudflare Tunnel instead).

- **Previews and rollback** (a worker's Previews tab and the deployment menu): turn previews
  on for a GitHub project and every open pull request from the same repository, into the
  tracked branch, is built as its own container (`name--branch`) on a free host port in
  20000-29999, rebuilt on each push and removed when the pull request closes or when more than
  the chosen maximum are open. Pull requests from forks are never built, and branches without a
  pull request are not previewed. With a base domain and the Preview URL switch on, each is
  served at `https://branch-name.<base domain>` (and inherits the worker's login setting);
  otherwise at `http://<device ip>:<port>`. Needs a recent agent. Any succeeded production
  deployment can be rolled back to from its "..." menu: GitHub builds reuse the image if the
  device still has it, otherwise rebuild that commit. Image projects redeploy the earlier
  image reference, so a moving tag such as `latest` is pulled again.

- **Accounts and roles** (Users page): sign in with a username and password instead of the
  shared admin token. The first visit asks for the admin token once, to create the first
  administrator. Roles: *viewer* (read-only), *operator* (deploy, restart, back up, projects,
  domains) and *admin* (users, settings, updates, deleting). Passwords are stored as salted
  PBKDF2-SHA256, sessions last 14 days and end when a password or role changes, and repeated
  wrong passwords lock that user and address out for a minute. The admin token keeps working
  as a fallback.
- **Login for a worker** (Domains tab, Access): Caddy on the device asks the control plane
  (`forward_auth`) to check the visitor's Orchestrator username and password, any role,
  before the worker loads. If the device's agent doesn't know the control plane address, the
  site refuses everyone rather than serving unprotected. Needs a recent agent.

- **Updates** (Updates page): check for the latest published build, update the server on
  demand, and set a schedule (daily or weekly, at a time in your time zone, or off). The
  server pulls the new image, then a short-lived helper container from that image swaps the
  server's container and keeps the old one as `orchestrator-previous`. If the new container
  doesn't report healthy within 90 seconds, the helper puts the old one back. Agents
  download the build the server carries, check its SHA-256 and that it runs, keep their old
  binary, and fall back to it if the new one never manages a heartbeat. Agents running as
  containers are updated by pulling a newer image instead.

  Updating the server from the dashboard needs the Docker socket inside the server
  container, which gives that container full control of Docker on the host. The installer
  and compose file turn it on by default; opt out with
  `curl ... install-server.sh | sudo sh -s -- --no-web-update` (or `ORCH_WEB_UPDATE=0`), or
  by removing the socket and `group_add` lines from compose. With it off, the page still
  shows versions, and agent updates and schedules for agents work. If you started the
  server some other way, add
  `-v /var/run/docker.sock:/var/run/docker.sock --group-add "$(stat -c %g /var/run/docker.sock)"`
  to its `docker run`.

### Supported devices

The agent is built for `linux/amd64`, `linux/arm64` (Pi 3/4/5 on 64-bit OS),
`linux/armv7` (32-bit Raspberry Pi OS) and `linux/armv6` (Pi Zero / Pi 1). The installer
picks the right one automatically. On Debian, Raspberry Pi OS and Ubuntu the installer sets
up everything from scratch: if Docker or `git` are missing it installs them (Docker through
get.docker.com), enables the Docker service, then installs the agent.

## Quick start

On a fresh Debian, Raspberry Pi OS or Ubuntu machine, one command installs Docker if needed
and starts the control plane:

```sh
curl -fsSL https://raw.githubusercontent.com/Maffewem/Orchestrator/main/scripts/install-server.sh | sudo sh
```

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

Run like this, the server is tied to your terminal and stops when the SSH session ends. To
keep it running, make it a service:

```sh
sudo tee /etc/systemd/system/orchestrator.service >/dev/null <<'UNIT'
[Unit]
Description=Orchestrator control plane
After=network-online.target

[Service]
WorkingDirectory=/opt/orchestrator
ExecStart=/opt/orchestrator/orchestrator-server -db /opt/orchestrator/orchestrator.db -downloads /opt/orchestrator/downloads
Restart=always

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl enable --now orchestrator
```

## When the dashboard stops loading

First check whether the server is still running. It answers `curl http://<server>:8080/api/health`
with `{"status":"ok",...}`.

| Server runs as | What happened | Check |
| --- | --- | --- |
| Docker (`install-server.sh`, `docker run`, compose) | The container stopped or was removed | `docker ps -a`, then `docker logs --tail 100 orchestrator` |
| `docker compose up` without `-d` | Compose stops when the terminal closes | Use `docker compose up -d` |
| A binary started by hand | It was killed with your SSH session | Run it as a service (above) |
| systemd service | It crashed or isn't enabled | `systemctl status orchestrator`, `journalctl -u orchestrator -n 100` |

Both installers enforce this: they enable the Docker service at boot, set `--restart unless-stopped`, repair workers that were created without a restart policy, and print a checklist at the end. Re-running `install-server.sh` (or the agent install command) repairs an existing install.

Docker restarts the container by itself (`--restart unless-stopped`) after a crash or reboot,
as long as the Docker service is enabled (`systemctl is-enabled docker`). Device agents run as
the `orchestrator-agent` service, so on a device that looks offline use
`systemctl status orchestrator-agent` and `journalctl -u orchestrator-agent -n 100`. Agents
reconnect on their own once the server is back.

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
| `POST /api/devices/{id}/containers/{cid}/backup` | back up a container |
| `GET /api/backups`, `GET /api/backups/{id}/download`, `DELETE /api/backups/{id}` | backups |
| `POST /api/backups/{id}/restore` | restore a backup into its container |
| `GET /api/system/update`, `POST /api/system/update/check`, `POST /api/system/update/apply` | update status, check, update the server |
| `PUT /api/system/update/schedule`, `POST /api/system/update/agents` | update schedule, update agents |
| `GET/PUT /api/devices/{id}/workers/{name}/backup-policy` | automatic backups for a worker |
| `GET/POST /api/projects/{id}/domains`, `PUT .../domains/urls`, `DELETE .../domains/{did}` | domains and the worker URLs |
| `PUT /api/settings/domain` | base domain |
| `POST /api/deployments/{id}/rollback` | roll back to an earlier deployment |
| `GET /api/projects/{id}/previews`, `PUT .../previews/settings`, `DELETE .../previews/{pid}` | preview deployments |
| `GET /api/auth/status`, `POST /api/auth/login`, `POST /api/auth/setup` | sign in, first-run setup (no token needed) |
| `GET /api/auth/me`, `POST /api/auth/logout`, `POST /api/auth/password` | the signed-in user |
| `GET/POST /api/users`, `PATCH/DELETE /api/users/{id}` | accounts (admin) |
| `PUT /api/projects/{id}/domains/access` | require login for a worker |
| `GET /api/events` | activity feed |
| `GET /api/settings`, `PUT /api/settings/github`, `POST /api/settings/join-token/rotate` | settings |

The agent uses `POST /api/agent/register`, `/heartbeat`, `/commands/{id}/result` and
`/deployments/{id}/steps/{n}`, plus `/backups/{id}` to upload or fetch a backup archive. GitHub calls `POST /api/github/webhook`, which is verified
with HMAC-SHA256.

## Security notes

- Dashboard accounts have roles (viewer, operator, admin); the API enforces them per route.
  The admin token acts as an administrator and works as a break-glass login.
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
