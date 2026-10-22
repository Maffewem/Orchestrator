import { useCan } from "../components/auth";
import { useState } from "react";
import { Link } from "react-router";
import { Plus, Server } from "lucide-react";
import { api, type Device } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CodeBlock, Empty, HealthBadge, Loading, Meter, Modal, PageHeader, StatusDot } from "../components/ui";
import { ago, bytesOrDash, cx, pct, usePoll } from "../lib";

export function AddDeviceModal({ onClose }: { onClose: () => void }) {
  const settings = usePoll(api.settings, 0);
  const [tab, setTab] = useState<"script" | "docker">("script");
  const origin = window.location.origin;
  const token = settings.data?.join_token ?? "…";
  const script = `curl -fsSL ${origin}/install.sh | sudo sh -s -- --token ${token}`;
  const docker = [
    "docker run -d --name orchestrator-agent --restart unless-stopped \\",
    "  --network host \\",
    "  -v /var/run/docker.sock:/var/run/docker.sock \\",
    "  -v /:/host:ro -v orchestrator-agent:/data \\",
    `  -e ORCH_SERVER=${origin} \\`,
    `  -e ORCH_JOIN_TOKEN=${token} \\`,
    "  ghcr.io/maffewem/orchestrator-agent:latest",
  ].join("\n");
  return (
    <Modal title="Add a device" onClose={onClose} wide>
      <p className="mb-4 text-sm text-neutral-600">
        Run this on the Raspberry Pi, VM or server you want to manage. It needs Docker installed. The agent connects out to this
        dashboard, so the device doesn't need any inbound ports. Builds for amd64, arm64 and 32-bit Raspberry Pi OS are available.
      </p>
      <div className="mb-3 inline-flex rounded-lg border border-neutral-200 bg-neutral-100 p-0.5 text-sm">
        {(["script", "docker"] as const).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={cx("rounded-md px-3 py-1", tab === t ? "bg-white font-medium shadow-sm" : "text-neutral-600")}
          >
            {t === "script" ? "Install script (systemd)" : "Docker container"}
          </button>
        ))}
      </div>
      <CodeBlock code={tab === "script" ? script : docker} />
      <p className="mt-4 text-xs text-neutral-500">
        The new device appears here within a few seconds. Add <code>--labels pi,edge</code> to the script (or <code>-e ORCH_LABELS=pi,edge</code>) to
        give it roles. Rotate the join token in Settings if it leaks; registered devices keep working.
      </p>
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
                <p className="mt-0.5">Seen {ago(d.last_seen)}</p>
              </div>
            </Link>
          ))}
        </Card>
      )}
    </>
  );
}
