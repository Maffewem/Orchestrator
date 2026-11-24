import { useState } from "react";
import { Link } from "react-router";
import { Archive, Download, Lock, RotateCcw, Trash2 } from "lucide-react";
import { api, type Backup, type BackupStorage } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, Empty, ErrorNote, Loading, PageHeader, Table, Td } from "../components/ui";
import { ago, bytes, usePoll } from "../lib";
import { confirmDialog } from "../components/dialogs";
import { useCan } from "../components/auth";

/** Says in words which device and folder the backup folder really is. */
function where(s: BackupStorage) {
  const l = s.location;
  const parts: string[] = [];
  if (l.host_path) parts.push(`${l.host_path}${l.device ? ` on ${l.device}` : " on the server's host"}`);
  else if (l.device) parts.push(`on ${l.device}`);
  if (l.volume) parts.push(`Docker volume ${l.volume}`);
  if (l.container) parts.push(`mounted into container ${l.container}`);
  return [parts.join(", ") || "unknown", l.note].filter(Boolean).join(". ");
}

/** What to do about a failed backup, from its error text. */
function nextStep(error: string) {
  const e = error.toLowerCase();
  if (e.includes("no space") || e.includes("disk space")) return "Free space on the backup disk: delete old backups, keep fewer, or move the folder to a larger disk. Then back up again.";
  if (e.includes("too old")) return "Update the agent from the Updates page, then back up again.";
  if (e.includes("could not read the container")) return "The container's data could not be read, for example because files changed while archiving. Try again; if it repeats, check the agent log on that device.";
  if (e.includes("http 4") || e.includes("http 5")) return "The server refused the upload. Check the server log and the free space shown above, then try again.";
  return "Try again from the Workers page. If it keeps failing, check the agent log on that device.";
}

const tone = { pending: "info", complete: "ok", failed: "bad" } as const;

export default function BackupsPage() {
  useCrumbs([{ label: "Backups" }]);
  const { data, error, reload } = usePoll(api.backups, 3000);
  const canOperate = useCan("operator");
  const isAdmin = useCan("admin");
  const storage = usePoll(() => (isAdmin ? api.backupStorage() : Promise.resolve(null)), 60000, [isAdmin]);
  const [busy, setBusy] = useState<string | null>(null);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const act = async (b: Backup, kind: "download" | "restore" | "delete") => {
    if (kind === "restore" && !(await confirmDialog({ title: `Restore ${b.container_name}?`, message: "Files in the backup overwrite the current data, and the container restarts.", confirmLabel: "Restore", danger: true }))) return;
    if (kind === "delete" && !(await confirmDialog({ title: "Delete this backup?", confirmLabel: "Delete", danger: true }))) return;
    setBusy(b.id + kind);
    setMsg(null);
    try {
      if (kind === "download") await api.downloadBackup(b);
      else if (kind === "delete") await api.deleteBackup(b.id);
      else {
        await api.restoreBackup(b.id);
        setMsg({ ok: true, text: `Restore of ${b.container_name} queued. Progress shows on the Activity page.` });
      }
      reload();
    } catch (e) {
      setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <>
      <PageHeader
        title="Backups"
        description="Each backup holds a container's configuration and the data in its volumes and bind mounts. Start one from the Workers page. Restoring writes the data back into the running container on its original device. Set a backup password in Settings to encrypt new backups."
      />
      {error && <ErrorNote>{error}</ErrorNote>}
      {storage.data && storage.data.total_bytes > 0 && (
        <p className={`mb-4 rounded-lg px-4 py-2 text-sm ${storage.data.level === "ok" ? "bg-neutral-100 text-neutral-700" : storage.data.level === "warning" ? "bg-amber-50" : "bg-red-50"}`}>
          {storage.data.level !== "ok" && <strong>Backups are using most of the disk. </strong>}
          {bytes(storage.data.backup_bytes)} of backups in <code>{storage.data.dir}</code> inside the server container, {bytes(storage.data.free_bytes)} free of {bytes(storage.data.total_bytes)}.
          {storage.data.level !== "ok" && " Delete old backups, keep fewer, or move them to a larger disk."}
          <span className="mt-1 block">
            <strong>Where that is:</strong> {where(storage.data)}
          </span>
          <span className="mt-1 block text-neutral-600">
            {bytes(storage.data.container_bytes)} worker backups, {bytes(storage.data.server_bytes)} server database backups, {bytes(storage.data.other_bytes)} other (partial uploads and leftovers). Copies sent elsewhere are listed under Settings, Server backups.
          </span>
        </p>
      )}
      {msg && (msg.ok ? <p className="mb-4 rounded-lg bg-blue-50 px-4 py-2 text-sm">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}
      {!data ? (
        <Loading />
      ) : data.length === 0 ? (
        <Empty icon={<Archive className="size-8" />} title="No backups yet">
          Open <Link to="/containers" className="text-primary hover:underline">Workers</Link> and use the archive button on a container.
        </Empty>
      ) : (
        <Card>
          <Table head={["Worker", "Device", "Stored at", "Status", "Size", "Taken", ""]}>
            {data.map((b) => (
              <tr key={b.id} className="hover:bg-neutral-50">
                <Td>
                  <span className="flex items-center gap-1.5 font-medium">
                    {b.container_name}
                    {b.encrypted && <Lock className="size-3 text-neutral-500" aria-label="Password-protected" />}
                  </span>
                  <span className="block font-mono text-xs text-neutral-500">{b.image}</span>
                </Td>
                <Td>
                  <Link to={`/devices/${b.device_id}`} className="text-primary hover:underline">
                    {b.device_name}
                  </Link>
                </Td>
                <Td className="max-w-56 text-xs text-neutral-600">
                  {storage.data ? (
                    <>
                      <code className="break-all">{storage.data.location.host_path ?? storage.data.dir}</code>
                      <span className="block">{storage.data.location.device ? `on ${storage.data.location.device} (server)` : "on the server"}{b.encrypted ? ", encrypted" : ""}</span>
                    </>
                  ) : (
                    "On the server"
                  )}
                  <span className="block text-neutral-500">Source: {b.device_name}</span>
                </Td>
                <Td>
                  <Badge tone={tone[b.status]}>{b.status === "pending" ? "In progress" : b.status === "complete" ? "Complete" : "Failed"}</Badge>
                  {b.error && <span className="mt-1 block max-w-64 text-xs text-bad">{b.error}</span>}
                  {b.status === "failed" && <span className="mt-1 block max-w-64 text-xs text-neutral-600">What to do: {nextStep(b.error ?? "")}</span>}
                </Td>
                <Td className="whitespace-nowrap text-neutral-600">{b.status === "complete" ? bytes(b.size) : "—"}</Td>
                <Td className="whitespace-nowrap text-neutral-600">{ago(b.created_at)}</Td>
                <Td>
                  <div className="flex justify-end gap-1">
                    {canOperate && b.status === "complete" && (
                      <>
                        <Button size="sm" variant="ghost" title="Download" loading={busy === b.id + "download"} icon={<Download className="size-3.5" />} onClick={() => act(b, "download")} />
                        <Button size="sm" variant="ghost" title="Restore" loading={busy === b.id + "restore"} icon={<RotateCcw className="size-3.5" />} onClick={() => act(b, "restore")} />
                      </>
                    )}
                    {canOperate && <Button size="sm" variant="ghost" title="Delete" loading={busy === b.id + "delete"} icon={<Trash2 className="size-3.5" />} onClick={() => act(b, "delete")} />}
                  </div>
                </Td>
              </tr>
            ))}
          </Table>
        </Card>
      )}
    </>
  );
}
