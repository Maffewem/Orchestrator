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
  copyLabel,
}: {
  value: DedicatedIP | null;
  onChange: (v: DedicatedIP | null) => void;
  device?: Device;
  projectId?: string;
  canEdit: boolean;
  /** Names the copy these settings belong to, such as "Claude (main)" or "cm4 (replica)". */
  copyLabel?: string;
}) {
  const [check, setCheck] = useState<DedicatedCheck | null>(null);
  // A network check belongs to the device it ran on (this form's device) and to the settings it
  // checked: drop it when either changes, or when the viewed copy changes.
  useEffect(() => {
    setCheck(null);
    setErr(null);
    setAutoHost(null);
  }, [device?.id, projectId, copyLabel]);
  const [busy, setBusy] = useState(false);
  // The device-side address the last check filled in; shown with a note until it is edited.
  const [autoHost, setAutoHost] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const supported = !!device?.host.capabilities?.includes("dedicated-ip");
  const nets = device?.host.network ?? [];
  const wifiOnly = nets.length > 0 && nets.every((n) => n.wireless);

  async function run() {
    if (!device || !value) return;
    if (subnetMissing) {
      setCheck(null);
      setErr("Enter the subnet mask for the VLAN first, for example 24 or 255.255.255.0.");
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      const r = await api.checkDedicated(device.id, { ...value, project_id: projectId });
      setCheck(r);
      const fillHost = !value.host_ip && !!r.suggest_host_ip;
      if ((!value.mac && r.mac) || fillHost) {
        onChange({ ...value, mac: value.mac || r.mac || "", ...(fillHost ? { host_ip: r.suggest_host_ip } : {}) });
      }
      setAutoHost(fillHost ? r.suggest_host_ip ?? null : null);
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

  // A static address on a VLAN can't be checked or saved without the VLAN's subnet mask.
  const subnetMissing = !!value && !!value.vlan && value.mode === "static" && !(value.subnet ?? "").trim();

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

  return (
    <div className="rounded-xl border border-neutral-200 bg-white p-5">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h4 className="flex items-center gap-2 text-sm font-semibold">
            <Network className="size-4" /> Own address on the network <span className="rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-800">Beta</span>
          </h4>
          <p className="mt-0.5 text-xs text-neutral-500">
            Gives this worker its own IP and MAC address, so it can use ports like 80 and 443 even when the device already does.{copyLabel ? ` These settings are for the copy on ${copyLabel}.` : ""}
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
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label="Address">
              <select className={inputClass} disabled={!canEdit} value={value.mode} onChange={(e) => set({ mode: e.target.value as DedicatedIP["mode"] })}>
                <option value="dhcp">From DHCP (reserve it in the router)</option>
                <option value="static">Static address</option>
              </select>
            </Field>
            {value.mode === "static" ? (
              <Field label="IP address" hint="A free address on this network.">
                <input className={inputClass} disabled={!canEdit} placeholder="192.168.0.201" value={value.ip ?? ""} onChange={(e) => set({ ip: e.target.value })} />
              </Field>
            ) : (
              <Field label="MAC address" hint="Reserve this MAC in your router to keep the IP fixed.">
                <div className="flex items-center gap-2">
                  <input className={`${inputClass} font-mono`} readOnly value={value.mac || "created when saved"} />
                  {value.mac && <CopyButton text={value.mac} />}
                </div>
              </Field>
            )}
          </div>
          <details className="text-sm" open={!!value.vlan || !!value.host_ip || undefined}>
            <summary className="cursor-pointer text-neutral-600">Advanced{value.vlan ? ` · VLAN ${value.vlan}` : ""}</summary>
            <div className="mt-3 grid grid-cols-1 gap-4 sm:grid-cols-2">
              <Field label="Ports on this address" hint="One per line, like 80:80. Leave empty to use the project's ports.">
                <textarea
                  className={inputClass}
                  rows={2}
                  disabled={!canEdit}
                  placeholder="Same as the project"
                  value={(value.ports ?? []).join("\n")}
                  onChange={(e) => set({ ports: e.target.value.split("\n").map((l) => l.trim()).filter(Boolean) })}
                />
              </Field>
              <Field label="Network interface" hint="Leave on Automatic unless the device has several networks.">
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
              <Field label="Address for this device to reach the worker" hint="Optional. Lets this device itself reach the worker, as Linux blocks that by default.">
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
                {autoHost && value.host_ip === autoHost && (
                  <p className="mt-1.5 text-xs text-neutral-600">Filled in for you from a free address. Change it if you like; it is only saved when you save the settings.</p>
                )}
                <p className="mt-1.5 text-xs text-amber-700">
                  Pick an address outside your router's DHCP range.
                </p>
              </Field>
            </div>
            <div className="mt-4 rounded-lg border border-neutral-200 bg-neutral-50 p-4">
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <Field label="VLAN ID" hint="Optional. Leave empty for the device's normal network.">
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
                  <Field label="Subnet mask (required)" hint="A length like 24, or a mask like 255.255.255.0.">
                    <input
                      className={`${inputClass} ${subnetMissing ? "border-bad" : ""}`}
                      disabled={!canEdit}
                      placeholder="24 or 255.255.255.0"
                      aria-invalid={subnetMissing || undefined}
                      value={value.subnet ?? ""}
                      onChange={(e) => set({ subnet: e.target.value })}
                    />
                    {subnetMissing && <span className="mt-1 block text-xs text-bad">Enter the subnet mask or prefix, for example 24 or 255.255.255.0.</span>}
                  </Field>
                )}
                {!!value.vlan && value.mode === "static" && (
                  <Field label="VLAN gateway" hint="The router's address on this VLAN.">
                    <input className={inputClass} disabled={!canEdit} placeholder="The router's address on the VLAN" value={value.gateway ?? ""} onChange={(e) => set({ gateway: e.target.value })} />
                  </Field>
                )}
              </div>
              {!!value.vlan && (
                <p className="mt-3 text-xs text-neutral-600">
                  The switch port must carry VLAN {value.vlan} tagged and the router must serve it. Other networks reach this address only if the router routes to this VLAN and its firewall allows it.
                </p>
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
                <p className={`flex items-start gap-2 ${check.vlan_reachable === "no" ? "text-amber-700" : "text-ok"}`}>
                  {check.vlan_reachable === "no" ? <AlertTriangle className="mt-0.5 size-4 shrink-0" /> : <CheckCircle2 className="mt-0.5 size-4 shrink-0" />}
                  {value.host_ip ? `The address for this device to reach the worker (${value.host_ip}) looks free. ` : ""}
                  {value.mode === "static" ? `${value.ip} looks free on ${check.subnet ?? "the network"}.` : `DHCP would offer ${check.offer ?? "an address"} on ${check.subnet ?? "the network"}.`}
                  {check.vlan_reachable === "yes" ? " The VLAN is working." : check.vlan_reachable === "no" ? " But nothing on the VLAN answered, so it may not be reaching this device." : ""}
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
          <p className="text-xs text-neutral-500">List ports as 80:80 or just 80. The left number is ignored.</p>
        </div>
      )}
    </div>
  );
}
