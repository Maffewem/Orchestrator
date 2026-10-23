import { useState, type ReactNode } from "react";
import { Link } from "react-router";
import { ArrowRight, ChevronDown, ChevronUp, Cloud, Github, Globe, GitBranch, Hexagon, Rocket, Archive, Plus } from "lucide-react";
import { api, type DomainsView, type Deployment, type FleetContainer, type Project } from "../api";
import { endpoints } from "../components/ContainerTable";
import { DeploymentLine, Sparkline } from "../components/DeploymentLine";
import { Card, Empty } from "../components/ui";
import { PreviewStatus } from "../components/Previews";
import { ago, bytes, cx, pct, usePoll, useTick } from "../lib";

/** One node of the diagram: a titled, collapsible list. */
function Node({ title, count, open: initial = false, children }: { title: string; count: number; open?: boolean; children?: ReactNode }) {
  const [open, setOpen] = useState(initial);
  return (
    <div className="rounded-xl border border-neutral-300 bg-white shadow-sm">
      <button className="flex w-full items-center justify-between gap-3 px-4 py-3 text-left" onClick={() => setOpen(!open)} aria-expanded={open}>
        <span className="flex items-center gap-2 font-medium">
          {title}
          <span className="rounded-md bg-neutral-200 px-1.5 py-0.5 text-xs font-semibold text-neutral-700">{count}</span>
        </span>
        {open ? <ChevronUp className="size-4 text-neutral-500" /> : <ChevronDown className="size-4 text-neutral-500" />}
      </button>
      {open && <div className="space-y-1 border-t border-neutral-200 px-4 py-3 text-sm text-neutral-600">{children}</div>}
    </div>
  );
}

function CardTitle({ title, to, badge }: { title: string; to?: string; badge?: string }) {
  return (
    <div className="flex items-center justify-between border-b border-neutral-200 px-5 py-3">
      <div className="flex items-center gap-3 text-sm font-medium text-neutral-600">
        {title}
        {badge && <span className="rounded-full bg-neutral-100 px-2.5 py-0.5 text-xs text-neutral-700">{badge}</span>}
      </div>
      {to && (
        <Link to={to} aria-label={`Open ${title}`} className="rounded p-1 text-neutral-500 hover:bg-neutral-100">
          <ArrowRight className="size-4" />
        </Link>
      )}
    </div>
  );
}

function Stat({ label, value, note, values, color }: { label: string; value: string; note?: string; values: number[]; color?: string }) {
  return (
    <div className="flex flex-col justify-between p-5">
      <div>
        <p className="text-sm text-neutral-500">{label}</p>
        <p className="mt-1 text-3xl font-semibold tracking-tight">
          {value} {note && <span className="text-sm font-normal text-neutral-500">{note}</span>}
        </p>
      </div>
      <div className="mt-4">{values.length > 1 ? <Sparkline values={values} color={color} /> : <p className="h-8 text-center text-xs text-neutral-400">No data</p>}</div>
    </div>
  );
}

const dayNames = ["Sundays", "Mondays", "Tuesdays", "Wednesdays", "Thursdays", "Fridays", "Saturdays"];

export default function WorkerOverview({ w, addresses, canBackup }: { w: FleetContainer; addresses: string[]; canBackup: boolean }) {
  const projectId = w.project_id;
  const project = usePoll(() => (projectId ? api.project(projectId) : Promise.resolve(null as Project | null)), 10000, [projectId]);
  const deps = usePoll(() => (projectId ? api.projectDeployments(projectId) : Promise.resolve([] as Deployment[])), 4000, [projectId]);
  const domains = usePoll(() => (projectId ? api.projectDomains(projectId) : Promise.resolve(null as DomainsView | null)), 30000, [projectId]);
  const previews = usePoll(() => (projectId ? api.projectPreviews(projectId).catch(() => null) : Promise.resolve(null)), 15000, [projectId]);
  const policy = usePoll(() => api.backupPolicy(w.device_id, w.name), 0, [w.device_id, w.name]);
  const metrics = usePoll(() => api.deviceMetrics(w.device_id), 15000, [w.device_id]);
  const device = usePoll(() => api.device(w.device_id), 10000, [w.device_id]);
  useTick(1000);

  const base = `/workers/${w.device_id}/${encodeURIComponent(w.name)}`;
  const p = project.data;
  const all = deps.data ?? [];
  const live = all.find((d) => d.status === "succeeded") ?? null;
  const latest = all[0] ?? null;
  const prod = domains.data?.production;
  const eps = w.state === "running" ? endpoints(w, addresses) : [];
  const mainUrl = prod?.enabled ? `https://${prod.hostname}` : eps[0] ? `http://${eps[0]}` : null;
  const env = p?.runtime.env.map((e) => e.split("=")[0]) ?? [];
  const publishedPorts = p?.runtime.ports ?? [];
  const mode = policy.data?.mode ?? "off";
  const samples = metrics.data ?? [];
  const m = device.data?.metrics;
  const failed24h = all.filter((d) => d.status === "failed" && Date.now() - Date.parse(d.created_at) < 864e5).length;
  const custom = domains.data?.domains ?? [];
  const routes = custom.filter((d) => d.type === "route");

  const steps: { icon: ReactNode; title: string; text: string; to: string }[] = [];
  if (mode === "off")
    steps.push({ icon: <Archive className="size-4" />, title: "Enable automatic backups", text: canBackup ? "Back this worker's data up daily or weekly" : "Update this device's agent, then schedule backups", to: canBackup ? `${base}/backups` : "/updates" });
  if (projectId && domains.data && !domains.data.base_domain && custom.length === 0)
    steps.push({ icon: <Globe className="size-4" />, title: "Give it a domain", text: "Set a base domain so it gets name.yourdomain", to: "/settings" });
  else if (projectId && custom.length === 0)
    steps.push({ icon: <Globe className="size-4" />, title: "Add a custom domain", text: "Serve this worker on your own hostname", to: `${base}/domains` });
  if (p?.source.type === "github" && !p.source.auto_deploy)
    steps.push({ icon: <Rocket className="size-4" />, title: "Turn on auto-deploy", text: `Deploy every push to ${p.source.branch}`, to: `/projects/${projectId}/settings` });
  if (!projectId) steps.push({ icon: <Plus className="size-4" />, title: "Manage it from a project", text: "Create a project to get deployments, domains and rollbacks", to: "/projects/new" });

  return (
    <div className="space-y-6">
      {/* Summary bar */}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-xl border border-neutral-200 bg-white px-5 py-3.5 text-sm">
        <Globe className="size-4 shrink-0 text-neutral-500" />
        {mainUrl ? (
          <a href={mainUrl} target="_blank" rel="noreferrer" className="font-medium underline decoration-neutral-300 underline-offset-2 hover:decoration-neutral-900">
            {mainUrl.replace(/^https?:\/\//, "")}
          </a>
        ) : (
          <span className="text-neutral-500">No address yet</span>
        )}
        <span className="min-w-0 flex-1 truncate text-neutral-600" title={latest?.commit?.message}>
          {latest ? (latest.commit?.message?.split("\n")[0] ?? latest.spec.image) : w.image}
        </span>
        {p && (
          <span className="inline-flex items-center gap-1.5 rounded-full border border-neutral-300 px-3 py-1 text-xs font-medium">
            {p.source.type === "github" ? <Github className="size-3.5" /> : <Cloud className="size-3.5" />}
            {p.source.type === "github" ? p.source.repo : p.source.image}
          </span>
        )}
        {latest && (
          <span className="whitespace-nowrap text-neutral-500">
            {latest.commit?.author ? `by ${latest.commit.author}` : latest.trigger === "manual" ? "by you" : ""} {ago(latest.created_at)}
          </span>
        )}
      </div>

      {/* Diagram */}
      <div className="rounded-xl border border-neutral-200 bg-neutral-50/60 p-4 sm:p-10">
        <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_2rem_minmax(0,1fr)_2rem_minmax(0,1fr)]">
          <div className="space-y-3">
            <Node title="Domains" count={(prod?.enabled ? 1 : 0) + custom.length}>
              {prod?.enabled && <p className="font-mono text-xs">{prod.hostname}</p>}
              {custom.map((d) => (
                <p key={d.id} className="font-mono text-xs">{d.hostname}{d.path}</p>
              ))}
              {!prod?.enabled && custom.length === 0 && <p>None yet.</p>}
              <Link to={`${base}/domains`} className="inline-block pt-1 text-primary hover:underline">Manage domains</Link>
            </Node>
            <Node title="Ports" count={publishedPorts.length}>
              {publishedPorts.length ? publishedPorts.map((x) => <p key={x} className="font-mono text-xs">{x}</p>) : <p>No published ports.</p>}
            </Node>
            <Node title="Env vars" count={env.length}>
              {env.length ? env.map((k) => <p key={k} className="break-all font-mono text-xs">{k}</p>) : <p>None set.</p>}
              {projectId && <Link to={`/projects/${projectId}/settings`} className="inline-block pt-1 text-primary hover:underline">Edit variables</Link>}
            </Node>
          </div>
          <Arrow />
          <div className="rounded-xl border border-neutral-300 bg-white shadow-sm">
            <div className="flex items-center justify-between px-4 py-3">
              <span className="flex items-center gap-2 font-medium">
                <Hexagon className="size-4" /> {w.name}
              </span>
              <span className={cx("size-2.5 rounded-full", w.state === "running" ? "bg-primary" : "bg-neutral-400")} title={w.status} />
            </div>
            <div className="space-y-2 border-t border-neutral-200 px-4 py-3 text-sm">
              <p className="text-neutral-500">Settings</p>
              <Row k="Auto-deploy" v={p?.source.type === "github" ? (p.source.auto_deploy ? "Enabled" : "Disabled") : "n/a"} />
              <Row k="Restart" v={p?.runtime.restart ?? "—"} />
              <Row k="State" v={w.status} />
            </div>
          </div>
          <Arrow />
          <Node title="Bindings" count={3} open>
            <Row k="Device" v={<Link to={`/devices/${w.device_id}`} className="text-primary hover:underline">{w.device_name}</Link>} />
            <Row k="Image" v={<span className="font-mono text-xs">{w.image}</span>} />
            <Row
              k="Backups"
              v={
                mode === "off" ? (
                  <Link to={`${base}/backups`} className="text-primary hover:underline">Off</Link>
                ) : (
                  <Link to={`${base}/backups`} className="text-primary hover:underline">
                    {mode === "daily" ? "Daily" : dayNames[policy.data?.weekday ?? 0]} {policy.data?.time}
                  </Link>
                )
              }
            />
          </Node>
        </div>
      </div>

      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_22rem]">
        <div className="space-y-6">
          <Card>
            <CardTitle title="Metrics" badge="Last hour · device" to={`${base}/deployments`} />
            <div className="grid divide-y divide-neutral-200 sm:grid-cols-3 sm:divide-x sm:divide-y-0">
              <Stat label="CPU" value={m ? `${m.cpu_percent.toFixed(0)}%` : "—"} values={samples.map((s) => s.cpu)} />
              <Stat label="Memory" value={m ? `${Math.round(pct(m.memory_used, m.memory_total))}%` : "—"} note={m ? `of ${bytes(m.memory_total)}` : undefined} values={samples.map((s) => s.mem)} color="#f6821f" />
              <Stat label="Failed deployments" value={String(failed24h)} note="24h" values={[]} />
            </div>
          </Card>

          <Card>
            <CardTitle title="Deployments" to={projectId ? `${base}/deployments` : undefined} />
            {!projectId ? (
              <Empty icon={<GitBranch className="size-8" />} title="Not deployed by Orchestrator">
                Deployments appear for workers created from a project.
              </Empty>
            ) : all.length === 0 ? (
              <Empty title="No deployments yet" />
            ) : (
              <div className="divide-y divide-neutral-100">
                {all.slice(0, 8).map((d) => (
                  <DeploymentLine key={d.id} d={d} live={live?.id === d.id} onChanged={deps.reload} compact />
                ))}
              </div>
            )}
          </Card>
        </div>

        <div className="space-y-6">
          <Card>
            <CardTitle title="Domains and routes" to={projectId ? `${base}/domains` : undefined} />
            <div className="divide-y divide-neutral-100 text-sm">
              <Block title="Worker URL">{prod?.enabled ? <p className="break-all">{prod.hostname}</p> : <p className="text-neutral-500">{eps[0] ?? "—"}</p>}</Block>
              <Block title="Custom domains">
                {custom.filter((d) => d.type === "domain").length ? custom.filter((d) => d.type === "domain").map((d) => <p key={d.id} className="break-all">{d.hostname}</p>) : <p className="text-neutral-400">—</p>}
              </Block>
              <Block title="Routes">{routes.length ? routes.map((d) => <p key={d.id} className="break-all">{d.hostname}{d.path}</p>) : <p className="text-neutral-400">—</p>}</Block>
            </div>
          </Card>

          <Card>
            <CardTitle title="Recent previews" to={projectId && p?.source.type === "github" ? `${base}/previews` : undefined} />
            {previews.data?.previews.length ? (
              <div className="divide-y divide-neutral-100">
                {previews.data.previews.slice(0, 5).map((pv) => (
                  <div key={pv.id} className="flex items-center gap-4 px-5 py-3 text-sm">
                    <GitBranch className="size-4 shrink-0 text-neutral-500" />
                    <span className="min-w-0 flex-1 truncate"><span className="font-medium">#{pv.pr}</span> {pv.title}</span>
                    <PreviewStatus s={pv.status} />
                    {pv.url && <a href={pv.url} target="_blank" rel="noreferrer" className="text-primary hover:underline">Visit</a>}
                    <span className="hidden whitespace-nowrap text-neutral-500 sm:block">{ago(pv.updated_at)}</span>
                  </div>
                ))}
              </div>
            ) : (
              <p className="px-5 py-6 text-sm text-neutral-500">
                {p?.source.type === "github" ? "No previews yet. Turn on previews to build every open pull request." : "Previews are available for workers deployed from GitHub."}
              </p>
            )}
          </Card>

          <Card>
            <CardTitle title="Next steps" />
            {steps.length === 0 ? (
              <p className="px-5 py-6 text-sm text-neutral-500">Everything recommended is set up.</p>
            ) : (
              <div className="divide-y divide-neutral-100">
                {steps.map((s) => (
                  <Link key={s.title} to={s.to} className="flex items-center justify-between gap-3 px-5 py-4 text-sm hover:bg-neutral-50">
                    <div className="min-w-0">
                      <p className="flex items-center gap-2 font-medium">{s.icon} {s.title}</p>
                      <p className="mt-0.5 text-neutral-500">{s.text}</p>
                    </div>
                    <ArrowRight className="size-4 shrink-0 text-neutral-400" />
                  </Link>
                ))}
              </div>
            )}
          </Card>
        </div>
      </div>
    </div>
  );
}

function Arrow() {
  return <div className="hidden items-center self-start pt-6 lg:flex" aria-hidden><div className="h-px flex-1 bg-neutral-400" /><div className="size-0 border-y-4 border-l-[6px] border-y-transparent border-l-neutral-400" /></div>;
}

function Row({ k, v }: { k: string; v: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <span className="text-neutral-500">{k}</span>
      <span className="min-w-0 truncate text-right">{v}</span>
    </div>
  );
}

function Block({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="space-y-1 px-5 py-4">
      <p className="text-neutral-500">{title}</p>
      {children}
    </div>
  );
}
