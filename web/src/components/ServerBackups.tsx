import { useRef, useState } from "react";
import { Download, RotateCcw, Trash2, Upload } from "lucide-react";
import { api, type ServerBackupPolicy } from "../api";
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

      {msg && (msg.ok ? <p className="text-sm text-ok">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}

      <div className="overflow-x-auto border-t border-neutral-200 pt-4">
        {v.backups.length === 0 ? (
          <p className="text-sm text-neutral-500">No server backups yet.</p>
        ) : (
          <table className="w-full text-sm">
            <tbody>
              {v.backups.map((b) => (
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
