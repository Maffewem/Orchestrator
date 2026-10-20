import { useState } from "react";
import { Link } from "react-router";
import { AlertTriangle, PackagePlus } from "lucide-react";
import { api, type AdoptItem, type Container, type FleetContainer } from "../api";
import { Badge, Button, ErrorNote, Modal, inputClass } from "./ui";

/** Containers Orchestrator can adopt: not already a project, not part of Orchestrator itself. */
export function adoptable(c: Container & { project_name?: string }): boolean {
  return !c.project_name && !c.labels?.["orchestrator.project"] && !c.name.startsWith("orchestrator");
}

/** Orchestrator's own containers: server, agent, proxy and update helper. */
export function isSystem(c: { name: string; project_name?: string; labels?: Record<string, string> }): boolean {
  return !c.project_name && !c.labels?.["orchestrator.project"] && c.name.startsWith("orchestrator");
}

/** Left behind by a deleted project: labelled as Orchestrator's but no project owns it any more. */
export function isLeftover(c: { name: string; project_name?: string; labels?: Record<string, string> }): boolean {
  return !c.project_name && !!c.labels?.["orchestrator.project"] && !isSystem(c);
}

/** Old blue/green copies (`name-previous-<timestamp>`) that nothing will start again. */
export function isPreviousCopy(c: { name: string; state: string; project_name?: string }): boolean {
  return c.state !== "running" && /-previous-\d+$/.test(c.name);
}

/** Exited containers that are safe to offer for clean-up: leftovers of deleted projects and old blue/green copies. */
export function cleanable(c: { name: string; state: string; project_name?: string; labels?: Record<string, string> }): boolean {
  return c.state !== "running" && !c.project_name && !isSystem(c) && (isLeftover(c) || isPreviousCopy(c));
}

/** A name for the project made from a container: lowercase letters, digits and dashes. */
export function projectNameFor(container: string) {
  return container.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63);
}

/**
 * Turns containers that already run on a device into projects. It reads each container first and
 * shows what it found; nothing is stopped, renamed or recreated by adopting.
 */
export function AdoptDialog({
  deviceId,
  deviceName,
  containers,
  selected,
  onClose,
  onDone,
}: {
  deviceId: string;
  deviceName: string;
  containers: Container[];
  selected?: string[];
  onClose: () => void;
  onDone: () => void;
}) {
  const choices = containers.filter(adoptable);
  const [picked, setPicked] = useState<Set<string>>(new Set(selected ?? []));
  const [names, setNames] = useState<Record<string, string>>({});
  const [preview, setPreview] = useState<AdoptItem[] | null>(null);
  const [result, setResult] = useState<AdoptItem[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const chosen = () => choices.filter((c) => picked.has(c.id)).map((c) => ({ id: c.id, name: names[c.id] || undefined }));
  const run = async (dry: boolean) => {
    setBusy(true);
    setError(null);
    try {
      const items = await api.adopt(deviceId, chosen(), dry);
      if (dry) setPreview(items);
      else {
        setResult(items);
        onDone();
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const toggle = (id: string) => {
    const next = new Set(picked);
    if (!next.delete(id)) next.add(id);
    setPicked(next);
  };

  if (result) {
    const ok = result.filter((i) => i.status === "adopted");
    return (
      <Modal title="Containers adopted" onClose={onClose} wide>
        <p className="mb-3 text-sm text-neutral-600">
          {ok.length} of {result.length} became projects. The containers were left running exactly as they were.
        </p>
        <ItemList items={result} />
        <div className="mt-5 flex justify-end gap-2">
          {ok.length === 1 && (
            <Link to={`/projects/${ok[0].project_id}`}>
              <Button>Open project</Button>
            </Link>
          )}
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        </div>
      </Modal>
    );
  }

  if (preview) {
    const ready = preview.filter((i) => i.status === "ready");
    return (
      <Modal title="Adopt as projects" onClose={onClose} wide>
        <p className="mb-3 text-sm text-neutral-600">
          This is what Orchestrator read from {deviceName}. Adopting only creates projects; it does not stop, rename or recreate any container. A
          container is replaced only when you deploy its project.
        </p>
        <ItemList items={preview} />
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="mt-5 flex justify-end gap-2">
          <Button onClick={() => setPreview(null)}>Back</Button>
          <Button variant="primary" disabled={ready.length === 0} loading={busy} icon={<PackagePlus className="size-4" />} onClick={() => run(false)}>
            Adopt {ready.length} {ready.length === 1 ? "container" : "containers"}
          </Button>
        </div>
      </Modal>
    );
  }

  return (
    <Modal title={`Import containers from ${deviceName}`} onClose={onClose} wide>
      <p className="mb-3 text-sm text-neutral-600">
        Choose containers that already run on this device (started by hand, with Docker Compose or from Portainer) to manage them as projects. They
        keep their image, environment, ports, volumes and restart policy.
      </p>
      {choices.length === 0 ? (
        <p className="py-6 text-center text-sm text-neutral-500">Every container on this device is already a project.</p>
      ) : (
        <>
          <div className="mb-2 flex items-center gap-3 text-sm">
            <button className="text-primary hover:underline" onClick={() => setPicked(new Set(choices.map((c) => c.id)))}>
              Select all
            </button>
            <button className="text-primary hover:underline" onClick={() => setPicked(new Set())}>
              Select none
            </button>
          </div>
          <div className="max-h-96 divide-y divide-neutral-100 overflow-y-auto rounded-lg border border-neutral-200">
            {choices.map((c) => (
              <div key={c.id} className="flex items-center gap-3 px-3 py-2 text-sm">
                <input type="checkbox" className="size-4 accent-primary" checked={picked.has(c.id)} onChange={() => toggle(c.id)} aria-label={`Adopt ${c.name}`} />
                <div className="min-w-0 flex-1">
                  <p className="truncate font-medium">
                    {c.name}
                    {c.compose_project && <span className="ml-2 text-xs font-normal text-neutral-500">compose: {c.compose_project}</span>}
                  </p>
                  <p className="truncate font-mono text-xs text-neutral-500">{c.image}</p>
                </div>
                {picked.has(c.id) && (
                  <input
                    className={`${inputClass} !h-8 max-w-48 font-mono text-xs`}
                    placeholder={projectNameFor(c.name)}
                    aria-label={`Project name for ${c.name}`}
                    value={names[c.id] ?? ""}
                    onChange={(e) => setNames({ ...names, [c.id]: e.target.value })}
                  />
                )}
              </div>
            ))}
          </div>
        </>
      )}
      {error && <ErrorNote>{error}</ErrorNote>}
      <div className="mt-5 flex justify-end gap-2">
        <Button onClick={onClose}>Cancel</Button>
        <Button variant="primary" disabled={picked.size === 0} loading={busy} onClick={() => run(true)}>
          Review {picked.size || ""} {picked.size === 1 ? "container" : "containers"}
        </Button>
      </div>
    </Modal>
  );
}

function ItemList({ items }: { items: AdoptItem[] }) {
  return (
    <div className="max-h-96 divide-y divide-neutral-100 overflow-y-auto rounded-lg border border-neutral-200">
      {items.map((i) => (
        <div key={i.container_id} className="px-3 py-2.5 text-sm">
          <div className="flex items-center justify-between gap-3">
            <p className="truncate font-medium">
              {i.container_name || i.container_id}
              {i.project_name && <span className="font-normal text-neutral-500"> → project {i.project_name}</span>}
            </p>
            <Badge tone={i.status === "failed" ? "bad" : i.status === "skipped" ? "neutral" : "ok"}>
              {i.status === "ready" ? "will adopt" : i.status}
            </Badge>
          </div>
          {i.reason && <p className="mt-1 text-xs text-neutral-600">{i.reason}</p>}
          {i.image && (
            <p className="mt-1 truncate font-mono text-xs text-neutral-500">
              {i.image} · {i.ports?.length ?? 0} ports · {i.volumes?.length ?? 0} volumes · {i.env_count} variables
            </p>
          )}
          {i.compose_project && i.status !== "skipped" && (
            <p className="mt-1 flex items-start gap-1.5 text-xs text-amber-700">
              <AlertTriangle className="mt-0.5 size-3 shrink-0" />
              Part of the compose project {i.compose_project}: deploying recreates it on its own, outside compose.
            </p>
          )}
          {i.warnings?.map((w) => (
            <p key={w} className="mt-1 flex items-start gap-1.5 text-xs text-amber-700">
              <AlertTriangle className="mt-0.5 size-3 shrink-0" />
              Not carried over by a redeploy: {w}
            </p>
          ))}
        </div>
      ))}
    </div>
  );
}

/** "Import containers" for the fleet page: picks a device, then opens the dialog for it. */
export function ImportContainers({ containers, onDone }: { containers: FleetContainer[]; onDone: () => void }) {
  const devices = new Map<string, string>();
  for (const c of containers) if (adoptable(c)) devices.set(c.device_id, c.device_name);
  const [device, setDevice] = useState("");
  const [open, setOpen] = useState(false);
  if (devices.size === 0) return null;
  const id = devices.has(device) ? device : [...devices.keys()][0];
  return (
    <>
      {devices.size > 1 && (
        <select className={inputClass.replace("w-full", "w-auto")} value={id} onChange={(e) => setDevice(e.target.value)} aria-label="Device to import from">
          {[...devices].map(([k, name]) => (
            <option key={k} value={k}>
              {name}
            </option>
          ))}
        </select>
      )}
      <Button icon={<PackagePlus className="size-4" />} onClick={() => setOpen(true)}>
        Import containers
      </Button>
      {open && (
        <AdoptDialog
          deviceId={id}
          deviceName={devices.get(id) ?? ""}
          containers={containers.filter((c) => c.device_id === id)}
          onClose={() => setOpen(false)}
          onDone={onDone}
        />
      )}
    </>
  );
}
