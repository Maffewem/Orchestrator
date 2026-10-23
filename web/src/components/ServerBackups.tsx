import { useRef, useState } from "react";
import { Download, RotateCcw, Trash2, Upload } from "lucide-react";
import { api, type BackupDest, type ServerBackupPolicy } from "../api";
import { Badge, Button, ErrorNote, Field, Loading, inputClass } from "./ui";
import { confirmDialog } from "./dialogs";
import { ago, bytes, usePoll } from "../lib";

const weekdays = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

export function ServerBackupsPanel() {
  const list = usePoll(api.serverBackups, 0);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState("");
  const [pol, setPol] = useState<ServerBackupPolicy | null>(null);
  const file = useRef<HTMLInputElement>(null);
  const v = list.data;
  if (!v) return <Loading />;
  const policy = pol ?? v.policy;

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

      <div className="space-y-3 border-t border-neutral-200 pt-4">
        <p className="text-sm font-medium">Schedule</p>
        <div className="flex flex-wrap items-end gap-3">
          <Field label="Run">
            <select className={inputClass} value={policy.mode} onChange={(e) => setPol({ ...policy, mode: e.target.value as ServerBackupPolicy["mode"] })}>
              <option value="off">Never</option>
              <option value="daily">Every day</option>
              <option value="weekly">Every week</option>
            </select>
          </Field>
          {policy.mode === "weekly" && (
            <Field label="On">
              <select className={inputClass} value={policy.weekday} onChange={(e) => setPol({ ...policy, weekday: Number(e.target.value) })}>
                {weekdays.map((d, i) => (
                  <option key={d} value={i}>{d}</option>
                ))}
              </select>
            </Field>
          )}
          {policy.mode !== "off" && (
            <Field label={`At (${policy.tz})`}>
              <input className={`${inputClass} w-28`} value={policy.time} onChange={(e) => setPol({ ...policy, time: e.target.value })} placeholder="03:00" />
            </Field>
          )}
          <Field label="Keep last">
            <input className={`${inputClass} w-24`} type="number" min={1} max={365} value={policy.keep} onChange={(e) => setPol({ ...policy, keep: Number(e.target.value) })} />
          </Field>
          <Button loading={busy === "policy"} onClick={() => run("policy", () => api.setServerBackupPolicy(policy).then(() => setPol(null)), "Schedule saved.")}>
            Save schedule
          </Button>
        </div>
        {v.next_run && v.policy.mode !== "off" && <p className="text-sm text-neutral-500">Next backup {new Date(v.next_run).toLocaleString()}. Stored in <code>{v.dir}</code>.</p>}
      </div>

      <Destinations dests={v.destinations ?? []} onChanged={() => list.reload()} />

      {msg && (msg.ok ? <p className="text-sm text-ok">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}

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

const kinds: Record<BackupDest["kind"], string> = { s3: "S3 compatible (AWS, R2, B2, MinIO)", folder: "Folder on this server (mount a NAS here)", device: "Another device running an agent" };

function Destinations({ dests, onChanged }: { dests: BackupDest[]; onChanged: () => void }) {
  const devices = usePoll(api.devices, 0);
  const [draft, setDraft] = useState<BackupDest[] | null>(null);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const list = draft ?? dests;
  const set = (i: number, patch: Partial<BackupDest>) => setDraft(list.map((d, j) => (j === i ? { ...d, ...patch } : d)));
  const add = (kind: BackupDest["kind"]) =>
    setDraft([...list, { id: "", kind, name: kind === "s3" ? "Bucket" : kind === "folder" ? "Folder" : "Device", enabled: true, region: "auto", path: kind === "device" ? "/srv/orchestrator-backups" : "" }]);
  const save = async () => {
    setMsg(null);
    try {
      await api.setBackupDests(list);
      setDraft(null);
      setMsg({ ok: true, text: "Saved. New backups are copied to every enabled destination." });
      onChanged();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
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
      <p className="text-sm font-medium">Send copies elsewhere</p>
      <p className="text-sm text-neutral-500">A backup on the same disk does not survive the disk. Every new backup is copied to each enabled destination. Folders and devices keep the same number of copies; expire S3 objects with a bucket lifecycle rule.</p>
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
          {d.last_error && <ErrorNote>{d.last_error}</ErrorNote>}
          {d.kind === "s3" && (
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Endpoint" hint="For R2: https://<account>.r2.cloudflarestorage.com"><input className={inputClass} value={d.endpoint ?? ""} onChange={(e) => set(i, { endpoint: e.target.value })} placeholder="https://s3.amazonaws.com" /></Field>
              <Field label="Region" hint="auto for R2, otherwise e.g. eu-west-2"><input className={inputClass} value={d.region ?? ""} onChange={(e) => set(i, { region: e.target.value })} /></Field>
              <Field label="Bucket"><input className={inputClass} value={d.bucket ?? ""} onChange={(e) => set(i, { bucket: e.target.value })} /></Field>
              <Field label="Folder in bucket"><input className={inputClass} value={d.prefix ?? ""} onChange={(e) => set(i, { prefix: e.target.value })} placeholder="orchestrator" /></Field>
              <Field label="Access key ID"><input className={inputClass} value={d.access_key ?? ""} onChange={(e) => set(i, { access_key: e.target.value })} /></Field>
              <Field label="Secret access key"><input className={inputClass} type="password" value={d.secret_key ?? ""} placeholder={d.secret_set ? "Saved. Leave blank to keep" : ""} onChange={(e) => set(i, { secret_key: e.target.value })} /></Field>
            </div>
          )}
          {d.kind === "folder" && (
            <Field label="Folder" hint="Absolute path inside the server container. Bind-mount your NAS share (SMB or NFS mounted on the host) to this path."><input className={inputClass} value={d.path ?? ""} onChange={(e) => set(i, { path: e.target.value })} placeholder="/backups-nas" /></Field>
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
      <div className="flex flex-wrap gap-2">
        {(Object.keys(kinds) as BackupDest["kind"][]).map((k) => (
          <Button key={k} size="sm" onClick={() => add(k)}>Add {k === "s3" ? "S3" : k === "folder" ? "folder" : "device"}</Button>
        ))}
        {draft && <Button variant="primary" onClick={save}>Save destinations</Button>}
        {draft && <Button variant="ghost" onClick={() => setDraft(null)}>Discard</Button>}
      </div>
      {msg && (msg.ok ? <p className="text-sm text-ok">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}
    </div>
  );
}
