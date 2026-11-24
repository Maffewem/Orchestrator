import { useEffect, useRef, useState } from "react";
import { Download, RotateCcw, Trash2, Upload } from "lucide-react";
import { api, type BackupDest, type BackupOAuthProvider, type ServerBackupPolicy } from "../api";
import { Badge, Button, ErrorNote, Field, Loading, inputClass, SecretInput } from "./ui";
import { confirmDialog } from "./dialogs";
import { ago, bytes, usePoll } from "../lib";

const weekdays = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

export function ServerBackupsPanel() {
  const list = usePoll(api.serverBackups, 0);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState("");
  const file = useRef<HTMLInputElement>(null);
  const v = list.data;
  if (!v) return <Loading />;

  const run = async (key: string, fn: () => Promise<unknown>, done: string) => {
    setBusy(key);
    setMsg(null);
    try {
      await fn();
      setMsg({ ok: true, text: done });
      list.reload();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy("");
    }
  };

  const restoreWarning = "The server restarts with that backup's data and everyone is signed out. The current database is kept as a .pre-restore file next to it.";
  const upload = async (f: File) => {
    const ok = await confirmDialog({ title: `Restore from ${f.name}?`, message: restoreWarning, confirmLabel: "Restore", danger: true });
    if (ok) await run("upload", () => api.restoreServerUpload(f, f.name.endsWith(".enc")), "Restoring. The server is restarting; reload in a moment.");
  };

  const storage = usePoll(() => api.backupStorage().catch(() => null), 60000);
  const l = storage.data?.location;
  const place = l?.host_path ? `, which is ${l.host_path} on ${l.device ?? "the server's host"}${l.volume ? ` (Docker volume ${l.volume})` : ""}` : l?.device ? `, on ${l.device}` : "";

  return (
    <div className="space-y-5 p-5">
      <p className="text-sm text-neutral-500">
        A copy of the server's database. Worker data is backed up separately on the Backups page.{" "}
        {v.password_set ? "Backups are password protected with the backup password." : "Set a backup password above to protect these files."}
      </p>
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="primary" loading={busy === "create"} onClick={() => run("create", api.createServerBackup, "Backup created.")}>
          Back up now
        </Button>
        <input ref={file} type="file" className="hidden" onChange={(e) => e.target.files?.[0] && upload(e.target.files[0])} />
        <Button icon={<Upload className="size-4" />} onClick={() => file.current?.click()}>
          Restore from file
        </Button>
      </div>
      {msg && (msg.ok ? <p role="status" className="text-sm text-ok">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}

      <Schedule saved={v.policy} nextRun={v.next_run} dir={v.dir} place={place} onSaved={() => list.reload()} />

      <Destinations dests={v.destinations ?? []} onChanged={() => list.reload()} />

      <div className="overflow-x-auto border-t border-neutral-200 pt-4">
        {(v.backups ?? []).length === 0 ? (
          <p className="text-sm text-neutral-500">No server backups yet.</p>
        ) : (
          <table className="w-full text-sm">
            <tbody>
              {(v.backups ?? []).map((b) => (
                <tr key={b.name} className="border-b border-neutral-100 last:border-0">
                  <td className="py-2 pr-3 font-mono text-xs">{b.name}</td>
                  <td className="whitespace-nowrap pr-3 text-neutral-500">{bytes(b.size)} · {ago(b.created_at)}</td>
                  <td className="pr-3">{b.encrypted ? <Badge tone="ok">Protected</Badge> : <Badge>Plain</Badge>}</td>
                  <td className="whitespace-nowrap text-right">
                    <Button size="sm" variant="ghost" icon={<Download className="size-3.5" />} aria-label={`Download ${b.name}`} onClick={() => run("dl", () => api.downloadServerBackup(b.name), "Downloaded.")} />
                    <Button
                      size="sm"
                      variant="ghost"
                      icon={<RotateCcw className="size-3.5" />}
                      aria-label={`Restore ${b.name}`}
                      onClick={async () => (await confirmDialog({ title: `Restore ${b.name}?`, message: restoreWarning, confirmLabel: "Restore", danger: true })) && run("restore", () => api.restoreServerBackup(b.name), "Restoring. The server is restarting; reload in a moment.")}
                    />
                    <Button
                      size="sm"
                      variant="ghost"
                      icon={<Trash2 className="size-3.5" />}
                      aria-label={`Delete ${b.name}`}
                      onClick={async () => (await confirmDialog({ title: "Delete this backup?", message: b.name, confirmLabel: "Delete", danger: true })) && run("del", () => api.deleteServerBackup(b.name), "Deleted.")}
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

function Schedule({ saved, nextRun, dir, place, onSaved }: { saved: ServerBackupPolicy; nextRun?: string; dir: string; place: string; onSaved: () => void }) {
  const [draft, setDraft] = useState<ServerBackupPolicy | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const policy = draft ?? saved;
  const dirty = draft !== null && JSON.stringify(draft) !== JSON.stringify(saved);
  const edit = (patch: Partial<ServerBackupPolicy>) => {
    setMsg(null);
    setDraft({ ...policy, ...patch });
  };
  const save = async () => {
    setBusy(true);
    setMsg(null);
    try {
      await api.setServerBackupPolicy(policy);
      setDraft(null);
      setMsg({ ok: true, text: "Schedule saved." });
      onSaved();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <section aria-labelledby="backup-schedule" className="space-y-3 border-t border-neutral-200 pt-4">
      <h3 id="backup-schedule" className="text-sm font-medium">Schedule</h3>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Field label="Run">
          <select className={inputClass} value={policy.mode} onChange={(e) => edit({ mode: e.target.value as ServerBackupPolicy["mode"] })}>
            <option value="off">Never</option>
            <option value="daily">Every day</option>
            <option value="weekly">Every week</option>
          </select>
        </Field>
        {policy.mode === "weekly" && (
          <Field label="On">
            <select className={inputClass} value={policy.weekday} onChange={(e) => edit({ weekday: Number(e.target.value) })}>
              {weekdays.map((d, i) => (
                <option key={d} value={i}>{d}</option>
              ))}
            </select>
          </Field>
        )}
        {policy.mode !== "off" && (
          <Field label={`At (${policy.tz})`}>
            <input className={inputClass} value={policy.time} onChange={(e) => edit({ time: e.target.value })} placeholder="03:00" />
          </Field>
        )}
        <Field label="Keep last" hint="Number of backups">
          <input className={inputClass} type="number" min={1} max={365} value={policy.keep} onChange={(e) => edit({ keep: Number(e.target.value) })} />
        </Field>
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="primary" loading={busy} disabled={!dirty} onClick={save}>Save schedule</Button>
        <Button variant="ghost" disabled={!dirty || busy} onClick={() => { setDraft(null); setMsg(null); }}>Discard</Button>
        {dirty && <span className="text-sm text-amber-700">Unsaved changes</span>}
        {msg && (msg.ok ? <span role="status" className="text-sm text-ok">{msg.text}</span> : <ErrorNote>{msg.text}</ErrorNote>)}
      </div>
      {nextRun && saved.mode !== "off" && !dirty && <p className="text-sm text-neutral-500">Next backup {new Date(nextRun).toLocaleString()}. Stored in <code>{dir}</code>{place}.</p>}
    </section>
  );
}

const kinds: Record<BackupDest["kind"], string> = { s3: "S3 compatible (AWS, R2, B2, MinIO)", folder: "Folder on this server (mount a NAS here)", device: "Another device running an agent", rclone: "Cloud storage (Google Drive, Dropbox, OneDrive and more via rclone)" };

// CloudSignIn connects Google Drive, Dropbox or OneDrive with the administrator's own OAuth app.
function CloudSignIn({ onMessage }: { onMessage: (m: { ok: boolean; text: string } | null) => void }) {
  const providers = usePoll(api.backupOAuth, 0);
  const [draft, setDraft] = useState<Record<string, { client_id: string; client_secret: string }>>({});
  const [busy, setBusy] = useState<string | null>(null);
  const redirect = `${window.location.origin}/api/backup-oauth/callback`;
  const get = (p: BackupOAuthProvider) => draft[p.id] ?? { client_id: p.client_id, client_secret: "" };
  const fail = (e: unknown) => onMessage({ ok: false, text: e instanceof Error ? e.message : String(e) });
  const connect = async (p: BackupOAuthProvider) => {
    setBusy(p.id);
    onMessage(null);
    try {
      const c = get(p);
      if (draft[p.id]) await api.setBackupOAuth({ [p.id]: c });
      const { url } = await api.startBackupOAuth(p.id);
      window.location.href = url;
    } catch (e) {
      fail(e);
      setBusy(null);
    }
  };
  return (
    <div className="space-y-2 rounded-lg border border-neutral-200 p-3">
      <h4 className="text-sm font-medium">Connect cloud storage</h4>
      <p className="text-sm text-neutral-500">
        Sign in to Google Drive, Dropbox or OneDrive from here. You register a free OAuth app with the provider once and enter its client ID and secret; the redirect URI to register is <code className="break-all">{redirect}</code>. Providers only accept <code>https://</code> addresses (or <code>localhost</code>), so open the dashboard over HTTPS first. Prefer another provider, or no app? Add a <em>Cloud storage</em> destination below and paste an rclone config instead.
      </p>
      {(providers.data ?? []).map((p) => (
        <details key={p.id} className="rounded border border-neutral-100 p-2">
          <summary className="cursor-pointer text-sm font-medium">{p.name}{p.client_id && p.secret_set ? " · app saved" : ""}</summary>
          <div className="mt-2 space-y-3">
            <p className="text-xs text-neutral-500">{p.setup} <a className="underline" href={p.console} target="_blank" rel="noreferrer">Open the developer console</a>.</p>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Client ID"><input className={inputClass} value={get(p).client_id} onChange={(e) => setDraft({ ...draft, [p.id]: { ...get(p), client_id: e.target.value } })} /></Field>
              <Field label="Client secret"><SecretInput className={inputClass} value={get(p).client_secret} placeholder={p.secret_set ? "Saved. Leave blank to keep" : ""} onChange={(e) => setDraft({ ...draft, [p.id]: { ...get(p), client_secret: e.target.value } })} /></Field>
            </div>
            <Button size="sm" variant="primary" loading={busy === p.id} disabled={!get(p).client_id || (!p.secret_set && !get(p).client_secret)} onClick={() => connect(p)}>Sign in with {p.name}</Button>
          </div>
        </details>
      ))}
    </div>
  );
}

function Destinations({ dests, onChanged }: { dests: BackupDest[]; onChanged: () => void }) {
  const devices = usePoll(api.devices, 0);
  const [draft, setDraft] = useState<BackupDest[] | null>(null);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    const q = new URLSearchParams(window.location.search);
    const ok = q.get("cloud"), bad = q.get("cloud_error");
    if (ok || bad) {
      setMsg({ ok: !!ok, text: ok ? `${ok}. It now receives every new server backup; use Test to check it.` : `Could not connect: ${bad}` });
      onChanged();
      window.history.replaceState(null, "", window.location.pathname);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const list = draft ?? dests;
  const set = (i: number, patch: Partial<BackupDest>) => setDraft(list.map((d, j) => (j === i ? { ...d, ...patch } : d)));
  const add = (kind: BackupDest["kind"]) =>
    setDraft([...list, { id: "", kind, name: kind === "s3" ? "Bucket" : kind === "folder" ? "Folder" : kind === "rclone" ? "Cloud storage" : "Device", enabled: true, region: "auto", path: kind === "device" ? "/srv/orchestrator-backups" : kind === "rclone" ? "Orchestrator backups" : "" }]);
  const save = async () => {
    setMsg(null);
    setSaving(true);
    try {
      await api.setBackupDests(list);
      setDraft(null);
      setMsg({ ok: true, text: "Saved. New backups are copied to every enabled destination." });
      onChanged();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setSaving(false);
    }
  };
  const test = async (d: BackupDest) => {
    setMsg(null);
    try {
      await api.testBackupDest(d.id);
      setMsg({ ok: true, text: `${d.name}: a test file was written and removed.` });
    } catch (e) {
      setMsg({ ok: false, text: `${d.name}: ${e instanceof Error ? e.message : String(e)}` });
    }
  };
  return (
    <div className="space-y-3 border-t border-neutral-200 pt-4">
      <h3 className="text-sm font-medium">Send copies elsewhere</h3>
      <p className="text-sm text-neutral-500">A backup on the same disk does not survive the disk. Every new backup is copied to each enabled destination. Folders, devices and cloud storage keep the same number of copies; expire S3 objects with a bucket lifecycle rule.</p>
      {list.map((d, i) => (
        <div key={d.id || i} className="space-y-3 rounded-lg border border-neutral-200 p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <label className="flex items-center gap-2 text-sm font-medium">
              <input type="checkbox" checked={d.enabled} onChange={(e) => set(i, { enabled: e.target.checked })} />
              <input className={`${inputClass} w-48`} aria-label="Name" value={d.name} onChange={(e) => set(i, { name: e.target.value })} />
              <span className="font-normal text-neutral-500">{kinds[d.kind]}</span>
            </label>
            <div className="flex items-center gap-2">
              {d.last_error ? <Badge tone="bad">Last copy failed</Badge> : d.last_at ? <Badge tone="ok">Copied {ago(d.last_at)}</Badge> : null}
              {d.id && !draft && <Button size="sm" onClick={() => test(d)}>Test</Button>}
              <Button size="sm" variant="ghost" aria-label={`Remove ${d.name}`} icon={<Trash2 className="size-3.5" />} onClick={() => setDraft(list.filter((_, j) => j !== i))} />
            </div>
          </div>
          {d.id && d.location && !draft && (
            <p className="text-sm text-neutral-600">
              <strong>Stored at:</strong> {d.location}.{" "}
              {d.used_bytes !== undefined ? `${bytes(d.used_bytes)} used there.` : d.last_size ? `Last copy ${bytes(d.last_size)}; the total used there is not measured for this kind.` : ""}
            </p>
          )}
          {d.last_error && <ErrorNote>{d.last_error}</ErrorNote>}
          {d.kind === "s3" && (
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Endpoint" hint="For R2: https://<account>.r2.cloudflarestorage.com"><input className={inputClass} value={d.endpoint ?? ""} onChange={(e) => set(i, { endpoint: e.target.value })} placeholder="https://s3.amazonaws.com" /></Field>
              <Field label="Region" hint="auto for R2, otherwise e.g. eu-west-2"><input className={inputClass} value={d.region ?? ""} onChange={(e) => set(i, { region: e.target.value })} /></Field>
              <Field label="Bucket"><input className={inputClass} value={d.bucket ?? ""} onChange={(e) => set(i, { bucket: e.target.value })} /></Field>
              <Field label="Folder in bucket"><input className={inputClass} value={d.prefix ?? ""} onChange={(e) => set(i, { prefix: e.target.value })} placeholder="orchestrator" /></Field>
              <Field label="Access key ID"><input className={inputClass} value={d.access_key ?? ""} onChange={(e) => set(i, { access_key: e.target.value })} /></Field>
              <Field label="Secret access key"><SecretInput className={inputClass} value={d.secret_key ?? ""} placeholder={d.secret_set ? "Saved. Leave blank to keep" : ""} onChange={(e) => set(i, { secret_key: e.target.value })} /></Field>
            </div>
          )}
          {d.kind === "folder" && (
            <Field label="Folder" hint="Absolute path inside the server container. Bind-mount your NAS share (SMB or NFS mounted on the host) to this path."><input className={inputClass} value={d.path ?? ""} onChange={(e) => set(i, { path: e.target.value })} placeholder="/backups-nas" /></Field>
          )}
          {d.kind === "rclone" && (
            <div className="space-y-3">
              <p className="text-sm text-neutral-500">
                Sign in once on any computer with <a className="underline" href="https://rclone.org/install/" target="_blank" rel="noreferrer">rclone</a>: run <code>rclone config</code>, create a remote for Google Drive, Dropbox, OneDrive or another provider (rclone opens the browser sign-in), then run <code>rclone config show &lt;remote&gt;</code> and paste the result here. The server stores it and renews the sign-in itself.
              </p>
              <Field label="rclone remote configuration" hint="Starts with [name]. Treat it like a password: it grants access to that storage.">
                <textarea className={`${inputClass} h-28 font-mono text-xs`} value={d.rclone_conf ?? ""} placeholder={d.secret_set ? "Saved. Leave blank to keep" : "[gdrive]\ntype = drive\ntoken = {...}"} onChange={(e) => set(i, { rclone_conf: e.target.value })} />
              </Field>
              <Field label="Folder in the cloud storage" hint="Created if missing. Old backups beyond the retention count are removed from it."><input className={inputClass} value={d.path ?? ""} onChange={(e) => set(i, { path: e.target.value })} /></Field>
            </div>
          )}
          {d.kind === "device" && (
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Device">
                <select className={inputClass} value={d.device_id ?? ""} onChange={(e) => set(i, { device_id: e.target.value })}>
                  <option value="">Choose a device</option>
                  {(devices.data ?? []).map((x) => (
                    <option key={x.id} value={x.id}>{x.name}</option>
                  ))}
                </select>
              </Field>
              <Field label="Folder on the device" hint="If the agent runs in a container, this must be a mounted path."><input className={inputClass} value={d.path ?? ""} onChange={(e) => set(i, { path: e.target.value })} /></Field>
            </div>
          )}
        </div>
      ))}
      <CloudSignIn onMessage={setMsg} />
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm text-neutral-500">Add a destination:</span>
        {(Object.keys(kinds) as BackupDest["kind"][]).map((k) => (
          <Button key={k} size="sm" onClick={() => add(k)}>{k === "s3" ? "S3 bucket" : k === "folder" ? "Folder" : k === "rclone" ? "Cloud storage" : "Device"}</Button>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-3 border-t border-neutral-100 pt-3">
        <Button variant="primary" loading={saving} disabled={!draft} onClick={save}>Save destinations</Button>
        <Button variant="ghost" disabled={!draft || saving} onClick={() => { setDraft(null); setMsg(null); }}>Discard</Button>
        {draft && <span className="text-sm text-amber-700">Unsaved changes</span>}
      </div>
      {msg && (msg.ok ? <p role="status" className="text-sm text-ok">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}
    </div>
  );
}
