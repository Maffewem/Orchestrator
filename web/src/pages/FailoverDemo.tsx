// DEMO ONLY: a visual mock of how a standby server with automatic failover could look.
// Everything here is scripted sample data held in this file. There is no replication, election or fencing behind it.
// The design it illustrates is written up in docs/FAILOVER.md.
import { useEffect, useState } from "react";
import { AlertTriangle, ArrowRight, Check, Crown, Lock, Pause, Play, RotateCcw, Server, ShieldCheck, SkipForward, Zap } from "lucide-react";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, Field, PageHeader, StatusDot, Toggle, inputClass } from "../components/ui";

type Tone = "neutral" | "ok" | "warn" | "bad" | "info";
type Srv = { role: string; tone: Tone; epoch: number; db: string; note: string; down?: boolean };
type Link = "A" | "B" | "-";
type Frame = { at: string; title: string; text: string; a: Srv; b: Srv; agents: [Link, Link, Link, Link]; log: string; guard?: { tone: Tone; text: string } };
type Scenario = { id: string; label: string; blurb: string; frames: Frame[] };

const agentNames = ["claude (same device as server A)", "portainer-vm", "ankere5000", "ha-pi"];
const A_UP: Srv = { role: "Active", tone: "ok", epoch: 1, db: "Read / write", note: "Serving dashboard, dispatching work" };
const B_STBY: Srv = { role: "Standby", tone: "neutral", epoch: 1, db: "Read-only replica", note: "Last sync 4 s ago · 1.2 MB" };

const scenarios: Scenario[] = [
  {
    id: "crash",
    label: "Active server crashes",
    blurb: "Power loss on the device running the active server.",
    frames: [
      { at: "0 s", title: "Healthy", text: "Every agent talks to server A. Server B keeps a copy of A's data, refreshed every 15 seconds, and holds a copy of the secret key.", a: A_UP, b: B_STBY, agents: ["A", "A", "A", "A"], log: "Standby in sync (lag 4 s, epoch 1)" },
      { at: "+0 s", title: "Server A disappears", text: "Heartbeats from the agents start failing. Server B notices that its sync pulls fail too.", a: { ...A_UP, role: "Down", tone: "bad", down: true, note: "No answer" }, b: B_STBY, agents: ["-", "-", "-", "-"], log: "Sync pull from A failed (1/3)" },
      { at: "+15 s", title: "B asks the agents", text: "After three missed pulls B does not trust its own view. It asks the agents it can reach whether they still reach A.", a: { ...A_UP, role: "Down", tone: "bad", down: true, note: "No answer" }, b: { ...B_STBY, role: "Checking", tone: "warn", note: "Collecting votes from agents" }, agents: ["-", "-", "-", "-"], log: "3 of 3 reachable agents cannot reach A, and can reach B", guard: { tone: "info", text: "Quorum rule: promote only if a majority of the agents on other devices cannot reach A and can reach B. A lone standby never promotes on its own opinion." } },
      { at: "+17 s", title: "B promotes itself", text: "B raises the epoch from 1 to 2, writes it into its database, opens it read/write and starts dispatching work.", a: { ...A_UP, role: "Down", tone: "bad", down: true, note: "No answer" }, b: { role: "Active", tone: "ok", epoch: 2, db: "Read / write", note: "Promoted automatically" }, agents: ["-", "-", "-", "-"], log: "Promoted: epoch 2. Data up to 4 s before the crash.", guard: { tone: "ok", text: "The epoch is a fencing token. Anything A says from now on carries epoch 1 and is ignored by agents." } },
      { at: "+20 s", title: "Agents follow", text: "Each agent keeps both addresses (the fallback list from the server-address work). The next heartbeat to B succeeds, B announces epoch 2 and the agents remember it.", a: { ...A_UP, role: "Down", tone: "bad", down: true, note: "No answer" }, b: { role: "Active", tone: "ok", epoch: 2, db: "Read / write", note: "3 of 3 reachable agents connected" }, agents: ["-", "B", "B", "B"], log: "Agents connected to B: 3 (the 4th device is down with server A)" },
      { at: "+2 min", title: "A comes back as standby", text: "A starts and finds that B holds a higher epoch. It starts fenced, with no writes. Its own changes after the last sync are saved as a file for review, and it re-copies from B.", a: { role: "Standby", tone: "neutral", epoch: 2, db: "Read-only replica", note: "Re-synced from B · diverged copy kept" }, b: { role: "Active", tone: "ok", epoch: 2, db: "Read / write", note: "All 4 agents connected" }, agents: ["B", "B", "B", "B"], log: "A rejoined as standby (epoch 2)", guard: { tone: "ok", text: "Nothing switches back by itself. Moving the active role back to A is a normal planned switch." } },
    ],
  },
  {
    id: "isolated",
    label: "Active server isolated but still running",
    blurb: "The classic split-brain trap: A is alive but cut off from the network.",
    frames: [
      { at: "0 s", title: "Healthy", text: "Same starting point.", a: A_UP, b: B_STBY, agents: ["A", "A", "A", "A"], log: "Standby in sync (lag 4 s, epoch 1)" },
      { at: "+0 s", title: "A loses the network", text: "A is running, but its switch port died. Nobody can reach it and it cannot reach anybody.", a: { ...A_UP, role: "Isolated", tone: "warn", note: "Running, no heartbeats arriving" }, b: B_STBY, agents: ["-", "-", "-", "-"], log: "Heartbeats missing from all agents" },
      { at: "+10 s", title: "A fences itself", text: "A's write lease lasts 10 s and is renewed by heartbeats from a majority of agents. It cannot renew, so it stops accepting changes and goes read-only. This happens before B is allowed to promote.", a: { ...A_UP, role: "Fenced", tone: "warn", db: "Read-only (lease expired)", note: "Dashboard shows a banner, writes return 423" }, b: { ...B_STBY, role: "Checking", tone: "warn", note: "Waiting out A's lease (15 s)" }, agents: ["-", "-", "-", "-"], log: "A: write lease expired, now read-only", guard: { tone: "ok", text: "Timing rule: B waits longer (15 s) than A's lease (10 s). A has already stopped writing when B starts." } },
      { at: "+17 s", title: "B promotes", text: "Quorum agrees A is unreachable. B promotes at epoch 2.", a: { ...A_UP, role: "Fenced", tone: "warn", db: "Read-only (lease expired)", note: "Still running, still isolated" }, b: { role: "Active", tone: "ok", epoch: 2, db: "Read / write", note: "Promoted automatically" }, agents: ["-", "B", "B", "B"], log: "B promoted at epoch 2; 3 agents connected", guard: { tone: "ok", text: "Two servers are running, but only one can write. That is the whole point of fencing." } },
      { at: "+3 min", title: "Network returns", text: "A sees B's higher epoch, stays read-only, and demotes itself to standby. It keeps a copy of anything it wrote after its last sync and re-syncs from B.", a: { role: "Standby", tone: "neutral", epoch: 2, db: "Read-only replica", note: "Re-synced from B" }, b: { role: "Active", tone: "ok", epoch: 2, db: "Read / write", note: "All 4 agents connected" }, agents: ["B", "B", "B", "B"], log: "A rejoined as standby (epoch 2)" },
    ],
  },
  {
    id: "link",
    label: "Only the link between A and B fails",
    blurb: "The standby cannot reach the active server, but everyone else can.",
    frames: [
      { at: "0 s", title: "Healthy", text: "Same starting point.", a: A_UP, b: B_STBY, agents: ["A", "A", "A", "A"], log: "Standby in sync (lag 4 s, epoch 1)" },
      { at: "+0 s", title: "A and B stop seeing each other", text: "Maybe a bad cable on the standby, or a firewall rule. The agents still talk to A normally.", a: A_UP, b: { ...B_STBY, role: "Standby", tone: "warn", note: "Cannot reach A · last sync 40 s ago" }, agents: ["A", "A", "A", "A"], log: "Sync pull from A failed" },
      { at: "+15 s", title: "B asks the agents, and does not promote", text: "All 4 agents say they reach A fine. There is no quorum, so B stays standby and raises a warning instead.", a: A_UP, b: { ...B_STBY, tone: "warn", note: "Cannot reach A · not promoting" }, agents: ["A", "A", "A", "A"], log: "4 of 4 agents reach A: promotion refused. Warning raised: standby out of sync.", guard: { tone: "ok", text: "This is the case that causes split-brain in two-node setups without a witness. Here the agents are the witness." } },
      { at: "+5 min", title: "Link restored", text: "B catches up and the warning clears. Nobody had to do anything, and no data was at risk.", a: A_UP, b: B_STBY, agents: ["A", "A", "A", "A"], log: "Standby back in sync" },
    ],
  },
  {
    id: "planned",
    label: "Planned switch",
    blurb: "Maintenance on A: hand over cleanly, no data loss.",
    frames: [
      { at: "0 s", title: "Healthy", text: "You press Switch to standby on the Failover page.", a: A_UP, b: B_STBY, agents: ["A", "A", "A", "A"], log: "Standby in sync (lag 4 s, epoch 1)" },
      { at: "+1 s", title: "A pauses and sends a final copy", text: "A stops accepting changes, sends the last state to B and waits for B to confirm it has applied it.", a: { ...A_UP, role: "Paused", tone: "warn", db: "Read-only (handing over)", note: "Final copy sent" }, b: { ...B_STBY, role: "Catching up", tone: "warn", note: "Applying final copy" }, agents: ["A", "A", "A", "A"], log: "Final copy applied on B (0 changes lost)" },
      { at: "+3 s", title: "B takes over, A steps down", text: "B promotes at epoch 2 and A becomes the standby in the same step. Agents are told to move on their next heartbeat.", a: { role: "Standby", tone: "neutral", epoch: 2, db: "Read-only replica", note: "Syncing from B" }, b: { role: "Active", tone: "ok", epoch: 2, db: "Read / write", note: "Switch complete" }, agents: ["B", "B", "B", "B"], log: "Planned switch complete: A is now standby", guard: { tone: "ok", text: "Same mechanism as Move the server, but the old server stays on as a standby and the data stays in sync both ways round." } },
    ],
  },
];

function SrvCard({ name, where, ip, s }: { name: string; where: string; ip: string; s: Srv }) {
  const active = s.role === "Active";
  return (
    <Card className={active ? "ring-1 ring-primary" : s.down ? "opacity-60" : ""}>
      <div className="flex items-start justify-between p-4">
        <div>
          <div className="flex items-center gap-2 font-medium">
            {active ? <Crown className="size-4 text-primary" /> : s.role === "Fenced" ? <Lock className="size-4 text-amber-600" /> : <ShieldCheck className="size-4 text-neutral-400" />}
            {name}
            <Badge tone={s.tone}>{s.role}</Badge>
          </div>
          <p className="mt-0.5 text-xs text-neutral-500">{where} · <span className="font-mono">{ip}</span></p>
        </div>
        <span className="rounded-md bg-neutral-100 px-2 py-0.5 font-mono text-xs">epoch {s.epoch}</span>
      </div>
      <div className="grid grid-cols-2 gap-4 border-t border-neutral-100 p-4 text-sm">
        <div><p className="text-xs text-neutral-500">Database</p><p>{s.db}</p></div>
        <div><p className="text-xs text-neutral-500">Status</p><p>{s.note}</p></div>
      </div>
    </Card>
  );
}

function Simulator() {
  const [sid, setSid] = useState("crash");
  const [i, setI] = useState(0);
  const [auto, setAuto] = useState(false);
  const sc = scenarios.find((s) => s.id === sid)!;
  const f = sc.frames[i];
  const last = i === sc.frames.length - 1;
  useEffect(() => {
    if (!auto || last) { if (last) setAuto(false); return; }
    const t = setTimeout(() => setI((n) => n + 1), 2600);
    return () => clearTimeout(t);
  }, [auto, i, last]);
  const pick = (id: string) => { setSid(id); setI(0); setAuto(false); };
  return (
    <>
      <div className="mb-4 grid gap-3 md:grid-cols-4">
        {scenarios.map((s) => (
          <button key={s.id} onClick={() => pick(s.id)} className={`rounded-xl border p-3 text-left text-sm ${s.id === sid ? "border-primary bg-blue-50" : "border-neutral-200 bg-white hover:border-neutral-300"}`}>
            <p className="font-medium">{s.label}</p>
            <p className="mt-1 text-xs text-neutral-600">{s.blurb}</p>
          </button>
        ))}
      </div>
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <Button variant="primary" onClick={() => setAuto(!auto)} icon={auto ? <Pause className="size-4" /> : <Play className="size-4" />}>{auto ? "Pause" : last ? "Play again" : "Play"}</Button>
        <Button onClick={() => { if (last) setI(0); else setI(i + 1); }} icon={<SkipForward className="size-4" />}>{last ? "Restart" : "Next step"}</Button>
        <Button onClick={() => { setI(0); setAuto(false); }} icon={<RotateCcw className="size-4" />}>Reset</Button>
        <div className="ml-auto flex items-center gap-1.5">
          {sc.frames.map((_, n) => <button key={n} aria-label={`Step ${n + 1}`} onClick={() => { setI(n); setAuto(false); }} className={`h-2 w-6 rounded-full ${n === i ? "bg-primary" : n < i ? "bg-blue-200" : "bg-neutral-200"}`} />)}
        </div>
      </div>
      <Card className="mb-4">
        <div className="flex items-start gap-3 p-4">
          <span className="mt-0.5 whitespace-nowrap rounded-md bg-neutral-900 px-2 py-0.5 font-mono text-xs text-white">{f.at}</span>
          <div><p className="font-medium">{f.title}</p><p className="mt-1 text-sm text-neutral-600">{f.text}</p></div>
        </div>
        {f.guard && (
          <div className={`mx-4 mb-4 flex gap-2 rounded-lg border px-3 py-2 text-sm ${f.guard.tone === "ok" ? "border-green-200 bg-green-50 text-green-900" : "border-blue-200 bg-blue-50 text-blue-900"}`}>
            <ShieldCheck className="mt-0.5 size-4 shrink-0" /><span><strong>Safeguard.</strong> {f.guard.text}</span>
          </div>
        )}
      </Card>
      <div className="grid gap-4 md:grid-cols-2">
        <SrvCard name="Server A" where="claude" ip="192.168.0.184" s={f.a} />
        <SrvCard name="Server B" where="cm4" ip="192.168.0.189" s={f.b} />
      </div>
      <Card className="mt-4">
        <CardHeader title="Agents" subtitle="Each agent keeps both addresses and talks to whichever one answers with the highest epoch." />
        <ul className="divide-y divide-neutral-100">
          {agentNames.map((n, k) => (
            <li key={n} className="flex items-center justify-between px-5 py-2.5 text-sm">
              <span className="flex items-center gap-2"><StatusDot status={f.agents[k] === "-" ? "offline" : "online"} />{n}</span>
              <span className="flex items-center gap-1.5 text-neutral-600">
                {f.agents[k] === "-" ? (k === 0 && f.a.down ? "Down with server A" : "Retrying fallback address") : <><ArrowRight className="size-3.5" />Server {f.agents[k]}</>}
              </span>
            </li>
          ))}
        </ul>
      </Card>
      <Card className="mt-4">
        <CardHeader title="Activity" />
        <ol className="divide-y divide-neutral-100 text-sm">
          {sc.frames.slice(0, i + 1).map((x, n) => (
            <li key={n} className="flex gap-3 px-5 py-2"><span className="w-14 shrink-0 font-mono text-xs text-neutral-500">{x.at}</span><span>{x.log}</span></li>
          ))}
        </ol>
      </Card>
    </>
  );
}

function SyncTab() {
  return (
    <>
      <div className="mb-4 grid gap-4 md:grid-cols-2">
        <SrvCard name="Server A" where="claude" ip="192.168.0.184" s={A_UP} />
        <SrvCard name="Server B" where="cm4" ip="192.168.0.189" s={B_STBY} />
      </div>
      <Card className="mb-4">
        <CardHeader title="Standby health" subtitle="What the standby holds, and whether it could take over right now." action={<Badge tone="ok">Ready to take over</Badge>} />
        <div className="grid gap-4 p-5 text-sm sm:grid-cols-4">
          <div><p className="text-xs text-neutral-500">Last sync</p><p>4 s ago</p></div>
          <div><p className="text-xs text-neutral-500">Copy size</p><p>1.2 MB (gzip 340 KB)</p></div>
          <div><p className="text-xs text-neutral-500">Secret key</p><p className="flex items-center gap-1"><Check className="size-4 text-ok" />Held, sealed in transit</p></div>
          <div><p className="text-xs text-neutral-500">Verified</p><p className="flex items-center gap-1"><Check className="size-4 text-ok" />Opens and passes integrity check</p></div>
          <div><p className="text-xs text-neutral-500">Same server image</p><p className="flex items-center gap-1"><Check className="size-4 text-ok" />v1.14.2 on both</p></div>
          <div><p className="text-xs text-neutral-500">Agents knowing both addresses</p><p className="flex items-center gap-1"><Check className="size-4 text-ok" />4 of 4</p></div>
          <div><p className="text-xs text-neutral-500">Agents able to reach B now</p><p className="flex items-center gap-1"><Check className="size-4 text-ok" />4 of 4</p></div>
          <div><p className="text-xs text-neutral-500">Reachable witnesses (excluding A and B)</p><p className="flex items-center gap-1"><AlertTriangle className="size-4 text-amber-600" />2 (needs 2 for quorum)</p></div>
        </div>
      </Card>
      <Card>
        <CardHeader title="Set up a standby" subtitle="Reuses the install step from Move the server." />
        <ol className="space-y-3 p-5 text-sm">
          {["Choose a device other than the active server. It needs Docker and the current agent.", "Orchestrator installs the same server image there in standby mode. It starts read-only and takes no work.", "The active server sends a verified copy of its data and the secret key, sealed with a one-off password, then keeps it fresh every 15 seconds.", "Every agent is given the standby's address as a fallback and learns the current epoch.", "The standby is marked Ready once a copy has been verified and a majority of witnesses can reach it."].map((t, n) => (
            <li key={n} className="flex gap-3"><span className="flex size-6 shrink-0 items-center justify-center rounded-full bg-neutral-100 text-xs font-medium">{n + 1}</span><span>{t}</span></li>
          ))}
        </ol>
      </Card>
    </>
  );
}

function SettingsTab() {
  const [auto, setAuto] = useState(true);
  const [s3, setS3] = useState(false);
  return (
    <Card>
      <CardHeader title="Failover settings" subtitle="Defaults are the safest automatic setup." />
      <div className="grid gap-4 p-5 sm:grid-cols-2">
        <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2">
          <span><span className="font-medium">Promote automatically</span><br /><span className="text-xs text-neutral-500">Off means the standby only ever takes over when you press the button.</span></span>
          <Toggle checked={auto} onChange={setAuto} label="Auto" />
        </div>
        <Field label="Treat the active server as lost after"><select className={inputClass} defaultValue="15"><option value="15">15 seconds</option><option value="30">30 seconds</option><option value="60">1 minute</option><option value="300">5 minutes</option></select></Field>
        <Field label="Write lease (active server stops writing if it cannot renew)"><select className={inputClass} defaultValue="10" disabled><option value="10">10 seconds (always shorter than the line above)</option></select></Field>
        <Field label="Quorum"><select className={inputClass} defaultValue="majority"><option value="majority">Majority of agents on other devices</option><option value="all">All agents on other devices</option></select></Field>
        <Field label="Minimum witnesses needed to promote"><select className={inputClass} defaultValue="2"><option value="2">2</option><option value="3">3</option></select></Field>
        <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2">
          <span><span className="font-medium">Extra tie-breaker: lease file in S3 / R2</span><br /><span className="text-xs text-neutral-500">Optional second opinion for fleets with fewer than 3 other devices. Uses the same bucket as backups, with a conditional write.</span></span>
          <Toggle checked={s3} onChange={setS3} label="S3" />
        </div>
        <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2">
          <span><span className="font-medium">Keep a copy of data written after the last sync</span><br /><span className="text-xs text-neutral-500">When the old active server rejoins, its unsynced tail is saved as a file you can inspect, then discarded from the live database.</span></span>
          <Toggle checked onChange={() => {}} label="Keep" />
        </div>
        <div className="flex items-center justify-between rounded-lg border border-neutral-200 px-3 py-2 text-sm sm:col-span-2">
          <span><span className="font-medium">Return the active role to the original server when it rejoins</span><br /><span className="text-xs text-neutral-500">Off by default. Switching back is a planned switch you choose.</span></span>
          <Toggle checked={false} onChange={() => {}} label="Failback" />
        </div>
        <div className="sm:col-span-2"><Button variant="primary">Save</Button></div>
      </div>
    </Card>
  );
}

function DesignTab() {
  const rows: [string, string, string][] = [
    ["Agents as witnesses (recommended)", "The standby promotes only if a majority of agents on other devices cannot reach the active server and can reach the standby. The active server stops writing if a majority cannot renew its lease.", "Needs at least 3 devices in total. Offline fleets work. Fails safe: with too few witnesses it never promotes and tells you."],
    ["Lease file in S3 / R2", "A small object updated with a conditional write. Whoever holds it is active.", "Works with two devices. Needs internet and a bucket. A bucket outage freezes failover, not the servers."],
    ["Manual only", "Standby never promotes by itself. You press Promote.", "No risk of surprise. Slowest. Still always available as the override."],
    ["Fully automatic on one opinion", "Standby promotes when it cannot reach the active server.", "Not offered: a broken cable on the standby would create two active servers."],
  ];
  return (
    <>
      <Card className="mb-4">
        <CardHeader title="Recommended approach" subtitle="Full write-up: docs/FAILOVER.md" />
        <ul className="space-y-2 p-5 text-sm">
          {["Sync: a verified, sealed copy of the database and secret key every 15 seconds. A failover loses at most that interval of changes (recent deploy history, not running containers). A planned switch loses nothing.", "Finding the active server: agents already keep several addresses. Add an epoch: agents obey only the highest epoch they have seen, so a stale server is ignored even if it is still running.", "No split-brain: a write lease on the active server, plus quorum from the agents before promotion, plus epoch fencing on every agent. Any one failing is covered by the other two.", "Rejoining: a server that finds a higher epoch demotes itself, saves any unsynced tail as a file, and re-copies from the new active server.", "Everything builds on Move the server: the same server.install step, the same identity check, the same agent redirect."].map((t, n) => (
            <li key={n} className="flex gap-2"><Check className="mt-0.5 size-4 shrink-0 text-ok" /><span>{t}</span></li>
          ))}
        </ul>
      </Card>
      <Card>
        <CardHeader title="Who decides which server is active?" subtitle="Two servers cannot safely decide alone." />
        <div className="grid gap-4 p-5 text-sm md:grid-cols-2">
          {rows.map(([t, how, tr], n) => (
            <div key={t} className={`rounded-xl border p-4 ${n === 0 ? "border-primary bg-blue-50" : "border-neutral-200"}`}>
              <p className="font-medium">{t}</p>
              <p className="mt-1 text-xs text-neutral-600">{how}</p>
              <p className="mt-2 text-xs text-neutral-500"><strong>Trade-off:</strong> {tr}</p>
            </div>
          ))}
        </div>
      </Card>
    </>
  );
}

export default function FailoverDemoPage() {
  useCrumbs([{ label: "Server failover (demo)" }]);
  const [tab, setTab] = useState("sim");
  const tabs: [string, string][] = [["sim", "Failover simulator"], ["sync", "Standby and sync"], ["settings", "Settings"], ["design", "Design"]];
  return (
    <>
      <PageHeader title="Server failover" description="A standby server that keeps a synced copy and takes over safely if the active one fails." actions={<Button icon={<Server className="size-4" />}>Set up standby</Button>} />
      <div className="mb-6 rounded-xl border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-900">
        <Zap className="mr-1 inline size-4" /><strong>Demo only.</strong> Failover is not a working feature. This page plays scripted scenarios with sample data to show how it could behave. Pick a scenario and press Play.
      </div>
      <div className="mb-6 flex flex-wrap gap-1 border-b border-neutral-200">
        {tabs.map(([k, l]) => (
          <button key={k} onClick={() => setTab(k)} className={`-mb-px border-b-2 px-4 py-2 text-sm ${tab === k ? "border-primary font-medium text-primary" : "border-transparent text-neutral-500 hover:text-neutral-800"}`}>{l}</button>
        ))}
      </div>
      {tab === "sim" && <Simulator />}
      {tab === "sync" && <SyncTab />}
      {tab === "settings" && <SettingsTab />}
      {tab === "design" && <DesignTab />}
    </>
  );
}
