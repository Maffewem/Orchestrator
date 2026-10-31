import { useCan } from "../components/auth";
import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { GitBranch, Github, Layers, Package, Plus, Rocket } from "lucide-react";
import { CloudflareBadge } from "../components/CloudflareProject";
import { api, type Deployment, type Project } from "../api";
import { useCrumbs } from "../components/Layout";
import { Button, Card, Empty, Loading, PageHeader, StatusDot, StatusIcon, deploymentLabel } from "../components/ui";
import PowerControls from "../components/PowerControls";
import { ago, cx, duration, shortId, span, usePoll, useTick } from "../lib";

export function SourceLabel({ p }: { p: Project }) {
  if (p.source.type === "cloudflare") {
    return (
      <span className="inline-flex min-w-0 items-center gap-1.5">
        <Github className="size-3.5 shrink-0" />
        <span className="truncate">{p.source.repo}</span>
        <GitBranch className="ml-1 size-3.5 shrink-0" />
        <span className="truncate">{p.source.branch}</span>
      </span>
    );
  }
  if (p.source.type === "github") {
    return (
      <span className="inline-flex min-w-0 items-center gap-1.5">
        <Github className="size-3.5 shrink-0" />
        <span className="truncate">{p.source.repo}</span>
        <GitBranch className="ml-1 size-3.5 shrink-0" />
        <span className="truncate">{p.source.branch}</span>
      </span>
    );
  }
  if (p.source.type === "compose" && p.source.repo) {
    return (
      <span className="inline-flex min-w-0 items-center gap-1.5">
        <Github className="size-3.5 shrink-0" />
        <span className="truncate">{p.source.repo}</span>
        <GitBranch className="ml-1 size-3.5 shrink-0" />
        <span className="truncate">{p.source.branch}</span>
      </span>
    );
  }
  if (p.source.type === "compose") {
    return (
      <span className="inline-flex min-w-0 items-center gap-1.5">
        <Layers className="size-3.5 shrink-0" />
        <span className="truncate">Compose stack</span>
      </span>
    );
  }
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      <Package className="size-3.5 shrink-0" />
      <span className="truncate">{p.source.image}</span>
    </span>
  );
}

export function CommitChip({ d }: { d: Deployment }) {
  if (!d.commit) return <span className="rounded-md border border-neutral-200 px-1.5 py-0.5 font-mono text-xs">{d.spec.compose ? "compose stack" : d.spec.image}</span>;
  return (
    <span className="inline-flex shrink-0 items-center overflow-hidden rounded-md border border-neutral-200 text-xs">
      <span className="px-1.5 py-0.5">{d.commit.branch}</span>
      <span className="border-l border-neutral-200 bg-neutral-50 px-1.5 py-0.5 font-mono text-neutral-600">{d.commit.sha.slice(0, 7)}</span>
    </span>
  );
}

export function deploymentTitle(d: Deployment) {
  if (d.commit) return d.commit.message.split("\n")[0];
  return d.spec.compose ? `Deploy stack ${d.spec.service}` : `Deploy ${d.spec.image}`;
}

const triggerLabel = { manual: "Manual", push: "Push", retry: "Retry", poll: "New commit", rollback: "Rollback" };

export function DeploymentRow({ d, title }: { d: Deployment; title?: string }) {
  useTick(1000);
  const took = span(d.started_at ?? d.created_at, d.finished_at);
  return (
    <Link to={`/deployments/${d.id}`} className="flex items-center gap-3 px-5 py-3 hover:bg-neutral-50">
      <StatusIcon status={d.status} />
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-center gap-2">
          {title && <span className="shrink-0 font-medium">{title}</span>}
          <span className={cx("truncate", title ? "text-neutral-600" : "font-medium")}>{deploymentTitle(d)}</span>
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-neutral-500">
          <CommitChip d={d} />
          <span>{triggerLabel[d.trigger]}</span>
          {d.commit?.author && <span>by {d.commit.author}</span>}
          <span className="font-mono">#{shortId(d.id)}</span>
        </div>
      </div>
      <div className="shrink-0 text-right text-xs text-neutral-500">
        <p className={cx("font-medium", d.status === "failed" ? "text-bad" : d.status === "succeeded" ? "text-ok" : "text-primary")}>
          {deploymentLabel(d.status)}
        </p>
        <p className="mt-0.5">
          {ago(d.created_at)}
          {d.status !== "queued" && ` · ${duration(took)}`}
        </p>
      </div>
    </Link>
  );
}

export default function ProjectsPage() {
  useCrumbs([{ label: "Projects" }]);
  const nav = useNavigate();
  const { data } = usePoll(api.projects, 5000);
  const fleet = usePoll(api.containers, 5000);
  const [error, setError] = useState<string | null>(null);
  const canOperate = useCan("operator");

  return (
    <>
      <PageHeader
        title="Projects"
        description="Services deployed to your devices, or to Cloudflare Pages and Workers, from GitHub or a container registry."
        actions={
          canOperate && (
            <Button variant="primary" icon={<Plus className="size-4" />} onClick={() => nav("/projects/new")}>
              Create project
            </Button>
          )
        }
      />
      {error && <p className="mb-3 text-sm text-bad">{error}</p>}
      {!data ? (
        <Loading />
      ) : data.length === 0 ? (
        <Card>
          <Empty icon={<Rocket className="size-8" />} title="Deploy your first project">
            Connect a GitHub repository with a Dockerfile, or run any image, on one of your devices.
            {canOperate && (
              <div className="mt-4">
                <Button variant="primary" onClick={() => nav("/projects/new")}>
                  Create project
                </Button>
              </div>
            )}
          </Empty>
        </Card>
      ) : (
        <div className="grid gap-3 md:grid-cols-2">
          {data.map((p) => (
            <div key={p.id} className="relative">
            <Link to={`/projects/${p.id}`}>
              <Card className="h-full p-5 transition-shadow hover:shadow-md">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <p className="truncate font-semibold">{p.name}</p>
                    <p className="mt-1 truncate text-xs text-neutral-500">
                      <SourceLabel p={p} />
                    </p>
                  </div>
                  {p.latest_deployment && <StatusIcon status={p.latest_deployment.status} />}
                </div>
                <div className="mt-4 border-t border-neutral-100 pt-3 text-sm">
                  {p.latest_deployment ? (
                    <>
                      <p className="truncate text-neutral-700">{deploymentTitle(p.latest_deployment)}</p>
                      <p className="mt-1 text-xs text-neutral-500">
                        {deploymentLabel(p.latest_deployment.status)} {ago(p.latest_deployment.created_at)} on {p.source.type === "cloudflare" ? "Cloudflare" : p.device_name || "a removed device"}
                      </p>
                    </>
                  ) : (
                    <p className="text-neutral-500">Not deployed yet · {p.device_name}</p>
                  )}
                </div>
                <div className="h-9" />
              </Card>
            </Link>
            {(() => {
              if (p.source.type === "cloudflare") {
                return (
                  <div className="absolute bottom-3 left-5 right-5 flex items-center justify-between text-xs text-neutral-600">
                    <CloudflareBadge p={p} />
                    {p.cloudflare?.requests ? <span className="tabular-nums">{p.cloudflare.requests.toLocaleString()} requests, 24h</span> : null}
                  </div>
                );
              }
              const w = fleet.data?.find((c) => c.project_id === p.id && c.device_id === p.device_id);
              return (
                <div className="absolute bottom-3 left-5 right-5 flex items-center justify-between">
                  <span className="flex items-center gap-1.5 text-xs text-neutral-600">
                    <StatusDot status={w?.state ?? "exited"} />
                    {w ? (w.state === "running" ? "Running" : "Stopped") : "No worker"}
                  </span>
                  {w && <div className="flex gap-1"><PowerControls deviceId={w.device_id} containerId={w.id} state={w.state} size="sm" onDone={fleet.reload} onError={setError} /></div>}
                </div>
              );
            })()}
            </div>
          ))}
        </div>
      )}
    </>
  );
}
