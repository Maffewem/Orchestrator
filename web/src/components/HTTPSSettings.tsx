import { useState } from "react";
import { api, type HTTPSView } from "../api";
import { Button, ErrorNote, Field, inputClass } from "./ui";

const modes = [
  ["off", "None, use my own reverse proxy or tunnel", "Keep serving plain HTTP. Use this behind Cloudflare Tunnel, Caddy, Nginx or Traefik."],
  ["auto", "Automatic certificate (Let's Encrypt)", "Orchestrator gets and renews a certificate for the hostname below. Port 443 on the internet must reach the HTTPS port; no other port is needed."],
  ["upload", "Upload a certificate", "For a private CA, a Cloudflare Origin certificate or an internal hostname."],
] as const;

export function HTTPSSettings({ value, onSaved }: { value: HTTPSView; onSaved: () => void }) {
  const [mode, setMode] = useState<HTTPSView["mode"]>(value.mode);
  const [host, setHost] = useState(value.host);
  const [addr, setAddr] = useState(value.addr || ":8443");
  const [hsts, setHsts] = useState(value.hsts);
  const [cert, setCert] = useState("");
  const [key, setKey] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);

  const save = async () => {
    setBusy(true);
    setMsg(null);
    try {
      const r = await api.setHTTPS({ mode, host, addr, hsts, cert_pem: cert, key_pem: key });
      setCert("");
      setKey("");
      setMsg({ ok: true, text: r.listening ? `Listening for HTTPS on ${r.addr}.` : "HTTPS is off." });
      onSaved();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4 p-5">
      {value.error && <ErrorNote>{value.error}</ErrorNote>}
      {value.listening && value.expires && (
        <p className="text-sm text-neutral-500">
          Certificate for <b>{value.subject}</b> expires {new Date(value.expires).toLocaleDateString()}.
        </p>
      )}
      {modes.map(([m, label, text]) => (
        <label key={m} className="flex cursor-pointer items-start gap-2 text-sm">
          <input type="radio" className="mt-1" name="https-mode" checked={mode === m} onChange={() => setMode(m)} />
          <span>
            <span className="font-medium">{label}</span>
            <br />
            <span className="text-neutral-500">{text}</span>
          </span>
        </label>
      ))}
      {mode !== "off" && (
        <div className="grid gap-3 sm:grid-cols-2">
          {mode === "auto" && (
            <Field label="Dashboard hostname" hint="Must already point at this server.">
              <input className={inputClass} value={host} onChange={(e) => setHost(e.target.value)} placeholder="orch.example.com" />
            </Field>
          )}
          <Field label="HTTPS listen address" hint="Publish this port from the container, for example -p 443:8443.">
            <input className={inputClass} value={addr} onChange={(e) => setAddr(e.target.value)} />
          </Field>
          {mode === "upload" && (
            <>
              <Field label="Certificate (PEM)" hint={value.cert_set ? "A certificate is saved. Leave both boxes empty to keep it." : undefined}>
                <textarea className={`${inputClass} h-28 font-mono text-xs`} value={cert} onChange={(e) => setCert(e.target.value)} placeholder="-----BEGIN CERTIFICATE-----" />
              </Field>
              <Field label="Private key (PEM)" hint="Stored on this server like the other tokens, and never shown again.">
                <textarea className={`${inputClass} h-28 font-mono text-xs`} value={key} onChange={(e) => setKey(e.target.value)} placeholder="-----BEGIN PRIVATE KEY-----" />
              </Field>
            </>
          )}
        </div>
      )}
      {mode !== "off" && (
        <label className="flex cursor-pointer items-start gap-2 text-sm">
          <input type="checkbox" className="mt-1" checked={hsts} onChange={(e) => setHsts(e.target.checked)} />
          <span>
            <span className="font-medium">HSTS</span>
            <br />
            <span className="text-neutral-500">Browsers refuse plain HTTP to this host for a year. Hard to undo, so leave it off until HTTPS works.</span>
          </span>
        </label>
      )}
      <p className="text-sm text-neutral-500">Once HTTPS works, open the dashboard on it and turn on "Redirect HTTP to HTTPS" under Sign-in security. Agents keep using whatever URL they were installed with.</p>
      <div className="flex items-center gap-3">
        <Button variant="primary" loading={busy} onClick={save}>Save</Button>
        {msg && (msg.ok ? <span className="text-sm text-ok">{msg.text}</span> : <ErrorNote>{msg.text}</ErrorNote>)}
      </div>
    </div>
  );
}
