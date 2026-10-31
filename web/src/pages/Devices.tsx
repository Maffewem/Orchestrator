import { useCan } from "../components/auth";
import { useState, type ReactNode } from "react";
import { Link } from "react-router";
import { Plus, Server } from "lucide-react";
import { api, type Device } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CodeBlock, Empty, Field, HealthBadge, Loading, Meter, Modal, PageHeader, StatusDot, Toggle, inputClass } from "../components/ui";
import { ago, bytesOrDash, cx, pct, usePoll } from "../lib";

const STEPS = ["Basics", "Networking", "Options", "Install"] as const;

export function AddDeviceModal({ onClose }: { onClose: () => void }) {
  const settings = usePoll(api.settings, 0);
  const [step, setStep] = useState(0);
  const [name, setName] = useState("");
  const [labels, setLabels] = useState("");
  const [installDocker, setInstallDocker] = useState(true);
  const [proxy, setProxy] = useState(false);
  const [tunnel, setTunnel] = useState(false);
  const [tunnelToken, setTunnelToken] = useState("");
  const [tunnelVia, setTunnelVia] = useState<"" | "proxy">("");
  const [maintenance, setMaintenance] = useState(false);
  const [note, setNote] = useState("");
  const [tab, setTab] = useState<"script" | "docker">("script");
  const [planId, setPlanId] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const origin = window.location.origin;
  const token = settings.data?.join_token ?? "…";
  const needsPlan = proxy || (tunnel && tunnelToken.trim() !== "") || maintenance;

  const next = async () => {
    setError("");
    if (step === 2) {
      // Secrets stay on the server: the command only carries the plan's ID.
      setBusy(true);
      try {
        setPlanId(
          needsPlan
            ? (await api.createDeviceSetup({
                proxy: proxy || (tunnel && tunnelVia === "proxy"),
                tunnel_token: tunnel ? tunnelToken : undefined,
                tunnel_via: tunnel ? tunnelVia : "",
                maintenance,
                maintenance_note: note,
              })).id
            : null,
        );
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setBusy(false);
        return;
      }
      setBusy(false);
    }
    setStep(step + 1);
  };

  const q = (v: string) => `'${v.replace(/'/g, "'\\''")}'`;
  const flags = [
    `--token ${token}`,
    name.trim() && `--name ${q(name.trim())}`,
    labels.trim() && `--labels ${q(labels.trim().replace(/\s+/g, ""))}`,
    !installDocker && "--no-docker-install",
    planId && `--setup ${planId}`,
  ].filter(Boolean);
  const script = `curl -fsSL ${origin}/install.sh | sudo sh -s -- ${flags.join(" ")}`;
  const docker = [
    "docker run -d --name orchestrator-agent --restart unless-stopped \\",
    "  --network host \\",
    "  -v /var/run/docker.sock:/var/run/docker.sock \\",
    "  -v /:/host:ro -v orchestrator-agent:/data \\",
    `  -e ORCH_SERVER=${origin} \\`,
    `  -e ORCH_JOIN_TOKEN=${token} \\`,
    name.trim() && `  -e ORCH_NAME=${q(name.trim())} \\`,
    labels.trim() && `  -e ORCH_LABELS=${q(labels.trim().replace(/\s+/g, ""))} \\`,
    planId && `  -e ORCH_SETUP=${planId} \\`,
    "  ghcr.io/maffewem/orchestrator-agent:latest",
  ].filter(Boolean).join("\n");

  const Choice = ({ on, set, title, children }: { on: boolean; set: (v: boolean) => void; title: string; children: ReactNode }) => (
    <div className="flex items-start justify-between gap-4 rounded-lg border border-neutral-200 p-3">
      <div className="min-w-0 text-sm">
        <p className="font-medium">{title}</p>
        <p className="mt-0.5 text-xs text-neutral-500">{children}</p>
      </div>
      <Toggle checked={on} onChange={set} label={title} />
    </div>
  );

  return (
    <Modal title="Add a device" onClose={onClose} wide>
      <ol className="mb-5 flex gap-2 text-xs">
        {STEPS.map((t, i) => (
          <li key={t} className={cx("flex-1 rounded-md px-2 py-1.5 text-center", i === step ? "bg-primary font-medium text-white" : i < step ? "bg-neutral-100 text-neutral-700" : "bg-neutral-50 text-neutral-400")}>
            {i + 1}. {t}
          </li>
        ))}
      </ol>
      {step === 0 && (
        <div className="space-y-4">
          <p className="text-sm text-neutral-600">
            Everything here is optional. The agent connects out to this dashboard, so the device needs no inbound ports.
          </p>
          <Field label="Device name" hint="Defaults to the hostname.">
            <input className={inputClass} value={name} onChange={(e) => setName(e.target.value)} placeholder="pi-kitchen" />
          </Field>
          <Field label="Labels" hint="Comma-separated roles, e.g. pi,edge.">
            <input className={inputClass} value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="pi,edge" />
          </Field>
          <Choice on={installDocker} set={setInstallDocker} title="Install Docker if it is missing">
            Turn off if Docker must already be installed; the script then stops instead of installing it.
          </Choice>
        </div>
      )}
      {step === 1 && (
        <div className="space-y-3">
          <p className="text-sm text-neutral-600">Does this device serve websites? You can change all of this later on its pages.</p>
          <Choice on={proxy || (tunnel && tunnelVia === "proxy")} set={setProxy} title="Web domains (Caddy proxy)">
            Runs a proxy container so workers get <code>name.your-domain</code> URLs, custom domains, HTTPS and optional login. Needs ports 80/443 free.
          </Choice>
          <Choice on={tunnel} set={setTunnel} title="Cloudflare Tunnel">
            Publishes workers through Cloudflare without opening ports; good behind CGNAT or a home router.
          </Choice>
          {tunnel && (
            <div className="space-y-3 rounded-lg bg-neutral-50 p-3">
              <Field label="Tunnel token" hint="Paste the token or the whole cloudflared command. Leave blank to add it later. It is stored on the server, not in the install command.">
                <input className={inputClass} value={tunnelToken} onChange={(e) => setTunnelToken(e.target.value)} placeholder="eyJhIjoi…" />
              </Field>
              <Field label="Send tunnel traffic" hint="Direct is lightest; through the proxy adds login, path routes and preview URLs.">
                <select className={inputClass} value={tunnelVia} onChange={(e) => setTunnelVia(e.target.value as "" | "proxy")}>
                  <option value="">Straight to the worker</option>
                  <option value="proxy">Through the device proxy</option>
                </select>
              </Field>
            </div>
          )}
          {!proxy && !tunnel && <p className="text-xs text-neutral-500">Neither: workers are reached by <code>http://device-ip:port</code> on your network.</p>}
        </div>
      )}
      {step === 2 && (
        <div className="space-y-3">
          <Choice on={maintenance} set={setMaintenance} title="Start in maintenance mode">
            Keeps new workers off the device until you release it, handy while you burn it in or finish configuring it.
          </Choice>
          {maintenance && (
            <Field label="Note">
              <input className={inputClass} value={note} onChange={(e) => setNote(e.target.value)} placeholder="Burn-in until Friday" />
            </Field>
          )}
          <p className="text-xs text-neutral-500">Backups are set per worker once it exists, so there is nothing to choose here.</p>
          {error && <p className="text-sm text-red-600">{error}</p>}
        </div>
      )}
      {step === 3 && (
        <div>
          <p className="mb-3 text-sm text-neutral-600">
            Run this on the Raspberry Pi, VM or server. Builds exist for amd64, arm64 and 32-bit Raspberry Pi OS.
          </p>
          <div className="mb-3 inline-flex rounded-lg border border-neutral-200 bg-neutral-100 p-0.5 text-sm">
            {(["script", "docker"] as const).map((t) => (
              <button key={t} onClick={() => setTab(t)} className={cx("rounded-md px-3 py-1", tab === t ? "bg-white font-medium shadow-sm" : "text-neutral-600")}>
                {t === "script" ? "Install script (systemd)" : "Docker container"}
              </button>
            ))}
          </div>
          <CodeBlock code={tab === "script" ? script : docker} />
          <p className="mt-4 text-xs text-neutral-500">
            {planId
              ? "The chosen setup is applied once, when the device first joins. "
              : ""}
            The device appears here within a few seconds. Rotate the join token in Settings if it leaks; registered devices keep working.
          </p>
        </div>
      )}
      <div className="mt-6 flex justify-between">
        <Button onClick={() => (step === 0 ? onClose() : setStep(step - 1))}>{step === 0 ? "Cancel" : "Back"}</Button>
        {step < 3 ? (
          <Button variant="primary" loading={busy} onClick={next}>
            {step === 2 ? "Generate command" : "Next"}
          </Button>
        ) : (
          <Button variant="primary" onClick={onClose}>Done</Button>
        )}
      </div>
    </Modal>
  );
}

export function DeviceSummary({ d }: { d: Device }) {
  return (
    <span className="truncate">
      {[d.host.model || d.host.platform || d.host.os, d.host.arch, d.host.addresses?.[0]].filter(Boolean).join(" · ")}
    </span>
  );
}

export default function DevicesPage() {
  useCrumbs([{ label: "Devices" }]);
  const { data } = usePoll(api.devices, 5000);
  const [adding, setAdding] = useState(false);
  const isAdmin = useCan("admin");

  return (
    <>
      <PageHeader
        title="Devices"
        description="Raspberry Pis, VMs and servers running the Orchestrator agent."
        actions={
          isAdmin && (
            <Button variant="primary" icon={<Plus className="size-4" />} onClick={() => setAdding(true)}>
              Add device
            </Button>
          )
        }
      />
      {adding && <AddDeviceModal onClose={() => setAdding(false)} />}
      {!data ? (
        <Loading />
      ) : data.length === 0 ? (
        <Card>
          <Empty icon={<Server className="size-8" />} title="No devices yet">
            Install the agent on a device to start managing it.
            <div className="mt-4">
              <Button variant="primary" onClick={() => setAdding(true)}>
                Add device
              </Button>
            </div>
          </Empty>
        </Card>
      ) : (
        <Card className="divide-y divide-neutral-100">
          {data.map((d) => (
            <Link key={d.id} to={`/devices/${d.id}`} className="grid items-center gap-4 px-5 py-4 hover:bg-neutral-50 md:grid-cols-[minmax(0,2fr)_minmax(0,2fr)_auto]">
              <div className="min-w-0">
                <p className="flex items-center gap-2 font-medium">
                  <StatusDot status={d.status} />
                  <span className="truncate">{d.name}</span>
                  <HealthBadge health={d.health} status={d.status} />
                  {d.maintenance && <Badge tone="warn">maintenance</Badge>}
                </p>
                <p className="mt-1 flex text-xs text-neutral-500">
                  <DeviceSummary d={d} />
                </p>
                {d.labels.length > 0 && (
                  <div className="mt-2 flex flex-wrap gap-1">
                    {d.labels.map((l) => (
                      <Badge key={l}>{l}</Badge>
                    ))}
                  </div>
                )}
              </div>
              <div className="grid grid-cols-3 gap-4">
                <Meter label={`CPU · ${d.host.cpus ? `${d.host.cpus} ${d.host.cpus === 1 ? "core" : "cores"}` : "—"}`} value={d.metrics.cpu_percent} />
                <Meter label={`Memory · ${bytesOrDash(d.metrics.memory_total)}`} value={pct(d.metrics.memory_used, d.metrics.memory_total)} />
                <Meter label={`Disk · ${bytesOrDash(d.metrics.disk_total)}`} value={pct(d.metrics.disk_used, d.metrics.disk_total)} />
              </div>
              <div className="text-xs text-neutral-500 md:text-right">
                <p>{d.containers.filter((c) => c.state === "running").length}/{d.containers.length} containers</p>
                <p className="mt-0.5">{d.metrics.temp_c ? `${d.metrics.temp_c.toFixed(0)}°C · ` : ""}Load {d.metrics.load5.toFixed(2)}</p>
                <p className="mt-0.5">Seen {ago(d.last_seen)}</p>
              </div>
            </Link>
          ))}
        </Card>
      )}
    </>
  );
}
