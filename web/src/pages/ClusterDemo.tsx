// DEMO ONLY: a visual mock of how clustering (automatic placement, failover and a standby server) could look.
// Everything here is sample data held in this file. There is no scheduler, no replication and no API behind it.
import { useMemo, useState } from "react";
import { AlertTriangle, ArrowRight, Check, Cpu, Crown, HardDrive, Loader2, Lock, Plus, Power, RefreshCw, Server, ShieldCheck, Wrench } from "lucide-react";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, Field, Meter, PageHeader, StatusDot, Table, Td, Toggle, inputClass } from "../components/ui";

type Node = { id: string; arch: "arm64" | "amd64"; model: string; ip: string; cores: number; ram: number; cpu: number; mem: number; labels: string[] };
type Load = { id: string; kind: "Worker" | "Stack"; image: string; replicas: number; arch: "any" | "amd64"; spread: boolean; pinned?: string; place: string[]; cpu: number; mem: number; port: string };

const nodes: Node[] = [
  { id: "claude", arch: "arm64", model: "Raspberry Pi 5", ip: "192.168.0.184", cores: 4, ram: 8, cpu: 22, mem: 41, labels: ["home", "ssd"] },
  { id: "cm4", arch: "arm64", model: "Compute Module 4", ip: "192.168.0.189", cores: 4, ram: 4, cpu: 35, mem: 58, labels: ["home"] },
  { id: "portainer-vm", arch: "amd64", model: "VM on Proxmox", ip: "192.168.0.231", cores: 4, ram: 8, cpu: 28, mem: 46, labels: ["home", "ssd"] },
  { id: "ankere5000", arch: "amd64", model: "Mini PC", ip: "192.168.0.205", cores: 8, ram: 16, cpu: 14, mem: 33, labels: ["home", "gpu"] },
];

const base: Load[] = [
  { id: "api", kind: "Worker", image: "ghcr.io/maffewem/api", replicas: 2, arch: "any", spread: true, place: ["claude", "cm4"], cpu: 6, mem: 3, port: "3000" },
  { id: "homepage", kind: "Worker", image: "nginx:alpine", replicas: 1, arch: "any", spread: false, place: ["cm4"], cpu: 1, mem: 1, port: "8080" },
  { id: "queue", kind: "Worker", image: "ghcr.io/maffewem/queue", replicas: 3, arch: "any", spread: true, place: ["claude", "portainer-vm", "ankere5000"], cpu: 4, mem: 4, port: "9000" },
  { id: "build-runner", kind: "Worker", image: "ghcr.io/maffewem/runner", replicas: 1, arch: "amd64", spread: false, place: ["ankere5000"], cpu: 12, mem: 10, port: "-" },
  { id: "postgres", kind: "Stack", image: "postgres:16 + pgdata volume", replicas: 1, arch: "any", spread: false, pinned: "portainer-vm", place: ["portainer-vm"], cpu: 8, mem: 14, port: "5432" },
];

const nodeById = (id: string) => nodes.find((n) => n.id === id)!;

/** The mock scheduler: replicas on a dead node move to the least loaded compatible node that doesn't already run one. */
function reschedule(down: string | null) {
  const result = base.map((l) => ({ ...l, place: [...l.place], moved: [] as [string, string][], stuck: false }));
  if (!down) return result;
  const count = (id: string) => result.reduce((n, l) => n + l.place.filter((p) => p === id).length, 0);
  for (const l of result) {
    if (!l.place.includes(down)) continue;
    if (l.pinned === down) { l.stuck = true; continue; }
    l.place = l.place.filter((p) => p !== down);
    const to = nodes.filter((n) => n.id !== down && !l.place.includes(n.id) && (l.arch === "any" || n.arch === l.arch)).sort((a, b) => count(a.id) - count(b.id))[0];
    if (to) { l.place.push(to.id); l.moved.push([down, to.id]); }
  }
  return result;
}

function Banner() {
  return (
    <div className="mb-6 rounded-xl border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-900">
      <strong>Demo only.</strong> Clustering is not a working feature. This page shows sample data to preview how automatic placement, failover and a standby server could look. Try “Simulate failure” on a device.
    </div>
  );
}

function Pill({ children, tone = "neutral" }: { children: string; tone?: "neutral" | "ok" | "warn" | "bad" | "info" }) {
  return <Badge tone={tone}>{children}</Badge>;
}

function NodesTab({ down, setDown, drained, setDrained }: { down: string | null; setDown: (v: string | null) => void; drained: string | null; setDrained: (v: string | null) => void }) {
  const placed = useMemo(() => reschedule(down), [down]);
  const moved = placed.flatMap((l) => l.moved.map(([f, t]) => ({ id: l.id, from: f, to: t })));
  const stuck = placed.filter((l) => l.stuck);
  return (
    <>
      {down && (
        <div className="mb-6 rounded-xl border border-red-300 bg-red-50 px-4 py-3 text-sm text-red-900">
          <strong>{down} stopped reporting 45 seconds ago.</strong> {moved.length} replica{moved.length === 1 ? "" : "s"} rescheduled
          {stuck.length > 0 && <>, {stuck.length} waiting because its data lives only on {down}</>}.
          <button className="ml-3 underline" onClick={() => setDown(null)}>Bring {down} back</button>
        </div>
      )}
      <div className="grid gap-4 md:grid-cols-2">
        {nodes.map((n) => {
          const off = down === n.id;
          const here = placed.filter((l) => l.place.includes(n.id) || (l.stuck && l.pinned === n.id));
          return (
            <Card key={n.id} className={off ? "opacity-80 ring-1 ring-red-300" : ""}>
              <div className="flex items-start justify-between gap-3 border-b border-neutral-100 p-4">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 font-medium"><StatusDot status={off ? "offline" : "online"} />{n.id}
                    <Pill tone={n.arch === "arm64" ? "info" : "neutral"}>{n.arch}</Pill>
                    {drained === n.id && <Pill tone="warn">draining</Pill>}
                  </div>
                  <p className="mt-0.5 text-xs text-neutral-500">{n.model} · <span className="font-mono">{n.ip}</span> · {n.cores} cores · {n.ram} GB</p>
                  <div className="mt-1 flex gap-1">{n.labels.map((l) => <span key={l} className="rounded bg-neutral-100 px-1.5 py-0.5 text-[11px] text-neutral-600">{l}</span>)}</div>
                </div>
                <div className="flex shrink-0 gap-2">
                  <Button size="sm" onClick={() => setDrained(drained === n.id ? null : n.id)} icon={<Wrench className="size-3.5" />}>{drained === n.id ? "Undrain" : "Drain"}</Button>
                  <Button size="sm" variant={off ? "primary" : "danger"} onClick={() => setDown(off ? null : n.id)} icon={<Power className="size-3.5" />}>{off ? "Restore" : "Simulate failure"}</Button>
                </div>
              </div>
              <div className="grid grid-cols-2 gap-4 px-4 pt-3">
                <Meter value={off ? 0 : n.cpu} label="CPU" /><Meter value={off ? 0 : n.mem} label="Memory" />
              </div>
              <div className="space-y-1.5 p-4">
                {here.length === 0 && <p className="text-sm text-neutral-500">Nothing scheduled here.</p>}
                {here.map((l) => {
                  const arrived = l.moved.some(([, to]) => to === n.id);
                  const wait = l.stuck && l.pinned === n.id;
                  return (
                    <div key={l.id} className={`flex items-center justify-between rounded-lg border px-3 py-2 text-sm ${wait ? "border-red-200 bg-red-50" : arrived ? "border-emerald-300 bg-emerald-50" : "border-neutral-200"}`}>
                      <span className="flex items-center gap-2 font-medium">{l.id}{l.kind === "Stack" && <Pill>stack</Pill>}{l.pinned && <Lock className="size-3 text-neutral-400" />}</span>
                      <span className="text-xs text-neutral-500">
                        {wait ? "waiting: data is on this device" : arrived ? "moved here automatically" : `port ${l.port}`}
                      </span>
                    </div>
                  );
                })}
              </div>
            </Card>
          );
        })}
      </div>
      {down && <Failover down={down} moved={moved} stuck={stuck.map((s) => s.id)} />}
    </>
  );
}

function Failover({ down, moved, stuck }: { down: string; moved: { id: string; from: string; to: string }[]; stuck: string[] }) {
  const rows: [string, string, string, "done" | "wait"][] = [
    ["Device offline", `${down} missed 3 heartbeats (45 s)`, "00:45", "done"],
    ["Marked unreachable", "Grace period so a reboot does not trigger a move", "00:15", "done"],
    ...moved.map((m): [string, string, string, "done" | "wait"] => [`Rescheduled ${m.id}`, `${m.from} → ${m.to}: pull image, start, health check, route traffic`, "00:21", "done"]),
    ...stuck.map((s): [string, string, string, "done" | "wait"] => [`${s} not moved`, "Local volume only exists on the dead device. Restore latest backup elsewhere, or wait.", "—", "wait"]),
  ];
  return (
    <Card className="mt-6">
      <CardHeader title="Failover timeline" subtitle="Same step list as a deployment, so it looks familiar." />
      <ol className="divide-y divide-neutral-100">
        {rows.map(([t, d, ts, st], i) => (
          <li key={i} className="flex items-center gap-3 px-5 py-3 text-sm">
            {st === "done" ? <Check className="size-4 text-emerald-600" /> : <AlertTriangle className="size-4 text-amber-600" />}
            <div className="min-w-0 flex-1"><p className="font-medium">{t}</p><p className="text-xs text-neutral-500">{d}</p></div>
            <span className="font-mono text-xs text-neutral-500">{ts}</span>
            {st === "wait" && <Button size="sm">Restore from backup on…</Button>}
          </li>
        ))}
      </ol>
    </Card>
  );
}

function PlacementTab() {
  const [sel, setSel] = useState("api");
  const l = base.find((x) => x.id === sel)!;
  const [replicas, setReplicas] = useState(l.replicas);
  const [spread, setSpread] = useState(l.spread);
  const [mode, setMode] = useState(l.pinned ? "pinned" : l.arch === "amd64" ? "arch" : "auto");
  const pick = (id: string) => { const x = base.find((b) => b.id === id)!; setSel(id); setReplicas(x.replicas); setSpread(x.spread); setMode(x.pinned ? "pinned" : x.arch === "amd64" ? "arch" : "auto"); };
  const modes: [string, string, string][] = [
    ["auto", "Anywhere", "Scheduler picks the least loaded device. Image must be multi-arch."],
    ["arch", "Matching architecture", "Only devices that can run the image (amd64 / arm64)."],
    ["labels", "Devices with labels", "Only devices tagged e.g. ssd or gpu."],
    ["pinned", "Pinned to one device", "For workers with a local volume. No automatic failover."],
  ];
  return (
    <div className="grid gap-4 lg:grid-cols-[220px_1fr]">
      <Card>
        <CardHeader title="Workers" />
        <div className="p-2">{base.map((b) => (
          <button key={b.id} onClick={() => pick(b.id)} className={`flex w-full items-center justify-between rounded-lg px-3 py-2 text-left text-sm ${sel === b.id ? "bg-blue-50 text-primary" : "hover:bg-neutral-50"}`}>{b.id}<span className="text-xs text-neutral-500">×{b.replicas}</span></button>
        ))}</div>
      </Card>
      <div className="grid gap-4">
        <Card>
          <CardHeader title={`Placement for ${l.id}`} subtitle="Saved with the project and applied on the next deployment or device change." />
          <div className="grid gap-3 p-5 sm:grid-cols-2">
            {modes.map(([k, t, d]) => (
              <button key={k} onClick={() => setMode(k)} className={`rounded-xl border p-4 text-left ${mode === k ? "border-primary bg-blue-50" : "border-neutral-200 hover:bg-neutral-50"}`}>
                <p className="text-sm font-medium">{t}</p><p className="text-xs text-neutral-500">{d}</p>
              </button>
            ))}
          </div>
          <div className="grid gap-4 border-t border-neutral-100 p-5 sm:grid-cols-2">
            <Field label="Replicas" hint="How many copies run at the same time."><input type="number" min={1} max={8} className={inputClass} value={replicas} onChange={(e) => setReplicas(Number(e.target.value))} /></Field>
            {mode === "labels" && <Field label="Required labels"><input className={inputClass} defaultValue="ssd, gpu" /></Field>}
            {mode === "pinned" && <Field label="Device"><select className={inputClass} defaultValue={l.pinned ?? "portainer-vm"}>{nodes.map((n) => <option key={n.id}>{n.id}</option>)}</select></Field>}
            <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2">
              <span><span className="font-medium">Spread across devices</span><br /><span className="text-xs text-neutral-500">Never put two replicas on the same device.</span></span>
              <Toggle checked={spread} onChange={setSpread} label="Spread" />
            </div>
            <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2">
              <span><span className="font-medium">Fail over automatically</span><br /><span className="text-xs text-neutral-500">Reschedule when a device is offline for more than 60 seconds.</span></span>
              <Toggle checked={mode !== "pinned"} onChange={() => {}} label="Failover" />
            </div>
          </div>
        </Card>
        <Card>
          <CardHeader title="Where it would run" subtitle="Preview of the scheduler's decision." />
          <div className="flex flex-wrap gap-2 p-5">
            {(mode === "pinned" ? [l.pinned ?? "portainer-vm"] : l.place.slice(0, replicas).concat(nodes.map((n) => n.id).filter((n) => !l.place.includes(n)).slice(0, Math.max(0, replicas - l.place.length)))).map((id, i) => (
              <span key={id + i} className="flex items-center gap-2 rounded-lg border border-neutral-200 px-3 py-2 text-sm"><Server className="size-4 text-neutral-400" />{id}<Pill tone="info">{nodeById(id).arch}</Pill></span>
            ))}
          </div>
          <div className="px-5 pb-5"><Button variant="primary">Save placement (demo)</Button></div>
        </Card>
      </div>
    </div>
  );
}

function ServerTab() {
  const [promoted, setPromoted] = useState(false);
  const [busy, setBusy] = useState(false);
  const primary = promoted ? "orch-standby" : "orch-primary";
  const secondary = promoted ? "orch-primary" : "orch-standby";
  const promote = () => { setBusy(true); setTimeout(() => { setPromoted((p) => !p); setBusy(false); }, 1200); };
  const srv = (name: string, ip: string, role: "Leader" | "Standby", where: string) => (
    <Card className={role === "Leader" ? "ring-1 ring-primary" : ""}>
      <div className="flex items-center justify-between p-4">
        <div>
          <div className="flex items-center gap-2 font-medium">{role === "Leader" ? <Crown className="size-4 text-primary" /> : <ShieldCheck className="size-4 text-neutral-400" />}{name}<Pill tone={role === "Leader" ? "ok" : "neutral"}>{role}</Pill></div>
          <p className="mt-0.5 text-xs text-neutral-500">{where} · <span className="font-mono">{ip}</span></p>
        </div>
      </div>
      <div className="grid grid-cols-2 gap-4 border-t border-neutral-100 p-4 text-sm">
        <div><p className="text-xs text-neutral-500">Database</p><p>{role === "Leader" ? "Read / write" : "Read-only replica"}</p></div>
        <div><p className="text-xs text-neutral-500">Replication lag</p><p>{role === "Leader" ? "—" : "0.8 s"}</p></div>
        <div><p className="text-xs text-neutral-500">Dashboard</p><p>{role === "Leader" ? "Serving" : "Redirects to leader"}</p></div>
        <div><p className="text-xs text-neutral-500">Agents connected</p><p>{role === "Leader" ? "4 of 4" : "4 of 4 (fallback)"}</p></div>
      </div>
    </Card>
  );
  return (
    <>
      <div className="grid gap-4 md:grid-cols-2">
        {srv(primary, promoted ? "192.168.0.189" : "192.168.0.184", "Leader", promoted ? "cm4" : "claude")}
        {srv(secondary, promoted ? "192.168.0.184" : "192.168.0.189", "Standby", promoted ? "claude" : "cm4")}
      </div>
      <div className="mt-4 flex items-center gap-3">
        <Button variant="primary" onClick={promote} disabled={busy} icon={busy ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}>{busy ? "Promoting…" : "Promote standby (planned switch)"}</Button>
        <span className="text-xs text-neutral-500">Unplanned: standby promotes itself after the leader misses 15 s of lease renewals.</span>
      </div>
      <Card className="mt-6">
        <CardHeader title="How the switch works" />
        <ol className="space-y-3 p-5 text-sm">
          {["Leader writes every change to its SQLite database and streams it to the standby (WAL shipping, about a second behind).", "The leader holds a lease. If it stops renewing, the standby takes the lease, opens the database read/write and becomes leader.", "Agents already list both URLs (the fallback list from PR #66), so they reconnect to whichever answers as leader.", "The old leader comes back as a standby and re-syncs from the new one. No split brain: only the lease holder accepts writes."].map((t, i) => (
            <li key={i} className="flex gap-3"><span className="flex size-6 shrink-0 items-center justify-center rounded-full bg-neutral-100 text-xs font-medium">{i + 1}</span><span>{t}</span></li>
          ))}
        </ol>
      </Card>
      <Card className="mt-6">
        <CardHeader title="Who decides which is leader?" subtitle="A two-node pair cannot safely decide alone." />
        <div className="grid gap-4 p-5 text-sm sm:grid-cols-3">
          <div className="rounded-xl border border-primary bg-blue-50 p-4"><p className="font-medium">Lease in object storage</p><p className="mt-1 text-xs text-neutral-600">A small file in S3/R2 (already supported for backups). Simple, needs internet. Recommended.</p></div>
          <div className="rounded-xl border border-neutral-200 p-4"><p className="font-medium">Third voter</p><p className="mt-1 text-xs text-neutral-600">A cheap witness (any device) breaks ties. Works offline.</p></div>
          <div className="rounded-xl border border-neutral-200 p-4"><p className="font-medium">Manual only</p><p className="mt-1 text-xs text-neutral-600">Standby never promotes by itself. You press the button. Safest, slowest.</p></div>
        </div>
      </Card>
    </>
  );
}

function RulesTab() {
  return (
    <>
      <Card className="mb-6">
        <CardHeader title="Cluster settings" subtitle="Defaults for every worker. Each worker can override them." />
        <div className="grid gap-4 p-5 sm:grid-cols-2">
          <Field label="Declare a device lost after"><select className={inputClass} defaultValue="60"><option value="30">30 seconds</option><option value="60">60 seconds</option><option value="300">5 minutes</option></select></Field>
          <Field label="Keep spare capacity"><select className={inputClass} defaultValue="1"><option value="0">None</option><option value="1">Survive losing 1 device</option><option value="2">Survive losing 2 devices</option></select></Field>
          <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2"><span><span className="font-medium">Rebalance when a device returns</span><br /><span className="text-xs text-neutral-500">Off by default so a flapping device does not shuffle your workloads.</span></span><Toggle checked={false} onChange={() => {}} label="Rebalance" /></div>
          <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2"><span><span className="font-medium">Drain before maintenance mode</span><br /><span className="text-xs text-neutral-500">Entering maintenance moves workers off first, using the same scheduler.</span></span><Toggle checked onChange={() => {}} label="Drain" /></div>
        </div>
      </Card>
      <Card>
        <CardHeader title="Capacity check" subtitle="Can the fleet absorb a lost device right now?" />
        <Table head={["If this device fails", "Workloads that can move", "Result"]}>
          {[["claude", "api, queue", "ok"], ["cm4", "api, homepage", "ok"], ["portainer-vm", "queue (postgres is pinned)", "warn"], ["ankere5000", "queue (build-runner needs amd64: nowhere to go)", "warn"]].map(([d, w, r]) => (
            <tr key={d} className="border-t border-neutral-100"><Td className="font-medium">{d}</Td><Td>{w}</Td><Td>{r === "ok" ? <Pill tone="ok">Safe</Pill> : <Pill tone="warn">Needs attention</Pill>}</Td></tr>
          ))}
        </Table>
      </Card>
    </>
  );
}

export default function ClusterDemoPage() {
  useCrumbs([{ label: "Cluster (demo)" }]);
  const [tab, setTab] = useState("nodes");
  const [down, setDown] = useState<string | null>(null);
  const [drained, setDrained] = useState<string | null>(null);
  const tabs: [string, string][] = [["nodes", "Devices and placement"], ["placement", "Worker rules"], ["rules", "Cluster settings"], ["server", "Server high availability"]];
  return (
    <>
      <PageHeader title="Cluster" description="Treat your devices as one pool: workers are placed automatically and move if a device dies." actions={<Button icon={<Plus className="size-4" />}>Add standby server</Button>} />
      <Banner />
      <div className="mb-6 flex flex-wrap gap-1 border-b border-neutral-200">
        {tabs.map(([k, l]) => (
          <button key={k} onClick={() => setTab(k)} className={`-mb-px border-b-2 px-4 py-2 text-sm ${tab === k ? "border-primary font-medium text-primary" : "border-transparent text-neutral-500 hover:text-neutral-800"}`}>{l}</button>
        ))}
      </div>
      {tab === "nodes" && <NodesTab down={down} setDown={setDown} drained={drained} setDrained={setDrained} />}
      {tab === "placement" && <PlacementTab />}
      {tab === "rules" && <RulesTab />}
      {tab === "server" && <ServerTab />}
      <p className="mt-8 hidden text-xs text-neutral-400"><Cpu /><HardDrive /><ArrowRight /></p>
    </>
  );
}
