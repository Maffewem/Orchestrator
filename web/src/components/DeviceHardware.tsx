import { useEffect, useState } from "react";
import { Lightbulb, Plus, Terminal as TerminalIcon, Trash2 } from "lucide-react";
import { api, type Device, type LEDConfig, type LEDMetric, type LEDRule, type SSHCheck, type SSHSettings } from "../api";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, Loading, Modal, Toggle, inputClass } from "./ui";
import { Shell } from "./Streams";
import { confirmDialog, notify } from "./dialogs";
import { useCan } from "./auth";
import { usePoll } from "../lib";

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));
export const hasCap = (d: Device, c: string) => !!d.host.capabilities?.includes(c);

/** True while the device is flashing its LEDs, re-rendering when the time runs out. */
export function useIdentifying(d: Device): boolean {
  const until = d.identifying_until ? new Date(d.identifying_until).getTime() : 0;
  const [, tick] = useState(0);
  useEffect(() => {
    const left = until - Date.now();
    if (left <= 0) return;
    const t = setTimeout(() => tick((n) => n + 1), left + 100);
    return () => clearTimeout(t);
  }, [until]);
  return until > Date.now();
}

/** Header buttons: find the device by its LEDs, and open a console on it. Administrators only. */
export function DeviceHardwareActions({ d, onChanged }: { d: Device; onChanged: () => void }) {
  const isAdmin = useCan("admin");
  const [console, setConsole] = useState(false);
  const [busy, setBusy] = useState(false);
  const identifying = useIdentifying(d);
  if (!isAdmin || d.status !== "online") return null;
  return (
    <div className="flex flex-wrap gap-2">
      {hasCap(d, "led") && (
        <Button
          icon={<Lightbulb className="size-4" />}
          loading={busy}
          onClick={async () => {
            setBusy(true);
            try {
              await api.identifyDevice(d.id);
              onChanged();
            } catch (e) {
              notify(errText(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          {identifying ? "Flashing…" : "Identify"}
        </Button>
      )}
      <Button
        icon={<TerminalIcon className="size-4" />}
        onClick={async () => {
          if (!hasCap(d, "console")) return notify(`The agent on ${d.name} is too old for the console. Update it from the Updates page.`);
          if (!(await confirmDialog({ title: `Open a root console on ${d.name}?`, message: "This is a root shell on the device itself. Everything you type is recorded in the device's Activity.", confirmLabel: "Open console" }))) return;
          setConsole(true);
        }}
      >
        Console
      </Button>
      {console && <Shell deviceId={d.id} name={d.name} onClose={() => setConsole(false)} />}
    </div>
  );
}

// ---------------------------------------------------------------- LEDs

const METRICS: { id: LEDMetric; label: string; unit: string; warn: number; crit: number }[] = [
  { id: "temp", label: "Temperature", unit: "°C", warn: 60, crit: 75 },
  { id: "cpu", label: "CPU", unit: "%", warn: 80, crit: 95 },
  { id: "memory", label: "Memory", unit: "%", warn: 80, crit: 90 },
  { id: "disk", label: "Disk", unit: "%", warn: 85, crit: 95 },
];

const STATE_TONE = { ok: "ok", warn: "warn", crit: "bad", identify: "info" } as const;

function LEDCard({ d }: { d: Device }) {
  // Each read is a command to the agent, so these load once and again after a change, not on a timer.
  const st = usePoll(() => api.leds(d.id), 0, [d.id]);
  const status = st.data;
  const [cfg, setCfg] = useState<LEDConfig | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    if (status && !cfg) setCfg({ ...status.config, rules: status.config.rules ?? [] });
  }, [status, cfg]);
  if (!hasCap(d, "led")) return <Card><CardHeader title="LEDs" /><p className="p-5 text-sm text-neutral-500">This agent is too old for LED control. Update it from the Updates page.</p></Card>;
  if (st.error && !status) return <ErrorNote>{st.error}</ErrorNote>;
  if (!status || !cfg) return <Loading />;
  const rules = cfg.rules ?? [];
  const setRule = (i: number, patch: Partial<LEDRule>) => setCfg({ ...cfg, rules: rules.map((r, j) => (j === i ? { ...r, ...patch } : r)) });
  const ledNames = status.leds.map((l) => l.name);
  const addRule = () => {
    const used = new Set(rules.map((r) => r.metric));
    const m = METRICS.find((x) => !used.has(x.id)) ?? METRICS[0];
    const led = status.leds.find((l) => l.role === "activity")?.name ?? ledNames[0] ?? "";
    setCfg({ ...cfg, rules: [...rules, { metric: m.id, led, warn: m.warn, crit: m.crit }] });
  };
  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const next = await api.setLEDConfig(d.id, cfg);
      setCfg({ ...next.config, rules: next.config.rules ?? [] });
      st.reload();
      notify("LED behaviour saved", "success");
    } catch (e) {
      setError(errText(e));
    } finally {
      setSaving(false);
    }
  };
  return (
    <Card>
      <CardHeader title="LEDs" subtitle="Flash a device to find it, or show a threshold on its LEDs." />
      <div className="space-y-5 p-5">
        {!status.supported && status.note && <ErrorNote>{status.note}</ErrorNote>}
        {status.leds.length > 0 && (
          <ul className="divide-y divide-neutral-200 rounded-lg border border-neutral-200 text-sm">
            {status.leds.map((l) => (
              <li key={l.name} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2">
                <span className="font-medium">{l.name} <span className="font-normal text-neutral-500">{l.role === "other" ? "" : l.role + " LED"}</span></span>
                <span className="flex items-center gap-2 text-xs text-neutral-500">
                  {l.trigger && <span>normally: {l.trigger}</span>}
                  {l.state ? <Badge tone={STATE_TONE[l.state as keyof typeof STATE_TONE] ?? "neutral"}>{l.state === "identify" ? "flashing" : l.state}</Badge> : <Badge>untouched</Badge>}
                </span>
              </li>
            ))}
          </ul>
        )}
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Profile" hint="Pi: the LEDs the kernel lists (activity and power). Compute Blade: the blade's top and edge LEDs.">
            <select className={inputClass} value={cfg.profile} onChange={(e) => setCfg({ ...cfg, profile: e.target.value as LEDConfig["profile"] })}>
              <option value="pi">Raspberry Pi (kernel LEDs)</option>
              <option value="blade">Compute Blade LEDs (CM5, experimental)</option>
            </select>
          </Field>
          <Field label="Show thresholds on the LEDs" as="div" hint="Normal: the LED does what it always does. Warning: slow blink. Critical: fast blink.">
            <Toggle label="Show thresholds on the LEDs" checked={cfg.enabled} onChange={(v) => setCfg({ ...cfg, enabled: v })} />
          </Field>
        </div>
        {cfg.profile === "blade" && (
          <p className="rounded-lg bg-neutral-50 p-3 text-xs text-neutral-600">
            Experimental: drives the blade's two LEDs (blade-top, blade-edge) through the Pi 5 / CM5 PIO block, on GPIO18. It needs <code>/dev/pio0</code> and has not been checked on real hardware yet. CM4 blades are not supported. Warning is amber, critical red, identify blue.
          </p>
        )}
        <div className="flex items-center justify-between gap-4">
          <span className="text-sm"><b>Stealth mode</b><br /><span className="text-neutral-500">Keep every LED of this profile off, whatever the rules say. Identify still flashes.</span></span>
          <Toggle label="Stealth mode" checked={!!cfg.stealth} onChange={(v) => setCfg({ ...cfg, stealth: v })} />
        </div>
        <div className="space-y-2">
          {rules.length === 0 && <p className="text-sm text-neutral-500">No rules yet. Add one to turn a threshold into a blink.</p>}
          {rules.map((r, i) => {
            const m = METRICS.find((x) => x.id === r.metric)!;
            const now = status.values?.[r.metric];
            return (
              <div key={i} className="grid items-start gap-2 rounded-lg border border-neutral-200 p-3 sm:grid-cols-[1fr_1fr_5.5rem_5.5rem_auto]">
                <Field label="Metric" hint={now === undefined ? undefined : `now ${now.toFixed(0)}${m.unit}`}>
                  <select className={inputClass} value={r.metric} onChange={(e) => { const nm = METRICS.find((x) => x.id === e.target.value)!; setRule(i, { metric: nm.id, warn: nm.warn, crit: nm.crit }); }}>
                    {METRICS.map((x) => <option key={x.id} value={x.id}>{x.label}</option>)}
                  </select>
                </Field>
                <Field label="LED">
                  <select className={inputClass} value={r.led} onChange={(e) => setRule(i, { led: e.target.value })}>
                    {!ledNames.includes(r.led) && <option value={r.led}>{r.led}</option>}
                    {ledNames.map((n) => <option key={n} value={n}>{n}</option>)}
                  </select>
                </Field>
                <Field label={`Warn ${m.unit}`}><input type="number" className={inputClass} value={r.warn} onChange={(e) => setRule(i, { warn: Number(e.target.value) })} /></Field>
                <Field label={`Critical ${m.unit}`}><input type="number" className={inputClass} value={r.crit} onChange={(e) => setRule(i, { crit: Number(e.target.value) })} /></Field>
                <Button variant="ghost" aria-label="Remove rule" onClick={() => setCfg({ ...cfg, rules: rules.filter((_, j) => j !== i) })}><Trash2 className="size-4" /></Button>
              </div>
            );
          })}
          <p className="text-xs text-neutral-500">If several rules use the same LED, the worst state wins.</p>
        </div>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex flex-wrap gap-2">
          <Button icon={<Plus className="size-4" />} onClick={addRule} disabled={ledNames.length === 0 && cfg.profile === "pi"}>Add rule</Button>
          <Button variant="primary" loading={saving} onClick={save}>Save</Button>
        </div>
      </div>
    </Card>
  );
}

// ---------------------------------------------------------------- SSH

const ROOT_LOGIN: { v: SSHSettings["root_login"]; label: string }[] = [
  { v: "prohibit-password", label: "Keys only (prohibit-password)" },
  { v: "no", label: "Not allowed" },
  { v: "yes", label: "Allowed, password too" },
  { v: "forced-commands-only", label: "Forced commands only" },
];

function useCountdown(iso?: string) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!iso) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [iso]);
  return iso ? Math.max(0, Math.round((new Date(iso).getTime() - now) / 1000)) : 0;
}

/** Asks for the device name when a change could lock everyone out. */
function LockoutModal({ d, warnings, onCancel, onConfirm, verb }: { d: Device; warnings: SSHCheck["warnings"]; onCancel: () => void; onConfirm: (typed: string) => Promise<void>; verb: string }) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  return (
    <Modal title="This could lock you out" onClose={onCancel}>
      <div className="space-y-4">
        <ul className="space-y-2">
          {warnings.map((w, i) => (
            <li key={i} className={`rounded-lg border px-3 py-2 text-sm ${w.level === "lockout" ? "border-red-200 bg-red-50 text-bad" : "border-neutral-200 bg-neutral-50"}`}>{w.message}</li>
          ))}
        </ul>
        <p className="text-sm text-neutral-600">The console in the dashboard keeps working whatever happens to SSH, and a change stays on trial for two minutes and reverts by itself unless you confirm it.</p>
        <Field label={`Type ${d.name} to continue`}>
          <input className={inputClass} value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button onClick={onCancel}>Cancel</Button>
          <Button variant="danger" loading={busy} disabled={typed.trim().toLowerCase() !== d.name.toLowerCase()} onClick={async () => {
            setBusy(true);
            try { await onConfirm(typed); } catch (e) { setError(errText(e)); setBusy(false); }
          }}>{verb}</Button>
        </div>
      </div>
    </Modal>
  );
}

function SSHCard({ d }: { d: Device }) {
  const [trial, setTrial] = useState(false);
  const st = usePoll(() => api.sshStatus(d.id), trial ? 6000 : 0, [d.id]);
  const s = st.data;
  useEffect(() => setTrial(!!s?.pending), [s?.pending]);
  const [form, setForm] = useState<SSHSettings | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [lock, setLock] = useState<{ warnings: SSHCheck["warnings"]; run: (typed: string) => Promise<void>; verb: string } | null>(null);
  const [keyUser, setKeyUser] = useState("");
  const [keyText, setKeyText] = useState("");
  const left = useCountdown(s?.pending?.revert_at);
  useEffect(() => {
    if (s && !form && s.installed) setForm({ ...s.effective });
  }, [s, form]);
  if (!hasCap(d, "ssh")) return <Card><CardHeader title="SSH" /><p className="p-5 text-sm text-neutral-500">This agent is too old for SSH control. Update it from the Updates page.</p></Card>;
  if (st.error && !s) return <ErrorNote>{st.error}</ErrorNote>;
  if (!s) return <Loading />;
  if (!s.installed) return <Card><CardHeader title="SSH" /><div className="p-5"><ErrorNote>{s.error ?? "OpenSSH server is not installed."}</ErrorNote></div></Card>;
  const f = form ?? s.effective;
  const changed = f.password_auth !== s.effective.password_auth || f.pubkey_auth !== s.effective.pubkey_auth || f.root_login !== s.effective.root_login;
  const users = s.users ?? [];

  const apply = async (typed?: string) => {
    await api.sshSet(d.id, { ...f, confirm: typed });
    setLock(null);
    st.reload();
  };
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      const ck = await api.sshCheck(d.id, f);
      if (ck.lockout) {
        setLock({ warnings: ck.warnings, run: apply, verb: "Apply anyway" });
      } else {
        const notes = ck.warnings.map((w) => w.message).join("\n");
        if (notes && !(await confirmDialog({ title: "Apply SSH change?", message: notes, confirmLabel: "Apply" }))) return;
        await apply();
      }
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(false);
    }
  };
  const removeKey = async (user: string, fp: string) => {
    if (!(await confirmDialog({ title: `Remove this key for ${user}?`, message: fp, confirmLabel: "Remove", danger: true }))) return;
    try {
      await api.sshRemoveKey(d.id, user, fp);
      st.reload();
    } catch (e) {
      const msg = errText(e);
      if (msg.startsWith("refused")) {
        setLock({
          warnings: [{ level: "lockout", message: msg.replace(/^refused: /, "").replace(/^./, (c) => c.toUpperCase()) }],
          verb: "Remove anyway",
          run: async (typed) => { await api.sshRemoveKey(d.id, user, fp, typed); setLock(null); st.reload(); },
        });
      } else notify(msg);
    }
  };
  return (
    <Card>
      <CardHeader
        title="SSH"
        subtitle={`${s.running ? "Running" : "Not running"}${s.service ? ` (${s.service})` : ""}. Changes are on trial for two minutes and revert unless you confirm them.`}
      />
      <div className="space-y-6 p-5">
        {s.pending && (
          <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-300 bg-amber-50 p-3 text-sm">
            <span>
              <b>Test it now.</b> Open a new SSH session and check you can still get in. This change reverts in <b>{Math.floor(left / 60)}:{String(left % 60).padStart(2, "0")}</b> unless you keep it.
            </span>
            <span className="flex gap-2">
              <Button variant="primary" onClick={async () => { await api.sshConfirm(d.id, false); st.reload(); }}>Keep change</Button>
              <Button onClick={async () => { await api.sshConfirm(d.id, true); setForm(null); st.reload(); }}>Revert now</Button>
            </span>
          </div>
        )}
        <div className="space-y-4">
          <div className="flex items-center justify-between gap-4">
            <span className="text-sm"><b>Password sign-in</b><br /><span className="text-neutral-500">Anyone with an account password can try to sign in.</span></span>
            <Toggle label="Password sign-in" checked={f.password_auth} onChange={(v) => setForm({ ...f, password_auth: v })} />
          </div>
          <div className="flex items-center justify-between gap-4">
            <span className="text-sm"><b>Key sign-in</b><br /><span className="text-neutral-500">Public keys listed below.</span></span>
            <Toggle label="Key sign-in" checked={f.pubkey_auth} onChange={(v) => setForm({ ...f, pubkey_auth: v })} />
          </div>
          <Field label="Root login">
            <select className={inputClass} value={f.root_login} onChange={(e) => setForm({ ...f, root_login: e.target.value as SSHSettings["root_login"] })}>
              {ROOT_LOGIN.map((o) => <option key={o.v} value={o.v}>{o.label}</option>)}
            </select>
          </Field>
          {error && <ErrorNote>{error}</ErrorNote>}
          <div className="flex flex-wrap gap-2">
            <Button variant="primary" loading={busy} disabled={!changed || !!s.pending} onClick={submit}>Review and apply</Button>
            {s.managed && !s.pending && (
              <Button onClick={async () => {
                if (!(await confirmDialog({ title: "Go back to the system's SSH settings?", message: "Removes the settings Orchestrator manages. Keys stay as they are.", confirmLabel: "Remove" }))) return;
                try { await api.sshConfirm(d.id, true); setForm(null); st.reload(); } catch (e) { notify(errText(e)); }
              }}>Reset to system settings</Button>
            )}
          </div>
        </div>

        <div>
          <h4 className="mb-2 text-sm font-semibold">Authorized keys</h4>
          {(s.keys ?? []).length === 0 ? (
            <p className="text-sm text-neutral-500">No keys yet.</p>
          ) : (
            <ul className="divide-y divide-neutral-200 rounded-lg border border-neutral-200 text-sm">
              {s.keys!.map((k) => (
                <li key={k.user + k.fingerprint} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2">
                  <span className="min-w-0">
                    <span className="font-medium">{k.user}</span> <span className="text-neutral-500">{k.type}{k.comment ? ` · ${k.comment}` : ""}</span>
                    <br /><code className="break-all text-xs text-neutral-500">{k.fingerprint}</code>
                  </span>
                  <Button size="sm" variant="danger" onClick={() => removeKey(k.user, k.fingerprint)}>Remove</Button>
                </li>
              ))}
            </ul>
          )}
          <form
            className="mt-3 space-y-3"
            onSubmit={async (e) => {
              e.preventDefault();
              try {
                await api.sshAddKey(d.id, keyUser || users[0]?.name || "", keyText.trim());
                setKeyText("");
                st.reload();
                notify("Key added", "success");
              } catch (err) {
                notify(errText(err));
              }
            }}
          >
            <div className="grid gap-3 sm:grid-cols-[12rem_1fr]">
              <Field label="Account">
                <select className={inputClass} value={keyUser || users[0]?.name || ""} onChange={(e) => setKeyUser(e.target.value)}>
                  {users.map((u) => <option key={u.name} value={u.name}>{u.name}</option>)}
                </select>
              </Field>
              <Field label="Public key" hint="One line, for example ssh-ed25519 AAAA… you@laptop.">
                <textarea className={inputClass + " font-mono text-xs"} rows={2} value={keyText} onChange={(e) => setKeyText(e.target.value)} placeholder="ssh-ed25519 AAAA…" />
              </Field>
            </div>
            <Button disabled={!keyText.trim() || users.length === 0}>Add key</Button>
          </form>
        </div>
      </div>
      {lock && <LockoutModal d={d} warnings={lock.warnings} verb={lock.verb} onCancel={() => setLock(null)} onConfirm={lock.run} />}
    </Card>
  );
}

/** The device's Hardware tab: LEDs and SSH. */
export function HardwareTab({ d }: { d: Device }) {
  return (
    <div className="max-w-3xl space-y-6">
      <LEDCard d={d} />
      <SSHCard d={d} />
    </div>
  );
}
