import { useEffect, useState, type ReactNode } from "react";
import { AlertTriangle, CheckCircle2, Network } from "lucide-react";
import { api, type DedicatedCheck, type DedicatedIP, type Device } from "../api";
import { Button, CopyButton, Field, Toggle, inputClass } from "./ui";

/**
 * Settings for a worker's own address on the device's network (Docker macvlan), so it can use ports
 * such as 80 and 443 that the device already holds. The address comes from the router's DHCP
 * server (under the worker's own MAC, so the router can reserve it) or is set by hand.
 */
export default function DedicatedAddress({
  value,
  onChange,
  device,
  projectId,
  canEdit,
  replicaView,
}: {
  value: DedicatedIP | null;
  onChange: (v: DedicatedIP | null) => void;
  device?: Device;
  projectId?: string;
  canEdit: boolean;
  /** Set to the replica's device name while a replica is being viewed. */
  replicaView?: string;
}) {
  const [check, setCheck] = useState<DedicatedCheck | null>(null);
  // A network check belongs to the device it ran on (this form's device) and to the settings it
  // checked: drop it when either changes, or when the viewed copy changes.
  useEffect(() => {
    setCheck(null);
    setErr(null);
  }, [device?.id, projectId, replicaView]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const supported = !!device?.host.capabilities?.includes("dedicated-ip");
  const nets = device?.host.network ?? [];
  const wifiOnly = nets.length > 0 && nets.every((n) => n.wireless);

  async function run() {
    if (!device || !value) return;
    setBusy(true);
    setErr(null);
    try {
      const r = await api.checkDedicated(device.id, { ...value, project_id: projectId });
      setCheck(r);
      if (!value.mac && r.mac) onChange({ ...value, mac: r.mac });
    } catch (e) {
      setCheck(null);
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }
  const set = (patch: Partial<DedicatedIP>) => {
    setCheck(null);
    if (value) onChange({ ...value, ...patch });
  };

  // Result of checking the typed address, shown right under the field.
  const hostPrefix = "Address for this device to reach the worker:";
  const agentCanCheckHost = !!device?.host.capabilities?.includes("dedicated-dhcp-raw");
  const hostProblems = (check?.problems ?? []).filter((p) => p.startsWith(hostPrefix));
  let hostStatus: ReactNode = null;
  if (value?.host_ip) {
    if (supported && !agentCanCheckHost) {
      hostStatus = (
        <p className="mt-1.5 flex items-start gap-2 text-xs text-amber-700">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0" /> This device's agent{device?.host.agent_version ? ` (${device.host.agent_version})` : ""} is too old to check this address on the network. Update it from the Updates page. Until then only clashes known to Orchestrator and the subnet are checked, when you save.
        </p>
      );
    } else if (busy) {
      hostStatus = <p className="mt-1.5 text-xs text-neutral-500">Checking on the network…</p>;
    } else if (err) {
      hostStatus = <p className="mt-1.5 text-xs text-bad">Couldn't check: {err}</p>;
    } else if (hostProblems.length > 0) {
      hostStatus = hostProblems.map((p) => (
        <p key={p} className="mt-1.5 flex items-start gap-2 text-xs text-bad">
          <AlertTriangle className="mt-0.5 size-3.5 shrink-0" /> {p.slice(hostPrefix.length).trim()}
        </p>
      ));
    } else if (check && !check.problems?.length) {
      hostStatus = (
        <p className="mt-1.5 flex items-start gap-2 text-xs text-ok">
          <CheckCircle2 className="mt-0.5 size-3.5 shrink-0" /> {value.host_ip} looks free on {check.subnet ?? "the network"}.
        </p>
      );
    } else if (!check) {
      hostStatus = <p className="mt-1.5 text-xs text-neutral-500">Not checked yet. It is checked when you leave this field, or with "Check on the network".</p>;
    }
  }

  // A replica never takes the worker's own address, so nothing about this section applies to the
  // copy being viewed. The settings (and the interface list and checks, which describe the main
  // device) are the main copy's: show only a summary, not controls that look like the replica's.
  if (replicaView) {
    return (
      <div className="rounded-xl border border-neutral-200 bg-white p-5">
        <h4 className="flex items-center gap-2 text-sm font-semibold">
          <Network className="size-4" /> Own address on the network
        </h4>
        <p className="mt-2 text-sm text-neutral-600">
          You are viewing the copy on {replicaView}, a replica. Replicas run on their device's normal ports and ignore the worker's own address and VLAN, so there is nothing to set or check for this copy.
        </p>
        <p className="mt-2 text-sm text-neutral-600">
          {value
            ? `The main copy${device ? ` on ${device.name}` : ""} uses its own address (${value.mode === "static" ? value.ip || "static" : "from DHCP"}${value.vlan ? `, VLAN ${value.vlan}` : ""}). Switch to the main copy to change it.`
            : `The main copy${device ? ` on ${device.name}` : ""} has no own address. Switch to the main copy to set one.`}
        </p>
      </div>
    );
  }

  return (
    <div className="rounded-xl border border-neutral-200 bg-white p-5">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h4 className="flex items-center gap-2 text-sm font-semibold">
            <Network className="size-4" /> Own address on the network <span className="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800">Beta</span>
          </h4>
          <p className="mt-0.5 text-xs text-neutral-500">
            Gives this worker its own IP and MAC address on the device's network, so it can listen on ports like 80 and 443 even when the device uses them. Other devices reach it at that address; it doesn't use the device's ports.
          </p>
        </div>
        <Toggle
          checked={!!value}
          label="Own address on the network"
          onChange={(on) => {
            if (!canEdit) return;
            setCheck(null);
            onChange(on ? { mode: "dhcp" } : null);
          }}
        />
      </div>
      {!canEdit && <p className="mt-3 text-sm text-neutral-500">Only administrators can change this, because it puts the worker directly on your network.</p>}
      {device && !supported && (
        <p className="mt-3 flex items-start gap-2 text-sm text-amber-700">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" /> This device's agent is too old for this. Update it from the Updates page, or re-run the install command on the device.
        </p>
      )}
      {wifiOnly && (
        <p className="mt-3 flex items-start gap-2 text-sm text-amber-700">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" /> This device is on Wi-Fi. Access points allow one MAC address per client, so this normally doesn't work. Use a wired device.
        </p>
      )}
      {value && (
        <div className="mt-4 space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Address">
              <select className={inputClass} disabled={!canEdit} value={value.mode} onChange={(e) => set({ mode: e.target.value as DedicatedIP["mode"] })}>
                <option value="dhcp">From DHCP (reserve it in the router)</option>
                <option value="static">Static address</option>
              </select>
            </Field>
            {value.mode === "static" ? (
              <Field label="IP address" hint="Must be inside the device's network and not used by anything else.">
                <input className={inputClass} disabled={!canEdit} placeholder="192.168.0.201" value={value.ip ?? ""} onChange={(e) => set({ ip: e.target.value })} />
              </Field>
            ) : (
              <Field label="MAC address" hint="Reserve an address for this MAC in your router's DHCP settings to keep the IP fixed. It is generated once and kept.">
                <div className="flex items-center gap-2">
                  <input className={`${inputClass} font-mono`} readOnly value={value.mac || "created when saved"} />
                  {value.mac && <CopyButton text={value.mac} />}
                </div>
              </Field>
            )}
          </div>
          <details className="text-sm" open={!!value.vlan || !!value.host_ip || undefined}>
            <summary className="cursor-pointer text-neutral-600">Advanced{value.vlan ? ` · VLAN ${value.vlan}` : ""}</summary>
            <div className="mt-3 grid gap-4 sm:grid-cols-2">
              <Field label="Network interface" hint="Empty: the one with the default route.">
                <select className={inputClass} disabled={!canEdit} value={value.parent ?? ""} onChange={(e) => set({ parent: e.target.value })}>
                  <option value="">Automatic</option>
                  {nets.map((n) => (
                    <option key={n.interface} value={n.interface}>
                      {n.interface} ({n.address}
                      {n.wireless ? ", Wi-Fi" : ""})
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="Address for this device to reach the worker" hint="Optional spare address that lets this device itself, and its Caddy and Cloudflare Tunnel, reach the worker. Linux blocks a device from reaching its own macvlan workers, so the agent adds a small interface on the device with this address. Checked on the network when you leave the field.">
                <div className="flex items-center gap-2">
                  <input
                    className={inputClass}
                    disabled={!canEdit}
                    placeholder="A spare address on this network"
                    value={value.host_ip ?? ""}
                    onChange={(e) => set({ host_ip: e.target.value })}
                    onBlur={() => {
                      if (value.host_ip && canEdit && supported && device?.status !== "offline") void run();
                    }}
                  />
                  {canEdit && !value.host_ip && (
                    <Button type="button" onClick={run} loading={busy} disabled={!device || !supported || device.status === "offline"}>
                      Suggest
                    </Button>
                  )}
                </div>
                {hostStatus}
                <p className="mt-1.5 text-xs text-amber-700">
                  The device holds this address itself, so nothing else may use it. Choose one <strong>outside your router's DHCP range</strong>, or reserve it there, or the router may hand it to another device.
                </p>
              </Field>
            </div>
            <div className="mt-4 rounded-lg border border-neutral-200 bg-neutral-50 p-4">
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="VLAN ID" hint="Optional, a number from 1 to 4094. Leave empty for the device's normal network: nothing VLAN-related is created.">
                  <input
                    className={inputClass}
                    disabled={!canEdit}
                    inputMode="numeric"
                    placeholder="None"
                    value={value.vlan ?? ""}
                    onChange={(e) => {
                      const n = parseInt(e.target.value.replace(/\D/g, ""), 10);
                      set({ vlan: Number.isFinite(n) ? Math.min(n, 4094) : undefined });
                    }}
                  />
                </Field>
                {!!value.vlan && value.mode === "static" && (
                  <Field label="VLAN subnet" hint="The VLAN's network, like 192.168.5.0/24. It isn't taken from the VLAN ID.">
                    <input className={inputClass} disabled={!canEdit} placeholder="Like 192.168.5.0/24" value={value.subnet ?? ""} onChange={(e) => set({ subnet: e.target.value })} />
                  </Field>
                )}
                {!!value.vlan && value.mode === "static" && (
                  <Field label="VLAN gateway" hint="The router's address on the VLAN. Needed for the worker to reach other networks.">
                    <input className={inputClass} disabled={!canEdit} placeholder="The router's address on the VLAN" value={value.gateway ?? ""} onChange={(e) => set({ gateway: e.target.value })} />
                  </Field>
                )}
              </div>
              {!!value.vlan && (
                <div className="mt-3 space-y-1.5 text-xs text-neutral-600">
                  <p>
                    The agent adds a tagged interface <span className="font-mono">{(value.parent || "eth0")}.{value.vlan}</span> on the device's network port and attaches the worker to it, so one cable can serve several VLANs. For example, VLAN ID 1005 for the 192.168.5.0/24 network gives <span className="font-mono">eth0.1005</span>.
                  </p>
                  <p>
                    Before this works: the <strong>switch port must carry VLAN {value.vlan} tagged</strong> (a trunk, alongside the untagged network), and the <strong>router must serve it</strong>: {value.mode === "dhcp" ? "DHCP for the VLAN (the subnet is learned from it)" : "a gateway address on the VLAN"} and routing to the networks that should reach the worker.
                  </p>
                  <p>The device itself still can't reach the worker at that address unless you set an address for this device to reach the worker, which must be a spare address on the VLAN's subnet.</p>
                </div>
              )}
            </div>
            {value.mode === "static" && value.mac && (
              <p className="mt-3 text-xs text-neutral-500">
                MAC address: <span className="font-mono">{value.mac}</span>
              </p>
            )}
          </details>
          <div className="flex flex-wrap items-center gap-3">
            <Button type="button" onClick={run} loading={busy} disabled={!device || !supported || device.status === "offline"}>
              Check on the network
            </Button>
            {device?.status === "offline" && <span className="text-xs text-neutral-500">The device is offline.</span>}
          </div>
          {err && <p className="text-sm text-bad">{err}</p>}
          {check && (
            <div className="space-y-1.5 text-sm">
              <p className="text-xs text-neutral-500">Checked on {device?.name ?? "this device"}.</p>
              {check.problems?.map((p) => (
                <p key={p} className="flex items-start gap-2 text-bad">
                  <AlertTriangle className="mt-0.5 size-4 shrink-0" /> {p}
                </p>
              ))}
              {!check.problems?.length && (
                <p className="flex items-start gap-2 text-ok">
                  <CheckCircle2 className="mt-0.5 size-4 shrink-0" />
                  {value.host_ip ? `The address for this device to reach the worker (${value.host_ip}) looks free. ` : ""}
                  {value.mode === "static" ? `${value.ip} looks free on ${check.subnet ?? "the network"}.` : `DHCP would offer ${check.offer ?? "an address"} on ${check.subnet ?? "the network"}.`}
                </p>
              )}
              {check.suggest_host_ip && !value.host_ip && (
                <p className="flex flex-wrap items-center gap-2 text-neutral-700">
                  Suggested address for this device to reach the worker: <span className="font-mono font-medium">{check.suggest_host_ip}</span>
                  <Button type="button" onClick={() => set({ host_ip: check.suggest_host_ip })}>
                    Use suggestion
                  </Button>
                  <span className="text-xs text-neutral-500">Not saved until you save the settings. Outside the DHCP range? Check your router.</span>
                </p>
              )}
              {check.notes?.map((n) => (
                <p key={n} className="text-neutral-600">{n}</p>
              ))}
            </div>
          )}
          <p className="text-xs text-neutral-500">
            Ports are served on the worker's own address (list them as 80:80 or just 80; the left number is ignored). Domains and the Cloudflare Tunnel reach the worker's address only if you set the address for this device to reach the worker above. Previews don't apply, and replicas keep using their devices' normal ports. Moving the worker stops the old copy first.
          </p>
        </div>
      )}
    </div>
  );
}
