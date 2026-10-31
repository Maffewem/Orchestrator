import { useEffect, useState } from "react";
import { useCan } from "../components/auth";
import { Shell } from "../components/Streams";
import { Link, Route, Routes, useNavigate, useParams } from "react-router";
import { ArrowUpRight, CloudUpload, Loader2, PackagePlus, Rocket, SquareTerminal, Trash2 } from "lucide-react";
import { api, ApiError, type Device, type Migration, type Project } from "../api";
import { useCrumbs } from "../components/Layout";
import { Button, Card, CardHeader, Empty, Field, Toggle, inputClass, ErrorNote, LineChart, Loading, StatusDot, StatusIcon, Tabs, deploymentLabel } from "../components/ui";
import { CommitChip, DeploymentRow, SourceLabel, deploymentTitle } from "./Projects";
import DomainsPanel from "../components/Domains";
import PreviewsPanel from "../components/Previews";
import TunnelPanel from "../components/ProjectTunnel";
import PowerControls from "../components/PowerControls";
import ProjectForm from "./ProjectForm";
import { ago, freeSpace, usePoll } from "../lib";
import { confirmDialog, confirmWithOption } from "../components/dialogs";

/** URL of the first published port on the device, if we know an address for it. */
export function visitUrl(p: Project, device?: Device | null): string | null {
  const host = device?.host.addresses?.[0];
  const mapping = p.runtime.ports.find((x) => x.includes(":") && !x.endsWith("/udp"));
  if (!host || !mapping) return null;
  return `http://${host}:${mapping.split(":")[0]}`;
}

/** Start, stop and remove all containers of a Compose stack. Redeploying brings a removed stack back. */
function StackControls({ p, onError }: { p: Project; onError: (e: string | null) => void }) {
  const [busy, setBusy] = useState<string | null>(null);
  const canOperate = useCan("operator");
  const run = async (action: "start" | "stop" | "down") => {
    let volumes = false;
    if (action === "down") {
      const res = await confirmWithOption({
        title: `Remove stack ${p.name}?`,
        message: "Its containers and networks are removed. The project stays, and New deployment brings the stack back.",
        confirmLabel: "Remove stack",
        danger: true,
        option: { label: "Also delete its volumes (data is lost)", checked: false },
      });
      if (res === null) return;
      volumes = res;
    }
    setBusy(action);
    onError(null);
    try {
      await api.stackAction(p.id, action, volumes);
    } catch (e) {
      onError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };
  if (!canOperate) return null;
  return (
    <>
      <Button onClick={() => run("start")} loading={busy === "start"}>Start</Button>
      <Button onClick={() => run("stop")} loading={busy === "stop"}>Stop</Button>
      <Button onClick={() => run("down")} loading={busy === "down"} icon={<Trash2 className="size-4" />}>Remove</Button>
    </>
  );
}

export default function ProjectPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const project = usePoll(() => api.project(id), 4000, [id]);
  const p = project.data;
  const device = usePoll(() => (p ? api.device(p.device_id) : Promise.resolve(null)), 10000, [p?.device_id]);
  const [deploying, setDeploying] = useState(false);
  const [shell, setShell] = useState(false);
  const isAdmin = useCan("admin");
  const canOperate = useCan("operator");
  const [error, setError] = useState<string | null>(null);
  const worker = device.data?.containers.find((c) => c.name === (p?.runtime.container || p?.name));
  useCrumbs([{ label: "Projects", to: "/projects" }, { label: p?.name ?? "…" }]);

  if (project.error && !p) return <ErrorNote>{project.error}</ErrorNote>;
  if (!p) return <Loading />;
  const latest = p.latest_deployment;
  const inProgress = latest && (latest.status === "queued" || latest.status === "running");
  const url = visitUrl(p, device.data);

  const deploy = async () => {
    setDeploying(true);
    setError(null);
    try {
      let d;
      try {
        d = await api.deployProject(p.id);
      } catch (e) {
        // An adopted container is replaced by its first deployment; ask before doing that.
        if (!(e instanceof ApiError && e.status === 409 && p.runtime.adopted && (await confirmDialog({ title: `Deploying recreates ${p.name}`, message: e.message, confirmLabel: "Deploy", danger: true })))) throw e;
        d = await api.deployProject(p.id, true);
      }
      nav(`/deployments/${d.id}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setDeploying(false);
    }
  };

  const base = `/projects/${p.id}`;
  return (
    <>
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <Tabs
          items={[
            { to: base, label: "Overview", end: true },
            { to: `${base}/deployments`, label: "Deployments" },
            { to: `${base}/metrics`, label: "Metrics" },
            { to: `${base}/domains`, label: "Domains" },
            ...(p.source.type === "github" ? [{ to: `${base}/previews`, label: "Previews" }] : []),
            ...(canOperate ? [{ to: `${base}/settings`, label: "Settings" }] : []),
          ]}
        />
        <div className="flex flex-wrap items-center gap-2">
          {inProgress && (
            <Link
              to={`/deployments/${latest.id}`}
              className="inline-flex h-9 items-center gap-2 rounded-full bg-neutral-100 px-3.5 text-sm text-neutral-700 hover:bg-neutral-200"
            >
              <Loader2 className="size-3.5 animate-spin" />
              Deployment in progress ›
            </Link>
          )}
          {p.source.type === "compose" && <StackControls p={p} onError={setError} />}
          {worker && (
            <PowerControls deviceId={p.device_id} containerId={worker.id} state={worker.state} onDone={device.reload} onError={setError} />
          )}
          {worker && worker.state === "running" && isAdmin && (
            <Button icon={<SquareTerminal className="size-4" />} onClick={() => setShell(true)}>
              Shell
            </Button>
          )}
          {canOperate && (
            <Button icon={<CloudUpload className="size-4" />} onClick={deploy} loading={deploying}>
              New deployment
            </Button>
          )}
          {url && (
            <a href={url} target="_blank" rel="noreferrer">
              <Button variant="primary" icon={<ArrowUpRight className="size-4" />}>
                Visit
              </Button>
            </a>
          )}
        </div>
      </div>
      {shell && worker && <Shell deviceId={p.device_id} container={worker.id} name={worker.name} onClose={() => setShell(false)} />}
      {error && (
        <div className="mb-4">
          <ErrorNote>{error}</ErrorNote>
        </div>
      )}
      <Routes>
        <Route index element={<ProjectOverview p={p} device={device.data} url={url} />} />
        <Route path="deployments" element={<ProjectDeployments p={p} />} />
        <Route
          path="domains"
          element={
            p.source.type === "compose" ? (
              <TunnelPanel projectId={p.id} />
            ) : (
              <DomainsPanel projectId={p.id} />
            )
          }
        />
        <Route path="previews" element={<PreviewsPanel projectId={p.id} />} />
        <Route path="metrics" element={<ProjectMetrics p={p} />} />
        {canOperate && <Route path="settings" element={<ProjectSettings p={p} onSaved={project.reload} />} />}
      </Routes>
    </>
  );
}

function ProjectOverview({ p, device, url }: { p: Project; device: Device | null; url: string | null }) {
  const deps = usePoll(() => api.projectDeployments(p.id), 4000, [p.id]);
  // Production is the newest deployment that went live, as on Cloudflare Pages.
  const d = deps.data?.find((x) => x.status === "succeeded") ?? p.latest_deployment;
  const latest = p.latest_deployment;
  const container = device?.containers.find((c) => c.name === (p.runtime.container || p.name));
  const isStack = p.source.type === "compose";
  // Public hostnames that reach the worker through the device's Cloudflare Tunnel.
  const domains = usePoll(() => (isStack ? Promise.resolve(null) : api.projectDomains(p.id)), 30000, [p.id, isStack]);
  const stackTunnel = usePoll(() => (isStack ? api.projectTunnel(p.id) : Promise.resolve(null)), 30000, [p.id, isStack]);
  const publicUrls: string[] = isStack
    ? (stackTunnel.data?.routes ?? []).map((r) => `https://${r.hostname}`)
    : [
        ...(domains.data?.production?.enabled && domains.data.production.tunnel ? [`https://${domains.data.production.hostname}`] : []),
        ...(domains.data?.domains ?? []).filter((x) => x.access === "tunnel").map((x) => `https://${x.hostname}${x.path ?? ""}`),
      ];
  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{p.name}</h1>
        <p className="mt-1 text-sm text-neutral-500">
          <SourceLabel p={p} />
        </p>
      </div>
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader title="Production" subtitle="The deployment currently live on the device." />
          {d ? (
            <Link to={`/deployments/${d.id}`} className="block p-5 hover:bg-neutral-50">
              <div className="flex items-center gap-3">
                <StatusIcon status={d.status} size="lg" />
                <div className="min-w-0">
                  <p className="truncate font-medium">{deploymentTitle(d)}</p>
                  <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-neutral-500">
                    <CommitChip d={d} />
                    <span>
                      {deploymentLabel(d.status)} {ago(d.created_at)}
                    </span>
                  </div>
                </div>
              </div>
              {d.error && <p className="mt-3 text-sm text-bad">{d.error}</p>}
              {latest && latest.id !== d.id && latest.status === "failed" && (
                <p className="mt-3 text-sm text-neutral-500">
                  A newer deployment failed and was rolled back: <span className="text-bad">{latest.error}</span>
                </p>
              )}
            </Link>
          ) : p.runtime.adopted ? (
            <Empty icon={<PackagePlus className="size-8" />} title="Adopted, running as it was">
              This project took over a container that already ran on the device. Orchestrator has not changed it. A new deployment pulls{" "}
              {p.source.image} and recreates the container from these settings.
              {p.runtime.unsupported?.length ? ` Not carried over: ${p.runtime.unsupported.join(", ")}.` : ""}
            </Empty>
          ) : (
            <Empty icon={<Rocket className="size-8" />} title="Not deployed yet">
              Use New deployment to roll this project out.
            </Empty>
          )}
        </Card>
        <Card>
          <CardHeader title="Runtime" />
          <dl className="space-y-3 p-5 text-sm">
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">Device</dt>
              <dd className="flex items-center gap-1.5 truncate">
                {device && <StatusDot status={device.status} />}
                {device ? <Link to={`/devices/${device.id}`} className="text-primary hover:underline">{device.name}</Link> : p.device_name || "—"}
              </dd>
            </div>
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">Container</dt>
              <dd className="flex items-center gap-1.5">
                {container ? (
                  <>
                    <StatusDot status={container.state} />
                    {container.status}
                  </>
                ) : (
                  "Not running"
                )}
              </dd>
            </div>
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">Ports</dt>
              <dd className="truncate font-mono text-xs">{p.runtime.ports.join(", ") || "—"}</dd>
            </div>
            {publicUrls.length > 0 && (
              <div className="flex justify-between gap-3">
                <dt className="text-neutral-500">Public URL</dt>
                <dd className="flex min-w-0 flex-col items-end gap-1">
                  {publicUrls.map((u) => (
                    <a key={u} href={u} target="_blank" rel="noreferrer" className="max-w-full truncate text-primary hover:underline">
                      {u.replace("https://", "")}
                    </a>
                  ))}
                </dd>
              </div>
            )}
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">{publicUrls.length > 0 ? "Local URL" : "URL"}</dt>
              <dd className="truncate">
                {url ? (
                  <a href={url} target="_blank" rel="noreferrer" className="text-primary hover:underline">
                    {url.replace("http://", "")}
                  </a>
                ) : (
                  "—"
                )}
              </dd>
            </div>
          </dl>
        </Card>
      </div>
      <Card>
        <CardHeader title="Recent deployments" action={<Link to={`/projects/${p.id}/deployments`} className="text-sm text-primary hover:underline">View all</Link>} />
        <div className="divide-y divide-neutral-100">
          {deps.data?.slice(0, 5).map((x) => <DeploymentRow key={x.id} d={x} />)}
          {deps.data?.length === 0 && <p className="px-5 py-6 text-sm text-neutral-500">No deployments yet.</p>}
        </div>
      </Card>
    </div>
  );
}

function ProjectDeployments({ p }: { p: Project }) {
  const deps = usePoll(() => api.projectDeployments(p.id), 4000, [p.id]);
  return (
    <Card>
      <CardHeader title="Deployments" subtitle={p.source.type === "github" && p.source.auto_deploy ? `Every push to ${p.source.branch} deploys automatically.` : undefined} />
      {!deps.data ? (
        <Loading />
      ) : deps.data.length === 0 ? (
        <Empty icon={<Rocket className="size-8" />} title="No deployments yet" />
      ) : (
        <div className="divide-y divide-neutral-100">
          {deps.data.map((d) => (
            <DeploymentRow key={d.id} d={d} />
          ))}
        </div>
      )}
    </Card>
  );
}

function ProjectMetrics({ p }: { p: Project }) {
  const samples = usePoll(() => api.deviceMetrics(p.device_id), 5000, [p.device_id]);
  const deps = usePoll(() => api.projectDeployments(p.id), 15000, [p.id]);
  const s = samples.data ?? [];
  const done = deps.data?.filter((d) => d.status === "succeeded" || d.status === "failed") ?? [];
  const ok = done.filter((d) => d.status === "succeeded").length;
  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-3">
        <Card className="p-4">
          <p className="text-xs font-medium text-neutral-500">Deployments</p>
          <p className="mt-1 text-2xl font-semibold">{deps.data?.length ?? "—"}</p>
        </Card>
        <Card className="p-4">
          <p className="text-xs font-medium text-neutral-500">Success rate</p>
          <p className="mt-1 text-2xl font-semibold">{done.length ? `${Math.round((ok / done.length) * 100)}%` : "—"}</p>
        </Card>
        <Card className="p-4">
          <p className="text-xs font-medium text-neutral-500">Device</p>
          <p className="mt-1 truncate text-2xl font-semibold">{p.device_name}</p>
        </Card>
      </div>
      <Card>
        <CardHeader title="Device resources" subtitle="Live samples from the agent (last hour)." />
        <div className="p-5">
          <LineChart
            series={[
              { label: "CPU", color: "#0051c3", values: s.map((x) => x.cpu) },
              { label: "Memory", color: "#f6821f", values: s.map((x) => x.mem) },
              { label: "Disk", color: "#16794c", values: s.map((x) => x.disk) },
            ]}
          />
        </div>
      </Card>
    </div>
  );
}

function ProjectSettings({ p, onSaved }: { p: Project; onSaved: () => void }) {
  const nav = useNavigate();
  const isAdmin = useCan("admin");
  const [saved, setSaved] = useState(false);
  return (
    <div className="space-y-6">
      <Card className="p-6">
        <ProjectForm
          initial={p}
          submitLabel={saved ? "Saved" : "Save changes"}
          onSubmit={async (input, previews) => {
            await api.updateProject(p.id, input);
            if (previews) await api.setPreviewSettings(p.id, previews);
            setSaved(true);
            setTimeout(() => setSaved(false), 1500);
            onSaved();
          }}
        />
      </Card>
      <MigrateCard p={p} onDone={onSaved} />
      {isAdmin && <Card className="border-red-200">
        <CardHeader title="Delete project" subtitle="Removes the project and its deployment history. You can also remove its container from the device." />
        <div className="p-5">
          <Button
            variant="danger"
            icon={<Trash2 className="size-4" />}
            onClick={async () => {
              const remove = await confirmWithOption({
                title: `Delete ${p.name}?`,
                message: "The project and its deployment history are removed.",
                confirmLabel: "Delete",
                danger: true,
                option: { label: "Also remove the container from the device", checked: !p.runtime?.adopted },
              });
              if (remove === null) return;
              await api.deleteProject(p.id, remove);
              nav("/projects");
            }}
          >
            Delete project
          </Button>
        </div>
      </Card>}
    </div>
  );
}

/** Moves the project to another device: data is backed up, copied across and verified before the old container is touched. */
function MigrateCard({ p, onDone }: { p: Project; onDone: () => void }) {
  const devices = usePoll(api.devices, 0);
  const [target, setTarget] = useState("");
  const [withData, setWithData] = useState(true);
  const [stopSource, setStopSource] = useState(true);
  const [fromBackup, setFromBackup] = useState(false);
  const isAdmin = useCan("admin");
  const [error, setError] = useState<string | null>(null);
  const [m, setM] = useState<Migration | null>(null);
  const running = m?.status === "running";

  useEffect(() => {
    api.migration(p.id).then(setM).catch(() => {});
  }, [p.id]);
  useEffect(() => {
    if (!running) return;
    const t = setInterval(async () => {
      try {
        const next = await api.migration(p.id);
        setM(next);
        if (next.status !== "running") onDone();
      } catch {}
    }, 2000);
    return () => clearInterval(t);
  }, [running, p.id, onDone]);

  if (!isAdmin) return null;
  const isStack = p.source.type === "compose";
  const source = devices.data?.find((d) => d.id === p.device_id);
  const sourceDown = source?.status === "offline";
  const others = devices.data?.filter((d) => d.id !== p.device_id && !d.maintenance) ?? [];
  return (
    <Card>
      <CardHeader title="Move to another device" subtitle="Backs up the data, deploys on the new device, copies the data across and checks it runs before anything is stopped." />
      <div className="space-y-4 p-5">
        {error && <ErrorNote>{error}</ErrorNote>}
        <Field label="Move to">
          <select className={inputClass} value={target} onChange={(e) => setTarget(e.target.value)} disabled={running}>
            <option value="">Choose a device</option>
            {others.map((d) => (
              <option key={d.id} value={d.id}>
                {d.name} ({d.host.arch}
                {d.status === "offline" ? ", offline" : ""}
                {freeSpace(d) ? `, ${freeSpace(d)}` : ""})
              </option>
            ))}
          </select>
        </Field>
        {isStack && <p className="text-sm text-neutral-500">A stack is redeployed on the new device. Its volumes are not copied, so it starts with fresh data.</p>}
        {!isStack && <div className="flex items-center gap-3 text-sm"><Toggle checked={withData} onChange={setWithData} label="Copy volumes and data" />Copy volumes and data</div>}
        {!isStack && withData && sourceDown && (
          <div className="flex items-center gap-3 text-sm">
            <Toggle checked={fromBackup} onChange={setFromBackup} label="Use the newest backup on the server" />
            {source?.name} is offline: restore the newest backup on the server instead. The data may be out of date.
          </div>
        )}
        <div className="flex items-center gap-3 text-sm"><Toggle checked={stopSource} onChange={setStopSource} label="Stop the old container afterwards" />Stop the old container afterwards</div>
        <Button
          disabled={!target || running}
          icon={running ? <Loader2 className="size-4 animate-spin" /> : <Rocket className="size-4" />}
          onClick={async () => {
            setError(null);
            try {
              setM(await api.migrateProject(p.id, { device_id: target, with_data: withData, stop_source: stopSource, from_backup: sourceDown && fromBackup }));
            } catch (e) {
              setError(e instanceof Error ? e.message : String(e));
            }
          }}
        >
          Move project
        </Button>
        {m && (
          <div className="rounded-md bg-slate-50 p-3 font-mono text-xs dark:bg-slate-900">
            <div className="mb-1 font-sans text-sm">
              {m.from} to {m.to}: {m.status === "failed" ? "failed" : m.status === "done" ? "done" : m.step}
            </div>
            {(m.log ?? []).map((l, i) => (
              <div key={i}>{l}</div>
            ))}
          </div>
        )}
      </div>
    </Card>
  );
}
