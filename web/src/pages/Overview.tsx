import { Link } from "react-router";
import { Boxes, Rocket, Server } from "lucide-react";
import { api } from "../api";
import { Section, useCrumbs } from "../components/Layout";
import { Card, Empty, HealthBadge, Loading, Meter, PageHeader, StatusDot } from "../components/ui";
import { DeploymentRow } from "./Projects";
import { EventList } from "./Activity";
import { pct, usePoll } from "../lib";
import { cx } from "../lib";

function Stat({ label, value, sub, tone }: { label: string; value: number | string; sub?: string; tone?: "ok" | "warn" | "bad" }) {
  return (
    <Card className="p-4">
      <p className="text-xs font-medium text-neutral-500">{label}</p>
      <p className={cx("mt-1 text-2xl font-semibold tabular-nums", tone === "bad" && "text-bad", tone === "warn" && "text-warn")}>{value}</p>
      {sub && <p className="mt-0.5 text-xs text-neutral-500">{sub}</p>}
    </Card>
  );
}

export default function OverviewPage() {
  useCrumbs([{ label: "Overview" }]);
  const overview = usePoll(api.overview, 5000);
  const devices = usePoll(api.devices, 5000);
  const deployments = usePoll(() => api.deployments({ limit: 6 }), 4000);
  const projects = usePoll(api.projects, 15000);

  const o = overview.data;
  if (!o || !devices.data) return <Loading />;
  const projectName = (id: string) => projects.data?.find((p) => p.id === id)?.name ?? "project";

  return (
    <>
      <PageHeader title="Overview" description="Health of every device, container and deployment in your fleet." />

      {devices.data.some((d) => d.maintenance) && (
        <div className="mb-6 rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900">
          In maintenance, so nothing new is placed on them:{" "}
          {devices.data
            .filter((d) => d.maintenance)
            .map((d, i) => (
              <span key={d.id}>
                {i > 0 && ", "}
                <Link className="font-medium underline" to={`/devices/${d.id}`}>
                  {d.name}
                </Link>
                {d.maintenance_note ? ` (${d.maintenance_note})` : ""}
              </span>
            ))}
        </div>
      )}

      <div className="mb-8 grid grid-cols-2 gap-3 md:grid-cols-4">
        <Stat label="Devices online" value={`${o.devices.online}/${o.devices.total}`} sub={o.devices.offline ? `${o.devices.offline} offline` : "All responding"} tone={o.devices.offline ? "bad" : undefined} />
        <Stat label="Needs attention" value={o.devices.warning + o.devices.critical} sub={`${o.devices.critical} critical, ${o.devices.warning} warning`} tone={o.devices.critical ? "bad" : o.devices.warning ? "warn" : undefined} />
        <Stat label="Containers running" value={`${o.containers.running}/${o.containers.total}`} sub={`${o.containers.stopped} stopped`} />
        <Stat label="Projects" value={projects.data?.length ?? "—"} sub={`${deployments.data?.filter((d) => d.status === "running" || d.status === "queued").length ?? 0} deploying now`} />
      </div>

      <Section title="Devices" action={<Link to="/devices" className="text-sm text-primary hover:underline">View all</Link>}>
        {devices.data.length === 0 ? (
          <Card>
            <Empty icon={<Server className="size-8" />} title="No devices yet">
              Add your first Raspberry Pi or VM from <Link className="text-primary" to="/devices">Devices</Link>.
            </Empty>
          </Card>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {devices.data.map((d) => (
              <Link key={d.id} to={`/devices/${d.id}`}>
                <Card className="h-full overflow-hidden transition-shadow hover:shadow-md">
                  <div className={cx("h-1", d.status === "offline" || d.health === "critical" ? "bg-bad" : d.health === "warning" ? "bg-warn" : "bg-ok")} />
                  <div className="p-4">
                    <div className="mb-3 flex items-start justify-between gap-2">
                      <div className="min-w-0">
                        <p className="flex items-center gap-2 truncate font-medium">
                          <StatusDot status={d.status} />
                          {d.name}
                        </p>
                        <p className="mt-0.5 truncate text-xs text-neutral-500">{d.host.model || d.host.platform || d.host.os} · {d.host.arch}</p>
                      </div>
                      <HealthBadge health={d.health} status={d.status} />
                    </div>
                    {d.issues.length > 0 && <p className="mb-3 truncate text-xs text-warn">{d.issues.join(" · ")}</p>}
                    <div className="grid grid-cols-3 gap-3">
                      <Meter label="CPU" value={d.metrics.cpu_percent} />
                      <Meter label="Memory" value={pct(d.metrics.memory_used, d.metrics.memory_total)} />
                      <Meter label="Disk" value={pct(d.metrics.disk_used, d.metrics.disk_total)} />
                    </div>
                    <CpuSpark id={d.id} />
                    <p className="mt-3 flex items-center gap-1.5 text-xs text-neutral-500">
                      <Boxes className="size-3.5" />
                      {d.containers.filter((c) => c.state === "running").length} of {d.containers.length} containers running
                    </p>
                  </div>
                </Card>
              </Link>
            ))}
          </div>
        )}
      </Section>

      <div className="grid gap-8 lg:grid-cols-5">
        <div className="lg:col-span-3">
          <Section title="Recent deployments">
            <Card>
              {deployments.data?.length ? (
                <div className="divide-y divide-neutral-100">
                  {deployments.data.map((d) => (
                    <DeploymentRow key={d.id} d={d} title={projectName(d.project_id)} />
                  ))}
                </div>
              ) : (
                <Empty icon={<Rocket className="size-8" />} title="No deployments yet">
                  Create a project to deploy from GitHub or a container image.
                </Empty>
              )}
            </Card>
          </Section>
        </div>
        <div className="min-w-0 lg:col-span-2">
          <Section title="Activity" action={<Link to="/activity" className="text-sm text-primary hover:underline">View all</Link>}>
            <Card>
              <EventList events={o.recent_events} devices={devices.data} compact />
            </Card>
          </Section>
        </div>
      </div>
    </>
  );
}

/** Recent CPU history for a device card, so a busy device stands out before it raises an alert. */
function CpuSpark({ id }: { id: string }) {
  const { data } = usePoll(() => api.deviceMetrics(id).catch(() => []), 60000, [id]);
  const v = (data ?? []).slice(-30).map((m) => m.cpu);
  if (v.length < 2) return null;
  const max = Math.max(20, ...v);
  const pts = v.map((x, i) => `${(i / (v.length - 1)) * 100},${22 - (x / max) * 20}`).join(" ");
  return (
    <svg viewBox="0 0 100 24" preserveAspectRatio="none" className="mt-3 h-6 w-full" role="img" aria-label="CPU over the last samples">
      <polyline points={pts} fill="none" stroke="var(--color-primary)" strokeWidth={1.5} vectorEffect="non-scaling-stroke" strokeLinejoin="round" />
    </svg>
  );
}
