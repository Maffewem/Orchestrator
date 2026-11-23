# Cluster (Beta)

Automatic failover of workers between devices. Off by default.

- **Settings:** Cluster page. Turn failover on and choose the wait (30 to 3600 s, default 120) a device must be offline before its workers move.
- **Per worker:** switch on *Fail over*, optionally limit the *Allowed devices*, *Move back automatically* and *Start with no data*.
- **How it works:** every server tick, a worker whose device has been offline for the wait (and is not in maintenance) is moved with the migration engine to the least-loaded online, allowed device that can run it. The newest backup on the server is restored into the new container, so data written after that backup is lost. With no backup the worker is only moved if *Start with no data* is ticked. Stacks move without volume data.
- **Safeguards:** nothing moves when every device is offline, during the wait after a server restart, or while a migration of that worker is running. A failed worker is retried at most every 10 minutes.
- **When the old device returns:** the stale container there is stopped (or the worker is moved back with its live data if *Move back automatically* is on).
- **Not included:** server high availability (see the failover design in PR #75), shared Cloudflare tunnel replicas, and live-test of any of this.

## Replicas

A worker can also run an extra, independent copy on other devices. Tick devices under *Replicas* on the Cluster page. Every deployment of the worker (manual, push, retry) is repeated on each replica device, and a missing replica is started by the server (retried every 10 minutes), for example on a newly added device or when one comes back online. Nothing is synced or moved between copies: the application itself must handle several copies (own replication, or stateless). Domains and the Caddy proxy still point at the main copy only, so traffic is not load-balanced yet. Replica deployments show on the project's Deployments list with the trigger `replica`.

## Server high availability: manual standby (Beta)

A second server (the standby) keeps a verified copy of the active server and can be promoted by hand. Cluster page > Server high availability.

1. On the active server, *Create a pairing token*.
2. On a **fresh** second server, enter the active server's address, the token and this server's own address, then *Make this server a standby*.
3. The standby is read-only (writes get HTTP 423), refuses agents (503) and runs no background work. Every 15 s it pulls a snapshot of the active database, including the secret key, encrypted with the token, checks that it opens, and keeps the newest good one. It shows *ready* while the last copy is under a minute old.
4. *Promote to active* stages the newest copy as the next database and restarts (the container needs a restart policy). Choosing "switch over" first asks the active server to step down and takes a last copy, so nothing is lost. "It is down" promotes immediately and can lose up to 15 s of changes.
5. The promoted server has a higher epoch (sent as `X-Orch-Epoch` on every response) and keeps telling the old server to step down until it answers. The old server then restarts as a standby of the new one, so there is never a second writer once the two can talk.

Not included yet: agents ignoring a lower epoch (a stale server that cannot be reached could still instruct agents that can reach it), adding the standby's address to agents automatically, automatic promotion, installing the standby from the dashboard, and repointing the dashboard address (DNS or tunnel). Add the standby under Settings > Server addresses yourself. Everything here is untested live.
