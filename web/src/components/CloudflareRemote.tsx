import { ExternalLink, GitBranch, GitPullRequest, Globe, Info, Rocket } from "lucide-react";
import { api, type CloudflareRemote, type Project } from "../api";
import { useState } from "react";
import { Badge, Card, CardHeader, Empty, RefreshButton, Skeleton } from "./ui";
import { useCan } from "./auth";
import { ago, usePoll } from "../lib";

type Dep = CloudflareRemote["deployments"][number];

/** Read-only data Cloudflare holds for a project, refreshed by the server's monitor. */
export function useCloudflareRemote(p: Project) {
  return usePoll(() => api.cloudflareRemote(p.id), 30000, [p.id]);
}

const STATUS: Record<string, { label: string; tone: "ok" | "warn" | "bad" | undefined }> = {
  active: { label: "Active", tone: "ok" },
  success: { label: "Success", tone: "ok" },
  failure: { label: "Failed", tone: "bad" },
  canceled: { label: "Canceled", tone: undefined },
  building: { label: "Building", tone: "warn" },
};

/** One line per section saying whether it was read from Cloudflare, or why not, plus a Sync button. */
export function SyncStatus({ p, q }: { p: Project; q: ReturnType<typeof useCloudflareRemote> }) {
  const can = useCan("operator");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const r = q.data;
  const sync = async () => {
    setBusy(true);
    setErr(null);
    try {
      await api.refreshCloudflare(p.id);
      await q.reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const worker = p.source.cloudflare?.kind === "worker";
  const sections = [["deployments", "Deployments"], ["previews", "Previews"], ["domains", "Domains"], ...(worker ? [["routes", "Routes"], ["build", "Build config"]] : []), ["settings", "Settings"]] as const;
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 border-b border-neutral-100 px-5 py-3">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs">
        {r ? (
          sections.map(([k, label]) => (
            <span key={k} title={r.notes?.[k]} className="inline-flex items-center gap-1.5">
              <Badge tone={r.notes?.[k] ? "warn" : "ok"}>{r.notes?.[k] ? "Needs attention" : "Synced"}</Badge>
              {label}
            </span>
          ))
        ) : (
          <span className="text-neutral-500">{q.error ?? "Not synced from Cloudflare yet."}</span>
        )}
        {r && <span className="text-neutral-500">Last synced {ago(r.fetched_at)}</span>}
        {err && <span className="text-bad">{err}</span>}
      </div>
      {can && <RefreshButton onClick={sync} busy={busy} label="Sync from Cloudflare" />}
    </div>
  );
}

function Note({ children }: { children: React.ReactNode }) {
  return (
    <p className="flex items-start gap-2 border-t border-neutral-100 px-5 py-3 text-xs text-neutral-500">
      <Info className="mt-0.5 size-3.5 shrink-0" />
      <span>{children}</span>
    </p>
  );
}

function Rows() {
  return (
    <div role="status" className="divide-y divide-neutral-100">
      <span className="sr-only">Loading</span>
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-3 px-5 py-4">
          <Skeleton className="size-6 rounded-full" />
          <div className="flex-1 space-y-2">
            <Skeleton className="h-3.5 w-1/3" />
            <Skeleton className="h-3 w-1/2" />
          </div>
          <Skeleton className="h-3 w-16" />
        </div>
      ))}
    </div>
  );
}

function DepRow({ d }: { d: Dep }) {
  const s = STATUS[d.status] ?? { label: d.status, tone: undefined };
  const title = d.message || (d.label ? `Version ${d.label}` : d.id.slice(0, 8));
  return (
    <div className="flex items-center gap-3 px-5 py-3">
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="truncate font-medium">{title}</span>
          {d.live && <Badge tone="ok">Live</Badge>}
          {d.environment === "preview" && <Badge>Preview</Badge>}
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-neutral-500">
          {d.label && d.message && <span className="font-mono">{d.label}</span>}
          {d.branch && (
            <span className="inline-flex items-center gap-1">
              <GitBranch className="size-3" />
              {d.branch}
            </span>
          )}
          {d.commit && <span className="font-mono">{d.commit.slice(0, 7)}</span>}
          {d.source && <span>{d.source}</span>}
          {d.author && <span>by {d.author}</span>}
          {d.percentage != null && d.percentage < 100 && <span>{d.percentage}% of traffic</span>}
          {d.url && (
            <a href={d.url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">
              Open <ExternalLink className="size-3" />
            </a>
          )}
        </div>
      </div>
      <div className="shrink-0 text-right text-xs text-neutral-500">
        <Badge tone={s.tone}>{s.label}</Badge>
        <p className="mt-1">{ago(d.created_at)}</p>
      </div>
    </div>
  );
}

/** Deployment history as Cloudflare has it, for projects whose deployments were made elsewhere. */
export function CloudflareHistory({ p, subtitle }: { p: Project; subtitle?: string }) {
  const q = useCloudflareRemote(p);
  const r = q.data;
  return (
    <Card>
      <CardHeader title="On Cloudflare" subtitle={subtitle ?? "Deployment history read from Cloudflare. Read-only: deployments made here appear above."} />
      <SyncStatus p={p} q={q} />
      {!r && !q.error ? (
        <Rows />
      ) : !r || r.deployments.length === 0 ? (
        <Empty icon={<Rocket className="size-8" />} title="No deployments on Cloudflare yet">
          {r?.notes?.deployments ?? q.error ?? "Cloudflare has no deployments for this project, or it has not been checked yet."}
        </Empty>
      ) : (
        <div className="divide-y divide-neutral-100">
          {r.deployments.map((d) => (
            <DepRow key={d.id} d={d} />
          ))}
        </div>
      )}
      {r?.notes?.deployments && r.deployments.length > 0 && <Note>{r.notes.deployments}</Note>}
      {r && Object.entries(r.notes ?? {}).filter(([k]) => k !== "deployments" && k !== "build").map(([k, v]) => <Note key={k}><b className="capitalize">{k}:</b> {v}</Note>)}
    </Card>
  );
}

/** The live production deployment on Cloudflare, for the Overview's Production card. */
export function RemoteProduction({ r, loading }: { r: CloudflareRemote | null; loading: boolean }) {
  const d = r?.deployments.find((x) => x.live) ?? r?.deployments.find((x) => x.environment === "production" && (x.status === "success" || x.status === "active"));
  if (loading) {
    return (
      <div role="status" className="space-y-3 p-5">
        <span className="sr-only">Loading</span>
        <Skeleton className="h-5 w-1/3" />
        <Skeleton className="h-3.5 w-1/2" />
      </div>
    );
  }
  if (!d) return null;
  return (
    <div>
      <DepRow d={d} />
      <Note>Deployed outside Orchestrator and read from Cloudflare {r ? ago(r.fetched_at) : ""}.</Note>
    </div>
  );
}

/** Preview deployments that exist on Cloudflare: Pages branch deployments, Worker versions. */
export function CloudflarePreviews({ p }: { p: Project }) {
  const q = useCloudflareRemote(p);
  const r = q.data;
  const worker = p.source.cloudflare?.kind === "worker";
  return (
    <Card>
      <CardHeader
        title="On Cloudflare"
        subtitle={worker ? "Versions that are not receiving production traffic, with their preview URLs." : "The newest preview deployment of each branch, read from Cloudflare."}
      />
      {!r && !q.error ? (
        <Rows />
      ) : !r || r.previews.length === 0 ? (
        <Empty icon={<GitPullRequest className="size-8" />} title="No previews on Cloudflare">
          {r?.notes?.previews ?? q.error ?? (worker ? "Every version of this Worker is live or none has been uploaded." : "No branch other than production has been deployed.")}
        </Empty>
      ) : (
        <div className="divide-y divide-neutral-100">
          {r.previews.map((v) => (
            <div key={v.id} className="flex items-center gap-3 px-5 py-3">
              <div className="min-w-0 flex-1">
                <p className="flex items-center gap-2 font-medium">
                  <span className="truncate font-mono text-[13px]">{v.label}</span>
                  {v.message && <span className="truncate font-normal text-neutral-600">{v.message}</span>}
                </p>
                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-neutral-500">
                  {v.commit && <span className="font-mono">{v.commit.slice(0, 7)}</span>}
                  {v.author && <span>by {v.author}</span>}
                  {[v.url, ...(v.aliases ?? [])].filter(Boolean).map((u) => (
                    <a key={u} href={u} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-primary hover:underline">
                      {u!.replace(/^https?:\/\//, "")} <ExternalLink className="size-3" />
                    </a>
                  ))}
                </div>
              </div>
              <div className="shrink-0 text-right text-xs text-neutral-500">
                <Badge tone={(STATUS[v.status] ?? {}).tone}>{(STATUS[v.status] ?? { label: v.status }).label}</Badge>
                <p className="mt-1">{ago(v.created_at)}</p>
              </div>
            </div>
          ))}
        </div>
      )}
      {r?.notes?.previews && r.previews.length > 0 && <Note>{r.notes.previews}</Note>}
      <Note>
        {p.source.repo
          ? "These are shown read-only. Previews that Orchestrator builds for pull requests are listed above."
          : "This project is watch-only: no repository is linked, so Orchestrator does not build previews. Add a repository in Settings to build pull requests."}
      </Note>
    </Card>
  );
}

/** Custom domains and routes attached to the project on Cloudflare. */
export function CloudflareDomains({ p }: { p: Project }) {
  const q = useCloudflareRemote(p);
  const r = q.data;
  const def = p.cloudflare?.default_url;
  const kind = { "custom-domain": "Custom domain", route: "Route", "pages-domain": "Custom domain" } as const;
  return (
    <Card>
      <CardHeader title="Domains" subtitle="Custom domains and routes read from Cloudflare. Change them in the Cloudflare dashboard." />
      {!r && !q.error ? (
        <Rows />
      ) : (
        <div className="divide-y divide-neutral-100">
          {(r?.domains ?? []).map((d) => (
            <div key={d.kind + d.host + d.detail} className="flex items-center gap-3 px-5 py-3">
              <Globe className="size-4 shrink-0 text-neutral-400" />
              <div className="min-w-0 flex-1">
                {d.host.includes("*") ? <span className="font-mono text-[13px]">{d.host}</span> : (
                  <a href={`https://${d.host}`} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 font-medium text-primary hover:underline">
                    {d.host} <ExternalLink className="size-3" />
                  </a>
                )}
                <p className="text-xs text-neutral-500">
                  {kind[d.kind]}
                  {d.detail ? ` · ${d.detail}` : ""}
                </p>
              </div>
              {d.status && <Badge tone={d.status === "active" ? "ok" : "warn"}>{d.status}</Badge>}
            </div>
          ))}
          {def && (
            <div className="flex items-center gap-3 px-5 py-3">
              <Globe className="size-4 shrink-0 text-neutral-400" />
              <div className="min-w-0 flex-1">
                <a href={def} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 font-medium text-primary hover:underline">
                  {def.replace(/^https:\/\//, "")} <ExternalLink className="size-3" />
                </a>
                <p className="text-xs text-neutral-500">Default address</p>
              </div>
            </div>
          )}
          {r && r.domains.length === 0 && !def && <p className="px-5 py-6 text-sm text-neutral-500">No custom domains found.</p>}
        </div>
      )}
      {(["domains", "routes"] as const).map((k) => r?.notes?.[k] && <Note key={k}>{r.notes[k]}</Note>)}
      {r && r.domains.length === 0 && def && <Note>No custom domain is attached on Cloudflare, so the address above is the one the Visit button uses.</Note>}
    </Card>
  );
}

/** Configuration read from Cloudflare. Secret values are never fetched, only their names. */
export function CloudflareSettings({ p }: { p: Project }) {
  const q = useCloudflareRemote(p);
  const r = q.data;
  const s = r?.settings;
  const b = s?.build;
  const hasBuild = !!s && !!(b || s.repo || s.build_command || s.output_dir || s.root_dir);
  const row = (k: string, v?: string | boolean | null) =>
    v === undefined || v === "" || v === null ? null : (
      <div key={k} className="flex justify-between gap-4 py-1.5 text-sm">
        <dt className="text-neutral-500">{k}</dt>
        <dd className="min-w-0 truncate text-right font-mono text-[13px]">{typeof v === "boolean" ? (v ? "On" : "Off") : v}</dd>
      </div>
    );
  return (
    <Card>
      <CardHeader title="Settings on Cloudflare" subtitle="Read from Cloudflare and shown here for reference. Change them in the Cloudflare dashboard or your Wrangler config." />
      {!r && !q.error ? (
        <div role="status" className="space-y-3 p-5">
          <span className="sr-only">Loading</span>
          <Skeleton className="h-4 w-1/2" />
          <Skeleton className="h-4 w-2/3" />
          <Skeleton className="h-4 w-1/3" />
        </div>
      ) : !s ? (
        <p className="px-5 py-6 text-sm text-neutral-500">{r?.notes?.settings ?? q.error ?? "Not read from Cloudflare yet."}</p>
      ) : (
        <div className="space-y-5 p-5">
          <dl className="divide-y divide-neutral-100">
            {row("Compatibility date", s.compatibility_date)}
            {row("Compatibility flags", s.compatibility_flags?.join(", "))}
            {row("Usage model", s.usage_model)}
            {row("Observability", s.observability)}
          </dl>
          <div>
            <h4 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">Build configuration</h4>
            {hasBuild ? (
              <dl className="mt-2 divide-y divide-neutral-100">
                {row("Repository", b?.repo ?? s.repo)}
                {row("Git provider", b?.provider)}
                {row("Production branch", s.production_branch)}
                {row("Branches built", b?.branches?.join(", "))}
                {row("Branches excluded", b?.exclude_branches?.join(", "))}
                {row("Root directory", b?.root_dir ?? s.root_dir)}
                {row("Build command", b?.build_command ?? s.build_command)}
                {row("Deploy command", b?.deploy_command)}
                {row("Output directory", s.output_dir)}
                {row("Paths watched", b?.path_includes?.join(", "))}
                {row("Paths ignored", b?.path_excludes?.join(", "))}
                {row("Build cache", b?.build_caching)}
              </dl>
            ) : (
              <p className="mt-2 text-sm text-neutral-500">
                {r?.notes?.build ?? (p.source.cloudflare?.kind === "pages" ? "Cloudflare has no build settings for this Pages project: it is deployed by direct upload rather than built from Git." : "Cloudflare did not report build settings.")}
              </p>
            )}
          </div>
          <div>
            <h4 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">Variables and secrets</h4>
            {s.variables.length === 0 ? (
              <p className="mt-2 text-sm text-neutral-500">None.</p>
            ) : (
              <ul className="mt-2 divide-y divide-neutral-100 text-sm">
                {s.variables.map((v) => (
                  <li key={v.name} className="flex justify-between gap-4 py-1.5">
                    <span className="font-mono text-[13px]">{v.name}</span>
                    <span className="min-w-0 truncate text-right font-mono text-[13px] text-neutral-600">{v.secret ? <Badge>Secret</Badge> : v.value}</span>
                  </li>
                ))}
              </ul>
            )}
          </div>
          <div>
            <h4 className="text-xs font-semibold uppercase tracking-wide text-neutral-500">Bindings</h4>
            {s.bindings.length === 0 ? (
              <p className="mt-2 text-sm text-neutral-500">None.</p>
            ) : (
              <ul className="mt-2 divide-y divide-neutral-100 text-sm">
                {s.bindings.map((b) => (
                  <li key={b.type + b.name} className="flex justify-between gap-4 py-1.5">
                    <span className="font-mono text-[13px]">{b.name}</span>
                    <span className="min-w-0 truncate text-right text-neutral-600">
                      {b.type.replace(/_/g, " ")}
                      {b.target ? ` · ${b.target}` : ""}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      )}
      {r?.notes?.settings && s && <Note>{r.notes.settings}</Note>}
      {r && <Note>Secret values are never read; only their names are listed. Checked {ago(r.fetched_at)}.</Note>}
    </Card>
  );
}
