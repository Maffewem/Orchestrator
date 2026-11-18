# Worker metrics

Each worker's **Metrics** tab shows its history over the last hour, 24 hours, 7 days or 30 days.
**Customise** chooses, per worker, which metrics are collected, how often a sample is stored
and how long history is kept. Switching a metric off stops new samples; history already stored
ages out at the retention limit.

## Where the numbers come from

| Worker | Source | Metrics |
| --- | --- | --- |
| Container on a device | Docker stats and inspect, read by the agent every 15 s | CPU (% of one core), memory (working set, against the limit if one is set), network in/out, disk reads/writes, processes, CPU throttling, restarts, out-of-memory kills, health |
| Container on a device | Docker disk usage, every 30 min | Named volume sizes (latest only) |
| Container behind the device's proxy | Caddy's Prometheus metrics on `127.0.0.1:2019` (loopback only) | Requests, status code classes, response time p50/p95/p99, bytes sent |
| Cloudflare Worker | GraphQL `workersInvocationsAdaptive`, polled every 2 min, stored by hour | Requests, errors, subrequests, CPU time p50/p99, wall time p50/p99, requests by outcome and by version |
| Every project | Orchestrator's own deployment records | Deployments, success rate, median and p95 duration, rollbacks |

Not available: request counts by country for Workers (the Workers dataset has no country
dimension) and request analytics for Pages sites (not in Cloudflare's API).

## Storage

Samples live in the server's SQLite database (`worker_samples`, `proxy_samples`, `cf_samples`,
`cf_breakdown`). One container at a 1-minute interval for 30 days is roughly 43,000 rows, a few
megabytes; the Customise dialog shows an estimate for the chosen settings. Each worker's history
is pruned to its retention hourly, and anything older than 91 days is removed for workers that no
longer exist.

## Requirements

- Agents send per-container samples from this version on (capability `stats`). Update older
  agents from the Updates page.
- Proxy metrics need the proxy's Caddyfile to be rewritten once, which happens on the next proxy
  sync (any domain change, or a redeploy).
- Worker analytics need the Cloudflare token's **Account Analytics: Read** permission. Wall time
  and the version split are asked for separately, so an account whose schema lacks them still
  gets the other figures; the Requests section says which were refused.
