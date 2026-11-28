import { useState } from "react";
import { Eraser, RefreshCw } from "lucide-react";
import { api, type Device } from "../api";
import { bytes, usePoll } from "../lib";
import { Badge, Button, Card, CardHeader, ErrorNote, Loading, Modal } from "./ui";
import { confirmDialog, notify } from "./dialogs";
import { waitFor } from "./ContainerTable";

/** Images and build cache held by the device's Docker engine, with an admin-only prune. */
export default function DockerStorage({ d }: { d: Device }) {
  const rep = usePoll(() => api.dockerStorage(d.id), 60000, [d.id]);
  const [open, setOpen] = useState(false);
  const r = rep.data;
  return (
    <Card>
      <CardHeader
        title="Docker storage"
        subtitle={`Images and build cache on ${d.name}${r?.root_dir ? ` (${r.root_dir})` : ""}. Nothing here is removed unless retention is set under Settings or you prune it.`}
        action={
          <div className="flex flex-wrap gap-2">
            <Button icon={<RefreshCw className="size-4" />} onClick={rep.reload}>Refresh</Button>
            <Button icon={<Eraser className="size-4" />} onClick={() => setOpen(true)} disabled={!r}>Prune…</Button>
          </div>
        }
      />
      <div className="space-y-4 p-5 text-sm">
        {rep.error && <ErrorNote>{rep.error}</ErrorNote>}
        {!r && !rep.error && <Loading />}
        {r && (
          <>
            <dl className="grid gap-3 sm:grid-cols-4">
              <Stat label="Images" value={`${r.images} · ${bytes(r.layers_size)}`} sub="shared layers counted once" />
              <Stat label="Untagged" value={`${r.dangling} · ${bytes(r.dangling_size)}`} sub="left behind by new pulls" />
              <Stat label="Build cache" value={bytes(r.build_cache)} sub={`${bytes(r.build_cache_reclaimable)} not in use`} />
              <Stat label="Could be freed" value={`~${bytes(r.reclaimable)}`} sub={`keeping ${r.keep || "all"} per worker`} />
            </dl>
            {r.workers.length === 0 && <p className="text-neutral-500">No images built by Orchestrator are on this device.</p>}
            {r.workers.map((g) => (
              <div key={g.repo}>
                <div className="mb-1 flex items-center gap-2 font-medium">
                  {g.worker}
                  {g.orphaned && <Badge tone="warn">no worker</Badge>}
                </div>
                <ul className="divide-y divide-neutral-100 rounded-md border border-neutral-200">
                  {g.versions.map((v) => (
                    <li key={v.tag} className="flex flex-wrap items-center justify-between gap-2 px-3 py-1.5">
                      <span className="min-w-0 break-all font-mono text-xs">{v.tag.split(":").pop()}</span>
                      <span className="flex items-center gap-2 text-xs text-neutral-500">
                        {new Date(v.created * 1000).toLocaleDateString()} · {bytes(v.size)}
                        {v.in_use && <Badge tone="ok">running</Badge>}
                        {v.removable && <Badge tone="warn">would be removed</Badge>}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            ))}
            <p className="text-xs text-neutral-500">
              A removed GitHub-built version is rebuilt from its commit when you roll back or redeploy it, so rollback still works, only slower.
            </p>
          </>
        )}
      </div>
      {open && r && <PruneDialog d={d} onClose={() => { setOpen(false); rep.reload(); }} />}
    </Card>
  );
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div>
      <dt className="text-xs text-neutral-500">{label}</dt>
      <dd className="font-medium">{value}</dd>
      {sub && <dd className="text-xs text-neutral-500">{sub}</dd>}
    </div>
  );
}

function PruneDialog({ d, onClose }: { d: Device; onClose: () => void }) {
  const [trim, setTrim] = useState(true);
  const [dangling, setDangling] = useState(true);
  const [unused, setUnused] = useState(false);
  const [cache, setCache] = useState(true);
  const [allCache, setAllCache] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const run = async () => {
    const ok = await confirmDialog({
      title: `Prune Docker storage on ${d.name}?`,
      message: "Images are removed from the device's disk and cannot be undone here. GitHub-built versions can be rebuilt from their commit; pulled images can be pulled again if the registry still has them. Running workers are never touched.",
      confirmLabel: "Prune",
      danger: true,
    });
    if (!ok) return;
    setBusy(true);
    setError("");
    try {
      const r = await waitFor((await api.dockerPrune(d.id, { trim, dangling: dangling || unused, unused, build_cache: cache, cache_days: allCache ? 0 : 7 })).id);
      if (r.status === "failed") throw new Error(r.error ?? "Failed");
      const out = JSON.parse(r.output ?? "{}") as { reclaimed?: number };
      notify(`Freed about ${bytes(out.reclaimed ?? 0)}.`, "success");
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <Modal title={`Prune ${d.name}`} onClose={onClose}>
      <div className="space-y-3 p-5 text-sm">
        <Check on={trim} set={setTrim} label="Old worker images" hint="Applies the retention from Settings: the newest versions of each worker stay, plus anything running." />
        <Check on={dangling} set={setDangling} label="Untagged images" hint="Leftovers of pulls and builds whose tag moved on." disabled={unused} />
        <Check on={unused} set={setUnused} label="Every image no container uses" hint="Includes images pulled for stopped workers, the proxy and the tunnel. They are pulled again when needed." />
        <Check on={cache} set={setCache} label="Build cache" hint="Speeds up repeat builds; removing it makes the next build slower." />
        {cache && <Check on={allCache} set={setAllCache} label="All of it, not just cache older than 7 days" />}
        <p className="text-xs text-neutral-500">Images created in the last hour are always kept, in case a deployment is still running.</p>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="danger" loading={busy} onClick={run} disabled={!trim && !dangling && !unused && !cache}>Prune</Button>
        </div>
      </div>
    </Modal>
  );
}

function Check({ on, set, label, hint, disabled }: { on: boolean; set: (v: boolean) => void; label: string; hint?: string; disabled?: boolean }) {
  return (
    <label className="flex items-start gap-2">
      <input type="checkbox" className="mt-1" checked={on || !!disabled} disabled={disabled} onChange={(e) => set(e.target.checked)} />
      <span>
        <span className="font-medium">{label}</span>
        {hint && <span className="block text-xs text-neutral-500">{hint}</span>}
      </span>
    </label>
  );
}
