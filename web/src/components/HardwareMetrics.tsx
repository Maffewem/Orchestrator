import { useState } from "react";
import type { Metrics, MetricSample } from "../api";
import { Badge, Card, CardHeader, LineChart, Table } from "./ui";
import { bytes } from "../lib";

// Disk, network, load and power graphs for the device Overview. Every card hides itself when the
// agent has not reported the figure (an older agent, or hardware without it).

/** Rounds up to 1, 2, 5 times a power of ten so the chart scale reads cleanly. */
function niceMax(v: number, floor: number): number {
  const x = Math.max(v, floor);
  const p = Math.pow(10, Math.floor(Math.log10(x)));
  for (const m of [1, 2, 5, 10]) if (x <= m * p) return m * p;
  return 10 * p;
}

const rate = (v: number) => `${bytes(v)}/s`;
const COLORS = { read: "#0051c3", write: "#f6821f", busy: "#7c3aed", rx: "#16794c", tx: "#0051c3", err: "#d92d20" };

function Stat({ label, value, tone }: { label: string; value: string; tone?: "bad" | "warn" }) {
  return (
    <div className="min-w-0">
      <dt className="truncate text-xs text-neutral-500">{label}</dt>
      <dd className={`text-sm font-medium tabular-nums ${tone === "bad" ? "text-bad" : tone === "warn" ? "text-amber-700" : ""}`}>{value}</dd>
    </div>
  );
}

function names(s: MetricSample[], pick: (x: MetricSample) => { name: string }[] | undefined): string[] {
  const out: string[] = [];
  for (const x of s) for (const d of pick(x) ?? []) if (!out.includes(d.name)) out.push(d.name);
  return out;
}

export type GraphKey = "disk" | "net" | "power" | "compare";

/** The extra graph groups this device has data for, in display order. */
export function availableGraphs(s: MetricSample[], m: Metrics): { key: GraphKey; label: string }[] {
  const out: { key: GraphKey; label: string }[] = [];
  if (s.some((x) => x.disks?.length)) out.push({ key: "disk", label: "Disk I/O" });
  if (s.some((x) => x.nets?.length)) out.push({ key: "net", label: "Network" });
  if (s.some((x) => x.has_power || (x.clock ?? 0) > 0) || m.power != null) out.push({ key: "power", label: "Power & clock" });
  return out;
}

/** Load per core, I/O wait and swap as percentages, to sit on the main performance chart. */
export function pressureSeries(s: MetricSample[], m: Metrics, cores: number) {
  const out: { label: string; color: string; values: number[] }[] = [];
  if (cores > 0 && s.some((x) => x.load1 > 0)) out.push({ label: "Load per core", color: "#7c3aed", values: s.map((x) => Math.min(100, (x.load1 / cores) * 100)) });
  if (s.some((x) => (x.iowait ?? 0) > 0)) out.push({ label: "I/O wait", color: "#d92d20", values: s.map((x) => x.iowait ?? 0) });
  if ((m.swap_total ?? 0) > 0) out.push({ label: "Swap", color: "#a3a3a3", values: s.map((x) => x.swap ?? 0) });
  return out;
}

/** Figures that do not graph well: load averages, swap and kernel out-of-memory kills. */
export function PressureStats({ m }: { m: Metrics }) {
  const swapTotal = m.swap_total ?? 0;
  const oom = m.oom_kills ?? 0;
  return (
    <dl className="grid grid-cols-2 gap-x-6 gap-y-2 sm:grid-cols-4">
      <Stat label="Load (1 / 5 / 15 min)" value={`${m.load1.toFixed(2)} ${m.load5.toFixed(2)} ${m.load15.toFixed(2)}`} />
      <Stat label="I/O wait" value={`${(m.iowait_percent ?? 0).toFixed(0)}%`} tone={(m.iowait_percent ?? 0) >= 40 ? "warn" : undefined} />
      <Stat label="Swap" value={swapTotal > 0 ? `${bytes(m.swap_used ?? 0)} of ${bytes(swapTotal)}` : "None"} tone={swapTotal > 0 && (m.swap_used ?? 0) / swapTotal >= 0.8 ? "warn" : undefined} />
      <Stat label="Out-of-memory kills" value={String(oom)} tone={oom > 0 ? "warn" : undefined} />
    </dl>
  );
}

const sum = (xs: number[]) => xs.reduce((a, b) => a + b, 0);

export default function HardwareMetrics({ s, m, only }: { s: MetricSample[]; m: Metrics; only: GraphKey }) {
  const disks = names(s, (x) => x.disks);
  const nets = names(s, (x) => x.nets);
  const p = m.power;
  const hasClock = s.some((x) => (x.clock ?? 0) > 0);

  if (only === "disk" && disks.length > 0) {
    const read = s.map((x) => sum((x.disks ?? []).map((d) => d.read_bps)));
    const write = s.map((x) => sum((x.disks ?? []).map((d) => d.write_bps)));
    const busy = s.map((x) => Math.max(0, ...(x.disks ?? []).map((d) => d.busy)));
    return (
      <Card>
        <CardHeader title="Disk I/O" subtitle="Read and write speed of all disks together (last hour); the busiest disk's busy % is the line underneath." />
        <div className="space-y-4 p-5">
          <LineChart max={niceMax(Math.max(...read, ...write), 1024 * 1024)} unit="B/s" format={rate} series={[{ label: "Read", color: COLORS.read, values: read }, { label: "Write", color: COLORS.write, values: write }]} />
          <LineChart height={64} series={[{ label: "Busiest disk", color: COLORS.busy, values: busy }]} />
          <Table head={["Disk", "Read", "Write", "Read IOPS", "Write IOPS", "Busy"]} compact>
            {disks.map((n) => {
              const c = m.disks?.find((d) => d.name === n);
              return (
                <tr key={n}>
                  <td className="font-mono">{n}</td>
                  <td>{c ? rate(c.read_bps) : "—"}</td>
                  <td>{c ? rate(c.write_bps) : "—"}</td>
                  <td>{c ? c.read_iops.toFixed(0) : "—"}</td>
                  <td>{c ? c.write_iops.toFixed(0) : "—"}</td>
                  <td>{c ? `${c.busy.toFixed(0)}%` : "—"}</td>
                </tr>
              );
            })}
          </Table>
        </div>
      </Card>
    );
  }

  if (only === "net" && nets.length > 0) {
    const rx = s.map((x) => sum((x.nets ?? []).map((d) => d.rx_bps)));
    const tx = s.map((x) => sum((x.nets ?? []).map((d) => d.tx_bps)));
    return (
      <Card>
        <CardHeader title="Network" subtitle="Traffic of all interfaces together (last hour), with each interface's current figures below." />
        <div className="space-y-4 p-5">
          <LineChart max={niceMax(Math.max(...rx, ...tx), 100 * 1024)} unit="B/s" format={rate} series={[{ label: "Received", color: COLORS.rx, values: rx }, { label: "Sent", color: COLORS.tx, values: tx }]} />
          <Table head={["Interface", "Received", "Sent", "Errors and drops", "Wi-Fi signal"]} compact>
            {nets.map((n) => {
              const c = m.nets?.find((d) => d.name === n);
              const sig = c?.wireless ? c.signal_dbm : undefined;
              return (
                <tr key={n}>
                  <td className="font-mono">{n}</td>
                  <td>{c ? rate(c.rx_bps) : "—"}</td>
                  <td>{c ? rate(c.tx_bps) : "—"}</td>
                  <td className={(c?.errors ?? 0) >= 1 ? "text-amber-700" : ""}>{`${(c?.errors ?? 0).toFixed(1)}/s`}</td>
                  <td className={sig != null && sig < -75 ? "text-amber-700" : ""}>{sig != null ? `${sig} dBm · ${sig >= -60 ? "good" : sig >= -75 ? "fair" : "weak"}` : "—"}</td>
                </tr>
              );
            })}
          </Table>
        </div>
      </Card>
    );
  }

  if (only === "power" && (p || hasClock)) {
    return (
      <Card>
        <CardHeader title="Power and CPU clock" subtitle="Under-voltage and throttling reported by the Raspberry Pi firmware, and the CPU clock (last hour)." />
        <div className="space-y-4 p-5">
          {p && (
            <ul className="space-y-1.5 text-sm">
              <PowerRow label="Under-voltage" now={p.under_voltage} since={p.since_under_voltage} help="The supply or cable can't keep the voltage up." />
              <PowerRow label="Clock capped" now={p.freq_capped} since={p.since_freq_capped} help="The firmware limits the CPU speed." />
              <PowerRow label="Throttled" now={p.throttled} since={p.since_throttled} help="The CPU is running slower than normal." />
              <PowerRow label="Temperature limit" now={p.temp_limit} since={p.since_temp_limit} help="The soft temperature limit has been reached." />
            </ul>
          )}
          {hasClock && (
            <LineChart
              max={niceMax(Math.max(...s.map((x) => Math.max(x.clock ?? 0, x.max_clock ?? 0))), 1000)}
              unit=" MHz"
              format={(v) => `${v.toFixed(0)} MHz`}
              series={[
                { label: "CPU clock", color: "#0051c3", values: s.map((x) => x.clock ?? 0) },
                ...(m.max_clock_mhz ? [{ label: "Maximum", color: "#a3a3a3", values: s.map((x) => x.max_clock ?? 0) }] : []),
              ]}
            />
          )}
          {p && <p className="text-xs text-neutral-500">“Since boot” flags stay set until the next reboot; the alert clears an hour after the last event.</p>}
        </div>
      </Card>
    );
  }
  return null;
}

function PowerRow({ label, now, since, help }: { label: string; now?: boolean; since?: boolean; help: string }) {
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span className="w-36 shrink-0 font-medium">{label}</span>
      {now ? <Badge tone="bad">Happening now</Badge> : since ? <Badge tone="warn">Since boot</Badge> : <Badge tone="ok">None</Badge>}
      <span className="min-w-0 text-xs text-neutral-500">{help}</span>
    </li>
  );
}

// --- Compare: overlay any metrics on one chart to see what happened at the same moment ---

type Kind = "pct" | "scaled";
interface Candidate {
  id: string;
  label: string;
  color: string;
  kind: Kind; // pct: already 0-100; scaled: shown as a share of its own highest value in the hour
  get: (x: MetricSample) => number;
  fmt: (v: number) => string;
  has: (s: MetricSample[], m: Metrics) => boolean;
}

const sumOf = (x: MetricSample, f: (d: NonNullable<MetricSample["disks"]>[number]) => number) => sum((x.disks ?? []).map(f));
const sumNet = (x: MetricSample, f: (d: NonNullable<MetricSample["nets"]>[number]) => number) => sum((x.nets ?? []).map(f));
const flag = (bit: number) => (x: MetricSample) => (x.has_power && (x.power ?? 0) & (1 << bit) ? 100 : 0);

const CANDIDATES: Candidate[] = [
  { id: "cpu", label: "CPU", color: "#0051c3", kind: "pct", get: (x) => x.cpu, fmt: (v) => `${v.toFixed(0)}%`, has: () => true },
  { id: "mem", label: "Memory", color: "#f6821f", kind: "pct", get: (x) => x.mem, fmt: (v) => `${v.toFixed(0)}%`, has: () => true },
  { id: "temp", label: "Temperature", color: "#d92d20", kind: "scaled", get: (x) => x.temp, fmt: (v) => `${v.toFixed(0)}°C`, has: (s) => s.some((x) => x.temp > 0) },
  { id: "load", label: "Load (1 min)", color: "#7c3aed", kind: "scaled", get: (x) => x.load1, fmt: (v) => v.toFixed(2), has: (s) => s.some((x) => x.load1 > 0) },
  { id: "iowait", label: "I/O wait", color: "#c026d3", kind: "pct", get: (x) => x.iowait ?? 0, fmt: (v) => `${v.toFixed(0)}%`, has: (s) => s.some((x) => (x.iowait ?? 0) > 0) },
  { id: "swap", label: "Swap", color: "#a3a3a3", kind: "pct", get: (x) => x.swap ?? 0, fmt: (v) => `${v.toFixed(0)}%`, has: (_s, m) => (m.swap_total ?? 0) > 0 },
  { id: "dread", label: "Disk read", color: "#0e7490", kind: "scaled", get: (x) => sumOf(x, (d) => d.read_bps), fmt: rate, has: (s) => s.some((x) => x.disks?.length) },
  { id: "dwrite", label: "Disk write", color: "#b45309", kind: "scaled", get: (x) => sumOf(x, (d) => d.write_bps), fmt: rate, has: (s) => s.some((x) => x.disks?.length) },
  { id: "dbusy", label: "Disk busy", color: "#4d7c0f", kind: "pct", get: (x) => Math.max(0, ...(x.disks ?? []).map((d) => d.busy)), fmt: (v) => `${v.toFixed(0)}%`, has: (s) => s.some((x) => x.disks?.length) },
  { id: "rx", label: "Network in", color: "#16794c", kind: "scaled", get: (x) => sumNet(x, (d) => d.rx_bps), fmt: rate, has: (s) => s.some((x) => x.nets?.length) },
  { id: "tx", label: "Network out", color: "#1d4ed8", kind: "scaled", get: (x) => sumNet(x, (d) => d.tx_bps), fmt: rate, has: (s) => s.some((x) => x.nets?.length) },
  { id: "neterr", label: "Network errors", color: "#be123c", kind: "scaled", get: (x) => sumNet(x, (d) => d.errors ?? 0), fmt: (v) => `${v.toFixed(1)}/s`, has: (s) => s.some((x) => x.nets?.some((d) => (d.errors ?? 0) > 0)) },
  { id: "clock", label: "CPU clock", color: "#0f766e", kind: "scaled", get: (x) => x.clock ?? 0, fmt: (v) => `${v.toFixed(0)} MHz`, has: (s) => s.some((x) => (x.clock ?? 0) > 0) },
  { id: "uv", label: "Under-voltage", color: "#dc2626", kind: "pct", get: flag(0), fmt: (v) => (v ? "yes" : "no"), has: (s) => s.some((x) => x.has_power) },
  { id: "throttle", label: "Throttled", color: "#ea580c", kind: "pct", get: (x) => (flag(2)(x) || flag(1)(x) || flag(3)(x) ? 100 : 0), fmt: (v) => (v ? "yes" : "no"), has: (s) => s.some((x) => x.has_power) },
];

export function CompareCard({ s, m }: { s: MetricSample[]; m: Metrics }) {
  const avail = CANDIDATES.filter((c) => c.has(s, m));
  const [picked, setPicked] = useState<string[]>(() => ["cpu", "temp", "iowait", "uv"].filter((id) => avail.some((c) => c.id === id)).slice(0, 3));
  const toggle = (id: string) => setPicked((p) => (p.includes(id) ? p.filter((x) => x !== id) : [...p, id]));
  const series = avail
    .filter((c) => picked.includes(c.id))
    .map((c) => {
      const raw = s.map(c.get);
      const top = c.kind === "pct" ? 100 : Math.max(...raw, 1e-9);
      return { label: c.label, color: c.color, values: raw.map((v) => (v / top) * 100), shown: raw.length ? c.fmt(raw[raw.length - 1]) : "—" };
    });
  return (
    <Card>
      <CardHeader title="Compare" subtitle="Overlay metrics to see what happened at the same moment. Pick as many as you like." />
      <div className="space-y-4 p-5">
        <div className="flex flex-wrap gap-2">
          {avail.map((c) => (
            <button
              key={c.id}
              type="button"
              aria-pressed={picked.includes(c.id)}
              onClick={() => toggle(c.id)}
              className={`inline-flex min-h-9 items-center gap-1.5 rounded-full border px-3 text-sm ${picked.includes(c.id) ? "border-neutral-900 bg-neutral-50 font-medium" : "border-neutral-200 text-neutral-600"}`}
            >
              <span className="size-2 rounded-full" style={{ background: c.color, opacity: picked.includes(c.id) ? 1 : 0.35 }} />
              {c.label}
            </button>
          ))}
        </div>
        {series.length > 0 ? <LineChart height={160} series={series} unit="%" /> : <p className="text-sm text-neutral-500">Pick at least one metric.</p>}
        <p className="text-xs text-neutral-500">Percentages are shown as they are. Everything else (temperature, load, speeds, clock) is drawn as a share of its own highest value in the last hour, so the shapes line up; the legend shows the real latest value.</p>
      </div>
    </Card>
  );
}
