import { useState } from "react";
import { GitPullRequest, Trash2 } from "lucide-react";
import { api, type Preview } from "../api";
import { Link } from "react-router";
import { Badge, Card, CardHeader, Empty, ErrorNote, Loading, Table, Td } from "./ui";
import { Toggle } from "./Domains";
import { useCan } from "./auth";
import { ago, usePoll } from "../lib";
import { confirmDialog } from "./dialogs";

export function PreviewStatus({ s }: { s: Preview["status"] }) {
  if (s === "succeeded") return <Badge tone="ok">Ready</Badge>;
  if (s === "failed") return <Badge tone="bad">Failed</Badge>;
  return <Badge>{s === "running" ? "Building" : "Queued"}</Badge>;
}

export default function PreviewsPanel({ projectId }: { projectId: string }) {
  const q = usePoll(() => api.projectPreviews(projectId), 5000, [projectId]);
  const can = useCan("operator");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const v = q.data;
  if (!v) return q.error ? <ErrorNote>{q.error}</ErrorNote> : <Loading />;
  const save = async (enabled: boolean, max: number) => {
    setBusy(true);
    setError(null);
    try {
      await api.setPreviewSettings(projectId, { enabled, max });
      q.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="space-y-6">
      {error && <ErrorNote>{error}</ErrorNote>}
      <Card>
        <CardHeader title="Preview deployments" subtitle="Every open pull request from this repository gets its own build and URL. They are removed when the pull request closes." />
        <div className="flex flex-wrap items-center justify-between gap-4 px-5 py-4">
          <label className="flex items-center gap-3 text-sm">
            <Toggle on={v.settings.enabled} label="Preview deployments" disabled={!can || busy} onChange={(on) => save(on, v.settings.max)} />
            {v.settings.enabled ? "On" : "Off"}
          </label>
          <label className="flex items-center gap-2 text-sm text-neutral-600">
            Keep at most
            <select
              className="h-8 rounded-md border border-neutral-300 bg-white px-2"
              value={v.settings.max}
              disabled={!can || busy}
              onChange={(e) => save(v.settings.enabled, Number(e.target.value))}
            >
              {[1, 2, 3, 5, 8, 10, 20].map((n) => <option key={n}>{n}</option>)}
            </select>
            previews
          </label>
        </div>
        {!v.supported && <p className="border-t border-neutral-100 px-5 py-3 text-sm text-warn">This device's agent is too old for previews. Update it from the Updates page.</p>}
        <p className="border-t border-neutral-100 px-5 py-3 text-xs text-neutral-500">
          Pull requests from forks are never built. Previews with a domain use <Link to="../domains" className="text-primary hover:underline">the Preview URL switch on Domains</Link>; otherwise they are served on the device's IP and a port.
        </p>
      </Card>
      <Card>
        {v.previews.length === 0 ? (
          <Empty icon={<GitPullRequest className="size-8" />} title="No previews yet">
            {v.settings.enabled ? "Open a pull request against the tracked branch and it will appear here." : "Turn previews on to build open pull requests."}
          </Empty>
        ) : (
          <Table head={["Pull request", "Branch", "Status", "URL", "Updated", ""]}>
            {v.previews.map((p) => (
              <tr key={p.id}>
                <Td>
                  <span className="font-medium">#{p.pr}</span> {p.title}
                  <p className="text-xs text-neutral-500">by {p.author}</p>
                </Td>
                <Td className="font-mono text-xs">{p.branch}</Td>
                <Td><Link to={`/deployments/${p.deployment_id}`}><PreviewStatus s={p.status} /></Link></Td>
                <Td>{p.url ? <a href={p.url} target="_blank" rel="noreferrer" className="break-all text-primary hover:underline">{p.url.replace(/^https?:\/\//, "")}</a> : "—"}</Td>
                <Td className="whitespace-nowrap text-neutral-500">{ago(p.updated_at)}</Td>
                <Td>
                  {can && (
                    <button
                      aria-label={`Delete preview ${p.branch}`}
                      className="rounded p-1 text-neutral-500 hover:bg-neutral-100 hover:text-bad"
                      onClick={async () => {
                        if (!(await confirmDialog({ title: `Delete the preview for ${p.branch}?`, confirmLabel: "Delete", danger: true }))) return;
                        await api.deletePreview(projectId, p.id);
                        q.reload();
                      }}
                    >
                      <Trash2 className="size-4" />
                    </button>
                  )}
                </Td>
              </tr>
            ))}
          </Table>
        )}
      </Card>
    </div>
  );
}
