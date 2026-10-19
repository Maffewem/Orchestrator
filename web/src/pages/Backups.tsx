import { useState } from "react";
import { Link } from "react-router";
import { Archive, Download, Lock, RotateCcw, Trash2 } from "lucide-react";
import { api, type Backup } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, Empty, ErrorNote, Loading, PageHeader, Table, Td } from "../components/ui";
import { ago, bytes, usePoll } from "../lib";
import { confirmDialog } from "../components/dialogs";

const tone = { pending: "info", complete: "ok", failed: "bad" } as const;

export default function BackupsPage() {
  useCrumbs([{ label: "Backups" }]);
  const { data, error, reload } = usePoll(api.backups, 3000);
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
      {msg && (msg.ok ? <p className="mb-4 rounded-lg bg-blue-50 px-4 py-2 text-sm">{msg.text}</p> : <ErrorNote>{msg.text}</ErrorNote>)}
      {!data ? (
        <Loading />
      ) : data.length === 0 ? (
        <Empty icon={<Archive className="size-8" />} title="No backups yet">
          Open <Link to="/containers" className="text-primary hover:underline">Workers</Link> and use the archive button on a container.
        </Empty>
      ) : (
        <Card>
          <Table head={["Worker", "Device", "Status", "Size", "Taken", ""]}>
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
                <Td>
                  <Badge tone={tone[b.status]}>{b.status === "pending" ? "In progress" : b.status === "complete" ? "Complete" : "Failed"}</Badge>
                  {b.error && <span className="mt-1 block max-w-64 text-xs text-bad">{b.error}</span>}
                </Td>
                <Td className="whitespace-nowrap text-neutral-600">{b.status === "complete" ? bytes(b.size) : "—"}</Td>
                <Td className="whitespace-nowrap text-neutral-600">{ago(b.created_at)}</Td>
                <Td>
                  <div className="flex justify-end gap-1">
                    {b.status === "complete" && (
                      <>
                        <Button size="sm" variant="ghost" title="Download" loading={busy === b.id + "download"} icon={<Download className="size-3.5" />} onClick={() => act(b, "download")} />
                        <Button size="sm" variant="ghost" title="Restore" loading={busy === b.id + "restore"} icon={<RotateCcw className="size-3.5" />} onClick={() => act(b, "restore")} />
                      </>
                    )}
                    <Button size="sm" variant="ghost" title="Delete" loading={busy === b.id + "delete"} icon={<Trash2 className="size-3.5" />} onClick={() => act(b, "delete")} />
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
