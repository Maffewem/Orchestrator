import { useState } from "react";
import { Route, Routes, useNavigate, useParams } from "react-router";
import { Trash2 } from "lucide-react";
import { api, type Device } from "../api";
import { useCrumbs } from "../components/Layout";
import ContainerTable from "../components/ContainerTable";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, HealthBadge, LineChart, Loading, Meter, StatusDot, Tabs, inputClass } from "../components/ui";
import { EventList } from "./Activity";
import { DeploymentRow } from "./Projects";
import { DeviceSummary } from "./Devices";
import { ago, bytes, pct, uptime, usePoll } from "../lib";

export default function DevicePage() {
  const { id = "" } = useParams();
  const dev = usePoll(() => api.device(id), 5000, [id]);
  const d = dev.data;
  useCrumbs([{ label: "Devices", to: "/devices" }, { label: d?.name ?? "…" }]);
  if (dev.error && !d) return <ErrorNote>{dev.error}</ErrorNote>;
  if (!d) return <Loading />;
  const base = `/devices/${d.id}`;
  return (
    <>
      <div className="mb-6 flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="flex items-center gap-2.5 text-2xl font-semibold tracking-tight">
            <StatusDot status={d.status} />
            {d.name}
            <HealthBadge health={d.health} status={d.status} />
          </h1>
          <p className="mt-1 flex text-sm text-neutral-500">
            <DeviceSummary d={d} />
          </p>
        </div>
      </div>
      <div className="mb-6">
        <Tabs
          items={[
            { to: base, label: "Overview", end: true },
            { to: `${base}/containers`, label: `Containers (${d.containers.length})` },
            { to: `${base}/deployments`, label: "Deployments" },
            { to: `${base}/activity`, label: "Activity" },
            { to: `${base}/settings`, label: "Settings" },
          ]}
        />
      </div>
      <Routes>
        <Route index element={<DeviceOverview d={d} />} />
        <Route
          path="containers"
          element={
            <Card>
              <ContainerTable containers={d.containers} deviceId={d.id} addresses={d.host.addresses ?? []} onChanged={dev.reload} />
            </Card>
          }
        />
        <Route path="deployments" element={<DeviceDeployments d={d} />} />
        <Route path="activity" element={<DeviceActivity d={d} />} />
        <Route path="settings" element={<DeviceSettings d={d} onSaved={dev.reload} />} />
      </Routes>
    </>
  );
}

function Prop({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex justify-between gap-4 py-2 text-sm">
      <dt className="shrink-0 text-neutral-500">{k}</dt>
      <dd className="min-w-0 truncate text-right">{v || "—"}</dd>
    </div>
  );
}

/** Server Manager-style properties, health and performance for one device. */
function DeviceOverview({ d }: { d: Device }) {
  const samples = usePoll(() => api.deviceMetrics(d.id), 5000, [d.id]);
  const s = samples.data ?? [];
  const m = d.metrics;
  return (
    <div className="grid gap-4 lg:grid-cols-3">
      <div className="space-y-4 lg:col-span-2">
        {d.issues.length > 0 && (
          <Card className="border-amber-200 bg-amber-50/50">
            <CardHeader title="Needs attention" />
            <ul className="space-y-1 px-5 py-3 text-sm">
              {d.issues.map((i) => (
                <li key={i}>• {i}</li>
              ))}
            </ul>
          </Card>
        )}
        <Card>
          <CardHeader title="Performance" subtitle="Live samples from the agent (last hour)." />
          <div className="space-y-6 p-5">
            <div className="grid grid-cols-3 gap-6">
              <Meter label="CPU" value={m.cpu_percent} />
              <Meter label={`Memory · ${bytes(m.memory_used)} of ${bytes(m.memory_total)}`} value={pct(m.memory_used, m.memory_total)} />
              <Meter label={`Disk · ${bytes(m.disk_used)} of ${bytes(m.disk_total)}`} value={pct(m.disk_used, m.disk_total)} />
            </div>
            <LineChart
              series={[
                { label: "CPU", color: "#0051c3", values: s.map((x) => x.cpu) },
                { label: "Memory", color: "#f6821f", values: s.map((x) => x.mem) },
                { label: "Disk", color: "#16794c", values: s.map((x) => x.disk) },
              ]}
            />
          </div>
        </Card>
        <Card>
          <CardHeader title="Recent activity" />
          <DeviceActivity d={d} limit={8} bare />
        </Card>
      </div>
      <div className="space-y-4">
        <Card>
          <CardHeader title="Properties" />
          <dl className="divide-y divide-neutral-100 px-5 py-1">
            <Prop k="Hostname" v={d.host.hostname} />
            <Prop k="Model" v={d.host.model} />
            <Prop k="Operating system" v={d.host.platform || d.host.os} />
            <Prop k="Kernel" v={d.host.kernel} />
            <Prop k="Architecture" v={d.host.arch} />
            <Prop k="Processors" v={d.host.cpus} />
            <Prop k="IP addresses" v={d.host.addresses?.join(", ")} />
            <Prop k="Uptime" v={uptime(m.uptime_seconds)} />
            <Prop k="Load average" v={`${m.load1.toFixed(2)} ${m.load5.toFixed(2)} ${m.load15.toFixed(2)}`} />
            <Prop k="Last seen" v={ago(d.last_seen)} />
            <Prop k="Agent" v={d.host.agent_version} />
          </dl>
        </Card>
        <Card>
          <CardHeader title="Docker" />
          <dl className="divide-y divide-neutral-100 px-5 py-1">
            {d.host.docker?.error ? (
              <p className="py-3 text-sm text-bad">{d.host.docker.error}</p>
            ) : (
              <>
                <Prop k="Version" v={d.host.docker?.version} />
                <Prop k="Containers" v={d.host.docker && `${d.host.docker.containers_running} running of ${d.host.docker.containers_total}`} />
                <Prop k="Images" v={d.host.docker?.images} />
              </>
            )}
          </dl>
        </Card>
        {d.labels.length > 0 && (
          <Card>
            <CardHeader title="Roles" />
            <div className="flex flex-wrap gap-1.5 p-5">
              {d.labels.map((l) => (
                <Badge key={l}>{l}</Badge>
              ))}
            </div>
          </Card>
        )}
      </div>
    </div>
  );
}

function DeviceDeployments({ d }: { d: Device }) {
  const deps = usePoll(() => api.deployments({ device: d.id }), 5000, [d.id]);
  const projects = usePoll(api.projects, 30000);
  const name = (id: string) => projects.data?.find((p) => p.id === id)?.name ?? "deleted project";
  return (
    <Card>
      {!deps.data ? (
        <Loading />
      ) : deps.data.length === 0 ? (
        <p className="px-5 py-6 text-sm text-neutral-500">Nothing has been deployed to this device yet.</p>
      ) : (
        <div className="divide-y divide-neutral-100">
          {deps.data.map((x) => (
            <DeploymentRow key={x.id} d={x} title={name(x.project_id)} />
          ))}
        </div>
      )}
    </Card>
  );
}

function DeviceActivity({ d, limit = 200, bare }: { d: Device; limit?: number; bare?: boolean }) {
  const events = usePoll(() => api.events(d.id, limit), 5000, [d.id, limit]);
  const body = !events.data ? <Loading /> : <EventList events={events.data} compact={bare} />;
  return bare ? body : <Card>{body}</Card>;
}

function DeviceSettings({ d, onSaved }: { d: Device; onSaved: () => void }) {
  const nav = useNavigate();
  const [name, setName] = useState(d.name);
  const [labels, setLabels] = useState(d.labels.join(", "));
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  return (
    <div className="max-w-2xl space-y-6">
      <Card>
        <CardHeader title="General" />
        <form
          className="space-y-4 p-5"
          onSubmit={async (e) => {
            e.preventDefault();
            setError(null);
            try {
              await api.updateDevice(d.id, { name, labels: labels.split(",").map((l) => l.trim()).filter(Boolean) });
              setSaved(true);
              setTimeout(() => setSaved(false), 1500);
              onSaved();
            } catch (err) {
              setError(err instanceof Error ? err.message : String(err));
            }
          }}
        >
          <Field label="Name">
            <input className={inputClass} value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Roles" hint="Comma-separated labels such as pi, edge, dns, worker.">
            <input className={inputClass} value={labels} onChange={(e) => setLabels(e.target.value)} />
          </Field>
          {error && <ErrorNote>{error}</ErrorNote>}
          <Button variant="primary">{saved ? "Saved" : "Save"}</Button>
        </form>
      </Card>
      <Card className="border-red-200">
        <CardHeader title="Remove device" subtitle="The agent will be disconnected. Its containers keep running. It can rejoin with the join token." />
        <div className="p-5">
          <Button
            variant="danger"
            icon={<Trash2 className="size-4" />}
            onClick={async () => {
              if (!window.confirm(`Remove ${d.name} from the fleet?`)) return;
              await api.deleteDevice(d.id);
              nav("/devices");
            }}
          >
            Remove device
          </Button>
        </div>
      </Card>
    </div>
  );
}
