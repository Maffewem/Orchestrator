# Branches, channels and testing

Two long-lived branches:

| Branch | Channel | Image tags | Who follows it |
| --- | --- | --- | --- |
| `main` | stable | `:stable` and `:latest` (same image), `:sha-<commit>` | Production servers and devices |
| `dev` | Beta | `:dev`, `:sha-<commit>` | A test server and its canary device |

`latest` is kept as an alias of `stable` so installs made before channels existed keep
updating exactly as before.

## Day-to-day flow

1. Branch from `dev`, open the pull request **into `dev`**.
2. CI (gofmt, `go vet`, `go test -race`, cross-compiling agents for amd64, arm64, armv7 and
   armv6, web typecheck, tests and build) must pass. It also runs on every push to `dev` and `main`.
3. After merging, CI publishes `ghcr.io/maffewem/orchestrator:dev` and the matching agent image.
   The Beta test server picks it up (Updates page, or on its schedule).
4. Live-test on the Beta server (below). Fixes go to `dev` the same way.
5. When the checklist passes, open one pull request **`dev` → `main`**. CI additionally
   builds both images on that pull request. Merging publishes `:stable`/`:latest`.
6. Production updates from the Updates page or its schedule, as before. Use a merge commit (not
   squash) for promotions so `dev` and `main` do not drift apart.

Hotfixes: branch from `main`, pull request into `main`, then merge `main` back into `dev`.

## How it is tested

* **Automatically, on both branches:** unit and integration tests run against a fake Docker
  socket, fake GitHub and fake Cloudflare. They cannot prove Caddy, real Docker builds, real
  tunnels or real devices work.
* **Live, on a Beta server before promoting.** Run a second server beside production, with its
  own port and data volume, and point one spare device (the canary) at it:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/Maffewem/Orchestrator/dev/scripts/install-server.sh \
    | sudo ORCH_NAME=orchestrator-dev ORCH_PORT=8081 sh -s -- --channel beta
  ```

  This creates the container `orchestrator-dev` on port 8081 with its own volume
  `orchestrator-dev-data`; production is untouched. Add the canary device from the dev
  dashboard's *Add device* wizard. Agents update from the server they report to, so a
  canary on the dev server runs dev agents and production devices stay on stable.
  Use a throwaway GitHub project and a spare domain or tunnel, not production ones.
* **Moving an existing server between channels** is possible from the Updates page (the
  Update channel selector, Stable or Beta, administrators only; agents follow the server).
  Prefer a separate Beta server for anything you care about.

## Always a way back from Beta to Stable

Every path is covered, from the dashboard and from the command line:

| Situation | Way back |
| --- | --- |
| Beta server is running | Updates page, Update channel, Stable, Switch to Stable. Data is kept and agents follow. |
| New image never becomes healthy | Automatic: the updater restores the previous container within 90 seconds. |
| Dashboard unreachable, or Stable won't start | On the host: `curl -fsSL https://raw.githubusercontent.com/Maffewem/Orchestrator/main/scripts/install-server.sh \| sudo sh -s -- --channel stable` (add `ORCH_NAME`/`ORCH_PORT` for a non-default server). Same volume, so data is kept. |
| A Beta build changed the database and Stable can't read it | The server saves a snapshot to `channel-snapshots/` in its data volume (newest 3 kept) before every switch. Re-run the command above with `--restore-snapshot`: the newest snapshot is put back, the current database is kept as `orchestrator.db.pre-restore`, and changes made on Beta since the switch are lost. The secrets key (`orchestrator.db.key` or `ORCH_SECRET_KEY`) is untouched. |
| Beta agent can't reach its server | The agent keeps its previous binary and falls back to it if the new one doesn't manage a heartbeat. If a device is stranded on a Beta agent, move the server to Stable and use *Update agents* on the Updates page, or re-run the device's install command. Agents compare versions for equality, so going back to an older build is treated as an update. |

Switching in either direction is refused unless a snapshot can be saved first.

## Live-test checklist before promoting

Only the items touched by the changes need re-running; tick what applies.

- [ ] Beta server updates itself from the Updates page and reconnects; rollback works when the new image is unhealthy
- [ ] Canary agent updates to the Beta build and reports the new version
- [ ] A GitHub push builds and deploys a project on the canary (Docker build, health check)
- [ ] A pull request creates a preview and closing it removes the container
- [ ] Domains via Caddy (certificate, `forward_auth` login) and the Cloudflare tunnel, if touched
- [ ] Backup then restore of a worker; server backup restore if touched
- [ ] Cloudflare Pages/Workers deploy and monitoring, if touched
- [ ] Switch back to Stable from the Updates page works and the canary agent follows
- [ ] Anything else the pull request description lists as untested

## One-time GitHub settings (needs the repository owner)

* Create the `dev` branch from `main`.
* Optional but recommended: protect `main` so it only accepts pull requests with the CI
  checks passing, and protect `dev` the same way. Nothing in this repository changes
  branch protection.
