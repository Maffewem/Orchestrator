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

export type GraphKey = "disk" | "net" | "power";

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
