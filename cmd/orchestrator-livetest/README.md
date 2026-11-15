# orchestrator-livetest

Live checks for features the unit tests can only cover with fakes: real Docker on a real device, Caddy,
GitHub, Cloudflare and S3. It talks to a **running** server with the admin token, creates only
`livetest-*` projects, and removes them (and their containers) afterwards. A project left behind by an
interrupted run is cleaned up at the start of the next one.

```sh
go build -o livetest ./cmd/orchestrator-livetest

export ORCH_URL=http://192.168.1.10:8080
export ORCH_ADMIN_TOKEN=...
./livetest -list          # every check, whether it can run, and what it is missing
./livetest                # run everything that is configured
./livetest -run 'streams|backup/container' -v
./livetest -yes           # CI: never prompt; guided checks are skipped
```

A check with a missing variable is **SKIP**ped, not failed. Exit status is 1 if any check fails.
Run it on a machine that can reach the devices' IPs (the checks fetch `http://<device-ip>:<port>`).

## Always needed

`ORCH_URL`, `ORCH_ADMIN_TOKEN`, and at least one online device with Docker. Optional:
`LIVETEST_DEVICE` (name or ID, default: first online device), `LIVETEST_PORT` (first of ~10 host ports
used, default 18080).

## Checks

| Check | What it proves | Also needs |
|---|---|---|
| `core/deploy-image` | image deploy to a real device, page served | nothing |
| `core/maintenance-mode` | maintenance refuses new workers, lifts again | nothing |
| `lifecycle/stop-start` | stop/start via the agent | nothing |
| `streams/logs` | live log stream carries a request we just made | nothing |
| `streams/shell` | shell runs `echo` in the container (real exec hijack) | nothing |
| `backup/container` | container backup, restore, still served | agent with backup capability |
| `backup/server` | server backup is created and listed | nothing |
| `backup/server-restore` | restore, server restarts itself, a project only the backup knew is back | `LIVETEST_SERVER_RESTORE=1`. **Rolls the server's data back to the backup moment**, so do it on a server you can lose a few seconds of changes on. The server must run in Docker with a restart policy. |
| `backup/dest-s3` | S3/R2 destination test and a replicated backup | `LIVETEST_S3_ENDPOINT`, `_BUCKET`, `_ACCESS_KEY`, `_SECRET_KEY`, optional `_REGION` |
| `backup/dest-device` | copy to another device | `LIVETEST_DEVICE2` |
| `domains/caddy` | the per-device Caddy proxy answers for `name.<base>` over HTTP and HTTPS | `LIVETEST_BASE_DOMAIN`; the name must be one Caddy can get a certificate for (or see below) |
| `domains/login-gate` | forward_auth gate: 401 without or with a wrong password, 204 and the page for a temporary user | `LIVETEST_BASE_DOMAIN` |
| `env/encryption` | a secret env value is not found in the SQLite file | `LIVETEST_DB_PATH` readable by the tool (run on the server host) |
| `env/fresh-restore` | guided: restore onto an empty server with and without the key | a terminal |
| `adopt/redeploy` | dry-run, adopt, refuse unconfirmed redeploy, confirmed redeploy | `LIVETEST_ADOPT_CONTAINER` on the device. **The container is recreated by Orchestrator**; use a disposable one. |
| `migrate/device` | migrate with data to a second device | `LIVETEST_DEVICE2` |
| `tunnel/state` | tunnel is set up and its container runs | `LIVETEST_TUNNEL=1` after setting the tunnel on the device |
| `sso/sign-in` | guided browser sign-in, allowed and refused account | SSO configured, a terminal |
| `github/webhook-signed` | forged signature gets 401; a correctly signed push deploys with trigger `push` | `LIVETEST_GH_REPO` (owner/name, Dockerfile serving on port 80), GitHub connected; optional `_GH_BRANCH`, `_GH_CONTEXT`, `_GH_TOKEN` for private repos |
| `github/real-push` | guided: you push, GitHub delivers, a deployment appears | as above, and the repo webhook pointed at the server |
| `github/preview-pr` | guided: open a PR, preview container and URL, close it, cleanup incl. tunnel CNAME | as above plus `LIVETEST_BASE_DOMAIN` |
| `node/build` | Node image build on the device, steps Cloning/Installing/Building succeed | `LIVETEST_NODE_REPO`; optional `_NODE_BRANCH`, `_CONTEXT`, `_INSTALL`, `_BUILD`, `_START`, `_VERSION`, `_STATIC=1`, `_OUTPUT` |
| `cloudflare/pages` | build and upload a Pages project, URL answers, monitoring reads it | `LIVETEST_CF_ACCOUNT_ID`, `_CF_TOKEN` (Pages Edit, Workers Scripts Edit, Account Analytics Read), `_CF_PAGES_REPO` |
| `cloudflare/worker-analytics` | deploy a Worker, send traffic, read back analytics (the GraphQL query that has never run live) | the two above plus `_CF_WORKER_REPO`; optional `_CF_WORKER_URL` |
| `cloudflare/discover` | list existing Pages projects and Workers | account ID and token |

Cloudflare repos can take `LIVETEST_CF_BRANCH`, `_CF_CONTEXT`, `_CF_INSTALL`, `_CF_BUILD`, `_CF_OUTPUT`.
Pages projects and Workers cannot be deleted through Orchestrator, so the run tells you which ones to
remove in your Cloudflare account.

## Notes and limits

- Caddy checks connect to the device on port 443 with the hostname as SNI and **do not verify the
  certificate** (the proxy may use an internal CA); they test routing and the gate, not the certificate.
- S3 and device-copy checks rely on the server's own report (connection test and the destination's last
  copy without error); they do not read the object back.
- Not covered because they cannot be driven from an API: real Cloudflare Access SSO (use `sso/sign-in`),
  Tunnel traffic from outside your network (use `github/preview-pr`'s prompts and a phone on mobile data).
- A failed check prints the server's or the failing deployment step's message; re-run with `-v` for more.
