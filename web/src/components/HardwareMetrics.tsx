import type { Metrics, MetricSample } from "../api";
import { Badge, FoldCard, LineChart } from "./ui";
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

export default function HardwareMetrics({ s, m, cores }: { s: MetricSample[]; m: Metrics; cores: number }) {
  const disks = names(s, (x) => x.disks);
  const nets = names(s, (x) => x.nets);
  const hasLoad = s.some((x) => x.load1 > 0 || (x.iowait ?? 0) > 0);
  const hasPower = s.some((x) => x.has_power) || m.power != null;
  const hasClock = s.some((x) => (x.clock ?? 0) > 0);
  const swapTotal = m.swap_total ?? 0;
  const oomNow = m.oom_kills ?? 0;
  const oomEver = oomNow > 0;
  const p = m.power;

  const diskVals = (name: string, f: (d: NonNullable<MetricSample["disks"]>[number]) => number) => s.map((x) => f(x.disks?.find((d) => d.name === name) ?? { name, read_bps: 0, write_bps: 0, read_iops: 0, write_iops: 0, busy: 0 }));
  const netVals = (name: string, f: (d: NonNullable<MetricSample["nets"]>[number]) => number) => s.map((x) => f(x.nets?.find((d) => d.name === name) ?? { name, rx_bps: 0, tx_bps: 0 }));

  return (
    <>
      {disks.length > 0 && (
        <FoldCard title="Disk I/O" subtitle="Read and write speed and how busy each disk is (last hour).">
          <div className="space-y-6 p-5">
            {disks.map((n) => {
              const cur = m.disks?.find((d) => d.name === n);
              const r = diskVals(n, (d) => d.read_bps);
              const w = diskVals(n, (d) => d.write_bps);
              return (
                <div key={n} className="space-y-3">
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <h4 className="font-mono text-sm font-semibold">{n}</h4>
                    {cur && (
                      <dl className="flex flex-wrap gap-x-5 gap-y-1">
                        <Stat label="Read IOPS" value={cur.read_iops.toFixed(0)} />
                        <Stat label="Write IOPS" value={cur.write_iops.toFixed(0)} />
                        <Stat label="Busy" value={`${cur.busy.toFixed(0)}%`} tone={cur.busy >= 90 ? "warn" : undefined} />
                      </dl>
                    )}
                  </div>
                  <LineChart
                    max={niceMax(Math.max(...r, ...w), 1024 * 1024)}
                    unit="B/s"
                    format={rate}
                    series={[
                      { label: "Read", color: COLORS.read, values: r },
                      { label: "Write", color: COLORS.write, values: w },
                    ]}
                  />
                  <LineChart height={64} series={[{ label: "Busy", color: COLORS.busy, values: diskVals(n, (d) => d.busy) }]} />
                </div>
              );
            })}
          </div>
        </FoldCard>
      )}

      {nets.length > 0 && (
        <FoldCard title="Network" subtitle="Traffic per interface, with errors and Wi-Fi signal (last hour).">
          <div className="space-y-6 p-5">
            {nets.map((n) => {
              const cur = m.nets?.find((d) => d.name === n);
              const rx = netVals(n, (d) => d.rx_bps);
              const tx = netVals(n, (d) => d.tx_bps);
              const errs = netVals(n, (d) => d.errors ?? 0);
              const sig = cur?.wireless ? cur.signal_dbm : undefined;
              return (
                <div key={n} className="space-y-3">
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <h4 className="font-mono text-sm font-semibold">{n}</h4>
                    <dl className="flex flex-wrap gap-x-5 gap-y-1">
                      {sig != null && <Stat label="Wi-Fi signal" value={`${sig} dBm · ${sig >= -60 ? "good" : sig >= -75 ? "fair" : "weak"}`} tone={sig < -75 ? "warn" : undefined} />}
                      <Stat label="Errors and drops" value={`${(cur?.errors ?? 0).toFixed(1)}/s`} tone={(cur?.errors ?? 0) >= 1 ? "warn" : undefined} />
                    </dl>
                  </div>
                  <LineChart
                    max={niceMax(Math.max(...rx, ...tx), 100 * 1024)}
                    unit="B/s"
                    format={rate}
                    series={[
                      { label: "Received", color: COLORS.rx, values: rx },
                      { label: "Sent", color: COLORS.tx, values: tx },
                    ]}
                  />
                  {errs.some((v) => v > 0) && (
                    <LineChart height={64} max={niceMax(Math.max(...errs), 5)} unit="/s" series={[{ label: "Errors and drops", color: COLORS.err, values: errs }]} />
                  )}
                </div>
              );
            })}
          </div>
        </FoldCard>
      )}

      {(hasLoad || swapTotal > 0 || oomEver) && (
        <FoldCard title="Load and memory pressure" subtitle="Run queue, time spent waiting for disk, swap, and processes the kernel killed for lack of memory.">
          <div className="space-y-4 p-5">
            <dl className="grid grid-cols-2 gap-x-6 gap-y-2 sm:grid-cols-4">
              <Stat label="Load (1 / 5 / 15 min)" value={`${m.load1.toFixed(2)} ${m.load5.toFixed(2)} ${m.load15.toFixed(2)}`} />
              <Stat label="I/O wait" value={`${(m.iowait_percent ?? 0).toFixed(0)}%`} tone={(m.iowait_percent ?? 0) >= 40 ? "warn" : undefined} />
              <Stat label="Swap" value={swapTotal > 0 ? `${bytes(m.swap_used ?? 0)} of ${bytes(swapTotal)}` : "None configured"} tone={swapTotal > 0 && (m.swap_used ?? 0) / swapTotal >= 0.8 ? "warn" : undefined} />
              <Stat label="Out-of-memory kills since boot" value={String(oomNow)} tone={oomNow > 0 ? "warn" : undefined} />
            </dl>
            <LineChart
              max={niceMax(Math.max(...s.map((x) => x.load1), cores || 1), 1)}
              unit=""
              format={(v) => v.toFixed(2)}
              series={[{ label: `Load (1 min)${cores ? ` · ${cores} cores` : ""}`, color: "#0051c3", values: s.map((x) => x.load1) }]}
            />
            <LineChart
              height={80}
              series={[
                { label: "I/O wait", color: COLORS.busy, values: s.map((x) => x.iowait ?? 0) },
                ...(swapTotal > 0 ? [{ label: "Swap used", color: COLORS.write, values: s.map((x) => x.swap ?? 0) }] : []),
              ]}
            />
          </div>
        </FoldCard>
      )}

      {(hasPower || hasClock) && (
        <FoldCard title="Power and CPU clock" subtitle="Under-voltage and throttling reported by the Raspberry Pi firmware, and the CPU clock (last hour).">
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
        </FoldCard>
      )}
    </>
  );
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
