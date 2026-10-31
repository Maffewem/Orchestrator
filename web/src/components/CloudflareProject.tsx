import { useState } from "react";
import { Link } from "react-router";
import { Cloud, ExternalLink, RefreshCw, Rocket } from "lucide-react";
import { api, type CloudflareStatus, type Project } from "../api";
import { Badge, Button, Card, CardHeader, Empty, ErrorNote, StatusIcon, deploymentLabel } from "./ui";
import { CommitChip, DeploymentRow, SourceLabel, deploymentTitle } from "../pages/Projects";
import { ago, usePoll } from "../lib";
import { useCan } from "./auth";

const STATE: Record<CloudflareStatus["state"], { label: string; tone: "ok" | "warn" | "bad" | undefined }> = {
  live: { label: "Live", tone: "ok" },
  building: { label: "Building", tone: "warn" },
  failed: { label: "Failed", tone: "bad" },
  missing: { label: "Not deployed", tone: undefined },
  error: { label: "Can't reach Cloudflare", tone: "bad" },
};

/** The state of a Cloudflare project as the monitor last saw it, for lists. */
export function CloudflareBadge({ p }: { p: Project }) {
  const st = p.cloudflare;
  const kind = p.source.cloudflare?.kind === "worker" ? "Workers" : "Pages";
  const s = st ? STATE[st.state] : null;
  return (
    <span className="inline-flex items-center gap-1.5">
      <Cloud className="size-3.5 text-[#f6821f]" />
      <span>{kind}</span>
      {s && <Badge tone={s.tone}>{s.label}</Badge>}
    </span>
  );
}

const fmt = (n: number) => n.toLocaleString();

export function CloudflareOverview({ p }: { p: Project }) {
  const deps = usePoll(() => api.projectDeployments(p.id), 4000, [p.id]);
  const status = usePoll(() => api.cloudflareStatus(p.id), 30000, [p.id]);
  const canOperate = useCan("operator");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const st = status.data ?? p.cloudflare ?? null;
  const d = deps.data?.find((x) => x.status === "succeeded") ?? p.latest_deployment;
  const latest = p.latest_deployment;
  const worker = p.source.cloudflare?.kind === "worker";

  const refresh = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.refreshCloudflare(p.id);
      status.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{p.name}</h1>
        <p className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-neutral-500">
          <SourceLabel p={p} />
          <CloudflareBadge p={p} />
        </p>
      </div>
      {error && <ErrorNote>{error}</ErrorNote>}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader title="Production" subtitle="The newest deployment that went live on Cloudflare." />
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
                  A newer deployment failed and the one above stays live: <span className="text-bad">{latest.error}</span>
                </p>
              )}
            </Link>
          ) : (
            <Empty icon={<Rocket className="size-8" />} title="Not deployed yet">
              Use New deployment to build this project and upload it to Cloudflare.
            </Empty>
          )}
        </Card>
        <Card>
          <CardHeader
            title="On Cloudflare"
            action={
              canOperate && (
                <Button size="sm" icon={<RefreshCw className="size-3.5" />} loading={busy} onClick={refresh}>
                  Refresh
                </Button>
              )
            }
          />
          <dl className="space-y-3 p-5 text-sm">
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">{worker ? "Worker" : "Pages project"}</dt>
              <dd className="truncate font-mono text-[13px]">{st?.name ?? p.source.cloudflare?.name ?? p.name}</dd>
            </div>
            <div className="flex justify-between gap-3">
              <dt className="text-neutral-500">Status</dt>
              <dd>{st ? <Badge tone={STATE[st.state].tone}>{STATE[st.state].label}</Badge> : "Not checked yet"}</dd>
            </div>
            {st?.detail && <p className="text-xs text-neutral-500">{st.detail}</p>}
            {st?.url && (
              <div className="flex justify-between gap-3">
                <dt className="text-neutral-500">Address</dt>
                <dd className="min-w-0">
                  <a href={st.url} target="_blank" rel="noreferrer" className="inline-flex max-w-full items-center gap-1 text-primary hover:underline">
                    <span className="truncate">{st.url.replace(/^https:\/\//, "")}</span>
                    <ExternalLink className="size-3 shrink-0" />
                  </a>
                </dd>
              </div>
            )}
            {st?.domains && st.domains.length > 1 && (
              <div className="flex justify-between gap-3">
                <dt className="text-neutral-500">Domains</dt>
                <dd className="truncate text-right">{st.domains.join(", ")}</dd>
              </div>
            )}
            {st && (
              <p className="text-xs text-neutral-400">Checked {ago(st.polled_at)}. Pull requests are deployed as previews on the Previews tab.</p>
            )}
          </dl>
        </Card>
      </div>
    </div>
  );
}

export function CloudflareMetrics({ p }: { p: Project }) {
  const status = usePoll(() => api.cloudflareStatus(p.id), 30000, [p.id]);
  const deps = usePoll(() => api.projectDeployments(p.id), 15000, [p.id]);
  const st = status.data ?? p.cloudflare ?? null;
  const done = deps.data?.filter((d) => d.status === "succeeded" || d.status === "failed") ?? [];
  const ok = done.filter((d) => d.status === "succeeded").length;
  const worker = p.source.cloudflare?.kind === "worker";
  const errRate = st && st.requests ? (st.errors / st.requests) * 100 : 0;
  const tile = (label: string, value: string, sub?: string, bad?: boolean) => (
    <Card className="p-4">
      <p className="text-xs font-medium text-neutral-500">{label}</p>
      <p className={`mt-1 text-2xl font-semibold tabular-nums ${bad ? "text-bad" : ""}`}>{value}</p>
      {sub && <p className="mt-0.5 text-xs text-neutral-500">{sub}</p>}
    </Card>
  );
  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-3">
        {tile("Deployments", String(deps.data?.length ?? "—"))}
        {tile("Success rate", done.length ? `${Math.round((ok / done.length) * 100)}%` : "—")}
        {tile("Status", st ? STATE[st.state].label : "—", st ? `Checked ${ago(st.polled_at)}` : undefined, st?.state === "failed" || st?.state === "error")}
      </div>
      {worker ? (
        st?.analytics === "ok" ? (
          <div className="grid gap-4 sm:grid-cols-4">
            {tile("Requests, 24h", fmt(st.requests), `${fmt(st.subrequests)} subrequests`)}
            {tile("Errors, 24h", fmt(st.errors), st.requests ? `${errRate.toFixed(1)}% of requests` : undefined, errRate > 5)}
            {tile("CPU time p50", `${(st.cpu_p50_us / 1000).toFixed(1)} ms`)}
            {tile("CPU time p99", `${(st.cpu_p99_us / 1000).toFixed(1)} ms`)}
          </div>
        ) : (
          <Card className="p-5 text-sm text-neutral-600">
            Request counts for this Worker aren't available{st?.analytics && st.analytics !== "none" ? `: ${st.analytics.replace(/^unavailable: /, "")}` : " yet"}. The API token needs <b>Account → Account Analytics → Read</b>.
          </Card>
        )
      ) : (
        <Card className="p-5 text-sm text-neutral-600">Cloudflare keeps no request analytics for static Pages sites in its API. Deployment status and domains are monitored; see Web Analytics in the Cloudflare dashboard for traffic.</Card>
      )}
      <Card>
        <CardHeader title="Recent deployments" />
        <div className="divide-y divide-neutral-100">
          {(deps.data ?? []).slice(0, 8).map((d) => (
            <DeploymentRow key={d.id} d={d} />
          ))}
        </div>
      </Card>
    </div>
  );
}

/** Offers to import Pages projects and Workers that exist in the Cloudflare account but not here. */
export function ImportCloudflare({ onDone }: { onDone: () => void }) {
  const settings = usePoll(api.settings, 0);
  const configured = settings.data?.cloudflare.deploy.configured ?? false;
  const found = usePoll(() => (configured ? api.cloudflareDiscover() : Promise.resolve([])), 0, [configured]);
  const [open, setOpen] = useState(false);
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  const todo = (found.data ?? []).filter((f) => !f.imported);
  if (!configured || (todo.length === 0 && !msg)) return null;
  const key = (f: { kind: string; name: string }) => `${f.kind}/${f.name}`;
  const chosen = todo.filter((f) => picked[key(f)] ?? true);

  const run = async () => {
    setBusy(true);
    setMsg(null);
    try {
      const res = await api.cloudflareImport(chosen.map((f) => ({ kind: f.kind, name: f.name })));
      const bad = res.filter((r) => r.error);
      setMsg(`Imported ${res.length - bad.length}.${bad.length ? " " + bad.map((r) => `${r.name}: ${r.error}`).join(" ") : ""}`);
      setOpen(false);
      found.reload();
      onDone();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="mb-4 border-orange-200 bg-orange-50/40">
      <div className="flex flex-wrap items-center justify-between gap-3 px-5 py-3.5">
        <div className="flex items-center gap-2.5 text-sm">
          <Cloud className="size-4 text-[#f6821f]" />
          <span>
            {todo.length > 0 ? (
              <>
                Found <b>{todo.length}</b> {todo.length === 1 ? "project" : "projects"} on Cloudflare that Orchestrator isn't managing.
              </>
            ) : (
              msg
            )}
          </span>
        </div>
        {todo.length > 0 && (
          <Button size="sm" onClick={() => setOpen(!open)}>
            {open ? "Hide" : "Review and import"}
          </Button>
        )}
      </div>
      {open && (
        <div className="border-t border-orange-200 px-5 py-4">
          <ul className="space-y-2 text-sm">
            {todo.map((f) => (
              <li key={key(f)}>
                <label className="flex items-start gap-2.5">
                  <input type="checkbox" className="mt-1 size-4 accent-primary" checked={picked[key(f)] ?? true} onChange={(e) => setPicked({ ...picked, [key(f)]: e.target.checked })} />
                  <span className="min-w-0">
                    <span className="font-medium">{f.name}</span> <span className="text-neutral-500">{f.kind === "worker" ? "Worker" : "Pages"}</span>
                    <span className="block truncate text-xs text-neutral-500">
                      {f.url?.replace(/^https:\/\//, "")}
                      {f.repo ? ` · ${f.repo}${f.branch ? ` (${f.branch})` : ""}` : f.kind === "pages" ? " · no GitHub repository connected" : ""}
                    </span>
                  </span>
                </label>
              </li>
            ))}
          </ul>
          <p className="mt-3 text-xs text-neutral-500">
            Importing changes nothing on Cloudflare. Each project starts out watched only: status, address and (for Workers) request counts. Automatic deploys stay off, and one without a repository needs one added in its settings before it can deploy from here.
          </p>
          <div className="mt-3">
            <Button variant="primary" size="sm" loading={busy} disabled={chosen.length === 0} onClick={run}>
              Import {chosen.length}
            </Button>
          </div>
        </div>
      )}
      {msg && todo.length > 0 && <p className="border-t border-orange-200 px-5 py-2.5 text-xs text-neutral-600">{msg}</p>}
    </Card>
  );
}
