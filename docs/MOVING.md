# Moving the main server

Dashboard: **Settings > Main server** (admin; the old /move address redirects there). The panel walks through five steps: choose the device, copy and start, check it, switch devices, confirm.

## How it works
1. The old server pauses dashboard changes, copies its database (including the key that seals secrets) into an encrypted one-off bundle and asks the chosen device's agent to run `server.install`.
2. The agent pulls the same image, seeds a new `orchestrator-data` volume with `orchestrator-server import-backup`, and starts a container named `orchestrator` (restart on boot, Docker socket mounted when present).
3. The old server checks the new one answers and signs a fresh nonce with the restored identity key, then deletes the bundle. Nothing has switched yet.
4. **Switch**: the old server enters standby (no work, only a redirect in heartbeats). Agents follow it, keeping the old address as a way back. Their tokens are in the copy, so they keep their identity.
5. **Confirm** on the new dashboard (or finish from the old one). The old server stays as a redirecting standby; shut it down when happy.

## Going back
Until you shut the old server down, either side can hand control back: **Move back** on the new dashboard, or **Take this server back** on the old one. Each tells the other (shared one-off token); if the peer is unreachable, agents fall back to the old address on their own. Changes made on the new server after the switch are not carried back.

## Dashboard address
Point your domain or Cloudflare Tunnel at the new device when you switch so bookmarks and the GitHub webhook keep working. This is manual.

## By hand
Settings: Main server > Moving by hand downloads a password-protected copy. On the new machine: `sudo sh install-server.sh --import FILE [--import-password-file F]`.
If `ORCH_SECRET_KEY` is set on the new server it wins over the key in the copy.
