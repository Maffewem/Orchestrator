# Server failover: design proposal

Status: **proposal with a clickable demo** (dashboard > Failover (demo)). Nothing here is implemented. It builds on *Move the server* (`docs/MOVING.md`) and the agent address work (fallback URLs, announced addresses, identity key).

## Goal
A second device holds a continuously refreshed copy of the server's data and secret key. If the active server dies, the standby takes over within about 20 seconds without a person, and the old server later rejoins as the standby. Two servers must never both accept changes.

## Recommended approach

### 1. Standby and sync
- Set up like a move: the existing `server.install` step starts the same image in **standby mode** (read-only, no background loops, no dispatch, same freeze as a paused server today: HTTP 423, webhooks refused).
- The active server sends a verified copy every **15 seconds**: a consistent SQLite snapshot (`VACUUM INTO`), gzip, SHA-256, sealed with a key derived from a one-off pairing secret. The standby pulls it over the existing peer endpoint (token authenticated, same as move). The secret key (`ORCH_SECRET_KEY` / `.key` file) is sent once at pairing and re-sent if it changes.
- The standby is **Ready** only when its last copy opens, passes `PRAGMA integrity_check`, runs the same server version, and enough witnesses can reach it.
- Recovery point: a failover loses at most one interval of changes (recent deployment history, setting edits). Running containers are unaffected. A planned switch loses nothing because the old server sends a final copy first.
- Snapshots are chosen over WAL shipping for the first version: the database is small, the server stays stdlib-only, and restore is the already-tested import path. WAL streaming can reduce the interval later.

### 2. How agents find the active server
- Agents already keep several addresses and announced fallbacks. Add an **epoch** (integer, starts at 1, +1 on every promotion). Every response from a server carries its epoch; the agent stores the highest epoch seen and **ignores commands from a server with a lower one**.
- On a failed heartbeat the agent walks its address list (existing behaviour), and accepts only a server that signs a nonce with the server identity key (existing) *and* reports an epoch at least as high as its own.
- The standby is added to every agent's address list at pairing, so no agent is stranded. LAN discovery from the address work remains the last resort.
- The dashboard address (DNS, Cloudflare Tunnel) is the open piece: with a Tunnel, both servers can run the connector and the tunnel routes to whichever answers `GET /healthz` as active; otherwise it stays a manual or DNS-health-check step, as in a move.

### 3. Promotion and split-brain safeguards
Three independent protections; any one failing is covered by the others.

1. **Write lease on the active server.** It may accept changes only while a majority of agents on *other* devices have acknowledged a heartbeat in the last **10 s** (the agents' normal heartbeats carry the ack). If not, it goes read-only on its own.
2. **Quorum before promotion.** The standby promotes only after it has missed pulls for **15 s** (longer than the lease, so the old server has already stopped writing) **and** a majority of agents on other devices report they cannot reach the active server but can reach the standby. A broken link between only the two servers therefore never promotes.
3. **Epoch fencing at the agents.** Even if both somehow run, agents obey only the higher epoch, so the stale server cannot deploy, restart or delete anything.

With fewer than 3 other devices there is no meaningful quorum. Then automatic promotion is disabled (the page says so), or the optional **S3/R2 lease file** (conditional write) acts as a tie-breaker. **Manual promote** is always available as an override and requires confirming that the old server is down.

Clock skew: leases are measured on each side's own monotonic clock from the last acknowledgement received, never by comparing wall clocks.

### 4. Rejoining
When a server starts, or regains the network, it asks its peer and its agents for the highest epoch. If one is higher than its own it starts **fenced**, saves anything it wrote after the last synced copy as `diverged-epoch-N.db` for inspection, re-copies from the active server and becomes the standby. Nothing switches back by itself; returning the active role is a planned switch.

## Trade-offs

| Option | Pros | Cons |
|---|---|---|
| Agents as witnesses (recommended) | No extra infrastructure, works offline, uses data we already have | Needs at least 3 devices; an agent that is itself partitioned counts as a missing vote |
| S3/R2 lease | Works with two devices | Needs internet and a bucket; a bucket outage blocks failover |
| Manual only | Zero surprise | Slow, and needs a person awake |
| Single-opinion automatic | Simplest | Creates split-brain on a bad cable; not offered |

| Sync option | Pros | Cons |
|---|---|---|
| Snapshot every 15 s (recommended first step) | Simple, stdlib-only, reuses import | Up to 15 s of changes lost on a crash |
| WAL streaming | Near zero loss | Much more code and edge cases; a later improvement |
| Shared storage (NFS) | No copy | Not safe for SQLite; one more single point of failure |

Other costs: a second always-on server uses a little memory and bandwidth; the secret key lives on two devices (one more place to protect); both servers must run the same version, so updates go standby first, then a planned switch, then the other.

## Failure scenarios covered by the demo
1. Active server crashes: lease already gone, quorum agrees, promote at about 17 s, agents move at about 20 s.
2. Active server running but isolated: it fences itself at 10 s, standby promotes at about 17 s; on return it demotes.
3. Only the link between the two servers fails: agents still reach the active server, so no promotion and a warning.
4. Planned switch: final copy, promote, old server becomes standby, zero loss.

## Suggested implementation order
1. Standby mode and the 15 s sync, with the Ready check and a manual promote and demote (reuses move code).
2. Epoch in heartbeats and agent fencing.
3. Write lease and quorum, automatic promotion and rejoin, with an integration test using several in-process servers and fake agents (as the move tests do).
4. Dashboard address handling (Tunnel/DNS) and the optional S3 tie-breaker.

## Not covered
Network partitions that split the agents themselves in half, running more than two servers, multi-site setups, and failover of the dashboard address.
