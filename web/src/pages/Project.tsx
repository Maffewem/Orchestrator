import { useState } from "react";
import { Link, Route, Routes, useNavigate, useParams } from "react-router";
import { ArrowUpRight, CloudUpload, Loader2, PackagePlus, Rocket, Trash2 } from "lucide-react";
import { api, ApiError, type Device, type Project } from "../api";
import { useCrumbs } from "../components/Layout";
import { Button, Card, CardHeader, Empty, ErrorNote, LineChart, Loading, StatusDot, StatusIcon, Tabs, deploymentLabel } from "../components/ui";
import { CommitChip, DeploymentRow, SourceLabel, deploymentTitle } from "./Projects";
import DomainsPanel from "../components/Domains";
import PreviewsPanel from "../components/Previews";
import PowerControls from "../components/PowerControls";
import ProjectForm from "./ProjectForm";
import { ago, usePoll } from "../lib";
import { confirmDialog } from "../components/dialogs";

/** URL of the first published port on the device, if we know an address for it. */
export function visitUrl(p: Project, device?: Device | null): string | null {
  const host = device?.host.addresses?.[0];
  const mapping = p.runtime.ports.find((x) => x.includes(":") && !x.endsWith("/udp"));
  if (!host || !mapping) return null;
  return `http://${host}:${mapping.split(":")[0]}`;
}

export default function ProjectPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  const project = usePoll(() => api.project(id), 4000, [id]);
  const p = project.data;
  const device = usePoll(() => (p ? api.device(p.device_id) : Promise.resolve(null)), 10000, [p?.device_id]);
  const [deploying, setDeploying] = useState(false);
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
            { to: `${base}/settings`, label: "Settings" },
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
          {worker && (
            <PowerControls deviceId={p.device_id} containerId={worker.id} state={worker.state} onDone={device.reload} onError={setError} />
          )}
          <Button icon={<CloudUpload className="size-4" />} onClick={deploy} loading={deploying}>
            New deployment
          </Button>
          {url && (
            <a href={url} target="_blank" rel="noreferrer">
              <Button variant="primary" icon={<ArrowUpRight className="size-4" />}>
                Visit
              </Button>
            </a>
          )}
        </div>
      </div>
      {error && (
        <div className="mb-4">
          <ErrorNote>{error}</ErrorNote>
        </div>
      )}
      <Routes>
        <Route index element={<ProjectOverview p={p} device={device.data} url={url} />} />
        <Route path="deployments" element={<ProjectDeployments p={p} />} />
        <Route path="domains" element={<DomainsPanel projectId={p.id} />} />
        <Route path="previews" element={<PreviewsPanel projectId={p.id} />} />
        <Route path="metrics" element={<ProjectMetrics p={p} />} />
        <Route path="settings" element={<ProjectSettings p={p} onSaved={project.reload} />} />
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
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">URL</dt>
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
      <Card className="border-red-200">
        <CardHeader title="Delete project" subtitle="Removes the project and its deployment history. The container keeps running on the device." />
        <div className="p-5">
          <Button
            variant="danger"
            icon={<Trash2 className="size-4" />}
            onClick={async () => {
              if (!(await confirmDialog({ title: `Delete ${p.name}?`, confirmLabel: "Delete", danger: true }))) return;
              await api.deleteProject(p.id);
              nav("/projects");
            }}
          >
            Delete project
          </Button>
        </div>
      </Card>
    </div>
  );
}
