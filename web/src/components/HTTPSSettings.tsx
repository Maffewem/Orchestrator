import { useState } from "react";
import { api, type HTTPSTest, type HTTPSView } from "../api";
import { Button, ErrorNote, Field, inputClass } from "./ui";

const modes = [
  ["off", "None, use my own reverse proxy or tunnel", "Keep serving plain HTTP. Use this behind Cloudflare Tunnel, Caddy, Nginx or Traefik."],
  ["auto", "Automatic certificate (Let's Encrypt)", "Orchestrator gets and renews a certificate for the hostname below. By default Let's Encrypt must reach port 443 from the internet, which a LAN-only name cannot allow. To use a LAN-only name, connect Cloudflare and choose the Cloudflare DNS challenge under SSL below: then no inbound access is needed."],
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
  const [test, setTest] = useState<HTTPSTest | null>(null);
  const [testing, setTesting] = useState(false);
  const [publishing, setPublishing] = useState(false);
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null);

  const runTest = async () => {
    setTesting(true);
    setNote(null);
    try {
      setTest(await api.testHTTPS());
    } catch (e) {
      setNote({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setTesting(false);
    }
  };

  const publish = async () => {
    if (!confirm(`Re-create the server container so it publishes port ${value.container_port}? The dashboard is unavailable for a few seconds. HTTP on the current port keeps working, and the old container is kept as a way back.`)) return;
    setPublishing(true);
    setNote(null);
    try {
      await api.publishHTTPSPort();
      setNote({ ok: true, text: "Restarting the server with the port published. This page reloads in a moment; reopen it if it does not." });
      setTimeout(() => location.reload(), 15000);
    } catch (e) {
      setNote({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setPublishing(false);
    }
  };

  const save = async () => {
    setBusy(true);
    setMsg(null);
    try {
      const r = await api.setHTTPS({ mode, host, addr, hsts, cert_pem: cert, key_pem: key });
      setCert("");
      setKey("");
      if (!r.hsts) setHsts(false);
      setMsg({
        ok: !r.hsts_blocked,
        text: !r.listening
          ? "HTTPS is off."
          : r.can_publish
            ? `Listening for HTTPS on ${r.addr}, but Docker does not publish port ${r.container_port} yet. Press "Publish port ${r.container_port}" in the status box to make it reachable (a previously published HTTPS port is removed).`
            : `Listening for HTTPS on ${r.addr}.`,
      });
      if (r.hsts_blocked) setMsg({ ok: false, text: "Saved, but " + r.hsts_blocked.charAt(0).toLowerCase() + r.hsts_blocked.slice(1) });
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
      {value.challenge === "dns" && <p className="text-sm text-neutral-500">Using the Cloudflare DNS challenge (works for LAN-only names).</p>}
      {value.expiry && value.expiry.level !== "ok" && (
        <div className={`rounded-lg border p-3 text-sm ${value.expiry.level === "critical" ? "border-danger text-danger" : "border-amber-400 text-amber-700"}`}>
          <p className="font-medium">{value.expiry.days < 0 ? "Certificate expired" : `Certificate ends in ${value.expiry.days} days`}</p>
          <p>{value.expiry.message}</p>
          {value.hsts && <p className="mt-1">HSTS is on, so browsers will not let anyone click through once it expires. If you are locked out, open <b>http://&lt;server IP address&gt;:8080</b> (by IP, not by name) and upload or renew the certificate, or see "Locked out?" below.</p>}
        </div>
      )}
      {value.listening && value.expires && (
        <p className="text-sm text-neutral-500">
          Certificate for <b>{value.subject}</b> expires {new Date(value.expires).toLocaleDateString()}.
        </p>
      )}
      {value.mode !== "off" && (
        <div className="space-y-2 rounded-lg border border-neutral-200 bg-neutral-50 p-3 text-sm">
          <p className="font-medium">Status</p>
          <ul className="space-y-1 text-neutral-600">
            <li>Listener: {value.listening ? <b className="text-ok">running on {value.addr}</b> : <b className="text-danger">not running</b>}</li>
            <li>
              Docker port:{" "}
              {!value.port_known ? <b>unknown</b> : value.published_host ? <b className="text-ok">{value.container_port} is published as host port {value.published_host}</b> : <b className="text-danger">{value.container_port} is not published</b>}
            </li>
            {value.subject && (
              <li>
                Certificate: <b>{value.subject}</b>, {value.self_signed ? "self-signed" : `issued by ${value.issuer || "unknown"}`}
                {value.expires ? `, expires ${new Date(value.expires).toLocaleDateString()}` : ""}
                {value.sans?.length ? <> · valid for {value.sans.join(", ")}</> : null}
              </li>
            )}
          </ul>
          {value.publish_note && <p className={value.published_host || value.port_known ? "text-danger" : "text-neutral-500"}>{value.publish_note}</p>}
          <div className="flex flex-wrap items-center gap-2">
            {value.can_publish && <Button variant="primary" loading={publishing} onClick={publish}>Publish port {value.container_port}</Button>}
            <Button loading={testing} onClick={runTest}>Test HTTPS</Button>
          </div>
          {note && (note.ok ? <p className="text-ok">{note.text}</p> : <ErrorNote>{note.text}</ErrorNote>)}
          {test && (
            <div className="space-y-1 border-t border-neutral-200 pt-2">
              <p>
                Handshake from the server itself: {test.handshake ? <b className="text-ok">worked</b> : <b className="text-danger">failed</b>}
                {test.error ? ` (${test.error})` : ""}
              </p>
              {test.handshake && (
                <p className="text-neutral-600">
                  Checked against <b>{test.tested_name}</b>: name {test.name_matches ? "matches" : "does not match"}, {test.trusted ? "trusted" : "not trusted by this server"}
                  {test.expired ? ", expired" : ""}.
                </p>
              )}
              <ul className="list-disc space-y-1 pl-5 text-neutral-700">
                {test.advice.map((a) => (
                  <li key={a}>{a}</li>
                ))}
              </ul>
            </div>
          )}
          <details className="text-neutral-600">
            <summary className="cursor-pointer font-medium">Locked out? (expired certificate, HSTS)</summary>
            <ul className="mt-1 list-disc space-y-1 pl-5">
              <li>Plain HTTP on port 8080 keeps working. Open <b>http://&lt;server IP address&gt;:8080</b> using the <b>IP address</b>: browsers remember HSTS per name, so the name keeps forcing HTTPS but an IP address does not. The HTTP to HTTPS redirect is skipped automatically while the certificate is expired.</li>
              <li>From a shell on the server: <code>docker exec orchestrator orchestrator-server https-reset</code> switches HSTS and the redirect off (add <code>-off</code> to turn dashboard HTTPS off), then <code>docker restart orchestrator</code>.</li>
              <li>Without shell access to the container: set <code>ORCH_HTTPS_RECOVERY=1</code> (or <code>off</code>) in its environment and restart it. Remove it afterwards.</li>
            </ul>
          </details>
          <details className="text-neutral-600">
            <summary className="cursor-pointer font-medium">What should I expect in the browser?</summary>
            <ul className="mt-1 list-disc space-y-1 pl-5">
              <li>Always use <b>https://</b> with the HTTPS port (for example https://this-server:{value.published_host || value.container_port || "8443"}). Using https:// on the plain HTTP port gives an SSL protocol error.</li>
              <li><b>Self-signed or private-CA certificate:</b> a "not private" warning is expected. Click through for your own server, or install the certificate on your devices.</li>
              <li><b>Real certificate</b> (Let's Encrypt or a purchased one): no warning, but only when you open the dashboard by the <b>name</b> on the certificate. An IP address never matches, so use the hostname.</li>
              <li>Plain HTTP keeps working as before. The "Redirect HTTP to HTTPS" setting is skipped automatically while the HTTPS port is not published, so you cannot lock yourself out.</li>
            </ul>
          </details>
        </div>
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
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          {mode === "auto" && (
            <Field label="Dashboard hostname" hint="Must already point at this server.">
              <input className={inputClass} value={host} onChange={(e) => setHost(e.target.value)} placeholder="orch.example.com" />
            </Field>
          )}
          <Field label="HTTPS listen address" hint="The container must publish this port (-p 8443:8443), or the browser gets connection refused. Servers installed before this was added need recreating with it.">
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
            <span className="text-neutral-500">Browsers refuse plain HTTP to this host for a year. Hard to undo, so it is only switched on when a test shows browsers would accept the certificate for the name you are using (not self-signed, name matches, port published); otherwise it stays off.</span>
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
