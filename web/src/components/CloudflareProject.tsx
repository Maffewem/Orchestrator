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
