import { useMemo, useState } from "react";
import { Box, Cloud, Rocket, Lightbulb, Globe } from "lucide-react";
import { Badge, Card, CardHeader, LineChart, PageHeader, RefreshButton, Skeleton, Table, Td } from "../components/ui";
import { useCrumbs } from "../components/Layout";
import { cx } from "../lib";

/**
 * DEMO ONLY. Every number on this page is generated locally from a fixed seed; nothing is read from
 * agents, Caddy or Cloudflare. It exists to agree what the per-worker Metrics tab should show.
 */

type Range = "24h" | "7d" | "30d";
const RANGES: Range[] = ["24h", "7d", "30d"];
const POINTS: Record<Range, number> = { "24h": 48, "7d": 56, "30d": 60 };
const SCALE: Record<Range, number> = { "24h": 1, "7d": 7, "30d": 30 };

function rng(seed: number) {
  let s = seed >>> 0;
  return () => ((s = (Math.imul(s, 1664525) + 1013904223) >>> 0) / 2 ** 32);
}
function series(seed: number, n: number, base: number, amp: number, noise = 0.15, min = 0, max = Infinity) {
  const r = rng(seed);
  return Array.from({ length: n }, (_, i) => Math.min(max, Math.max(min, base + Math.sin((i / n) * Math.PI * 4 + seed) * amp + (r() - 0.5) * 2 * noise * base)));
}
const fmt = (n: number) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${(n / 1e3).toFixed(1)}k` : String(Math.round(n)));

function Tile({ label, value, sub, loading, bad }: { label: string; value: string; sub?: string; loading: boolean; bad?: boolean }) {
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
          <p className={cx("mt-1 text-2xl font-semibold tabular-nums", bad && "text-bad")}>{value}</p>
          {sub && <p className="mt-0.5 text-xs text-neutral-500">{sub}</p>}
        </>
      )}
    </Card>
  );
}

function Panel({ title, subtitle, loading, children }: { title: string; subtitle?: string; loading: boolean; children: React.ReactNode }) {
  return (
    <Card>
      <CardHeader title={title} subtitle={subtitle} />
      <div className="p-5">{loading ? <Skeleton className="h-28 w-full" /> : children}</div>
    </Card>
  );
}

/** Horizontal share bars: status codes, countries, versions. */
function Split({ rows, loading }: { rows: { label: string; value: number; color: string }[]; loading: boolean }) {
  const total = rows.reduce((a, r) => a + r.value, 0) || 1;
  if (loading) return <Skeleton className="h-28 w-full" />;
  return (
    <div className="space-y-2.5">
      {rows.map((r) => (
        <div key={r.label} className="text-xs">
          <div className="mb-1 flex justify-between">
            <span className="text-neutral-700">{r.label}</span>
            <span className="tabular-nums text-neutral-500">
              {fmt(r.value)} · {((r.value / total) * 100).toFixed(1)}%
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

/** Vertical bars, used for per-bucket counts such as deployments per day. */
function Bars({ values, color = "#0051c3", failed }: { values: number[]; color?: string; failed?: number[] }) {
  const max = Math.max(...values, 1);
  return (
    <div className="flex h-28 items-end gap-1">
      {values.map((v, i) => (
        <div key={i} className="flex flex-1 flex-col justify-end" style={{ height: "100%" }}>
          <div style={{ height: `${((failed?.[i] ?? 0) / max) * 100}%`, background: "#c2362b" }} className="rounded-t-sm" />
          <div style={{ height: `${((v - (failed?.[i] ?? 0)) / max) * 100}%`, background: color }} className={failed?.[i] ? "" : "rounded-t-sm"} />
        </div>
      ))}
    </div>
  );
}

const SECTIONS = [
  { id: "container", label: "Device worker", icon: Box },
  { id: "proxy", label: "Proxy traffic", icon: Globe },
  { id: "cloudflare", label: "Cloudflare", icon: Cloud },
  { id: "deploys", label: "Deployments", icon: Rocket },
  { id: "more", label: "More ideas", icon: Lightbulb },
] as const;
type Section = (typeof SECTIONS)[number]["id"];

export default function MetricsDemo() {
  useCrumbs([{ label: "Metrics demo" }]);
  const [range, setRange] = useState<Range>("24h");
  const [section, setSection] = useState<Section>("container");
  const [busy, setBusy] = useState(false);
  const [tick, setTick] = useState(0);
  const n = POINTS[range];
  const k = SCALE[range];
  const seed = tick * 7 + 1;

  const refresh = () => {
    setBusy(true);
    setTimeout(() => {
      setTick((t) => t + 1);
      setBusy(false);
    }, 900);
  };
  const pick = (r: Range) => {
    setRange(r);
    refresh();
  };

  const d = useMemo(
    () => ({
      cpu: series(seed + 1, n, 22, 12, 0.2, 1, 100),
      mem: series(seed + 2, n, 58, 6, 0.04, 1, 100),
      netIn: series(seed + 3, n, 1.4, 0.8, 0.25, 0.05),
      netOut: series(seed + 4, n, 3.1, 1.6, 0.25, 0.05),
      reqs: series(seed + 5, n, 90, 50, 0.2, 5),
      p50: series(seed + 6, n, 14, 4, 0.2, 2),
      p95: series(seed + 7, n, 62, 25, 0.3, 5),
      cfReq: series(seed + 8, n, 4200, 2200, 0.2, 100),
      cfCpuP50: series(seed + 9, n, 1.8, 0.6, 0.15, 0.2),
      cfCpuP99: series(seed + 10, n, 9.5, 4, 0.25, 1),
      cfWallP50: series(seed + 11, n, 12, 4, 0.2, 1),
      cfWallP99: series(seed + 12, n, 84, 30, 0.25, 5),
      deploys: series(seed + 13, Math.min(n, 30), 3, 2, 0.4, 0).map(Math.round),
    }),
    [seed, n],
  );
  const failed = d.deploys.map((v, i) => (i % 6 === 2 ? Math.min(v, 1) : 0));
  const reqTotal = Math.round(d.reqs.reduce((a, b) => a + b, 0) * 60 * (24 / n) * k);
  const cfTotal = Math.round(d.cfReq.reduce((a, b) => a + b, 0) * (24 / n) * k * 3);

  return (
    <div>
      <PageHeader
        title="Worker metrics"
        description="Proposed layout for the Metrics tab. Per-container and per-Cloudflare-worker measurements."
        actions={
          <>
            <div role="group" aria-label="Time range" className="inline-flex rounded-lg border border-neutral-300 bg-white p-0.5 text-sm">
              {RANGES.map((r) => (
                <button key={r} type="button" onClick={() => pick(r)} aria-pressed={range === r} className={cx("rounded-md px-3 py-1.5", range === r ? "bg-primary text-white" : "text-neutral-600 hover:bg-neutral-50")}>
                  {r === "24h" ? "Last 24 hours" : r === "7d" ? "Last 7 days" : "Last 30 days"}
                </button>
              ))}
            </div>
            <RefreshButton onClick={refresh} busy={busy} label="Refresh metrics" />
          </>
        }
      />
      <div role="note" className="mb-4 rounded-lg border border-warn/40 bg-warn/10 px-4 py-2.5 text-sm">
        <b>Demo data.</b> Nothing on this page is measured. Figures are generated from a fixed seed so the layout can be reviewed.
      </div>
      <div className="mb-4 flex flex-wrap gap-1 border-b border-neutral-200">
        {SECTIONS.map((s) => (
          <button key={s.id} type="button" onClick={() => setSection(s.id)} aria-pressed={section === s.id} className={cx("inline-flex items-center gap-1.5 border-b-2 px-3 py-2 text-sm", section === s.id ? "border-primary font-medium text-primary" : "border-transparent text-neutral-600 hover:text-neutral-900")}>
            <s.icon className="size-4" />
            {s.label}
          </button>
        ))}
      </div>

      <div className="space-y-4" aria-busy={busy}>
        {section === "container" && (
          <>
            <div className="grid gap-4 sm:grid-cols-4">
              <Tile loading={busy} label="Status" value="Healthy" sub="Health check passing" />
              <Tile loading={busy} label="Uptime" value={`${Math.round(11 + tick)}d 4h`} sub="Since 6 Nov, 09:12" />
              <Tile loading={busy} label="Restarts" value="2" sub="Last: OOM killed, 3 days ago" bad />
              <Tile loading={busy} label="Volume size" value="1.8 GB" sub="/data · 38% of 4.7 GB" />
            </div>
            <div className="grid gap-4 lg:grid-cols-2">
              <Panel loading={busy} title="CPU" subtitle="% of one core, from docker stats">
                <LineChart series={[{ label: "CPU", color: "#0051c3", values: d.cpu }]} />
              </Panel>
              <Panel loading={busy} title="Memory" subtitle="Working set against the 512 MB limit">
                <LineChart series={[{ label: "Memory", color: "#f6821f", values: d.mem }]} />
              </Panel>
              <Panel loading={busy} title="Network" subtitle="MB/s through the container's interface">
                <LineChart unit=" MB/s" max={6} series={[{ label: "In", color: "#16794c", values: d.netIn }, { label: "Out", color: "#8250df", values: d.netOut }]} />
              </Panel>
              <Panel loading={busy} title="Disk I/O and processes" subtitle="Block device reads and writes; PIDs">
                <LineChart unit=" MB/s" max={6} series={[{ label: "Read", color: "#0051c3", values: d.netIn.map((v) => v * 0.4) }, { label: "Write", color: "#c2362b", values: d.netOut.map((v) => v * 0.3) }]} />
                <p className="mt-2 text-xs text-neutral-500">Processes: 14 · open file handles: 212</p>
              </Panel>
            </div>
          </>
        )}

        {section === "proxy" && (
          <>
            <div className="grid gap-4 sm:grid-cols-4">
              <Tile loading={busy} label="Requests" value={fmt(reqTotal)} sub={`Through Caddy, ${range}`} />
              <Tile loading={busy} label="Error rate" value="0.8%" sub="5xx responses" />
              <Tile loading={busy} label="Latency p50" value="14 ms" sub="Time to last byte" />
              <Tile loading={busy} label="Latency p95" value="62 ms" sub="p99 188 ms" />
            </div>
            <div className="grid gap-4 lg:grid-cols-2">
              <Panel loading={busy} title="Requests per minute">
                <LineChart unit=" rpm" max={200} series={[{ label: "Requests", color: "#0051c3", values: d.reqs }]} />
              </Panel>
              <Panel loading={busy} title="Latency">
                <LineChart unit=" ms" max={140} series={[{ label: "p50", color: "#16794c", values: d.p50 }, { label: "p95", color: "#f6821f", values: d.p95 }]} />
              </Panel>
              <Card>
                <CardHeader title="Status codes" />
                <div className="p-5">
                  <Split loading={busy} rows={[{ label: "2xx success", value: reqTotal * 0.931, color: "#16794c" }, { label: "3xx redirect", value: reqTotal * 0.041, color: "#0051c3" }, { label: "4xx client error", value: reqTotal * 0.02, color: "#f6821f" }, { label: "5xx server error", value: reqTotal * 0.008, color: "#c2362b" }]} />
                </div>
              </Card>
              <Card>
                <CardHeader title="Top paths" subtitle="Needs access logs enabled on the proxy" />
                {busy ? <div className="p-5"><Skeleton className="h-28 w-full" /></div> : (
                  <Table head={["Path", "Requests", "p95"]} compact>
                    {[["/api/items", 0.42, 48], ["/", 0.27, 21], ["/static/app.js", 0.14, 9], ["/api/login", 0.06, 120]].map(([p, f, l]) => (
                      <tr key={p as string}><Td>{p}</Td><Td className="tabular-nums">{fmt(reqTotal * (f as number))}</Td><Td className="tabular-nums">{l} ms</Td></tr>
                    ))}
                  </Table>
                )}
              </Card>
            </div>
          </>
        )}

        {section === "cloudflare" && (
          <>
            <div className="grid gap-4 sm:grid-cols-4">
              <Tile loading={busy} label={`Requests, ${range}`} value={fmt(cfTotal)} sub={`${fmt(Math.round(cfTotal * 1.7))} subrequests`} />
              <Tile loading={busy} label="Errors" value="0.4%" sub="Exceptions and 5xx" />
              <Tile loading={busy} label="CPU time p50 / p99" value="1.8 / 9.5 ms" />
              <Tile loading={busy} label="Wall time p50 / p99" value="12 / 84 ms" />
            </div>
            <div className="grid gap-4 lg:grid-cols-2">
              <Panel loading={busy} title="Requests">
                <LineChart unit="" max={8000} series={[{ label: "Requests", color: "#f6821f", values: d.cfReq }]} />
              </Panel>
              <Panel loading={busy} title="CPU time" subtitle="Milliseconds per invocation">
                <LineChart unit=" ms" max={20} series={[{ label: "p50", color: "#16794c", values: d.cfCpuP50 }, { label: "p99", color: "#c2362b", values: d.cfCpuP99 }]} />
              </Panel>
              <Panel loading={busy} title="Wall time" subtitle="Includes time waiting on subrequests">
                <LineChart unit=" ms" max={160} series={[{ label: "p50", color: "#16794c", values: d.cfWallP50 }, { label: "p99", color: "#c2362b", values: d.cfWallP99 }]} />
              </Panel>
              <Card>
                <CardHeader title="By status code" />
                <div className="p-5"><Split loading={busy} rows={[{ label: "2xx", value: cfTotal * 0.944, color: "#16794c" }, { label: "3xx", value: cfTotal * 0.03, color: "#0051c3" }, { label: "4xx", value: cfTotal * 0.022, color: "#f6821f" }, { label: "5xx / exception", value: cfTotal * 0.004, color: "#c2362b" }]} /></div>
              </Card>
              <Card>
                <CardHeader title="By country" />
                <div className="p-5"><Split loading={busy} rows={[{ label: "United Kingdom", value: cfTotal * 0.46, color: "#0051c3" }, { label: "United States", value: cfTotal * 0.24, color: "#0051c3" }, { label: "Germany", value: cfTotal * 0.11, color: "#0051c3" }, { label: "France", value: cfTotal * 0.07, color: "#0051c3" }, { label: "Other", value: cfTotal * 0.12, color: "#a3a3a3" }]} /></div>
              </Card>
              <Card>
                <CardHeader title="By version" subtitle="Useful while a gradual rollout is running" />
                <div className="p-5"><Split loading={busy} rows={[{ label: "a41f9c2 · current", value: cfTotal * 0.9, color: "#16794c" }, { label: "7be03d1 · previous", value: cfTotal * 0.1, color: "#a3a3a3" }]} /></div>
              </Card>
            </div>
            <Card>
              <CardHeader title="Pages build duration and success" subtitle="Pages projects only" />
              <div className="grid gap-4 p-5 sm:grid-cols-3">
                <Tile loading={busy} label="Build time p50" value="48 s" sub="p95 1 m 31 s" />
                <Tile loading={busy} label="Build success" value="93%" sub="14 of 15 builds" />
                <Tile loading={busy} label="Requests" value="Not in API" sub="Use Web Analytics for Pages traffic" />
              </div>
            </Card>
          </>
        )}

        {section === "deploys" && (
          <>
            <div className="grid gap-4 sm:grid-cols-4">
              <Tile loading={busy} label="Deployments" value={String(d.deploys.reduce((a, b) => a + b, 0))} sub={range} />
              <Tile loading={busy} label="Success rate" value="94%" />
              <Tile loading={busy} label="Median deploy time" value="1 m 12 s" sub="Queued to Verified" />
              <Tile loading={busy} label="Rollbacks" value="1" sub="Auto-rollback after failed verify" bad />
            </div>
            <Panel loading={busy} title="Deployment frequency" subtitle={`Per ${range === "24h" ? "hour" : "day"}; red is failed`}>
              <Bars values={d.deploys} failed={failed} />
            </Panel>
          </>
        )}

        {section === "more" && (
          <Card>
            <CardHeader title="Other metrics worth considering" subtitle="Feasibility and the storage each would need" />
            <Table head={["Metric", "Source", "Feasible", "Storage (SQLite)"]}>
              {IDEAS.map((r) => (
                <tr key={r[0]}>
                  <Td className="min-w-48"><span className="font-medium">{r[0]}</span><p className="text-xs text-neutral-500">{r[1]}</p></Td>
                  <Td className="whitespace-nowrap text-xs">{r[2]}</Td>
                  <Td><Badge tone={r[3] === "Yes" ? "ok" : r[3] === "Partly" ? "warn" : "bad"}>{r[3]}</Badge></Td>
                  <Td className="min-w-48 text-xs text-neutral-600">{r[4]}</Td>
                </tr>
              ))}
            </Table>
          </Card>
        )}
      </div>
    </div>
  );
}

const IDEAS: string[][] = [
  ["CPU throttling", "Time the container hit its CPU limit.", "Docker stats API", "Yes", "Counter, 1 sample/min."],
  ["OOM kill count", "Memory-limit kills; explains restarts.", "Docker events / inspect", "Yes", "Event row per kill, keep 90 days."],
  ["Health check history", "Pass/fail timeline with last output.", "Docker inspect", "Yes", "One row per state change."],
  ["Log volume and error lines", "Lines/min, count matching error or panic.", "Docker logs API", "Yes", "Per-minute counts only, not the text."],
  ["Time to first byte per route", "Latency split by path or domain.", "Caddy access logs", "Partly", "Needs JSON logs on; aggregate per minute and route."],
  ["Bandwidth by domain", "Bytes served per hostname.", "Caddy metrics (Prometheus)", "Yes", "Counter per host, 1 sample/min."],
  ["Unique visitors", "Distinct client IPs (hashed).", "Caddy logs", "Partly", "HyperLogLog per day; privacy decision needed."],
  ["Cache hit ratio", "Cloudflare edge cache hits.", "Cloudflare GraphQL (zone)", "Yes", "Hourly rollup, zone token needed."],
  ["Worker CPU limit exceeded", "Invocations ended for CPU or memory limits.", "GraphQL invocations (status)", "Yes", "Counts per hour."],
  ["Durable Object / KV / D1 usage", "Reads, writes, storage per binding.", "GraphQL per product", "Partly", "Separate datasets; hourly rollup."],
  ["Cost estimate", "Billable requests and CPU against the plan.", "GraphQL + plan limits", "Partly", "Derived at read time; no extra storage."],
  ["Pages request analytics", "Pages traffic.", "Not in the GraphQL API", "No", "Link out to Web Analytics."],
  ["GPU, temperature, power", "Device-level, not per container.", "Agent sysfs/vcgencmd", "Partly", "Per device, not per worker."],
];
