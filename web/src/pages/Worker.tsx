import { useEffect, useRef, useState } from "react";
import { Link, Route, Routes, useNavigate, useParams } from "react-router";
import { Archive, Check, Copy, Download, GitBranch, MoreHorizontal, Play, RefreshCw, RotateCcw, RotateCw, Square, Trash2 } from "lucide-react";
import { api, type Backup, type BackupPolicy, type Deployment, type FleetContainer } from "../api";
import { useCrumbs } from "../components/Layout";
import DomainsPanel from "../components/Domains";
import { endpoints, waitFor } from "../components/ContainerTable";
import { Badge, Button, Card, CardHeader, Empty, ErrorNote, Field, Loading, PageHeader, StatusDot, StatusIcon, Table, Tabs, Td, inputClass } from "../components/ui";
import { ago, bytes, cx, duration, pct, span, usePoll, useTick } from "../lib";
import { deploymentTitle } from "./Projects";

const days = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

export default function WorkerPage() {
  const { deviceId = "", name = "" } = useParams();
  const fleet = usePoll(api.containers, 5000);
  const device = usePoll(() => api.device(deviceId), 10000, [deviceId]);
  const w = fleet.data?.find((c) => c.device_id === deviceId && c.name === name);
  useCrumbs([{ label: "Workers", to: "/containers" }, { label: name }]);

  if (fleet.error && !fleet.data) return <ErrorNote>{fleet.error}</ErrorNote>;
  if (!fleet.data) return <Loading />;
  if (!w) return <Empty title="Worker not found">No worker named {name} is reported by that device right now.</Empty>;

  const base = `/workers/${deviceId}/${encodeURIComponent(name)}`;
  return (
    <>
      <PageHeader
        back={{ to: "/containers", label: "Workers" }}
        title={
          <span className="flex items-center gap-3">
            <StatusDot status={w.state} />
            {w.name}
          </span>
        }
        description={
          <>
            on <Link to={`/devices/${w.device_id}`} className="text-primary hover:underline">{w.device_name}</Link> · {w.status}
          </>
        }
        actions={<WorkerActions w={w} onDone={fleet.reload} />}
      />
      <div className="mb-6">
        <Tabs
          items={[
            { to: base, label: "Overview", end: true },
            { to: `${base}/deployments`, label: "Deployments" },
            { to: `${base}/domains`, label: "Domains" },
            { to: `${base}/backups`, label: "Backups" },
          ]}
        />
      </div>
      <Routes>
        <Route index element={<Overview w={w} addresses={device.data?.host.addresses ?? w.device_addresses} />} />
        <Route path="deployments" element={<Deployments w={w} />} />
        <Route
          path="domains"
          element={
            w.project_id ? (
              <DomainsPanel projectId={w.project_id} />
            ) : (
              <Card>
                <Empty icon={<GitBranch className="size-8" />} title="Not deployed by Orchestrator">
                  Domains are available for workers created from a project.
                </Empty>
              </Card>
            )
          }
        />
        <Route path="backups" element={<Backups w={w} canBackup={!!device.data?.host.capabilities?.includes("backup")} deviceLoaded={!!device.data} />} />
      </Routes>
    </>
  );
}

function WorkerActions({ w, onDone }: { w: FleetContainer; onDone: () => void }) {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const run = async (action: string) => {
    setBusy(action);
    setError(null);
    try {
      const r = await waitFor((await api.command(w.device_id, action, w.id)).id);
      if (r.status === "failed") setError(r.error ?? "Failed");
      onDone();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };
  const running = w.state === "running";
  return (
    <>
      {error && <span className="text-sm text-bad">{error}</span>}
      {running ? (
        <Button loading={busy === "container.stop"} icon={<Square className="size-4" />} onClick={() => run("container.stop")}>
          Stop
        </Button>
      ) : (
        <Button loading={busy === "container.start"} icon={<Play className="size-4" />} onClick={() => run("container.start")}>
          Start
        </Button>
      )}
      <Button loading={busy === "container.restart"} icon={<RotateCw className="size-4" />} onClick={() => run("container.restart")}>
        Restart
      </Button>
    </>
  );
}

function Overview({ w, addresses }: { w: FleetContainer; addresses: string[] }) {
  const eps = w.state === "running" ? endpoints(w, addresses) : [];
  const ports = [...new Set((w.ports ?? []).map((p) => (p.public_port ? `${p.public_port}→${p.private_port}/${p.type}` : `${p.private_port}/${p.type}`)))];
  const rows: [string, React.ReactNode][] = [
    ["State", w.status],
    ["Image", <span className="font-mono text-xs">{w.image}</span>],
    ["Device", <Link to={`/devices/${w.device_id}`} className="text-primary hover:underline">{w.device_name}</Link>],
    ["Addresses", <span className="font-mono text-xs">{addresses.join(", ") || "none reported"}</span>],
    ["Ports", <span className="font-mono text-xs">{ports.join(", ") || "none"}</span>],
    [
      "Endpoints",
      eps.length ? eps.map((e) => (
        <a key={e} href={`http://${e}`} target="_blank" rel="noreferrer" className="block font-mono text-xs text-primary hover:underline">{e}</a>
      )) : "none",
    ],
    [
      "Project",
      w.project_id ? <Link to={`/projects/${w.project_id}`} className="text-primary hover:underline">{w.project_name}</Link> : w.compose_project ? `Compose stack ${w.compose_project}` : "Standalone container",
    ],
  ];
  return (
    <Card>
      <dl className="divide-y divide-neutral-100">
        {rows.map(([k, v]) => (
          <div key={k} className="grid gap-1 px-5 py-3 text-sm sm:grid-cols-[10rem_1fr]">
            <dt className="text-neutral-500">{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
    </Card>
  );
}

// --- Deployments, laid out like Cloudflare's deployment list ---

function Sparkline({ values, color = "#0051c3" }: { values: number[]; color?: string }) {
  if (values.length < 2) return <div className="h-8 w-full rounded bg-neutral-100" />;
  const max = Math.max(1, ...values), w = 200, h = 32;
  const pts = values.map((v, i) => `${(i / (values.length - 1)) * w},${h - 2 - (v / max) * (h - 4)}`);
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="h-8 w-full">
      <defs>
        <linearGradient id="spark" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor={color} stopOpacity="0.3" />
          <stop offset="1" stopColor={color} stopOpacity="0" />
        </linearGradient>
      </defs>
      <polygon points={`0,${h} ${pts.join(" ")} ${w},${h}`} fill="url(#spark)" />
      <polyline points={pts.join(" ")} fill="none" stroke={color} strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

function versionId(d: Deployment) {
  return d.commit ? d.commit.sha.slice(0, 8) : d.id.replace(/^[a-z]+_/, "").slice(0, 8);
}

function Deployments({ w }: { w: FleetContainer }) {
  const projectId = w.project_id;
  const deps = usePoll(() => (projectId ? api.projectDeployments(projectId) : Promise.resolve([] as Deployment[])), 4000, [projectId]);
  const metrics = usePoll(() => api.deviceMetrics(w.device_id), 15000, [w.device_id]);
  const device = usePoll(() => api.device(w.device_id), 10000, [w.device_id]);
  useTick(1000);
  if (!projectId) {
    return (
      <Card>
        <Empty icon={<GitBranch className="size-8" />} title="Not deployed by Orchestrator">
          Deployments appear here for workers created from a project. This one was started outside Orchestrator.
        </Empty>
      </Card>
    );
  }
  if (!deps.data) return <Loading />;
  const active = deps.data.find((d) => d.status === "succeeded");
  const samples = metrics.data ?? [];
  const m = device.data?.metrics;
  return (
    <div className="space-y-4">
      <Card>
        <div className="border-b border-neutral-200 px-5 py-3 text-sm font-medium text-neutral-600">Active deployment</div>
        {active ? (
          <dl className="grid gap-5 p-5 text-sm sm:grid-cols-2 lg:grid-cols-5">
            <div>
              <dt className="text-neutral-500">Version ID</dt>
              <dd className="mt-1 font-mono text-base">{versionId(active)}</dd>
            </div>
            <div>
              <dt className="text-neutral-500">Deployed</dt>
              <dd className="mt-1 text-base">{ago(active.finished_at ?? active.created_at)}</dd>
            </div>
            <div>
              <dt className="text-neutral-500">Status</dt>
              <dd className="mt-1 text-base">{w.state === "running" ? <span className="text-ok">Running</span> : <span className="text-bad">{w.status}</span>}</dd>
              <p className="text-xs text-neutral-500">{w.state === "running" ? w.status : ""}</p>
            </div>
            <div>
              <dt className="text-neutral-500">Device CPU{m ? ` · ${m.cpu_percent.toFixed(0)}%` : ""}</dt>
              <dd className="mt-1"><Sparkline values={samples.map((s) => s.cpu)} /></dd>
            </div>
            <div>
              <dt className="text-neutral-500">Device memory{m ? ` · ${pct(m.memory_used, m.memory_total)}% of ${bytes(m.memory_total)}` : ""}</dt>
              <dd className="mt-1"><Sparkline values={samples.map((s) => s.mem)} color="#f6821f" /></dd>
            </div>
          </dl>
        ) : (
          <p className="p-5 text-sm text-neutral-500">Nothing has deployed successfully yet.</p>
        )}
      </Card>

      <div className="flex items-center justify-between">
        <Link to={`/projects/${projectId}/deployments`}>
          <Button>View rollout history</Button>
        </Link>
        <Button aria-label="Refresh" icon={<RefreshCw className="size-4" />} onClick={deps.reload} />
      </div>

      <Card>
        {deps.data.length === 0 ? (
          <Empty title="No deployments yet" />
        ) : (
          <div className="divide-y divide-neutral-100">
            {deps.data.map((d) => (
              <DeploymentLine key={d.id} d={d} live={active?.id === d.id} onChanged={deps.reload} />
            ))}
          </div>
        )}
      </Card>
    </div>
  );
}

function DeploymentLine({ d, live, onChanged }: { d: Deployment; live: boolean; onChanged: () => void }) {
  const nav = useNavigate();
  const [copied, setCopied] = useState(false);
  const menu = useRef<HTMLDetailsElement>(null);
  const took = span(d.started_at ?? d.created_at, d.finished_at);
  const hash = d.commit?.sha;
  useEffect(() => {
    const close = (e: MouseEvent) => {
      if (menu.current && !menu.current.contains(e.target as Node)) menu.current.open = false;
    };
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, []);
  const copy = async () => {
    if (!hash) return;
    try {
      await navigator.clipboard.writeText(hash);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard blocked */
    }
  };
  const label = { queued: "Queued", running: "Building", succeeded: "Ready", failed: "Failed" }[d.status];
  return (
    <div className="grid grid-cols-[7rem_1fr_auto] items-center gap-x-4 gap-y-2 px-5 py-3 text-sm lg:grid-cols-[7rem_minmax(0,1fr)_9rem_6rem_7rem_9rem_5rem_2rem]">
      <div className="flex items-center gap-2 font-mono text-neutral-500">
        {hash ? (
          <>
            <span>{hash.slice(0, 8)}</span>
            <button onClick={copy} aria-label="Copy commit hash" className="rounded p-0.5 hover:bg-neutral-100">
              {copied ? <Check className="size-3.5 text-ok" /> : <Copy className="size-3.5" />}
            </button>
          </>
        ) : (
          "—"
        )}
      </div>
      <Link to={`/deployments/${d.id}`} className="min-w-0 truncate hover:underline" title={deploymentTitle(d)}>
        {deploymentTitle(d)}
      </Link>
      <div className="flex items-center gap-2 whitespace-nowrap">
        {live ? <span className="h-4 w-1 rounded bg-primary" title="Active deployment" /> : <StatusIcon status={d.status} size="sm" />}
        <span className={cx(d.status === "failed" && "text-bad")}>{label}</span>
        {d.status !== "queued" && <span className="text-neutral-500">{duration(took)}</span>}
      </div>
      <Link to={`/deployments/${d.id}`} className="hidden w-fit rounded-full border border-neutral-300 px-3 py-1 text-xs hover:bg-neutral-50 lg:block">
        Build ↗
      </Link>
      <span className="hidden w-fit items-center gap-1 rounded-full border border-neutral-300 px-3 py-1 text-xs lg:inline-flex">
        <GitBranch className="size-3" />
        {d.commit?.branch ?? "image"}
      </span>
      <span className="hidden truncate text-neutral-600 lg:block">{d.commit?.author ? `by ${d.commit.author}` : d.trigger === "manual" ? "by you" : ""}</span>
      <span className="hidden whitespace-nowrap text-neutral-500 lg:block">{ago(d.created_at)}</span>
      <details ref={menu} className="relative">
        <summary className="flex cursor-pointer list-none justify-end rounded p-1 hover:bg-neutral-100" aria-label="Actions">
          <MoreHorizontal className="size-4" />
        </summary>
        <div className="absolute right-0 z-10 mt-1 w-48 rounded-lg border border-neutral-200 bg-white py-1 text-sm shadow-lg">
          <Link to={`/deployments/${d.id}`} className="block px-3 py-1.5 hover:bg-neutral-50">View build log</Link>
          {hash && <button onClick={copy} className="block w-full px-3 py-1.5 text-left hover:bg-neutral-50">Copy commit hash</button>}
          {d.commit?.url && <a href={d.commit.url} target="_blank" rel="noreferrer" className="block px-3 py-1.5 hover:bg-neutral-50">Open commit on GitHub</a>}
          <button
            className="block w-full px-3 py-1.5 text-left hover:bg-neutral-50"
            onClick={async () => {
              const r = await api.retryDeployment(d.id);
              onChanged();
              nav(`/deployments/${r.id}`);
            }}
          >
            Redeploy this version
          </button>
        </div>
      </details>
    </div>
  );
}

// --- Backups ---

function Backups({ w, canBackup, deviceLoaded }: { w: FleetContainer; canBackup: boolean; deviceLoaded: boolean }) {
  const policy = usePoll(() => api.backupPolicy(w.device_id, w.name), 0, [w.device_id, w.name]);
  const backups = usePoll(() => api.backups().then((all) => all.filter((b) => b.device_id === w.device_id && b.container_name === w.name)), 3000, [w.device_id, w.name]);
  const [p, setP] = useState<BackupPolicy | null>(null);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  useEffect(() => {
    if (policy.data && !p) {
      const tz = policy.data.mode === "off" && policy.data.tz === "UTC" ? Intl.DateTimeFormat().resolvedOptions().timeZone : policy.data.tz;
      setP({ ...policy.data, tz });
    }
  }, [policy.data, p]);
  const set = (x: Partial<BackupPolicy>) => {
    setSaved(false);
    setP({ ...(p as BackupPolicy), ...x });
  };
  const fail = (e: unknown) => setError(e instanceof Error ? e.message : String(e));

  const save = async () => {
    if (!p) return;
    setError(null);
    try {
      const { next_run: _n, last_run: _l, ...rest } = p;
      void _n; void _l;
      setP(await api.setBackupPolicy(w.device_id, w.name, rest));
      setSaved(true);
    } catch (e) {
      fail(e);
    }
  };
  const act = async (b: Backup, kind: "download" | "restore" | "delete") => {
    if (kind === "restore" && !window.confirm(`Restore ${b.container_name} from this backup? Files in the backup overwrite the current data, and the container restarts.`)) return;
    if (kind === "delete" && !window.confirm("Delete this backup?")) return;
    setBusy(b.id + kind);
    setError(null);
    try {
      if (kind === "download") await api.downloadBackup(b);
      else if (kind === "delete") await api.deleteBackup(b.id);
      else await api.restoreBackup(b.id);
      backups.reload();
    } catch (e) {
      fail(e);
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="space-y-4">
      {deviceLoaded && !canBackup && (
        <p className="rounded-lg bg-amber-50 px-4 py-3 text-sm">
          The agent on {w.device_name} is older than this server and can't back up workers. Update it from the <Link to="/updates" className="font-medium underline">Updates page</Link>, or if it's too old for that, re-run the install command from Devices &gt; Add device on that device.
        </p>
      )}
      {error && <ErrorNote>{error}</ErrorNote>}
      <Card>
        <CardHeader title="Automatic backups" subtitle="Back this worker up on a schedule and keep the newest few. Manual backups are never deleted automatically." />
        {!p ? (
          <Loading />
        ) : (
          <div className="space-y-4 p-5">
            <div className="grid gap-4 sm:grid-cols-5">
              <Field label="Back up">
                <select className={inputClass} value={p.mode} onChange={(e) => set({ mode: e.target.value as BackupPolicy["mode"] })}>
                  <option value="off">Never (manual only)</option>
                  <option value="daily">Every day</option>
                  <option value="weekly">Every week</option>
                </select>
              </Field>
              {p.mode === "weekly" && (
                <Field label="Day">
                  <select className={inputClass} value={p.weekday} onChange={(e) => set({ weekday: Number(e.target.value) })}>
                    {days.map((d, i) => <option key={d} value={i}>{d}</option>)}
                  </select>
                </Field>
              )}
              {p.mode !== "off" && (
                <>
                  <Field label="At">
                    <input type="time" className={inputClass} value={p.time} onChange={(e) => set({ time: e.target.value })} />
                  </Field>
                  <Field label="Time zone">
                    <input className={inputClass} value={p.tz} onChange={(e) => set({ tz: e.target.value })} />
                  </Field>
                  <Field label="Keep">
                    <input type="number" min={1} max={100} className={inputClass} value={p.keep} onChange={(e) => set({ keep: Number(e.target.value) })} />
                  </Field>
                </>
              )}
            </div>
            {p.mode !== "off" && p.next_run && <p className="text-sm text-neutral-600">Next backup: {new Date(p.next_run).toLocaleString()}{p.last_run ? ` · last run ${ago(p.last_run)}` : ""}</p>}
            <div className="flex items-center gap-3">
              <Button variant="primary" onClick={save}>Save</Button>
              {saved && <span className="text-sm text-neutral-600">Saved</span>}
              <Button
                className="ml-auto"
                icon={<Archive className="size-4" />}
                loading={busy === "now"}
                onClick={async () => {
                  setBusy("now");
                  setError(null);
                  try {
                    await api.createBackup(w.device_id, w.id);
                    backups.reload();
                  } catch (e) {
                    fail(e);
                  } finally {
                    setBusy(null);
                  }
                }}
              >
                Back up now
              </Button>
            </div>
          </div>
        )}
      </Card>
      <Card>
        <CardHeader title="Backups of this worker" />
        {!backups.data ? (
          <Loading />
        ) : backups.data.length === 0 ? (
          <Empty icon={<Archive className="size-8" />} title="No backups yet" />
        ) : (
          <Table head={["Taken", "Type", "Status", "Size", ""]}>
            {backups.data.map((b) => (
              <tr key={b.id} className="hover:bg-neutral-50">
                <Td className="whitespace-nowrap">{ago(b.created_at)}</Td>
                <Td><Badge>{b.source === "scheduled" ? "Scheduled" : "Manual"}</Badge></Td>
                <Td>
                  <Badge tone={{ pending: "info", complete: "ok", failed: "bad" }[b.status] as "info" | "ok" | "bad"}>{b.status === "pending" ? "In progress" : b.status === "complete" ? "Complete" : "Failed"}</Badge>
                  {b.error && <span className="mt-1 block max-w-64 text-xs text-bad">{b.error}</span>}
                </Td>
                <Td className="whitespace-nowrap text-neutral-600">{b.status === "complete" ? bytes(b.size) : "—"}</Td>
                <Td>
                  <div className="flex justify-end gap-1">
                    {b.status === "complete" && (
                      <>
                        <Button size="sm" variant="ghost" title="Download" loading={busy === b.id + "download"} icon={<Download className="size-3.5" />} onClick={() => act(b, "download")} />
                        <Button size="sm" variant="ghost" title="Restore" loading={busy === b.id + "restore"} icon={<RotateCcw className="size-3.5" />} onClick={() => act(b, "restore")} />
                      </>
                    )}
                    <Button size="sm" variant="ghost" title="Delete" loading={busy === b.id + "delete"} icon={<Trash2 className="size-3.5" />} onClick={() => act(b, "delete")} />
                  </div>
                </Td>
              </tr>
            ))}
          </Table>
        )}
      </Card>
    </div>
  );
}
