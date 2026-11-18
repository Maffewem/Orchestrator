import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router";
import { Box, Cloud, Globe, Rocket, SlidersHorizontal, Activity } from "lucide-react";
import { api, type MetricPrefs, type MetricPrefsView, type MetricsRange, type Project, type ProjectMetrics, type WorkerMetrics } from "../api";
import { bytes, cx, uptime, usePoll } from "../lib";
import { useCan } from "./auth";
import { Badge, Button, Card, CardHeader, ErrorNote, LineChart, Modal, RefreshButton, Skeleton } from "./ui";
import { CloudflareMetrics } from "./CloudflareProject";

// The Metrics tab for a worker: a container on a device (resources and proxy traffic), or a
// Cloudflare Worker or Pages project, plus deployment history for both.

const RANGES: { id: MetricsRange; short: string; long: string }[] = [
  { id: "1h", short: "1h", long: "Last hour" },
  { id: "24h", short: "24h", long: "Last 24 hours" },
  { id: "7d", short: "7d", long: "Last 7 days" },
  { id: "30d", short: "30d", long: "Last 30 days" },
];

// --- catalogue of what a worker can collect ---

type MetricInfo = { id: string; label: string; source: string; storage: string; available?: false; reason?: string };

const CONTAINER_METRICS: MetricInfo[] = [
  { id: "cpu", label: "CPU", source: "Docker stats, percent of one core", storage: "One value per sample" },
  { id: "memory", label: "Memory", source: "Docker stats, working set against the limit", storage: "One value per sample" },
  { id: "network", label: "Network in and out", source: "Docker stats; not for containers on the host network", storage: "Two values per sample" },
  { id: "disk_io", label: "Disk reads and writes", source: "Docker stats (block I/O)", storage: "Two values per sample" },
  { id: "processes", label: "Processes", source: "Docker stats (PIDs)", storage: "One value per sample" },
  { id: "throttling", label: "CPU throttling", source: "Time held back by the container's CPU limit", storage: "One value per sample" },
  { id: "restarts", label: "Restarts and health", source: "Docker inspect: restarts, out-of-memory kills, health checks", storage: "One value per sample" },
  { id: "volumes", label: "Volume size", source: "Docker disk usage, measured every 30 minutes", storage: "Latest only, not stored" },
  { id: "proxy_requests", label: "Proxy requests and status codes", source: "Caddy metrics on the device's proxy", storage: "One row per site per sample" },
  { id: "proxy_latency", label: "Proxy response time p50, p95, p99", source: "Caddy request duration histogram", storage: "Adds the histogram to each row" },
  { id: "proxy_bandwidth", label: "Proxy bandwidth", source: "Caddy response sizes", storage: "One value per row" },
];

const WORKER_METRICS: MetricInfo[] = [
  { id: "cf_requests", label: "Requests, errors and subrequests", source: "Cloudflare GraphQL analytics, hourly", storage: "One row an hour" },
  { id: "cf_cpu_time", label: "CPU time p50 and p99", source: "Cloudflare GraphQL analytics, hourly", storage: "Two values an hour" },
  { id: "cf_wall_time", label: "Wall time p50 and p99", source: "Cloudflare GraphQL analytics; not offered on every account", storage: "Two values an hour" },
  { id: "cf_by_status", label: "Requests by outcome", source: "Success, exceptions, exceeded limits, disconnects", storage: "A few rows an hour" },
  { id: "cf_by_version", label: "Requests by version", source: "Useful during gradual deployments", storage: "One row per live version an hour" },
  { id: "cf_country", label: "Requests by country", source: "", storage: "", available: false, reason: "Cloudflare's Workers analytics has no country breakdown" },
];

const PAGES_METRICS: MetricInfo[] = [
  { id: "pages_requests", label: "Pages request analytics", source: "", storage: "", available: false, reason: "Cloudflare's API keeps no request analytics for Pages; use Web Analytics" },
];

const OUTCOME: Record<string, string> = {
  success: "Success",
  scriptThrewException: "Exception",
  exceededResources: "Exceeded limits",
  clientDisconnected: "Client disconnected",
  internalError: "Internal error",
};

// --- formatting and small parts ---

const fmtNum = (n: number) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e4 ? `${(n / 1e3).toFixed(1)}k` : n.toLocaleString(undefined, { maximumFractionDigits: 0 }));
const fmtMs = (n?: number) => (n == null ? "—" : n >= 1000 ? `${(n / 1000).toFixed(2)} s` : `${n < 10 ? n.toFixed(1) : Math.round(n)} ms`);
const fixed = (digits: number, unit = "") => (v: number) => `${v.toFixed(v !== 0 && Math.abs(v) < 1 && digits === 0 ? 1 : digits)}${unit}`;
const fmtSecs = (n?: number) => (n == null ? "—" : n >= 60 ? `${Math.floor(n / 60)} m ${Math.round(n % 60)} s` : `${Math.round(n)} s`);

/** Picks a byte unit for a set of per-second rates and scales them. */
function rateUnit(values: number[]): { unit: string; div: number } {
  const m = Math.max(0, ...values);
  if (m >= 1024 * 1024) return { unit: " MB/s", div: 1024 * 1024 };
  if (m >= 1024) return { unit: " KB/s", div: 1024 };
  return { unit: " B/s", div: 1 };
}

function niceMax(values: number[], floor = 1): number {
  const m = Math.max(floor, ...values);
  const p = 10 ** Math.floor(Math.log10(m));
  for (const f of [1, 2, 2.5, 5, 10]) if (m <= f * p) return f * p;
  return m;
}

const defined = <T,>(xs: (T | undefined | null)[]): T[] => xs.filter((x): x is T => x != null);

function Tile({ label, value, sub, loading, tone }: { label: string; value: ReactNode; sub?: ReactNode; loading: boolean; tone?: "bad" | "warn" }) {
  return (
    <Card className="p-4">
      <p className="text-xs font-medium text-neutral-500">{label}</p>
      {loading ? (
        <div className="mt-2 space-y-2">
          <Skeleton className="h-7 w-24" />
          <Skeleton className="h-3 w-32" />
        </div>
      ) : (
        <>
          <p className={cx("mt-1 truncate text-2xl font-semibold tabular-nums", tone === "bad" && "text-bad", tone === "warn" && "text-warn")}>{value}</p>
          {sub && <p className="mt-0.5 truncate text-xs text-neutral-500">{sub}</p>}
        </>
      )}
    </Card>
  );
}

function Panel({ title, subtitle, loading, empty, children }: { title: string; subtitle?: string; loading: boolean; empty?: boolean; children: ReactNode }) {
  return (
    <Card>
      <CardHeader title={title} subtitle={subtitle} />
      <div className="p-5">
        {loading ? <Skeleton className="h-28 w-full" /> : empty ? <p className="flex h-28 items-center justify-center text-sm text-neutral-500">No samples in this range yet.</p> : children}
      </div>
    </Card>
  );
}

function Split({ rows, loading }: { rows: { label: string; value: number; color: string }[]; loading: boolean }) {
  const total = rows.reduce((a, r) => a + r.value, 0);
  if (loading) return <Skeleton className="h-28 w-full" />;
  if (!total) return <p className="py-6 text-center text-sm text-neutral-500">No requests in this range.</p>;
  return (
    <div className="space-y-2.5">
      {rows
        .filter((r) => r.value > 0)
        .map((r) => (
          <div key={r.label} className="text-xs">
            <div className="mb-1 flex justify-between gap-3">
              <span className="truncate text-neutral-700">{r.label}</span>
              <span className="shrink-0 tabular-nums text-neutral-500">
                {fmtNum(r.value)} · {((r.value / total) * 100).toFixed(1)}%
              </span>
            </div>
            <div className="h-2 rounded-full bg-neutral-100">
              <div className="h-2 rounded-full" style={{ width: `${(r.value / total) * 100}%`, background: r.color }} />
            </div>
          </div>
        ))}
    </div>
  );
}

function Bars({ points }: { points: { t: number; succeeded: number; failed: number }[] }) {
  const max = Math.max(1, ...points.map((p) => p.succeeded + p.failed));
  return (
    <div>
      <div className="flex h-28 items-end gap-1">
        {points.map((p) => (
          <div key={p.t} className="flex h-full flex-1 flex-col justify-end" title={`${new Date(p.t * 1000).toLocaleString()}: ${p.succeeded} succeeded, ${p.failed} failed`}>
            <div style={{ height: `${(p.failed / max) * 100}%` }} className="rounded-t-sm bg-bad" />
            <div style={{ height: `${(p.succeeded / max) * 100}%` }} className={cx("bg-primary", !p.failed && "rounded-t-sm")} />
          </div>
        ))}
      </div>
      <div className="mt-2 flex gap-4 text-xs text-neutral-600">
        <span className="inline-flex items-center gap-1.5"><span className="size-2 rounded-full bg-primary" />Succeeded</span>
        <span className="inline-flex items-center gap-1.5"><span className="size-2 rounded-full bg-bad" />Failed</span>
      </div>
    </div>
  );
}

/** Fills the range with empty buckets so deployment bars line up in time. */
function fillBuckets(series: { t: number; succeeded: number; failed: number }[], range: MetricsRange, step: number) {
  const span = { "1h": 3600, "24h": 86400, "7d": 7 * 86400, "30d": 30 * 86400 }[range];
  // Deployments are counted per hour, day or so; finer buckets would be mostly empty.
  const bucket = range === "1h" ? 300 : range === "24h" ? 3600 : range === "7d" ? 86400 / 2 : 86400;
  const now = Math.floor(Date.now() / 1000);
  const start = Math.floor((now - span) / bucket) * bucket;
  const out = new Map<number, { t: number; succeeded: number; failed: number }>();
  for (let t = start; t <= now; t += bucket) out.set(t, { t, succeeded: 0, failed: 0 });
  for (const p of series) {
    const k = Math.floor(p.t / bucket) * bucket;
    const e = out.get(k);
    if (e) {
      e.succeeded += p.succeeded;
      e.failed += p.failed;
    }
  }
  void step;
  return [...out.values()];
}

// --- toolbar and section switcher ---

function Toolbar({ range, setRange, busy, onRefresh, onCustomise }: { range: MetricsRange; setRange: (r: MetricsRange) => void; busy: boolean; onRefresh: () => void; onCustomise?: () => void }) {
  return (
    <div className="flex items-center gap-2">
      <div role="group" aria-label="Time range" className="inline-flex flex-1 rounded-lg border border-neutral-300 bg-white p-0.5 text-sm sm:flex-none">
        {RANGES.map((r) => (
          <button
            key={r.id}
            type="button"
            onClick={() => setRange(r.id)}
            aria-pressed={range === r.id}
            className={cx("flex-1 rounded-md px-3 py-1.5 whitespace-nowrap sm:flex-none", range === r.id ? "bg-primary text-white" : "text-neutral-600 hover:bg-neutral-50")}
          >
            <span className="sm:hidden">{r.short}</span>
            <span className="hidden sm:inline">{r.long}</span>
          </button>
        ))}
      </div>
      {onCustomise && (
        <button
          type="button"
          onClick={onCustomise}
          aria-label="Customise metrics"
          title="Customise metrics"
          className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-neutral-300 bg-white px-2.5 text-sm text-neutral-700 hover:bg-neutral-50 focus-visible:outline-2 focus-visible:outline-primary"
        >
          <SlidersHorizontal className="size-4" />
          <span className="hidden sm:inline">Customise</span>
        </button>
      )}
      <RefreshButton onClick={onRefresh} busy={busy} label="Refresh metrics" />
    </div>
  );
}

type Section = { id: string; label: string; icon: typeof Box };

function SectionTabs({ sections, value, onChange }: { sections: Section[]; value: string; onChange: (id: string) => void }) {
  return (
    <div role="tablist" className="-mx-4 flex gap-1 overflow-x-auto border-b border-neutral-200 px-4 [scrollbar-width:none] sm:mx-0 sm:px-0">
      {sections.map((s) => (
        <button
          key={s.id}
          type="button"
          role="tab"
          aria-selected={value === s.id}
          onClick={() => onChange(s.id)}
          className={cx(
            "inline-flex shrink-0 items-center gap-1.5 border-b-2 px-3 py-2 text-sm whitespace-nowrap",
            value === s.id ? "border-primary font-medium text-primary" : "border-transparent text-neutral-600 hover:text-neutral-900",
          )}
        >
          <s.icon className="size-4" />
          {s.label}
        </button>
      ))}
    </div>
  );
}

// --- customise dialog ---

function estimate(p: MetricPrefs, cloudflare: boolean): number {
  if (!p.metrics.length) return 0;
  if (cloudflare) return p.retention_days * 24 * 8 * 80;
  const rows = (p.retention_days * 86400) / p.interval;
  const proxy = ["proxy_requests", "proxy_latency", "proxy_bandwidth"].some((m) => p.metrics.includes(m)) ? rows * 120 : 0;
  return rows * (60 + 8 * p.metrics.length) + proxy;
}

function Customise({ title, kind, load, save, onClose, onSaved }: {
  title: string;
  kind: "container" | "worker" | "pages";
  load: () => Promise<MetricPrefsView>;
  save: (p: MetricPrefs) => Promise<MetricPrefsView>;
  onClose: () => void;
  onSaved: () => void;
}) {
  const canEdit = useCan("operator");
  const [prefs, setPrefs] = useState<MetricPrefs | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    load().then((v) => setPrefs(v.prefs), (e) => setError(e instanceof Error ? e.message : String(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const list = kind === "container" ? CONTAINER_METRICS : kind === "worker" ? WORKER_METRICS : PAGES_METRICS;
  const cloudflare = kind !== "container";
  const toggle = (id: string) =>
    setPrefs((p) => (p ? { ...p, metrics: p.metrics.includes(id) ? p.metrics.filter((m) => m !== id) : [...p.metrics, id] } : p));
  const submit = async () => {
    if (!prefs) return;
    setSaving(true);
    setError(null);
    try {
      await save(prefs);
      onSaved();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };
  return (
    <Modal title={`Metrics for ${title}`} onClose={onClose} wide>
      <p className="mb-4 text-sm text-neutral-600">
        Only ticked metrics are collected and stored for this worker. Switching one off stops new samples; history already stored stays until it ages out.
      </p>
      {error && <ErrorNote>{error}</ErrorNote>}
      {!prefs ? (
        <div className="space-y-2">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-10 w-full" />
          ))}
        </div>
      ) : (
        <>
          <ul className="divide-y divide-neutral-100 rounded-lg border border-neutral-200">
            {list.map((m) => {
              const off = m.available === false;
              return (
                <li key={m.id}>
                  <label className={cx("flex items-start gap-3 px-4 py-3", off ? "opacity-60" : "cursor-pointer")}>
                    <input type="checkbox" className="mt-0.5 size-4 shrink-0" checked={!off && prefs.metrics.includes(m.id)} disabled={off || !canEdit} onChange={() => toggle(m.id)} />
                    <span className="min-w-0 flex-1">
                      <span className="block text-sm font-medium">{m.label}</span>
                      <span className="block text-xs text-neutral-500">{off ? m.reason : `${m.source} · ${m.storage}`}</span>
                    </span>
                    {off && <Badge tone="neutral">Not available</Badge>}
                  </label>
                </li>
              );
            })}
          </ul>
          <div className="mt-4 flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
            {!cloudflare && (
              <label className="inline-flex items-center gap-2">
                <span className="text-neutral-600">Sample every</span>
                <select disabled={!canEdit} className="rounded-md border border-neutral-300 bg-white px-2 py-1" value={prefs.interval} onChange={(e) => setPrefs({ ...prefs, interval: Number(e.target.value) })}>
                  <option value={15}>15 seconds</option>
                  <option value={60}>1 minute</option>
                  <option value={300}>5 minutes</option>
                </select>
              </label>
            )}
            <label className="inline-flex items-center gap-2">
              <span className="text-neutral-600">Keep for</span>
              <select disabled={!canEdit} className="rounded-md border border-neutral-300 bg-white px-2 py-1" value={prefs.retention_days} onChange={(e) => setPrefs({ ...prefs, retention_days: Number(e.target.value) })}>
                <option value={7}>7 days</option>
                <option value={30}>30 days</option>
                <option value={90}>90 days</option>
              </select>
            </label>
            <span className="text-xs text-neutral-500 sm:ml-auto">About {bytes(estimate(prefs, cloudflare))} of database space</span>
          </div>
          <div className="mt-5 flex justify-end gap-2">
            <Button variant="secondary" onClick={onClose}>
              {canEdit ? "Cancel" : "Close"}
            </Button>
            {canEdit && (
              <Button variant="primary" onClick={submit} disabled={saving}>
                {saving ? "Saving…" : "Save"}
              </Button>
            )}
          </div>
        </>
      )}
    </Modal>
  );
}

// --- container (device worker) sections ---

function ContainerResources({ m, loading }: { m: WorkerMetrics | null; loading: boolean }) {
  const on = (id: string) => !m || m.prefs.metrics.includes(id);
  const s = m?.series ?? [];
  const cur = m?.current;
  // The newest reading says whether a limit is set now; older samples may predate a change.
  const memLimit = cur ? cur.mem_limit : Math.max(0, ...defined(s.map((x) => x.mem_limit)));
  const net = rateUnit(defined(s.flatMap((x) => [x.net_rx, x.net_tx])));
  const blk = rateUnit(defined(s.flatMap((x) => [x.blk_read, x.blk_write])));
  const cpuVals = defined(s.map((x) => x.cpu));
  const memVals = defined(s.map((x) => (x.mem != null ? x.mem / 1024 / 1024 : undefined)));
  const health = cur?.health;
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4 sm:gap-4">
        <Tile
          loading={loading}
          label="Health"
          value={!cur ? "—" : health ? health[0].toUpperCase() + health.slice(1) : "Running"}
          sub={!cur ? "No sample yet" : health ? "Health check" : "No health check defined"}
          tone={health === "unhealthy" ? "bad" : health === "starting" ? "warn" : undefined}
        />
        <Tile loading={loading} label="Uptime" value={cur?.started_at ? uptime(Math.max(0, Math.floor(Date.now() / 1000) - cur.started_at)) : "—"} sub={cur?.started_at ? `Since ${new Date(cur.started_at * 1000).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })}` : undefined} />
        <Tile
          loading={loading}
          label="Restarts"
          value={on("restarts") ? String(m?.restarts ?? 0) : "Off"}
          sub={cur ? `${cur.restarts} in total${cur.oom_killed ? " · last ran out of memory" : ""}` : "In this range"}
          tone={(m?.restarts ?? 0) > 0 ? "bad" : undefined}
        />
        <Tile
          loading={loading}
          label="Volume size"
          value={!on("volumes") ? "Off" : m?.volumes.length ? bytes(m.volumes.reduce((a, v) => a + v.size, 0)) : "—"}
          sub={!on("volumes") ? undefined : m?.volumes.length ? m.volumes.map((v) => v.name).join(", ") : "No named volumes measured yet"}
        />
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        {on("cpu") && (
          <Panel loading={loading} empty={!cpuVals.length} title="CPU" subtitle="Percent of one core; 200% is two full cores">
            <LineChart max={niceMax(cpuVals, 10)} format={fixed(0, "%")} series={[{ label: "CPU", color: "#0051c3", values: cpuVals }]} />
          </Panel>
        )}
        {on("memory") && (
          <Panel loading={loading} empty={!memVals.length} title="Memory" subtitle={memLimit ? `Working set against its ${bytes(memLimit)} limit` : "Working set; no memory limit set"}>
            <LineChart unit=" MB" format={fixed(1, " MB")} max={memLimit ? memLimit / 1024 / 1024 : niceMax(memVals)} series={[{ label: "Memory", color: "#f6821f", values: memVals }]} />
          </Panel>
        )}
        {on("network") && (
          <Panel loading={loading} empty={!s.some((x) => x.net_rx != null)} title="Network" subtitle={cur?.host_network ? "Not counted for containers on the host network" : "Traffic through the container's interfaces"}>
            <LineChart
              unit={net.unit}
              format={fixed(1, net.unit)}
              max={niceMax(defined(s.flatMap((x) => [x.net_rx, x.net_tx])).map((v) => v / net.div), 0.1)}
              series={[
                { label: "In", color: "#16794c", values: defined(s.map((x) => x.net_rx)).map((v) => v / net.div) },
                { label: "Out", color: "#8250df", values: defined(s.map((x) => x.net_tx)).map((v) => v / net.div) },
              ]}
            />
          </Panel>
        )}
        {on("disk_io") && (
          <Panel loading={loading} empty={!s.some((x) => x.blk_read != null)} title="Disk reads and writes">
            <LineChart
              unit={blk.unit}
              format={fixed(1, blk.unit)}
              max={niceMax(defined(s.flatMap((x) => [x.blk_read, x.blk_write])).map((v) => v / blk.div), 0.1)}
              series={[
                { label: "Read", color: "#0051c3", values: defined(s.map((x) => x.blk_read)).map((v) => v / blk.div) },
                { label: "Write", color: "#c2362b", values: defined(s.map((x) => x.blk_write)).map((v) => v / blk.div) },
              ]}
            />
          </Panel>
        )}
        {on("processes") && (
          <Panel loading={loading} empty={!s.some((x) => x.pids != null)} title="Processes">
            <LineChart unit="" format={fixed(0)} max={niceMax(defined(s.map((x) => x.pids)), 5)} series={[{ label: "Processes", color: "#0051c3", values: defined(s.map((x) => x.pids)) }]} />
          </Panel>
        )}
        {on("throttling") && (
          <Panel loading={loading} empty={!s.some((x) => x.throttled != null)} title="CPU throttling" subtitle="Share of time held back by the CPU limit">
            <LineChart format={fixed(1, "%")} max={niceMax(defined(s.map((x) => x.throttled)), 5)} series={[{ label: "Throttled", color: "#c2362b", values: defined(s.map((x) => x.throttled)) }]} />
          </Panel>
        )}
      </div>
    </div>
  );
}

function ProxyTraffic({ m, loading }: { m: WorkerMetrics | null; loading: boolean }) {
  const on = (id: string) => !m || m.prefs.metrics.includes(id);
  if (m && !["proxy_requests", "proxy_latency", "proxy_bandwidth"].some(on)) {
    return <Card className="p-5 text-sm text-neutral-600">Proxy metrics are switched off for this worker. Turn them on under Customise.</Card>;
  }
  if (m && !m.proxy) {
    return <Card className="p-5 text-sm text-neutral-600">{m.proxy_note ?? "This container is not served through the device's proxy, so there is no proxy traffic to count."}</Card>;
  }
  const p = m?.proxy;
  const s = p?.series ?? [];
  const t = p?.totals;
  const perMin = (v: number) => (m ? v / (m.step / 60) : v);
  const err = t && t.requests ? (t.c5xx / t.requests) * 100 : 0;
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4 sm:gap-4">
        <Tile loading={loading} label="Requests" value={t ? fmtNum(t.requests) : "—"} sub={p?.hosts.join(", ")} />
        <Tile loading={loading} label="Server errors" value={t?.requests ? `${err.toFixed(1)}%` : "—"} sub={t ? `${fmtNum(t.c5xx)} 5xx responses` : undefined} tone={err > 5 ? "bad" : undefined} />
        <Tile loading={loading} label="Response time p50" value={on("proxy_latency") ? fmtMs(t?.p50) : "Off"} sub={on("proxy_latency") ? `p95 ${fmtMs(t?.p95)}` : undefined} />
        <Tile loading={loading} label={on("proxy_bandwidth") ? "Bandwidth" : "Response time p99"} value={on("proxy_bandwidth") ? (t?.bytes != null ? bytes(t.bytes) : "—") : fmtMs(p?.p99)} sub={on("proxy_bandwidth") ? "Sent to visitors" : undefined} />
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        <Panel loading={loading} empty={!s.length} title="Requests per minute">
          <LineChart unit="" format={fixed(0)} max={niceMax(s.map((x) => perMin(x.requests)), 1)} series={[{ label: "Requests", color: "#0051c3", values: s.map((x) => perMin(x.requests)) }]} />
        </Panel>
        {on("proxy_latency") && (
          <Panel loading={loading} empty={!s.some((x) => x.p50 != null)} title="Response time" subtitle="From the proxy receiving a request to the last byte sent">
            <LineChart
              unit=" ms"
              format={fmtMs}
              max={niceMax(defined(s.map((x) => x.p95)), 10)}
              series={[
                { label: "p50", color: "#16794c", values: defined(s.map((x) => x.p50)) },
                { label: "p95", color: "#f6821f", values: defined(s.map((x) => x.p95)) },
              ]}
            />
          </Panel>
        )}
        <Card>
          <CardHeader title="Status codes" />
          <div className="p-5">
            <Split
              loading={loading}
              rows={[
                { label: "2xx success", value: t?.c2xx ?? 0, color: "#16794c" },
                { label: "3xx redirect", value: t?.c3xx ?? 0, color: "#0051c3" },
                { label: "4xx client error", value: t?.c4xx ?? 0, color: "#f6821f" },
                { label: "5xx server error", value: t?.c5xx ?? 0, color: "#c2362b" },
              ]}
            />
          </div>
        </Card>
      </div>
    </div>
  );
}

// --- Cloudflare Worker section ---

function WorkerRequests({ pm, loading }: { pm: ProjectMetrics | null; loading: boolean }) {
  const cf = pm?.cloudflare;
  if (cf && cf.analytics !== "ok" && !cf.series.length) {
    return (
      <Card className="p-5 text-sm text-neutral-600">
        Request analytics for this Worker aren't available{cf.analytics && cf.analytics !== "none" ? `: ${cf.analytics.replace(/^unavailable: /, "")}` : " yet"}. The API token needs <b>Account → Account Analytics → Read</b>.
      </Card>
    );
  }
  const s = cf?.series ?? [];
  const t = cf?.totals;
  const errRate = t?.requests ? (t.errors / t.requests) * 100 : 0;
  const statusRows = Object.entries(cf?.by_status ?? {}).map(([k, v]) => ({ label: OUTCOME[k] ?? k, value: v, color: k === "success" ? "#16794c" : k === "clientDisconnected" ? "#a3a3a3" : "#c2362b" }));
  const versions = Object.entries(cf?.by_version ?? {}).sort((a, b) => b[1] - a[1]);
  return (
    <div className="space-y-4">
      {cf?.note && <p className="rounded-lg border border-neutral-200 bg-white px-4 py-2.5 text-sm text-neutral-600">{cf.note}</p>}
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4 sm:gap-4">
        <Tile loading={loading} label="Requests" value={t ? fmtNum(t.requests) : "—"} sub={t ? `${fmtNum(t.subrequests)} subrequests` : undefined} />
        <Tile loading={loading} label="Errors" value={t?.requests ? `${errRate.toFixed(1)}%` : "—"} sub={t ? `${fmtNum(t.errors)} failed invocations` : undefined} tone={errRate > 5 ? "bad" : undefined} />
        <Tile loading={loading} label="CPU time p50" value={fmtMs(t?.cpu_p50)} sub={t?.cpu_p99 != null ? `p99 ${fmtMs(t.cpu_p99)}` : undefined} />
        <Tile loading={loading} label="Wall time p50" value={fmtMs(t?.wall_p50)} sub={t?.wall_p99 != null ? `p99 ${fmtMs(t.wall_p99)}` : "Real time per request, including waits"} />
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        <Panel loading={loading} empty={!s.length} title="Requests" subtitle="Per hour">
          <LineChart
            unit=""
            format={fixed(0)}
            max={niceMax(s.map((x) => x.requests), 1)}
            series={[
              { label: "Requests", color: "#f6821f", values: s.map((x) => x.requests) },
              { label: "Errors", color: "#c2362b", values: s.map((x) => x.errors) },
            ]}
          />
        </Panel>
        <Panel loading={loading} empty={!s.some((x) => x.cpu_p50 != null)} title="CPU time" subtitle="Time spent computing per request">
          <LineChart
            unit=" ms"
            format={fmtMs}
            max={niceMax(defined(s.map((x) => x.cpu_p99)), 1)}
            series={[
              { label: "p50", color: "#16794c", values: defined(s.map((x) => x.cpu_p50)) },
              { label: "p99", color: "#c2362b", values: defined(s.map((x) => x.cpu_p99)) },
            ]}
          />
        </Panel>
        <Panel loading={loading} empty={!s.some((x) => x.wall_p50 != null)} title="Wall time" subtitle="Real time per request, including waits on subrequests">
          <LineChart
            unit=" ms"
            format={fmtMs}
            max={niceMax(defined(s.map((x) => x.wall_p99)), 1)}
            series={[
              { label: "p50", color: "#16794c", values: defined(s.map((x) => x.wall_p50)) },
              { label: "p99", color: "#c2362b", values: defined(s.map((x) => x.wall_p99)) },
            ]}
          />
        </Panel>
        <Card>
          <CardHeader title="By outcome" />
          <div className="p-5">
            <Split loading={loading} rows={statusRows} />
          </div>
        </Card>
        {versions.length > 0 && (
          <Card>
            <CardHeader title="By version" subtitle="Useful while a gradual deployment is running" />
            <div className="p-5">
              <Split loading={loading} rows={versions.map(([k, v], i) => ({ label: k.length > 12 ? k.slice(0, 8) : k, value: v, color: i === 0 ? "#16794c" : "#a3a3a3" }))} />
            </div>
          </Card>
        )}
      </div>
    </div>
  );
}

function Deployments({ pm, loading, isCF }: { pm: ProjectMetrics | null; loading: boolean; isCF: boolean }) {
  const d = pm?.deployments;
  const done = d ? d.succeeded + d.failed : 0;
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4 sm:gap-4">
        <Tile loading={loading} label="Deployments" value={d ? String(d.total) : "—"} sub={d?.previews ? `${d.previews} for previews` : "Production and previews"} />
        <Tile loading={loading} label="Success rate" value={done ? `${Math.round(((d?.succeeded ?? 0) / done) * 100)}%` : "—"} sub={d ? `${d.failed} failed` : undefined} tone={d && done && d.failed / done > 0.2 ? "bad" : undefined} />
        <Tile loading={loading} label={isCF ? "Median build and deploy" : "Median deploy time"} value={fmtSecs(d?.median_seconds)} sub={d?.p95_seconds != null ? `p95 ${fmtSecs(d.p95_seconds)}` : "Queued to finished"} />
        <Tile loading={loading} label="Rollbacks" value={d ? String(d.rollbacks) : "—"} sub="Started from the dashboard" />
      </div>
      <Panel loading={loading} empty={!d?.total} title="Deployment frequency" subtitle={pm?.range === "1h" ? "Per 5 minutes" : pm?.range === "24h" ? "Per hour" : pm?.range === "7d" ? "Per 12 hours" : "Per day"}>
        {pm && d && <Bars points={fillBuckets(d.series, pm.range, pm.step)} />}
      </Panel>
    </div>
  );
}

// --- entry points ---

function NotSupported({ deviceId }: { deviceId: string }) {
  return (
    <Card className="p-5 text-sm text-neutral-600">
      The agent on <Link to={`/devices/${deviceId}`} className="text-primary hover:underline">this device</Link> is too old to send per-container metrics. Update it from the{" "}
      <Link to="/updates" className="text-primary hover:underline">Updates</Link> page; graphs fill in from then on.
    </Card>
  );
}

/** Metrics for one container on a device (the Worker page, and device projects). */
function ContainerMetricsBody({ deviceId, name, range, tick, pm, pmLoading, sections }: { deviceId: string; name: string; range: MetricsRange; tick: number; pm: ProjectMetrics | null; pmLoading: boolean; sections: Section[] }) {
  const [section, setSection] = useState(sections[0].id);
  const wm = usePoll(() => api.workerMetrics(deviceId, name, range), 30000, [deviceId, name, range, tick]);
  const loading = !wm.data && !wm.error;
  return (
    <div className="space-y-4">
      <SectionTabs sections={sections} value={section} onChange={setSection} />
      {wm.error && <ErrorNote>{wm.error}</ErrorNote>}
      {wm.data && !wm.data.supported && section !== "deployments" && <NotSupported deviceId={deviceId} />}
      {wm.data?.supported && !wm.data.series.length && !wm.data.current && section === "resources" && (
        <p className="rounded-lg border border-neutral-200 bg-white px-4 py-2.5 text-sm text-neutral-600">Collecting. The first samples arrive within a minute of the agent sending them.</p>
      )}
      {section === "resources" && <ContainerResources m={wm.data} loading={loading} />}
      {section === "proxy" && <ProxyTraffic m={wm.data} loading={loading} />}
      {section === "deployments" && <Deployments pm={pm} loading={pmLoading} isCF={false} />}
    </div>
  );
}

const DEVICE_SECTIONS: Section[] = [
  { id: "resources", label: "Resources", icon: Box },
  { id: "proxy", label: "Proxy traffic", icon: Globe },
  { id: "deployments", label: "Deployments", icon: Rocket },
];

/** The Metrics tab of a project. */
export function ProjectMetricsTab({ p }: { p: Project }) {
  const isCF = p.source.type === "cloudflare";
  const isWorker = isCF && p.source.cloudflare?.kind === "worker";
  const canOperate = useCan("operator");
  const [range, setRange] = useState<MetricsRange>("24h");
  const [tick, setTick] = useState(0);
  const [busy, setBusy] = useState(false);
  const [customise, setCustomise] = useState(false);
  const [picked, setPicked] = useState<string | null>(null);
  const [section, setSection] = useState(isCF ? "status" : "resources");
  const pm = usePoll(() => api.projectMetrics(p.id, range), 60000, [p.id, range, tick]);
  const pmLoading = (!pm.data && !pm.error) || busy;

  const refresh = async () => {
    setBusy(true);
    try {
      if (isCF && canOperate) await api.refreshCloudflare(p.id).catch(() => {});
      setTick((t) => t + 1);
      await pm.reload();
    } finally {
      setBusy(false);
    }
  };
  const workers = pm.data?.workers ?? [];
  const current = workers.find((w) => w.name === picked) ?? workers[0];
  const cfSections: Section[] = [
    { id: "status", label: "Status", icon: Activity },
    ...(isWorker ? [{ id: "requests", label: "Requests", icon: Cloud }] : []),
    { id: "deployments", label: "Deployments", icon: Rocket },
  ];

  return (
    <div className="space-y-4">
      <Toolbar range={range} setRange={setRange} busy={busy} onRefresh={refresh} onCustomise={isCF && !isWorker ? undefined : () => setCustomise(true)} />
      {pm.error && <ErrorNote>{pm.error}</ErrorNote>}
      {isCF ? (
        <>
          <SectionTabs sections={cfSections} value={section} onChange={setSection} />
          {section === "status" && <CloudflareMetrics p={p} />}
          {section === "requests" && <WorkerRequests pm={pm.data} loading={pmLoading} />}
          {section === "deployments" && <Deployments pm={pm.data} loading={pmLoading} isCF />}
        </>
      ) : (
        <>
          {workers.length > 1 && (
            <div className="flex flex-wrap gap-2" role="group" aria-label="Container">
              {workers.map((w) => (
                <button
                  key={w.device_id + w.name}
                  type="button"
                  onClick={() => setPicked(w.name)}
                  aria-pressed={current?.name === w.name}
                  className={cx("rounded-lg border px-3 py-1.5 text-sm", current?.name === w.name ? "border-primary bg-primary/5 font-medium" : "border-neutral-300 bg-white hover:bg-neutral-50")}
                >
                  {w.name}
                </button>
              ))}
            </div>
          )}
          {current ? (
            <ContainerMetricsBody key={current.device_id + current.name} deviceId={current.device_id} name={current.name} range={range} tick={tick} pm={pm.data} pmLoading={pmLoading} sections={DEVICE_SECTIONS} />
          ) : pmLoading ? (
            <Skeleton className="h-40 w-full" />
          ) : (
            <>
              <Card className="p-5 text-sm text-neutral-600">No running container belongs to this project yet, so there is nothing to measure. Deployment history is below.</Card>
              <Deployments pm={pm.data} loading={pmLoading} isCF={false} />
            </>
          )}
        </>
      )}
      {customise && (
        <Customise
          title={p.name}
          kind={isWorker ? "worker" : "container"}
          load={() => api.projectMetricPrefs(p.id)}
          save={(prefs) => api.setProjectMetricPrefs(p.id, prefs)}
          onClose={() => setCustomise(false)}
          onSaved={() => setTick((t) => t + 1)}
        />
      )}
    </div>
  );
}

/** The Metrics tab of a worker reached from the Workers list (any container on a device). */
export function WorkerMetricsTab({ deviceId, name, projectId }: { deviceId: string; name: string; projectId?: string }) {
  const [range, setRange] = useState<MetricsRange>("24h");
  const [tick, setTick] = useState(0);
  const [busy, setBusy] = useState(false);
  const [customise, setCustomise] = useState(false);
  const pm = usePoll(() => (projectId ? api.projectMetrics(projectId, range) : Promise.resolve(null)), 60000, [projectId, range, tick]);
  const refresh = async () => {
    setBusy(true);
    setTick((t) => t + 1);
    await pm.reload();
    setTimeout(() => setBusy(false), 400);
  };
  const sections = projectId ? DEVICE_SECTIONS : DEVICE_SECTIONS.filter((s) => s.id !== "deployments");
  return (
    <div className="space-y-4">
      <Toolbar range={range} setRange={setRange} busy={busy} onRefresh={refresh} onCustomise={() => setCustomise(true)} />
      <ContainerMetricsBody deviceId={deviceId} name={name} range={range} tick={tick} pm={pm.data} pmLoading={(!pm.data && !pm.error) || busy} sections={sections} />
      {customise && (
        <Customise
          title={name}
          kind="container"
          load={() => api.workerMetricPrefs(deviceId, name)}
          save={(prefs) => api.setWorkerMetricPrefs(deviceId, name, prefs)}
          onClose={() => setCustomise(false)}
          onSaved={() => setTick((t) => t + 1)}
        />
      )}
    </div>
  );
}
