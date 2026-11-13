# Branches, channels and testing

Two long-lived branches:

| Branch | Channel | Image tags | Who follows it |
| --- | --- | --- | --- |
| `main` | stable | `:stable` and `:latest` (same image), `:sha-<commit>` | Production servers and devices |
| `dev` | dev | `:dev`, `:sha-<commit>` | A test server and its canary device |

`latest` is kept as an alias of `stable` so installs made before channels existed keep
updating exactly as before.

## Day-to-day flow

1. Branch from `dev`, open the pull request **into `dev`**.
2. CI (gofmt, `go vet`, `go test -race`, cross-compiling agents for amd64, arm64, armv7 and
   armv6, web typecheck, tests and build) must pass. It also runs on every push to `dev` and `main`.
3. After merging, CI publishes `ghcr.io/maffewem/orchestrator:dev` and the matching agent image.
   The dev test server picks it up (Updates page, or on its schedule).
4. Live-test on the dev server (below). Fixes go to `dev` the same way.
5. When the checklist passes, open one pull request **`dev` → `main`**. CI additionally
   builds both images on that pull request. Merging publishes `:stable`/`:latest`.
6. Production updates from the Updates page or its schedule, as before. Use a merge commit (not
   squash) for promotions so `dev` and `main` do not drift apart.

Hotfixes: branch from `main`, pull request into `main`, then merge `main` back into `dev`.

## How it is tested

* **Automatically, on both branches:** unit and integration tests run against a fake Docker
  socket, fake GitHub and fake Cloudflare. They cannot prove Caddy, real Docker builds, real
  tunnels or real devices work.
* **Live, on a dev server before promoting.** Run a second server beside production, with its
  own port and data volume, and point one spare device (the canary) at it:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/Maffewem/Orchestrator/dev/scripts/install-server.sh \
    | sudo ORCH_NAME=orchestrator-dev ORCH_PORT=8081 sh -s -- --channel dev
  ```

  This creates the container `orchestrator-dev` on port 8081 with its own volume
  `orchestrator-dev-data`; production is untouched. Add the canary device from the dev
  dashboard's *Add device* wizard. Agents update from the server they report to, so a
  canary on the dev server runs dev agents and production devices stay on stable.
  Use a throwaway GitHub project and a spare domain or tunnel, not production ones.
* **Moving an existing server between channels** is possible from the Updates page
  (*Switch to dev* / *Switch to stable*, administrators only). Prefer a separate dev server:
  a dev build may change the database in a way stable cannot read, in which case stable
  refuses to start and the previous container is restored.

## Live-test checklist before promoting

Only the items touched by the changes need re-running; tick what applies.

- [ ] Dev server updates itself from the Updates page and reconnects; rollback works when the new image is unhealthy
- [ ] Canary agent updates to the dev build and reports the new version
- [ ] A GitHub push builds and deploys a project on the canary (Docker build, health check)
- [ ] A pull request creates a preview and closing it removes the container
- [ ] Domains via Caddy (certificate, `forward_auth` login) and the Cloudflare tunnel, if touched
- [ ] Backup then restore of a worker; server backup restore if touched
- [ ] Cloudflare Pages/Workers deploy and monitoring, if touched
- [ ] Anything else the pull request description lists as untested

## One-time GitHub settings (needs the repository owner)

* Create the `dev` branch from `main`.
* Optional but recommended: protect `main` so it only accepts pull requests with the CI
  checks passing, and protect `dev` the same way. Nothing in this repository changes
  branch protection.
