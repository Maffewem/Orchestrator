import { useEffect, useState } from "react";
import { Fan, ThermometerSun } from "lucide-react";
import { api, type Device, type ThermalPolicy } from "../api";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, Loading, inputClass } from "./ui";
import { confirmDialog, notify } from "./dialogs";
import { usePoll } from "../lib";

const defaultCurve = [
  { c: 40, pct: 30 },
  { c: 55, pct: 50 },
  { c: 70, pct: 80 },
  { c: 80, pct: 100 },
];

/** Fan control and thermal protection for one device (admin only). */
export default function Thermal({ d }: { d: Device }) {
  const v = usePoll(() => api.thermal(d.id), 5000, [d.id]);
  const [p, setP] = useState<ThermalPolicy | null>(null);
  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (v.data && !dirty) setP(v.data.policy);
  }, [v.data, dirty]);

  if (v.error && !v.data) return <ErrorNote>{v.error}</ErrorNote>;
  if (!v.data || !p) return <Loading />;
  const { status, supported, min_pct: minPct, crit_c: crit } = v.data;
  const fan = status?.fan;
  const canFan = !!fan?.controllable;
  const edit = (f: (x: ThermalPolicy) => ThermalPolicy) => {
    setP(f(p));
    setDirty(true);
  };
  const pr = p.protect;
  const curve = p.curve?.length ? p.curve : defaultCurve;
  const workers = d.containers.map((c) => c.name.replace(/^\//, "")).filter((n) => !n.startsWith("orchestrator"));
  const toggle = (list: string[] | undefined, name: string) => ((list ?? []).includes(name) ? (list ?? []).filter((x) => x !== name) : [...(list ?? []), name]);

  const save = async () => {
    const stopping = (pr.enabled ? pr.stop ?? [] : []).filter((n) => !(p.critical ?? []).includes(n));
    const ok = await confirmDialog({
      title: `Apply thermal policy to ${d.name}?`,
      message:
        `This changes hardware and workloads on the device itself, as root. ` +
        `Fan: ${p.fan_mode === "auto" ? "left to the device's own controller" : p.fan_mode === "manual" ? `fixed at ${Math.max(p.fan_manual, p.min_pct)}%` : "follows the curve"}. ` +
        (pr.enabled
          ? `Protection: after ${pr.temp_c}°C for ${pr.for_seconds}s it ${pr.fan_full ? "sets the fan to 100%" : ""}${pr.fan_full && stopping.length ? " and " : ""}${stopping.length ? `stops ${stopping.join(", ")}` : ""}, and starts them again below ${pr.resume_below_c}°C. `
          : "Protection is off. ") +
        `The device applies this by itself, even when the server is unreachable.`,
      confirmLabel: "Apply",
      danger: pr.enabled && stopping.length > 0,
    });
    if (!ok) return;
    setSaving(true);
    setError(null);
    try {
      const next = { ...p, curve: p.fan_mode === "curve" ? curve : p.curve };
      await api.setThermal(d.id, next);
      setDirty(false);
      notify("Thermal policy saved. The device picks it up within a few seconds.", "success");
      v.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  const num = (n: number, set: (x: number) => void, extra = "") => (
    <input className={inputClass + extra} type="number" inputMode="decimal" value={Number.isFinite(n) ? n : ""} onChange={(e) => set(e.target.value === "" ? NaN : Number(e.target.value))} />
  );

  return (
    <div className="space-y-4">
      {!supported && <ErrorNote>This device's agent is too old for fan control and thermal protection. Update it from the Updates page.</ErrorNote>}
      {status?.protecting && (
        <Card className="border-red-200 bg-red-50/50">
          <div className="px-5 py-3 text-sm">
            <b>Thermal protection is active.</b> {status.stopped?.length ? `Stopped: ${status.stopped.join(", ")}. They start again once the device cools below ${pr.resume_below_c}°C.` : "The fan is at full speed."}
          </div>
        </Card>
      )}
      {status?.failsafe && (
        <Card className="border-amber-200 bg-amber-50/50">
          <div className="px-5 py-3 text-sm">Fan forced to full speed as a precaution: {status.failsafe}.</div>
        </Card>
      )}

      <Card>
        <CardHeader title="Fan" subtitle="Detected fan, and how it is controlled." action={<Fan className="size-5 text-neutral-400" />} />
        <div className="space-y-5 p-5">
          {!fan ? (
            <p className="text-sm text-neutral-500">
              No fan the agent can read was found. Fan control appears when the device exposes a PWM fan through Linux (such as the Raspberry Pi 5 fan), or a Compute Blade fan unit is set up
              (see the agent's <code>ORCH_PWM_FAN</code> and <code>bladectl</code> options).
            </p>
          ) : (
            <>
              <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm sm:grid-cols-4">
                <div><dt className="text-neutral-500">Fan</dt><dd className="font-medium">{fan.name}</dd></div>
                <div><dt className="text-neutral-500">Speed</dt><dd className="font-medium tabular-nums">{fan.percent >= 0 ? `${fan.percent}%` : "—"}{fan.rpm ? ` · ${fan.rpm} RPM` : ""}</dd></div>
                <div><dt className="text-neutral-500">Control</dt><dd>{canFan ? <Badge tone="ok">Controllable</Badge> : <Badge>Read only</Badge>}</dd></div>
                <div><dt className="text-neutral-500">Driver</dt><dd>{fan.driver}</dd></div>
              </dl>
              {fan.note && <p className="text-xs text-neutral-500">{fan.note}</p>}
            </>
          )}
          {canFan && (
            <>
              <p className="rounded-lg bg-neutral-50 px-3 py-2 text-xs text-neutral-600">
                Two blades can share one fan unit. Only one of them should control it: leave the other on Auto, or its curve will fight this one. The standard fan unit can only be set from the blade in Port A.
              </p>
              <Field label="Mode" hint="Auto leaves the fan to the device's own controller. The agent applies manual and curve modes by itself.">
                <select className={inputClass} value={p.fan_mode} onChange={(e) => edit((x) => ({ ...x, fan_mode: e.target.value as ThermalPolicy["fan_mode"], curve: x.curve?.length ? x.curve : defaultCurve }))}>
                  <option value="auto">Auto (device controller)</option>
                  <option value="manual">Manual (fixed speed)</option>
                  <option value="curve">Curve (temperature to speed)</option>
                </select>
              </Field>
              <Field label={`Minimum speed (${minPct}% or more)`} hint="The fan never runs slower than this in manual or curve mode.">
                {num(p.min_pct, (n) => edit((x) => ({ ...x, min_pct: n })))}
              </Field>
              {p.fan_mode === "manual" && <Field label="Fixed speed (%)">{num(p.fan_manual, (n) => edit((x) => ({ ...x, fan_manual: n })))}</Field>}
              {p.fan_mode === "curve" && (
                <div className="space-y-2">
                  <p className="text-sm font-medium">Curve</p>
                  {curve.map((pt, i) => (
                    <div key={i} className="flex items-center gap-2">
                      <div className="min-w-0 flex-1">{num(pt.c, (n) => edit((x) => ({ ...x, curve: curve.map((q, j) => (j === i ? { ...q, c: n } : q)) })))}</div>
                      <span className="text-sm text-neutral-500">°C →</span>
                      <div className="min-w-0 flex-1">{num(pt.pct, (n) => edit((x) => ({ ...x, curve: curve.map((q, j) => (j === i ? { ...q, pct: n } : q)) })))}</div>
                      <span className="text-sm text-neutral-500">%</span>
                      <Button size="sm" variant="ghost" title="Remove point" disabled={curve.length <= 2} onClick={() => edit((x) => ({ ...x, curve: curve.filter((_, j) => j !== i) }))}>×</Button>
                    </div>
                  ))}
                  <div className="flex items-center justify-between gap-3">
                    <Button size="sm" disabled={curve.length >= 10} onClick={() => edit((x) => ({ ...x, curve: [...curve, { c: Math.min(curve[curve.length - 1].c + 5, 120), pct: 100 }] }))}>Add point</Button>
                    <p className="text-xs text-neutral-500">The last point must reach 100% by {crit}°C.</p>
                  </div>
                </div>
              )}
            </>
          )}
        </div>
      </Card>

      <Card>
        <CardHeader title="Thermal protection" subtitle="Acts on this device when it stays too hot." action={<ThermometerSun className="size-5 text-neutral-400" />} />
        <div className="space-y-5 p-5">
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={pr.enabled} onChange={(e) => edit((x) => ({ ...x, protect: { ...x.protect, enabled: e.target.checked, temp_c: x.protect.temp_c || 75, for_seconds: x.protect.for_seconds || 120, resume_below_c: x.protect.resume_below_c || 65 } }))} />
            Turn on protective actions
          </label>
          {pr.enabled && (
            <>
              <div className="grid gap-4 sm:grid-cols-3">
                <Field label="Temperature (°C)">{num(pr.temp_c, (n) => edit((x) => ({ ...x, protect: { ...x.protect, temp_c: n } })))}</Field>
                <Field label="Sustained for (seconds)">{num(pr.for_seconds, (n) => edit((x) => ({ ...x, protect: { ...x.protect, for_seconds: n } })))}</Field>
                <Field label="Resume below (°C)" hint="At least 3°C under the temperature, held for a minute.">{num(pr.resume_below_c, (n) => edit((x) => ({ ...x, protect: { ...x.protect, resume_below_c: n } })))}</Field>
              </div>
              {canFan && (
                <label className="flex items-center gap-2 text-sm">
                  <input type="checkbox" checked={pr.fan_full} onChange={(e) => edit((x) => ({ ...x, protect: { ...x.protect, fan_full: e.target.checked } }))} />
                  Set the fan to 100%
                </label>
              )}
              <Field as="div" label="Workers" hint="Stop lists the workers to stop (then start again when cooled). Critical workers are never stopped, even if listed.">
                {workers.length === 0 ? (
                  <p className="text-sm text-neutral-500">No workers on this device.</p>
                ) : (
                  <ul className="divide-y divide-neutral-100 rounded-lg border border-neutral-200">
                    {workers.map((n) => (
                      <li key={n} className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-3 py-2 text-sm">
                        <span className="min-w-0 truncate font-medium">{n}</span>
                        <span className="flex gap-4">
                          <label className="flex items-center gap-1.5"><input type="checkbox" checked={(pr.stop ?? []).includes(n)} onChange={() => edit((x) => ({ ...x, protect: { ...x.protect, stop: toggle(x.protect.stop, n) } }))} />Stop</label>
                          <label className="flex items-center gap-1.5"><input type="checkbox" checked={(p.critical ?? []).includes(n)} onChange={() => edit((x) => ({ ...x, critical: toggle(x.critical, n) }))} />Critical</label>
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
              </Field>
              <p className="text-xs text-neutral-500">Every automatic action is recorded in Activity and raises an alert under Needs attention. Alerts by email, Discord or webhook are not available yet.</p>
            </>
          )}
        </div>
      </Card>

      {error && <ErrorNote>{error}</ErrorNote>}
      <div className="flex justify-end">
        <Button variant="primary" loading={saving} disabled={!dirty || !supported} onClick={save}>Save policy</Button>
      </div>
    </div>
  );
}
