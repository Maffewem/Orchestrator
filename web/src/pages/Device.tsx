import { useEffect, useState } from "react";
import { Route, Routes, useNavigate, useParams } from "react-router";
import { Trash2 } from "lucide-react";
import { api, type Device, type Migration, type Project, type TunnelRoute, type TunnelView } from "../api";
import { useCrumbs } from "../components/Layout";
import ContainerTable from "../components/ContainerTable";
import { ImportContainers } from "../components/Adopt";
import { useCan } from "../components/auth";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, FoldCard, HealthBadge, LineChart, Loading, Meter, Modal, StatusDot, Tabs, inputClass, SecretInput } from "../components/ui";
import { EventList } from "./Activity";
import { DeploymentRow } from "./Projects";
import { DeviceSummary } from "./Devices";
import { ago, bytes, bytesOrDash, freeSpace, pct, uptime, usePoll } from "../lib";
import { confirmDialog } from "../components/dialogs";
import Disks from "../components/Disks";
import HardwareMetrics, { PressureStats, availableGraphs, pressureSeries, type GraphKey } from "../components/HardwareMetrics";

export default function DevicePage() {
  const { id = "" } = useParams();
  const dev = usePoll(() => api.device(id), 5000, [id]);
  const d = dev.data;
  // The fleet list knows which containers already belong to a project; the device's own list doesn't.
  const fleet = usePoll(api.containers, 5000);
  const isAdmin = useCan("admin");
  const rows = fleet.data?.filter((c) => c.device_id === id);
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
            {d.maintenance && <Badge tone="warn">Maintenance</Badge>}
          </h1>
          {d.maintenance && (
            <p className="mt-1 text-sm text-neutral-500">
              Maintenance mode: no new workloads will be scheduled on this device.
              {maintenanceReason(d.maintenance_note) ? ` Reason: ${maintenanceReason(d.maintenance_note)}` : ""}
            </p>
          )}
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
            ...(isAdmin ? [{ to: `${base}/disks`, label: "Disks" }] : []),
            { to: `${base}/deployments`, label: "Deployments" },
            { to: `${base}/activity`, label: "Activity" },
            ...(isAdmin ? [{ to: `${base}/settings`, label: "Settings" }] : []),
          ]}
        />
      </div>
      <Routes>
        <Route index element={<DeviceOverview d={d} onChanged={dev.reload} />} />
        <Route
          path="containers"
          element={
            <>
              {isAdmin && rows && (
                <div className="mb-4 flex justify-end gap-2">
                  <ImportContainers containers={rows} onDone={fleet.reload} />
                </div>
              )}
              <Card>
                <ContainerTable containers={rows ?? d.containers} deviceId={d.id} addresses={d.host.addresses ?? []} onChanged={() => { dev.reload(); fleet.reload(); }} />
              </Card>
            </>
          }
        />
        {isAdmin && <Route path="disks" element={<Disks d={d} />} />}
        <Route path="deployments" element={<DeviceDeployments d={d} />} />
        <Route path="activity" element={<DeviceActivity d={d} />} />
        {isAdmin && <Route path="settings" element={<DeviceSettings d={d} onSaved={dev.reload} />} />}
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

/**
 * The per-device Caddy proxy only serves domains, custom domains and login-protected
 * workers. Without those, workers are still reachable at ip:port, so it is optional.
 */
function ProxyCard({ d }: { d: Device }) {
  const isAdmin = useCan("admin");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const text: Record<Device["proxy"], string> = {
    running: "Running. Serves domains, previews and login-protected workers on ports 80 and 443.",
    stopped: "The proxy container exists but is not running.",
    none: "Not created yet. It is optional: workers are reachable at their IP and port without it. It is created automatically when you add a domain, and you can enable it now.",
    agent_too_old: "This agent is too old to run the proxy. Re-run the install command on the device to update it.",
  };
  return (
    <FoldCard title="Domain proxy">
      <div className="space-y-3 p-5 text-sm">
        <p className="flex items-center gap-2">
          <Badge>{d.proxy === "agent_too_old" ? "agent too old" : d.proxy}</Badge>
        </p>
        <p className="text-neutral-500">{text[d.proxy]}</p>
        {error && <ErrorNote>{error}</ErrorNote>}
        {isAdmin && (d.proxy === "none" || d.proxy === "stopped") && (
          <Button
            variant="primary"
            disabled={busy || d.status !== "online"}
            onClick={async () => {
              setBusy(true);
              setError(null);
              try {
                await api.enableProxy(d.id);
              } catch (err) {
                setError(err instanceof Error ? err.message : String(err));
              } finally {
                setBusy(false);
              }
            }}
          >
            {d.proxy === "none" ? "Enable proxy" : "Restart proxy"}
          </Button>
        )}
      </div>
    </FoldCard>
  );
}

/**
 * One Cloudflare Tunnel per device (a cloudflared container). Any worker on the device can be
 * published through it, so it is set up here rather than per project.
 */
function TunnelCard({ d }: { d: Device }) {
  const isAdmin = useCan("admin");
  const tunnel = usePoll(() => api.tunnel(d.id), 5000, [d.id]);
  const t = tunnel.data;
  const projects = usePoll(api.projects, 0);
  const [token, setToken] = useState("");
  const [routes, setRoutes] = useState<TunnelRoute[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warnings, setWarnings] = useState<string[]>([]);
  const shown = routes ?? t?.routes ?? [];
  const mine = (projects.data ?? []).filter((p) => p.device_id === d.id && p.source.type !== "compose");
  const act = async (fn: () => Promise<TunnelView>, after?: () => void) => {
    setBusy(true);
    setError(null);
    try {
      const v = await fn();
      setWarnings(v.warnings ?? []);
      after?.();
      tunnel.reload();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <FoldCard title="Cloudflare Tunnel" subtitle="Publish workers on this device through Cloudflare without opening ports.">
      <div className="space-y-3 p-5 text-sm">
        {!t ? (
          <p className="text-neutral-500">Loading…</p>
        ) : !t.supported ? (
          <p className="text-neutral-500">This agent is too old to run a tunnel. Update it from the Updates page.</p>
        ) : (
          <>
            <p className="flex items-center gap-2">
              <Badge>{t.enabled ? (t.state ?? "starting") : "off"}</Badge>
              {t.tunnel_id && <span className="font-mono text-xs text-neutral-500">{t.tunnel_id}</span>}
            </p>
            {t.enabled && (
              <div className="space-y-2">
                <p className="flex flex-wrap items-center gap-2">
                  <span className="text-neutral-500">cloudflared</span>
                  <span className="font-mono text-xs">{t.version ?? (t.can_update ? "version unknown" : "version not reported")}</span>
                  {t.update_available ? (
                    <Badge>update available{t.latest ? `: ${t.latest}` : ""}</Badge>
                  ) : t.version && t.latest ? (
                    <Badge>up to date</Badge>
                  ) : null}
                  {isAdmin && t.can_update && (
                    <Button
                      variant={t.update_available ? "primary" : undefined}
                      disabled={busy || d.status !== "online"}
                      title="Pulls the newest cloudflared and recreates the container with the same token. The tunnel is down for a few seconds."
                      onClick={() => act(() => api.updateTunnel(d.id))}
                    >
                      Update tunnel
                    </Button>
                  )}
                </p>
                {!t.can_update && <p className="text-neutral-500">This agent is too old to update the tunnel. Update the agent from the Updates page, then update the tunnel here.</p>}
                {isAdmin && t.can_update && (
                  <label className="flex items-start gap-2">
                    <input type="checkbox" className="mt-1" checked={t.auto_update} disabled={busy} onChange={(e) => act(() => api.setTunnelAutoUpdate(d.id, e.target.checked))} />
                    <span>
                      Update cloudflared automatically
                      <span className="block text-xs text-neutral-500">
                        {t.auto_update_when
                          ? `Runs with the server's update schedule (${t.auto_update_when}): if a newer release is out, the tunnel on this device is updated then. Skipped while the device is in maintenance mode.`
                          : "Follows the server's update schedule, which is off, so nothing runs automatically until you set one on the Updates page. Use Update tunnel for a manual update."}
                      </span>
                    </span>
                  </label>
                )}
              </div>
            )}
            {!t.enabled && (
              <p className="text-neutral-500">
                Create a tunnel in Cloudflare Zero Trust (Networks, Tunnels), then paste its token or the whole connector command here. Orchestrator runs cloudflared on this device.
              </p>
            )}
            {isAdmin && (
              <div className="flex gap-2">
                <SecretInput
                  className={inputClass}
                  autoComplete="off"
                  placeholder={t.enabled ? "Paste a new token to replace the current one" : "Tunnel token"}
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                />
                <Button variant="primary" disabled={busy || !token.trim() || d.status !== "online"} onClick={() => act(() => api.setTunnel(d.id, token), () => setToken(""))}>
                  {t.enabled ? "Replace" : "Enable"}
                </Button>
                {t.enabled && (
                  <Button disabled={busy} onClick={() => act(() => api.setTunnel(d.id, ""))}>
                    Remove
                  </Button>
                )}
              </div>
            )}
            {t.enabled && isAdmin && (
              <div className="space-y-2 border-t border-neutral-200 pt-3">
                <p className="font-medium">Where tunnel traffic goes</p>
                {(
                  [
                    ["", "Straight to the worker (default)", "Lightest: cloudflared sends each request directly to the worker's port. Nothing else runs in the path. Login protection, path routes and preview URLs cannot be used through the tunnel."],
                    ["proxy", "Through the device proxy (Caddy)", "Adds one more process in the path on this device: cloudflared sends requests to Caddy, which forwards them to the worker. Caddy uses little CPU and tens of MB of memory for plain HTTP, but on a very small device that is extra load. It enables login protection, path routes and per-preview hostnames through the tunnel."],
                  ] as const
                ).map(([val, label, text]) => (
                  <label key={val} className="flex cursor-pointer items-start gap-2">
                    <input type="radio" className="mt-1" name={`via-${d.id}`} checked={t.via === val} disabled={busy} onChange={() => act(() => api.setTunnelVia(d.id, val))} />
                    <span>
                      <span className="font-medium">{label}</span>
                      <br />
                      <span className="text-neutral-500">{text}</span>
                    </span>
                  </label>
                ))}
              </div>
            )}
            {t.enabled && (
              <div className="space-y-2 border-t border-neutral-200 pt-3">
                <p className="font-medium">Public hostnames</p>
                <p className="text-xs text-neutral-500">
                  {t.can_route
                    ? "Applied to the tunnel through the Cloudflare API connected in Settings, which also creates the DNS records."
                    : "Connect Cloudflare in Settings to apply these from here (token needs Account > Cloudflare Tunnel > Edit). Until then add the same hostnames in the Cloudflare dashboard."}
                </p>
                {shown.map((r, i) => (
                  <div key={i} className="flex gap-2">
                    <input className={inputClass} placeholder="app.example.com" value={r.hostname} disabled={!isAdmin} onChange={(e) => setRoutes(shown.map((x, j) => (j === i ? { ...x, hostname: e.target.value } : x)))} />
                    <input className={inputClass} list={`svc-${d.id}`} placeholder="http://localhost:3000" value={r.service} disabled={!isAdmin} onChange={(e) => setRoutes(shown.map((x, j) => (j === i ? { ...x, service: e.target.value } : x)))} />
                    {isAdmin && <Button onClick={() => setRoutes(shown.filter((_, j) => j !== i))}>×</Button>}
                  </div>
                ))}
                <datalist id={`svc-${d.id}`}>
                  {mine.map((p) => (
                    <option key={p.id} value={`project:${p.name}`} />
                  ))}
                </datalist>
                {isAdmin && (
                  <div className="flex gap-2">
                    <Button onClick={() => setRoutes([...shown, { hostname: "", service: "" }])}>Add hostname</Button>
                    {routes && (
                      <Button variant="primary" disabled={busy} onClick={() => act(() => api.setTunnelRoutes(d.id, shown.filter((r) => r.hostname.trim())), () => setRoutes(null))}>
                        Save and apply
                      </Button>
                    )}
                  </div>
                )}
              </div>
            )}
          </>
        )}
        {error && <ErrorNote>{error}</ErrorNote>}
        {warnings.map((w) => (
          <p key={w} className="text-xs text-amber-700">{w}</p>
        ))}
      </div>
    </FoldCard>
  );
}

/** Server Manager-style properties, health and performance for one device. */
function DeviceOverview({ d, onChanged }: { d: Device; onChanged: () => void }) {
  const canOperate = useCan("operator");
  const [acking, setAcking] = useState(false);
  const [ackError, setAckError] = useState<string | null>(null);
  const ack = async () => {
    setAcking(true);
    setAckError(null);
    try {
      await api.ackNetworkNotice(d.id);
      onChanged();
    } catch (err) {
      setAckError(err instanceof Error ? err.message : String(err));
    } finally {
      setAcking(false);
    }
  };
  const samples = usePoll(() => api.deviceMetrics(d.id), 5000, [d.id]);
  const thresholds = usePoll(async () => (await api.settings()).temperature, 60000);
  const s = samples.data ?? [];
  const m = d.metrics;
  const hwTabs = availableGraphs(s, m);
  const graphs: { key: "perf" | "temp" | GraphKey; label: string }[] = [
    { key: "perf", label: "Performance" },
    ...(s.some((x) => x.temp > 0) || hwTabs.some((g) => g.key === "power") ? [{ key: "temp" as const, label: hwTabs.some((g) => g.key === "power") ? "Temperature & power" : "Temperature" }] : []),
    ...hwTabs.filter((g) => g.key !== "power"),
  ];
  const [picked, setPicked] = useState<string>(() => {
    try {
      return localStorage.getItem("orch.deviceGraph") ?? "perf";
    } catch {
      return "perf";
    }
  });
  const graph = (graphs.find((g) => g.key === picked)?.key ?? "perf") as (typeof graphs)[number]["key"];
  const pickGraph = (k: string) => {
    setPicked(k);
    try {
      localStorage.setItem("orch.deviceGraph", k);
    } catch {
      /* remembering the choice is optional */
    }
  };
  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
      <div className="space-y-4 lg:col-span-2">
        {d.issues.length > 0 && (
          <Card className="border-amber-200 bg-amber-50/50">
            <CardHeader title="Needs attention" />
            <ul className="space-y-1 px-5 py-3 text-sm">
              {d.issues.map((i) => (
                <li key={i}>• {i}</li>
              ))}
            </ul>
            {d.network_notice && (
              <div className="flex flex-wrap items-center gap-3 border-t border-amber-200 px-5 py-3 text-sm">
                <p className="min-w-0 flex-1 text-neutral-600">
                  Network change seen {ago(d.network_notice.at)}. Acknowledge it once you know the new address is expected; it comes back only if the network changes again.
                </p>
                {canOperate && <Button size="sm" loading={acking} onClick={ack}>Acknowledge</Button>}
              </div>
            )}
            {ackError && <div className="px-5 pb-3"><ErrorNote>{ackError}</ErrorNote></div>}
          </Card>
        )}
        <div role="tablist" aria-label="Graphs" className="-mx-1 flex gap-2 overflow-x-auto px-1 pb-1">
          {graphs.map((g) => (
            <button
              key={g.key}
              type="button"
              role="tab"
              aria-selected={graph === g.key}
              onClick={() => pickGraph(g.key)}
              className={`min-h-9 shrink-0 rounded-full border px-3.5 text-sm ${graph === g.key ? "border-neutral-900 bg-neutral-900 text-white" : "border-neutral-200 bg-white text-neutral-700"}`}
            >
              {g.label}
            </button>
          ))}
        </div>
        {graph === "perf" && (
          <>
        <Card>
          <CardHeader title="Performance" subtitle="Live samples from the agent (last hour)." />
          <div className="space-y-6 p-5">
            <div className="grid grid-cols-3 gap-6">
              <Meter label={`CPU · ${d.host.cpus ? `${d.host.cpus} ${d.host.cpus === 1 ? "core" : "cores"}` : "—"}`} value={m.cpu_percent} />
              <Meter label={`Memory · ${bytes(m.memory_used)} of ${bytesOrDash(m.memory_total)}`} value={pct(m.memory_used, m.memory_total)} />
              <Meter label={`Disk · ${bytes(m.disk_used)} of ${bytesOrDash(m.disk_total)}`} value={pct(m.disk_used, m.disk_total)} />
            </div>
            <LineChart
              series={[
                { label: "CPU", color: "#0051c3", values: s.map((x) => x.cpu) },
                { label: "Memory", color: "#f6821f", values: s.map((x) => x.mem) },
                { label: "Disk", color: "#16794c", values: s.map((x) => x.disk) },
                ...pressureSeries(s, m, d.host.cpus),
              ]}
            />
            <PressureStats m={m} />
          </div>
        </Card>
          </>
        )}
        {graph === "temp" && s.some((x) => x.temp > 0) && (
          <Card>
            <CardHeader title="Temperature" subtitle={thresholds.data ? `Hottest sensor, last hour. Alerts at ${thresholds.data.warn_c}°C (warning) and ${thresholds.data.crit_c}°C (critical).` : "Hottest sensor, last hour."} />
            <div className="p-5">
              <LineChart
                max={Math.max(100, Math.ceil(((thresholds.data?.crit_c ?? 85) + 5) / 10) * 10)}
                unit="°C"
                format={(v) => `${v.toFixed(0)}°C`}
                series={[{ label: "Temperature", color: "#d92d20", values: s.map((x) => x.temp) }]}
              />
              {(m.sensors?.length ?? 0) > 1 && (
                <dl className="mt-4 grid grid-cols-2 gap-x-6 gap-y-1 text-sm sm:grid-cols-3">
                  {m.sensors!.map((x) => (
                    <div key={x.name} className="flex min-w-0 justify-between gap-2">
                      <dt className="truncate text-neutral-500">{x.name}</dt>
                      <dd className="font-medium tabular-nums">{x.c.toFixed(0)}°C</dd>
                    </div>
                  ))}
                </dl>
              )}
            </div>
          </Card>
        )}
        {graph === "temp" && <HardwareMetrics s={s} m={m} only="power" />}
        {(graph === "disk" || graph === "net") && <HardwareMetrics s={s} m={m} only={graph} />}
        <FoldCard title="Recent activity">
          <DeviceActivity d={d} limit={8} bare />
        </FoldCard>
      </div>
      <div className="space-y-4">
        <FoldCard title="Properties">
          <dl className="divide-y divide-neutral-100 px-5 py-1">
            <Prop k="Hostname" v={d.host.hostname} />
            <Prop k="Model" v={d.host.model} />
            <Prop k="Operating system" v={d.host.platform || d.host.os} />
            <Prop k="Kernel" v={d.host.kernel} />
            <Prop k="Architecture" v={d.host.arch} />
            <Prop k="CPU cores" v={d.host.cpus || "—"} />
            <Prop k="Total RAM" v={bytesOrDash(m.memory_total || d.host.memory_total)} />
            <Prop k="Total disk" v={bytesOrDash(m.disk_total)} />
            <Prop k="IP addresses" v={d.host.addresses?.join(", ")} />
            <Prop k="Uptime" v={uptime(m.uptime_seconds)} />
            <Prop k="Load average" v={`${m.load1.toFixed(2)} ${m.load5.toFixed(2)} ${m.load15.toFixed(2)}`} />
            <Prop k="Temperature" v={m.temp_c ? `${m.temp_c.toFixed(0)}°C` : "Not reported (no sensor, or the agent needs updating)"} />
            <Prop k="Last seen" v={ago(d.last_seen)} />
            <Prop k="Agent" v={d.host.agent_version} />
          </dl>
        </FoldCard>
        <FoldCard title="Docker">
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
        </FoldCard>
        <ProxyCard d={d} />
        <TunnelCard d={d} />
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
      <MaintenanceCard d={d} onSaved={onSaved} />
      <Card className="border-red-200">
        <CardHeader title="Remove device" subtitle="The agent will be disconnected. Its containers keep running. It can rejoin with the join token." />
        <div className="p-5">
          <Button
            variant="danger"
            icon={<Trash2 className="size-4" />}
            onClick={async () => {
              if (!(await confirmDialog({ title: `Remove ${d.name}?`, confirmLabel: "Remove", danger: true }))) return;
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

/**
 * Maintenance mode keeps new workers, previews and adoptions off the device. Turning it on lists
 * the workers that would become unreachable if the device goes down, and offers to move them.
 */
/** Hides the automatic reasons older versions stored when a device was added in maintenance mode. */
function maintenanceReason(note?: string) {
  return note && note !== "Set up in maintenance mode" && note !== "Added in maintenance mode" ? note : "";
}

function MaintenanceCard({ d, onSaved }: { d: Device; onSaved: () => void }) {
  const isAdmin = useCan("admin");
  const [open, setOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (!isAdmin) return null;
  const leave = async () => {
    setError(null);
    try {
      await api.updateDevice(d.id, { maintenance: false });
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };
  return (
    <Card>
      <CardHeader title="Maintenance mode" subtitle="While a device is in maintenance mode, no new workers, previews or adopted containers are scheduled on it. Existing workers keep running and can still be redeployed." />
      <div className="space-y-3 p-5">
        {error && <ErrorNote>{error}</ErrorNote>}
        {d.maintenance && (
          <p className="text-sm text-neutral-600">
            {maintenanceReason(d.maintenance_note) ? <>Reason: {maintenanceReason(d.maintenance_note)}. </> : null}
            {d.maintenance_until ? `Maintenance mode ends automatically on ${new Date(d.maintenance_until).toLocaleString()}.` : "Maintenance mode remains active until you turn it off."}
          </p>
        )}
        {d.maintenance ? (
          <Button onClick={leave}>Exit maintenance mode</Button>
        ) : (
          <Button variant="primary" onClick={() => setOpen(true)}>
            Enter maintenance mode
          </Button>
        )}
      </div>
      {open && <MaintenanceModal d={d} onClose={() => setOpen(false)} onDone={onSaved} />}
    </Card>
  );
}

const hourOptions: [number, string][] = [
  [0, "Until I turn it off"],
  [1, "1 hour"],
  [4, "4 hours"],
  [24, "1 day"],
  [168, "1 week"],
];

const GB = 1024 ** 3;

/** Before/after worker counts and free RAM/disk for every device that would receive workers. Per-worker size is not tracked, so a warning means the device is already tight. */
function Impact({ targets, mine, dest, source }: { targets: Device[]; mine: Project[]; dest: Record<string, string>; source: Device }) {
  const rows = targets
    .map((t) => ({ t, add: mine.filter((p) => dest[p.id] === t.id) }))
    .filter((r) => r.add.length > 0);
  const free = (total: number, used: number) => Math.max(total - used, 0);
  return (
    <div className="rounded-md border border-neutral-200">
      <div className="border-b border-neutral-100 px-3 py-2 text-xs font-medium uppercase tracking-wide text-neutral-500">Projected impact</div>
      <ul className="divide-y divide-neutral-100">
        {rows.map(({ t, add }) => {
          const m = t.metrics;
          const ram = free(m.memory_total, m.memory_used);
          const disk = free(m.disk_total, m.disk_used);
          const lowRam = m.memory_total > 0 && (ram < 0.5 * GB || ram < m.memory_total * 0.15);
          const lowDisk = m.disk_total > 0 && (disk < 2 * GB || disk < m.disk_total * 0.1);
          const stacks = add.filter((p) => p.source.type === "compose").length;
          const before = t.containers.filter((c) => c.state === "running").length;
          return (
            <li key={t.id} className="space-y-1 px-3 py-2">
              <div className="flex flex-wrap items-baseline justify-between gap-x-3">
                <span className="font-medium">{t.name}</span>
                <span className="text-xs text-neutral-500">
                  Workers {before} → {before + add.length} (+{add.length})
                </span>
              </div>
              <div className="text-xs text-neutral-500">
                Free now: {m.memory_total ? bytes(ram) : "?"} RAM, {m.disk_total ? bytes(disk) : "?"} disk · {t.host.arch}
                {t.host.arch !== source.host.arch && <span className="text-amber-700"> · different architecture from {source.name}: image is rebuilt</span>}
              </div>
              {(lowRam || lowDisk) && (
                <div className="text-xs text-red-700">
                  Low {[lowRam && "memory", lowDisk && "disk space"].filter(Boolean).join(" and ")} on {t.name}: adding {add.length} worker{add.length === 1 ? "" : "s"} may exhaust it.
                </div>
              )}
              {stacks > 0 && <div className="text-xs text-neutral-500">{stacks} stack{stacks === 1 ? "" : "s"} will be redeployed; volumes are not copied.</div>}
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function MaintenanceModal({ d, onClose, onDone }: { d: Device; onClose: () => void; onDone: () => void }) {
  const projects = usePoll(api.projects, 0);
  const devices = usePoll(api.devices, 0);
  const mine = (projects.data ?? []).filter((p: Project) => p.device_id === d.id);
  const targets = (devices.data ?? []).filter((t) => t.id !== d.id && !t.maintenance && t.status === "online");
  const [note, setNote] = useState("");
  const [hours, setHours] = useState(0);
  // The device each worker moves to; "" means leave it where it is.
  const [dest, setDest] = useState<Record<string, string>>({});
  const [bulk, setBulk] = useState("");
  const [started, setStarted] = useState<Record<string, Migration | string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const running = Object.entries(started).filter(([, v]) => typeof v !== "string" && v.status === "running").map(([id]) => id);
  useEffect(() => {
    if (running.length === 0) return;
    const t = setInterval(async () => {
      for (const id of running) {
        try {
          const m = await api.migration(id);
          setStarted((s) => ({ ...s, [id]: m }));
        } catch {}
      }
    }, 2000);
    return () => clearInterval(t);
  }, [running.join(",")]);

  const moving = Object.keys(started).length > 0;
  const chosen = mine.filter((p) => dest[p.id]);
  const enter = async (move: boolean) => {
    setBusy(true);
    setError(null);
    try {
      await api.updateDevice(d.id, { maintenance: true, maintenance_note: note, maintenance_hours: hours });
      onDone();
      if (!move) {
        onClose();
        return;
      }
      for (const p of chosen) {
        try {
          const m = await api.migrateProject(p.id, { device_id: dest[p.id], with_data: p.source.type !== "compose", stop_source: true });
          setStarted((s) => ({ ...s, [p.id]: m }));
        } catch (err) {
          setStarted((s) => ({ ...s, [p.id]: err instanceof Error ? err.message : String(err) }));
        }
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  const label = (t: Device) => `${t.name} (${t.host.arch}${freeSpace(t) ? `, ${freeSpace(t)}` : ""})`;
  return (
    <Modal title={`Put ${d.name} into maintenance mode?`} onClose={onClose}>
      <div className="space-y-4 p-5 text-sm">
        {mine.length > 0 ? (
          <>
            <p>
              These workers run on {d.name} and will be unavailable if it goes offline, unless you migrate them to another device first. A migrated worker uses the new device's IP address, so
              anything that references the old IP must be updated. Cloudflare Tunnel routes and Cloudflare-managed DNS records are updated automatically.
            </p>
            {!moving && targets.length > 0 && (
              <Field label="Migrate all workers to">
                <select
                  className={inputClass}
                  value={bulk}
                  onChange={(e) => {
                    setBulk(e.target.value);
                    setDest(Object.fromEntries(mine.map((p) => [p.id, e.target.value])));
                  }}
                >
                  <option value="">Keep on this device</option>
                  {targets.map((t) => (
                    <option key={t.id} value={t.id}>
                      {label(t)}
                    </option>
                  ))}
                </select>
                <p className="mt-1 text-xs text-neutral-500">{d.name} is the current device, so it isn't offered as a destination.</p>
              </Field>
            )}
            <ul className="divide-y divide-neutral-100 rounded-md border border-neutral-200">
              {mine.map((p) => {
                const s = started[p.id];
                return (
                  <li key={p.id} className="grid grid-cols-1 items-center gap-2 px-3 py-2 sm:grid-cols-[minmax(0,1fr)_16rem]">
                    <span className="min-w-0 truncate">
                      {p.name}
                      {p.source.type === "compose" && <span className="ml-2 text-xs text-neutral-500">stack: redeployed, volumes not copied</span>}
                    </span>
                    {moving ? (
                      <span className="text-xs text-neutral-500 sm:text-right">
                        {typeof s === "string" ? s : s ? (s.status === "failed" ? `failed: ${s.error}` : s.status === "done" ? "moved" : s.step) : "not migrating"}
                      </span>
                    ) : (
                      <select className={`${inputClass} w-full`} value={dest[p.id] ?? ""} onChange={(e) => setDest({ ...dest, [p.id]: e.target.value })} aria-label={`Move ${p.name} to`}>
                        <option value="">Do not migrate</option>
                        {targets.map((t) => (
                          <option key={t.id} value={t.id}>
                            {label(t)}
                          </option>
                        ))}
                      </select>
                    )}
                  </li>
                );
              })}
            </ul>
            {!moving && chosen.length > 0 && <Impact targets={targets} mine={mine} dest={dest} source={d} />}
          </>
        ) : (
          <p>No workers are running on this device. New workers will be placed on other devices.</p>
        )}
        {!moving && (
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <Field label="Reason (optional)">
              <input className={inputClass} value={note} maxLength={300} onChange={(e) => setNote(e.target.value)} placeholder="e.g. Replacing the SD card" />
            </Field>
            <Field label="Ends">
              <select className={inputClass} value={hours} onChange={(e) => setHours(Number(e.target.value))}>
                {hourOptions.map(([h, l]) => (
                  <option key={h} value={h}>
                    {l}
                  </option>
                ))}
              </select>
            </Field>
          </div>
        )}
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex flex-wrap justify-end gap-2">
          <Button onClick={onClose}>{moving ? "Close" : "Cancel"}</Button>
          {!moving && (
            <>
              <Button variant={mine.length && chosen.length < mine.length ? "danger" : "primary"} disabled={busy} onClick={() => enter(false)}>
                {mine.length ? "Enter without migrating" : "Enter maintenance mode"}
              </Button>
              {chosen.length > 0 && (
                <Button variant="primary" disabled={busy} onClick={() => enter(true)}>
                  Migrate {chosen.length} and enter
                </Button>
              )}
            </>
          )}
        </div>
      </div>
    </Modal>
  );
}
