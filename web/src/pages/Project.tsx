import { useEffect, useRef, useState, type MutableRefObject } from "react";
import { useCan } from "../components/auth";
import { Shell } from "../components/Streams";
import { Link, Route, Routes, useNavigate, useParams } from "react-router";
import {
  ArrowUpRight,
  CloudUpload,
  Loader2,
  PackagePlus,
  Rocket,
  SquareTerminal,
  Trash2,
} from "lucide-react";
import {
  api,
  ApiError,
  type Device,
  type Migration,
  type Project,
} from "../api";
import { useCrumbs } from "../components/Layout";
import {
  Button,
  Card,
  CardHeader,
  Empty,
  Field,
  Toggle,
  inputClass,
  ErrorNote,
  WarnNote,
  Loading,
  StatusDot,
  StatusIcon,
  Tabs,
  deploymentLabel,
} from "../components/ui";
import {
  CommitChip,
  DeploymentRow,
  SourceLabel,
  deploymentTitle,
} from "./Projects";
import DomainsPanel from "../components/Domains";
import PreviewsPanel from "../components/Previews";
import TunnelPanel from "../components/ProjectTunnel";
import PowerControls from "../components/PowerControls";
import ProjectForm from "./ProjectForm";
import { ReplicaBadge } from "./Cluster";
import { ProjectMetricsTab } from "../components/WorkerMetrics";
import {
  CloudflareOverview,
} from "../components/CloudflareProject";
import {
  CloudflareDomains,
  CloudflareHistory,
  CloudflarePreviews,
  CloudflareSettings,
} from "../components/CloudflareRemote";
import { ago, freeSpace, usePoll } from "../lib";
import { confirmDialog, confirmWithOption } from "../components/dialogs";

/** URL of the first published port on the device, if we know an address for it. */
export function visitUrl(p: Project, device?: Device | null): string | null {
  const host = device?.host.addresses?.[0];
  const mapping = p.runtime.ports.find(
    (x) => x.includes(":") && !x.endsWith("/udp"),
  );
  if (!host || !mapping) return null;
  return `http://${host}:${mapping.split(":")[0]}`;
}

/** Start, stop and remove all containers of a Compose stack. Redeploying brings a removed stack back. */
function StackControls({
  p,
  onError,
}: {
  p: Project;
  onError: (e: string | null) => void;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  const canOperate = useCan("operator");
  const run = async (action: "start" | "stop" | "down") => {
    let volumes = false;
    if (action === "down") {
      const res = await confirmWithOption({
        title: `Remove stack ${p.name}?`,
        message:
          "Its containers and networks are removed. The project stays, and Deploy brings the stack back.",
        confirmLabel: "Remove stack",
        danger: true,
        option: {
          label: "Also delete its volumes (data is lost)",
          checked: false,
        },
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
      <Button onClick={() => run("start")} loading={busy === "start"}>
        Start
      </Button>
      <Button onClick={() => run("stop")} loading={busy === "stop"}>
        Stop
      </Button>
      <Button
        onClick={() => run("down")}
        loading={busy === "down"}
        icon={<Trash2 className="size-4" />}
      >
        Remove
      </Button>
    </>
  );
}

export default function ProjectPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const project = usePoll(() => api.project(id), 4000, [id]);
  const p = project.data;
  const isCF = p?.source.type === "cloudflare";
  // With replicas a project runs on several devices; this picks the copy the runtime views
  // (container, power, shell, metrics) are about. Settings and deployments apply to all copies.
  const [picked, setPicked] = useState<string | null>(null);
  // Unsaved edits in the Settings form (reported by the form) and a counter that resets it.
  const formDirty = useRef(false);
  const [formKey, setFormKey] = useState(0);
  async function pickCopy(id: string) {
    if (id === picked || (picked === null && id === p?.device_id)) return;
    if (formDirty.current) {
      const ok = await confirmDialog({
        title: "Discard unsaved changes?",
        message: "You have unsaved edits in this project's settings. Switching copies reloads the saved settings and discards them.",
        confirmLabel: "Discard and switch",
      });
      if (!ok) return;
    }
    formDirty.current = false;
    setFormKey((k) => k + 1);
    setPicked(id);
  }
  const instances = p && !isCF ? [{ id: p.device_id, name: p.device_name, main: true }, ...(p.replicas ?? []).map((r) => ({ id: r.device_id, name: r.device, main: false }))] : [];
  const viewId = instances.find((i) => i.id === picked)?.id ?? p?.device_id ?? "";
  const viewName = instances.find((i) => i.id === viewId)?.name ?? "";
  const device = usePoll(
    () => (p && !isCF ? api.device(viewId) : Promise.resolve(null)),
    10000,
    [viewId, isCF],
  );
  const [deploying, setDeploying] = useState(false);
  const [shell, setShell] = useState(false);
  const isAdmin = useCan("admin");
  const canOperate = useCan("operator");
  const [error, setError] = useState<string | null>(null);
  const worker = device.data?.containers.find(
    (c) => c.name === (p?.runtime.container || p?.name),
  );
  useCrumbs([
    { label: "Projects", to: "/projects" },
    { label: p?.name ?? "…" },
  ]);

  if (project.error && !p) return <ErrorNote>{project.error}</ErrorNote>;
  if (!p) return <Loading />;
  const latest = p.latest_deployment;
  const inProgress =
    latest && (latest.status === "queued" || latest.status === "running");
  const url = isCF
    ? (p.cloudflare?.url ?? latest?.url ?? null)
    : visitUrl(p, device.data);

  const deploy = async () => {
    setDeploying(true);
    setError(null);
    try {
      let d;
      try {
        d = await api.deployProject(p.id);
      } catch (e) {
        // An adopted container is replaced by its first deployment; ask before doing that.
        if (
          !(
            e instanceof ApiError &&
            e.status === 409 &&
            p.runtime.adopted &&
            (await confirmDialog({
              title: `Deploying recreates ${p.name}`,
              message: e.message,
              confirmLabel: "Deploy",
              danger: true,
            }))
          )
        )
          throw e;
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
      {(p.replicas ?? []).filter((r) => !r.running && (r.deployment_status === "failed" || (r.conflicts?.length ?? 0) > 0)).map((r) => (
        <div key={r.device_id} className="mb-4 rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 text-sm">
          <p className="font-medium">The copy of {p.name} on {r.device} is not running</p>
          <p className="mt-1"><ReplicaBadge st={r} onRetry={canOperate ? () => api.retryReplica(p.id, r.device_id).then(project.reload).catch((e) => setError(e instanceof Error ? e.message : String(e))) : undefined} /></p>
          {(r.conflicts?.length ?? 0) > 0 && <p className="mt-1 text-xs text-neutral-700">{r.conflicts?.join("; ")}</p>}
        </div>
      ))}
      {!isCF && instances.length > 0 && (
        <div className="mb-4 flex flex-wrap items-center gap-2 text-sm" role="group" aria-label="Copy of this worker">
          <span className="text-neutral-600">
            Viewing: <span className="font-medium text-neutral-900">{p.name} · {viewName} ({viewId === p.device_id ? "main" : "replica"})</span>
          </span>
          {instances.length > 1 &&
            instances.map((i) => (
              <button
                key={i.id}
                type="button"
                onClick={() => void pickCopy(i.id)}
                aria-pressed={i.id === viewId}
                className={"rounded-lg border px-3 py-1.5 " + (i.id === viewId ? "border-primary bg-primary/10 font-medium text-primary" : "border-neutral-300 bg-white hover:bg-neutral-50")}
              >
                {i.name} ({i.main ? "main" : "replica"})
              </button>
            ))}
        </div>
      )}
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <Tabs
          items={[
            { to: base, label: "Overview", end: true },
            { to: `${base}/deployments`, label: "Deployments" },
            { to: `${base}/metrics`, label: "Metrics" },
            { to: `${base}/domains`, label: "Domains" },
            ...(p.source.type === "github" || isCF
              ? [{ to: `${base}/previews`, label: "Previews" }]
              : []),
            ...(canOperate
              ? [{ to: `${base}/settings`, label: "Settings" }]
              : []),
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
          {p.source.type === "compose" && (
            <StackControls p={p} onError={setError} />
          )}
          {worker && (
            <PowerControls
              deviceId={viewId}
              containerId={worker.id}
              state={worker.state}
              onDone={device.reload}
              onError={setError}
            />
          )}
          {worker && worker.state === "running" && isAdmin && (
            <Button
              icon={<SquareTerminal className="size-4" />}
              onClick={() => setShell(true)}
            >
              Shell
            </Button>
          )}
          {canOperate && (
            <Button
              icon={<CloudUpload className="size-4" />}
              onClick={deploy}
              loading={deploying}
            >
              {worker ? "Redeploy" : "Deploy"}
            </Button>
          )}
          {url && (
            <a href={url} target="_blank" rel="noreferrer">
              <Button
                variant="primary"
                icon={<ArrowUpRight className="size-4" />}
              >
                Visit
              </Button>
            </a>
          )}
        </div>
      </div>
      {shell && worker && (
        <Shell
          deviceId={viewId}
          container={worker.id}
          name={worker.name}
          onClose={() => setShell(false)}
        />
      )}
      {error && (
        <div className="mb-4">
          <ErrorNote>{error}</ErrorNote>
        </div>
      )}
      {!isAdmin && !!p.host_access?.length && (
        <div className="mb-4">
          <WarnNote>
            This project uses options that give its containers control of the device: {p.host_access.join("; ")}. It keeps
            running, but only an administrator can change these. You can still deploy, restart, roll back and edit environment variables.
          </WarnNote>
        </div>
      )}
      {!isAdmin && !!p.network_access?.length && (
        <div className="mb-4">
          <WarnNote>
            This project has its own network address{p.network_access.length > 1 ? "es" : ""}: {p.network_access.join("; ")}. Only an
            administrator can change them, because an own address can put a worker on another VLAN or on an address the network
            trusts. You can still deploy, restart, roll back and edit environment variables.
          </WarnNote>
        </div>
      )}
      <Routes key={viewId}>
        <Route
          index
          element={
            isCF ? (
              <CloudflareOverview p={p} />
            ) : (
              <ProjectOverview p={p} device={device.data} url={url} deviceName={viewName} />
            )
          }
        />
        <Route path="deployments" element={<ProjectDeployments p={p} />} />
        <Route
          path="domains"
          element={
            isCF ? (
              <CloudflareDomains p={p} />
            ) : p.source.type === "compose" ? (
              <TunnelPanel projectId={p.id} />
            ) : (
              <DomainsPanel projectId={p.id} />
            )
          }
        />
        <Route
          path="previews"
          element={
            isCF ? (
              <div className="space-y-6">
                {p.source.repo && <PreviewsPanel projectId={p.id} />}
                <CloudflarePreviews p={p} />
              </div>
            ) : (
              <PreviewsPanel projectId={p.id} />
            )
          }
        />
        <Route
          path="metrics"
          element={
            <ProjectMetricsTab p={p} deviceId={viewId} />
          }
        />
        {canOperate && (
          <Route
            path="settings"
            element={<ProjectSettings p={p} onSaved={project.reload} viewId={viewId} viewName={viewName} formKey={formKey} dirty={formDirty} />}
          />
        )}
      </Routes>
    </>
  );
}

function ProjectOverview({
  p,
  device,
  url,
  deviceName,
}: {
  p: Project;
  device: Device | null;
  url: string | null;
  deviceName: string;
}) {
  const deps = usePoll(() => api.projectDeployments(p.id), 4000, [p.id]);
  // Production is the newest deployment that went live, as on Cloudflare Pages.
  // With replicas, only this copy's deployments count.
  const mine = deps.data?.filter((x) => !device || x.device_id === device.id);
  const latest = device && device.id !== p.device_id ? (mine?.[0] ?? null) : p.latest_deployment;
  const d = mine?.find((x) => x.status === "succeeded") ?? latest;
  const container = device?.containers.find(
    (c) => c.name === (p.runtime.container || p.name),
  );
  const isStack = p.source.type === "compose";
  // Public hostnames that reach the worker through the device's Cloudflare Tunnel.
  const domains = usePoll(
    () => (isStack ? Promise.resolve(null) : api.projectDomains(p.id)),
    30000,
    [p.id, isStack],
  );
  const stackTunnel = usePoll(
    () => (isStack ? api.projectTunnel(p.id) : Promise.resolve(null)),
    30000,
    [p.id, isStack],
  );
  const publicUrls: string[] = isStack
    ? (stackTunnel.data?.routes ?? []).map((r) => `https://${r.hostname}`)
    : [
        ...(domains.data?.production?.enabled && domains.data.production.tunnel
          ? [`https://${domains.data.production.hostname}`]
          : []),
        ...(domains.data?.domains ?? [])
          .filter((x) => x.access === "tunnel")
          .map((x) => `https://${x.hostname}${x.path ?? ""}`),
      ];
  return (
    <div className="space-y-6">
      {(p.replicas?.length ?? 0) > 0 && (
        <Card>
          <CardHeader title="Copies" subtitle="Each copy answers on its own device. Domains, the proxy and tunnels point at the main copy only; replicas have no public route." />
          <div className="divide-y divide-neutral-100 text-sm">
            {[{ device_id: p.device_id, device: p.device_name, url: p.main_url, running_url: p.main_running_url, pending: p.main_pending, running: undefined as boolean | undefined, main: true }, ...(p.replicas ?? []).map((r) => ({ device_id: r.device_id, device: r.device, url: r.url, running_url: r.running_url, pending: r.pending, running: r.running, main: false }))].map((c) => (
              <div key={c.device_id} className="flex flex-wrap items-center justify-between gap-2 px-5 py-3">
                <span>
                  <span className="font-medium">{p.name} · {c.device}</span> <span className="text-neutral-500">({c.main ? "main" : "replica"})</span>
                </span>
                <span className="flex flex-wrap items-center justify-end gap-x-3 gap-y-1">
                  {c.running === false && <span className="text-xs text-neutral-500">not running</span>}
                  {c.pending ? (
                    <>
                      <span className="rounded bg-amber-100 px-1.5 py-0.5 text-xs text-amber-800">Pending redeploy</span>
                      <span className="text-xs text-neutral-500">running at {c.running_url ? c.running_url.replace("http://", "") : "—"}</span>
                      <span className="text-xs text-neutral-500">configured:</span>
                      {c.url ? <span className="text-neutral-700">{c.url.replace("http://", "")}</span> : <span className="text-neutral-500">from DHCP</span>}
                    </>
                  ) : c.url ? (
                    <a href={c.url} target="_blank" rel="noreferrer" className="text-primary hover:underline">{c.url.replace("http://", "")}</a>
                  ) : (
                    <span className="text-neutral-500">no direct address</span>
                  )}
                </span>
              </div>
            ))}
          </div>
        </Card>
      )}
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{p.name}</h1>
        <p className="mt-1 text-sm text-neutral-500">
          <SourceLabel p={p} />
        </p>
      </div>
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader
            title="Production"
            subtitle="The deployment currently live on the device."
          />
          {d ? (
            <Link
              to={`/deployments/${d.id}`}
              className="block p-5 hover:bg-neutral-50"
            >
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
                  A newer deployment failed and was rolled back:{" "}
                  <span className="text-bad">{latest.error}</span>
                </p>
              )}
            </Link>
          ) : p.runtime.adopted ? (
            <Empty
              icon={<PackagePlus className="size-8" />}
              title="Adopted, running as it was"
            >
              This project took over a container that already ran on the device.
              Orchestrator has not changed it. A new deployment pulls{" "}
              {p.source.image} and recreates the container from these settings.
              {p.runtime.unsupported?.length
                ? ` Not carried over: ${p.runtime.unsupported.join(", ")}.`
                : ""}
            </Empty>
          ) : (
            <Empty
              icon={<Rocket className="size-8" />}
              title="Not deployed yet"
            >
              Use Deploy to roll this project out.
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
                {device ? (
                  <Link
                    to={`/devices/${device.id}`}
                    className="text-primary hover:underline"
                  >
                    {device.name}
                  </Link>
                ) : (
                  deviceName || "—"
                )}
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
              <dd className="truncate font-mono text-xs">
                {p.runtime.ports.join(", ") || "—"}
              </dd>
            </div>
            {p.main_url && (
              <div className="flex justify-between gap-3">
                <dt className="text-neutral-500">Address</dt>
                <dd className="flex min-w-0 flex-col items-end text-xs">
                  {p.main_pending ? (
                    <>
                      <span className="rounded bg-amber-100 px-1.5 py-0.5 text-amber-800">Pending redeploy</span>
                      <span className="text-neutral-500">running: {p.main_running_url?.replace("http://", "") ?? "—"}</span>
                      <span className="text-neutral-500">configured: {p.main_url.replace("http://", "")}</span>
                    </>
                  ) : (
                    <span className="font-mono">{p.main_url.replace("http://", "")}</span>
                  )}
                </dd>
              </div>
            )}
            {publicUrls.length > 0 && (
              <div className="flex justify-between gap-3">
                <dt className="text-neutral-500">Public URL</dt>
                <dd className="flex min-w-0 flex-col items-end gap-1">
                  {publicUrls.map((u) => (
                    <a
                      key={u}
                      href={u}
                      target="_blank"
                      rel="noreferrer"
                      className="max-w-full truncate text-primary hover:underline"
                    >
                      {u.replace("https://", "")}
                    </a>
                  ))}
                </dd>
              </div>
            )}
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">
                {publicUrls.length > 0 ? "Local URL" : "URL"}
              </dt>
              <dd className="truncate">
                {url ? (
                  <a
                    href={url}
                    target="_blank"
                    rel="noreferrer"
                    className="text-primary hover:underline"
                  >
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
        <CardHeader
          title="Recent deployments"
          action={
            <Link
              to={`/projects/${p.id}/deployments`}
              className="text-sm text-primary hover:underline"
            >
              View all
            </Link>
          }
        />
        <div className="divide-y divide-neutral-100">
          {deps.data?.slice(0, 5).map((x) => (
            <DeploymentRow key={x.id} d={x} />
          ))}
          {deps.data?.length === 0 && (
            <p className="px-5 py-6 text-sm text-neutral-500">
              No deployments yet.
            </p>
          )}
        </div>
      </Card>
    </div>
  );
}

function ProjectDeployments({ p }: { p: Project }) {
  const deps = usePoll(() => api.projectDeployments(p.id), 4000, [p.id]);
  if (p.source.type === "cloudflare" && deps.data?.length === 0)
    return <CloudflareHistory p={p} />;
  return (
    <div className="space-y-6">
      <Card>
        <CardHeader
          title="Deployments"
          subtitle={
            p.source.type === "github" && p.source.auto_deploy
              ? `Every push to ${p.source.branch} deploys automatically.`
              : undefined
          }
        />
        {!deps.data ? (
          <Loading />
        ) : deps.data.length === 0 ? (
          <Empty
            icon={<Rocket className="size-8" />}
            title="No deployments yet"
          />
        ) : (
          <div className="divide-y divide-neutral-100">
            {deps.data.map((d) => (
              <DeploymentRow key={d.id} d={d} device={(p.replicas?.length ?? 0) > 0 ? (d.device_id === p.device_id ? p.device_name + " (main)" : (p.replicas?.find((r) => r.device_id === d.device_id)?.device ?? "another device") + " (replica)") : undefined} />
            ))}
          </div>
        )}
      </Card>
      {p.source.type === "cloudflare" && <CloudflareHistory p={p} />}
    </div>
  );
}

function ProjectSettings({ p, onSaved, viewId, viewName, formKey, dirty }: { p: Project; onSaved: () => void; viewId: string; viewName: string; formKey: number; dirty: MutableRefObject<boolean> }) {
  const nav = useNavigate();
  const isAdmin = useCan("admin");
  const [saved, setSaved] = useState(false);
  // Saving changes the stored settings only; the running container keeps what it was deployed with.
  const [needsDeploy, setNeedsDeploy] = useState(false);
  const [deployErr, setDeployErr] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  async function deployNow() {
    setStarting(true);
    setDeployErr(null);
    try {
      await api.deployProject(p.id);
      nav(`/projects/${p.id}/deployments`);
    } catch (e) {
      setDeployErr(e instanceof Error ? e.message : String(e));
    } finally {
      setStarting(false);
    }
  }
  return (
    <div className="space-y-6">
      {needsDeploy && p.source.type !== "cloudflare" && (
        <div role="status" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 text-sm">
          <span>Saved. The running worker keeps its old settings until you deploy it again.{deployErr ? ` ${deployErr}` : ""}</span>
          <Button onClick={deployNow} loading={starting}>
            Deploy now
          </Button>
        </div>
      )}
      {p.source.type === "cloudflare" && <CloudflareSettings p={p} />}
      {(p.replicas?.length ?? 0) > 0 && (
        <p className="rounded-lg bg-blue-50 px-4 py-2 text-sm">
          These settings apply to every copy of {p.name} (on {[p.device_name + " (main)", ...(p.replicas ?? []).map((r) => r.device + " (replica)")].join(", ")}), not only the one you are viewing. The next deployment applies them to all of them, one at a time. Domains and the proxy point at the main copy ({p.device_name}) only.
        </p>
      )}
      <Card className="p-6">
        <ProjectForm
          key={formKey}
          onDirty={(d) => (dirty.current = d)}
          initial={p}
          replicaView={viewId && viewId !== p.device_id ? viewName : undefined}
          viewDeviceId={viewId}
          submitLabel={saved ? "Saved" : "Save changes"}
          onSubmit={async (input, previews) => {
            await api.updateProject(p.id, input);
            if (previews) await api.setPreviewSettings(p.id, previews);
            const diff = (a: unknown, b: unknown) => JSON.stringify(a ?? null) !== JSON.stringify(b ?? null);
            const was = p.runtime as unknown as Record<string, unknown>;
            const now = input.runtime as unknown as Record<string, unknown>;
            const what: string[] = [];
            if (diff(was.dedicated, now.dedicated) || diff(was.copy_addresses, now.copy_addresses)) what.push("own address");
            if (diff(was.ports, now.ports)) what.push("ports");
            if (diff(was.env, now.env)) what.push("environment");
            if (diff(input.source, p.source)) what.push("source");
            const changed = diff(input.runtime, p.runtime) || diff(input.source, p.source);
            if (changed) setNeedsDeploy(true);
            setSaved(true);
            setTimeout(() => setSaved(false), 1500);
            onSaved();
            if (changed && p.source.type !== "cloudflare") {
              const copies = [p.device_name + " (main)", ...(p.replicas ?? []).map((r) => r.device + " (replica)")];
              const list = what.length ? what.join(", ") : "settings";
              void confirmDialog({
                title: "Redeploy now to apply these changes?",
                message: `Saved changes to ${list}. The running worker keeps its old settings until it is redeployed. Redeploying updates ${copies.length > 1 ? `${copies.join(", ")}: the main copy first, then each replica one at a time, stopping if one fails` : copies[0]}.`,
                confirmLabel: "Redeploy",
                cancelLabel: "Later",
              }).then((ok) => {
                if (ok) void deployNow();
              });
            }
          }}
        />
      </Card>
      {p.source.type !== "cloudflare" && <MigrateCard p={p} onDone={onSaved} />}
      {isAdmin && (
        <Card className="border-red-200">
          <CardHeader
            title="Delete project"
            subtitle={
              p.source.type === "cloudflare"
                ? "Removes the project and its deployment history here. What it deployed stays on Cloudflare; delete it there if you no longer want it."
                : "Removes the project and its deployment history. You can also remove its container from the device."
            }
          />
          <div className="p-5">
            <Button
              variant="danger"
              icon={<Trash2 className="size-4" />}
              onClick={async () => {
                const remove = await confirmWithOption({
                  title: `Delete ${p.name}?`,
                  message:
                    "The project and its deployment history are removed.",
                  confirmLabel: "Delete",
                  danger: true,
                  option:
                    p.source.type === "cloudflare"
                      ? undefined
                      : {
                          label: "Also remove the container from the device",
                          checked: !p.runtime?.adopted,
                        },
                });
                if (remove === null) return;
                await api.deleteProject(p.id, remove);
                nav("/projects");
              }}
            >
              Delete project
            </Button>
          </div>
        </Card>
      )}
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
    api
      .migration(p.id)
      .then(setM)
      .catch(() => {});
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
  // The current device stays in the list, labelled and disabled, so it is clear where the project runs now.
  const choices = (devices.data ?? []).filter((d) => d.id === p.device_id || !d.maintenance);
  return (
    <Card>
      <CardHeader
        title="Move to another device"
        subtitle="Backs up the data, deploys on the new device, copies the data across and checks it runs before anything is stopped."
      />
      <div className="space-y-4 p-5">
        {error && <ErrorNote>{error}</ErrorNote>}
        <Field label="Move to">
          <select
            className={inputClass}
            value={target}
            onChange={(e) => setTarget(e.target.value)}
            disabled={running}
          >
            <option value="">Choose a device</option>
            {choices.map((d) =>
              d.id === p.device_id ? (
                <option key={d.id} value={d.id} disabled title="The project already runs on this device">
                  {d.name} (current device)
                </option>
              ) : (
                <option key={d.id} value={d.id}>
                  {d.name} ({d.host.arch}
                  {d.status === "offline" ? ", offline" : ""}
                  {freeSpace(d) ? `, ${freeSpace(d)}` : ""})
                </option>
              ),
            )}
          </select>
          {source && <p className="mt-1 text-xs text-neutral-500">Currently on {source.name}. Pick a different device to move it to.</p>}
        </Field>
        {isStack && (
          <p className="text-sm text-neutral-500">
            A stack is redeployed on the new device. Its volumes are not copied,
            so it starts with fresh data.
          </p>
        )}
        {!isStack && (
          <div className="flex items-center gap-3 text-sm">
            <Toggle
              checked={withData}
              onChange={setWithData}
              label="Copy volumes and data"
            />
            Copy volumes and data
          </div>
        )}
        {!isStack && withData && sourceDown && (
          <div className="flex items-center gap-3 text-sm">
            <Toggle
              checked={fromBackup}
              onChange={setFromBackup}
              label="Use the newest backup on the server"
            />
            {source?.name} is offline: restore the newest backup on the server
            instead. The data may be out of date.
          </div>
        )}
        <div className="flex items-center gap-3 text-sm">
          <Toggle
            checked={stopSource}
            onChange={setStopSource}
            label="Stop the old container afterwards"
          />
          Stop the old container afterwards
        </div>
        <Button
          disabled={!target || running}
          icon={
            running ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Rocket className="size-4" />
            )
          }
          onClick={async () => {
            setError(null);
            try {
              setM(
                await api.migrateProject(p.id, {
                  device_id: target,
                  with_data: withData,
                  stop_source: stopSource,
                  from_backup: sourceDown && fromBackup,
                }),
              );
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
              {m.from} to {m.to}:{" "}
              {m.status === "failed"
                ? "failed"
                : m.status === "done"
                  ? "done"
                  : m.step}
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
