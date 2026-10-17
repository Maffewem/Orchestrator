import { useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { ChevronDown, ChevronUp, Copy, Download, Github, Globe, RotateCw, Server } from "lucide-react";
import { api, type Deployment, type Step } from "../api";
import { useCrumbs } from "../components/Layout";
import { Button, Card, ErrorNote, Loading, StatusIcon } from "../components/ui";
import { visitUrl } from "./Project";
import { ago, clock, cx, duration, shortId, span, usePoll, useTick } from "../lib";

type LogLine = { time: string; text: string; step: number };

function parseLog(steps: Step[]): LogLine[] {
  const out: LogLine[] = [];
  for (const s of steps) {
    for (const raw of s.log.split("\n")) {
      if (!raw) continue;
      const i = raw.indexOf(" ");
      out.push({ time: raw.slice(0, i), text: raw.slice(i + 1), step: s.index });
    }
  }
  return out;
}

export default function DeploymentPage() {
  const { id = "" } = useParams();
  const nav = useNavigate();
  // Poll quickly while the deployment is live; stop once it finishes.
  const [live, setLive] = useState(true);
  const dep = usePoll(
    async () => {
      const d = await api.deployment(id);
      setLive(d.status === "queued" || d.status === "running");
      return d;
    },
    live ? 1500 : 0,
    [id],
  );
  const d = dep.data;
  const project = usePoll(() => (d ? api.project(d.project_id) : Promise.resolve(null)), 0, [d?.project_id]);
  const device = usePoll(() => (d ? api.device(d.device_id).catch(() => null) : Promise.resolve(null)), 0, [d?.device_id]);
  const domains = usePoll(() => (d ? api.projectDomains(d.project_id).catch(() => null) : Promise.resolve(null)), 0, [d?.project_id]);
  const [retrying, setRetrying] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useTick(live ? 500 : 60000);

  const projectName = project.data?.name ?? "…";
  useCrumbs([
    { label: "Projects", to: "/projects" },
    { label: projectName, to: d ? `/projects/${d.project_id}` : undefined },
    { label: "Deployments", to: d ? `/projects/${d.project_id}/deployments` : undefined },
    { label: `#${shortId(id)}` },
  ]);

  if (dep.error && !d) return <ErrorNote>{dep.error}</ErrorNote>;
  if (!d) return <Loading />;
  const total = span(d.started_at ?? d.created_at, d.finished_at);
  const prod = domains.data?.production;
  const url = prod?.enabled ? `https://${prod.hostname}` : project.data ? visitUrl(project.data, device.data) : null;

  return (
    <>
      <Link to={`/projects/${d.project_id}/deployments`} className="mb-4 inline-flex items-center gap-1.5 text-sm text-neutral-500 hover:text-neutral-900">
        ← Back to deployments
      </Link>
      <div className="mb-6 flex flex-wrap items-center justify-between gap-4">
        <h1 className="flex items-center gap-3 text-2xl font-semibold tracking-tight sm:text-3xl">
          <StatusIcon status={d.status} size="lg" />
          Deployment <span className="font-mono font-normal text-neutral-400">#{shortId(d.id)}</span>
        </h1>
        <div className="flex items-center gap-4">
          <span className="text-neutral-500 tabular-nums">{d.status === "queued" ? "Waiting for device…" : duration(total)}</span>
          <Button
            icon={<RotateCw className="size-4" />}
            loading={retrying}
            disabled={d.status === "queued" || d.status === "running"}
            onClick={async () => {
              setRetrying(true);
              setError(null);
              try {
                const n = await api.retryDeployment(d.id);
                nav(`/deployments/${n.id}`);
                setLive(true);
              } catch (e) {
                setError(e instanceof Error ? e.message : String(e));
              } finally {
                setRetrying(false);
              }
            }}
          >
            Retry deployment
          </Button>
        </div>
      </div>
      {error && (
        <div className="mb-4">
          <ErrorNote>{error}</ErrorNote>
        </div>
      )}

      <SourceCard d={d} deviceName={device.data?.name} url={url} />

      {d.error && (
        <div className="mt-4">
          <ErrorNote>{d.error}</ErrorNote>
        </div>
      )}

      <div className="-mx-4 mt-8 border-y border-neutral-200 bg-white sm:-mx-8">
        <Timeline steps={d.steps ?? []} />
        <LogView d={d} projectName={projectName} />
      </div>
    </>
  );
}

function SourceCard({ d, deviceName, url }: { d: Deployment; deviceName?: string; url?: string | null }) {
  const [open, setOpen] = useState(false);
  const c = d.commit;
  const settings: [string, string][] = [
    ["Service", d.spec.service],
    ["Image", d.spec.image],
    ...(d.spec.build ? ([["Build context", d.spec.build.remote.replace(/^https:\/\/github\.com\//, "")], ["Dockerfile", d.spec.build.dockerfile || "Dockerfile"]] as [string, string][]) : []),
    ["Ports", d.spec.ports?.join(", ") || "None"],
    ["Environment", d.spec.env?.length ? d.spec.env.map((e) => e.split("=")[0]).join(", ") : "None"],
    ["Restart policy", d.spec.restart || "—"],
  ];
  return (
    <Card>
      <div className="flex flex-wrap items-center gap-3 px-5 py-4">
        {url && (
          <a href={url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-2 text-sm font-medium underline decoration-neutral-300 underline-offset-2 hover:decoration-neutral-900">
            <Globe className="size-4 text-neutral-500" />
            {url.replace(/^https?:\/\//, "")}
          </a>
        )}
        {c ? (
          <>
            <span className="inline-flex items-center overflow-hidden rounded-full border border-neutral-200 text-sm">
              <span className="px-2.5 py-0.5">{c.branch}</span>
              <span className="border-l border-neutral-200 px-2.5 py-0.5 font-mono text-neutral-500">{c.sha.slice(0, 7)}</span>
            </span>
            <a href={c.url} target="_blank" rel="noreferrer" className="min-w-0 flex-1 truncate hover:underline">
              {c.message.split("\n")[0]}
            </a>
            <span className="text-sm text-neutral-500">
              by {c.author} · {ago(d.created_at)}
            </span>
          </>
        ) : (
          <>
            <span className="rounded-full border border-neutral-200 px-2.5 py-0.5 font-mono text-sm">{d.spec.image}</span>
            <span className="min-w-0 flex-1 truncate">Deploy {d.spec.image}</span>
            <span className="text-sm text-neutral-500">
              {d.trigger} · {ago(d.created_at)}
            </span>
          </>
        )}
      </div>
      <button
        onClick={() => setOpen(!open)}
        className="flex w-full items-center justify-between border-t border-neutral-200 px-5 py-2.5 text-sm text-neutral-600 hover:bg-neutral-50"
      >
        <span className="inline-flex items-center gap-1.5">
          Deployment settings {open ? <ChevronUp className="size-4" /> : <ChevronDown className="size-4" />}
        </span>
        <span className="inline-flex items-center gap-1.5 text-neutral-900">
          {c ? <Github className="size-4" /> : <Server className="size-4" />}
          {c ? c.repo : deviceName}
        </span>
      </button>
      {open && (
        <dl className="grid gap-x-8 gap-y-3 border-t border-neutral-200 px-5 py-4 text-sm sm:grid-cols-[180px_1fr]">
          <dt className="font-medium">Device</dt>
          <dd>{deviceName ?? d.device_id}</dd>
          {settings.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="font-medium">{k}</dt>
              <dd className={cx("break-all", v === "None" ? "text-neutral-500" : "font-mono text-[13px]")}>{v}</dd>
            </div>
          ))}
        </dl>
      )}
    </Card>
  );
}

/** Horizontal step bar; each segment's width follows how long the step took. */
function Timeline({ steps }: { steps: Step[] }) {
  const times = steps.map((s) => (s.started_at ? Math.max(span(s.started_at, s.finished_at), 1000) : 0));
  const known = times.filter(Boolean);
  const avg = known.length ? known.reduce((a, b) => a + b, 0) / known.length : 1000;
  return (
    <div className="flex gap-1.5 overflow-x-auto px-4 py-5 sm:px-8">
      {steps.map((s, i) => (
        <div key={s.index} className="min-w-28" style={{ flexGrow: times[i] || avg * 0.6, flexBasis: 0 }}>
          <p className={cx("mb-1.5 truncate text-sm", s.status === "pending" || s.status === "skipped" ? "text-neutral-400" : "text-neutral-700")}>{s.name}</p>
          <div
            className={cx(
              "flex h-10 items-center justify-end gap-2 rounded-lg border px-2.5",
              s.status === "running" && "progress-stripes border-primary/40",
              s.status === "failed" && "border-red-300 bg-red-50",
              (s.status === "succeeded" || s.status === "pending" || s.status === "skipped") && "border-neutral-200",
            )}
          >
            {s.started_at && <span className="text-sm text-neutral-600 tabular-nums">{duration(span(s.started_at, s.finished_at))}</span>}
            <StatusIcon status={s.status} size="sm" />
          </div>
          <p className="mt-1.5 font-mono text-xs text-neutral-500">{s.started_at ? clock(s.started_at) : " "}</p>
        </div>
      ))}
    </div>
  );
}

function LogView({ d, projectName }: { d: Deployment; projectName: string }) {
  const lines = useMemo(() => parseLog(d.steps ?? []), [d.steps]);
  const text = lines.map((l) => `${l.time} ${l.text}`).join("\n");
  const [copied, setCopied] = useState(false);
  return (
    <div className="relative border-t border-neutral-200 bg-neutral-50">
      <div className="sticky top-14 z-10 flex justify-end px-4 pt-3 sm:px-8">
        <div className="inline-flex overflow-hidden rounded-lg border border-neutral-300 bg-white text-sm shadow-sm">
          <button
            className="inline-flex items-center gap-1.5 px-3 py-1.5 hover:bg-neutral-100"
            onClick={() => {
              const a = document.createElement("a");
              a.href = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
              a.download = `${projectName}-${shortId(d.id)}.log`;
              a.click();
              URL.revokeObjectURL(a.href);
            }}
          >
            <Download className="size-4" /> Download log
          </button>
          <button
            className="inline-flex items-center gap-1.5 border-l border-neutral-300 px-3 py-1.5 hover:bg-neutral-100"
            onClick={async () => {
              try {
                await navigator.clipboard.writeText(text);
                setCopied(true);
                setTimeout(() => setCopied(false), 1500);
              } catch {
                /* clipboard blocked */
              }
            }}
          >
            <Copy className="size-4" /> {copied ? "Copied" : "Copy log"}
          </button>
        </div>
      </div>
      <div className="-mt-6 overflow-x-auto px-4 pb-8 font-mono text-[13px] leading-7 sm:px-8">
        {lines.length === 0 ? (
          <p className="pt-2 text-neutral-500">{d.status === "queued" ? "Waiting for the device to pick up this deployment…" : "No output yet."}</p>
        ) : (
          lines.map((l, i) => (
            <div key={i} className={cx("flex gap-6 whitespace-pre", /^(Error|Rollback failed)/.test(l.text) && "text-bad", l.text.startsWith("Success") && "text-ok")}>
              <span className="shrink-0 text-neutral-500 select-none">{clock(l.time)}</span>
              <span>{l.text}</span>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
