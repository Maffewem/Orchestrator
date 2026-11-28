import { useState } from "react";
import { Power, RotateCw } from "lucide-react";
import { api, type Device } from "../api";
import { useCan } from "./auth";
import { Button, Card, CardHeader, ErrorNote, Field, Modal, inputClass } from "./ui";

const AGENT_TOO_OLD = "This device's agent is too old to shut down or restart the machine. Update the agent from the Updates page first.";

/** Shut down or restart the whole machine. Admin only, and only while the device is in maintenance mode. */
export default function DevicePower({ d, onChanged }: { d: Device; onChanged: () => void }) {
  const isAdmin = useCan("admin");
  const [mode, setMode] = useState<"shutdown" | "restart" | null>(null);
  if (!isAdmin) return null;
  const capable = d.host.capabilities?.includes("power") ?? false;
  const pending = d.power;
  const disabledWhy = pending
    ? null
    : !d.maintenance
      ? "Put the device into maintenance mode first. That keeps new work off it while it is down."
      : d.status === "offline"
        ? "The device is offline."
        : !capable
          ? AGENT_TOO_OLD
          : null;
  const blocked = !!disabledWhy || !!pending;
  return (
    <Card>
      <CardHeader title="Shut down or restart" subtitle="Stops every container gracefully, flushes the disks, then shuts the machine down or restarts it." />
      <div className="space-y-3 p-5">
        {pending && (
          <p className="text-sm text-neutral-600">
            {pending.mode === "restart"
              ? `${pending.by} asked this device to restart. It is shown as Restarting until the agent reconnects after boot.`
              : `${pending.by} asked this device to shut down. It cannot be started again from the dashboard: switch it on at the device (power, or the blade or BMC).`}
            {pending.exit_maintenance ? " Maintenance mode ends when it is back." : " It stays in maintenance mode when it is back."}
          </p>
        )}
        {disabledWhy && <p className="text-sm text-neutral-500">{disabledWhy}</p>}
        <div className="flex flex-wrap gap-2">
          <Button icon={<RotateCw className="size-4" />} disabled={blocked} onClick={() => setMode("restart")}>
            Restart…
          </Button>
          <Button variant="danger" icon={<Power className="size-4" />} disabled={blocked} onClick={() => setMode("shutdown")}>
            Shut down…
          </Button>
        </div>
      </div>
      {mode && <PowerModal d={d} mode={mode} onClose={() => setMode(null)} onDone={onChanged} />}
    </Card>
  );
}

function PowerModal({ d, mode, onClose, onDone }: { d: Device; mode: "shutdown" | "restart"; onClose: () => void; onDone: () => void }) {
  const [confirm, setConfirm] = useState("");
  const [exit, setExit] = useState(mode === "restart");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const imp = d.power_impact;
  const restart = mode === "restart";
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.powerDevice(d.id, mode, confirm, exit);
      onDone();
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  };
  return (
    <Modal title={`${restart ? "Restart" : "Shut down"} ${d.name}?`} onClose={onClose}>
      <form onSubmit={submit} className="space-y-4">
        {imp?.hosts_server && (
          <ErrorNote>
            <strong>The Orchestrator server runs on this device.</strong> The dashboard goes offline while it is {restart ? "restarting" : "shut down"}, and every other device loses its control plane.{" "}
            {restart
              ? "The server comes back by itself after boot as long as its container has a restart policy (the default install does), and the dashboard reconnects."
              : "It will not come back until someone switches the device on."}
            {imp.ha_paired && " A standby server is paired: it is promoted by hand and does not take over automatically."}
          </ErrorNote>
        )}
        <ul className="list-disc space-y-1 pl-5 text-sm text-neutral-700">
          <li>
            Docker is stopped first, so {imp ? `the ${imp.running_workers} running container${imp.running_workers === 1 ? "" : "s"} get` : "running containers get"} their stop signal and grace period. Disks are flushed before power-off.
          </li>
          {restart ? (
            <li>Containers start again after boot according to their restart policy. Containers set to “no” stay stopped. The device shows as Restarting and keeps its maintenance mode unless you choose otherwise below.</li>
          ) : (
            <li>
              <strong>A shut-down device can't be started again from the dashboard.</strong> Someone has to switch it on at the device, or through its blade or BMC.
            </li>
          )}
          <li>
            Its workers stay down while it is away: failover skips devices in maintenance mode
            {imp?.cluster_enabled && imp.failover_workers > 0 ? `, so the ${imp.failover_workers} worker${imp.failover_workers === 1 ? "" : "s"} here with a failover policy will not move.` : ". Migrate any worker that must keep running first."}
          </li>
        </ul>
        {restart && (
          <label className="flex items-start gap-2 text-sm">
            <input type="checkbox" className="mt-0.5" checked={exit} onChange={(e) => setExit(e.target.checked)} />
            Leave maintenance mode automatically once the device is back
          </label>
        )}
        <Field label={`Type ${d.name} to confirm`}>
          <input className={inputClass} value={confirm} onChange={(e) => setConfirm(e.target.value)} autoFocus autoComplete="off" />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex flex-wrap justify-end gap-2">
          <Button type="button" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="danger" disabled={busy || confirm.trim() !== d.name}>
            {busy ? "Sending…" : restart ? "Restart device" : "Shut down device"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}
