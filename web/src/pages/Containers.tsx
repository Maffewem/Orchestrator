import { useState } from "react";
import { api, type FleetContainer } from "../api";
import { useCrumbs } from "../components/Layout";
import ContainerTable from "../components/ContainerTable";
import { waitFor } from "../components/ContainerTable";
import { confirmDialog, notify } from "../components/dialogs";
import { Button } from "../components/ui";
import { Trash2 } from "lucide-react";
import { ImportContainers, cleanable, isSystem } from "../components/Adopt";
import { useCan } from "../components/auth";
import { Card, CardHeader, Loading, PageHeader, inputClass } from "../components/ui";
import { usePoll } from "../lib";

const SYSTEM = "System · Orchestrator (read-only)";

/** A stack is an Orchestrator project or a Docker Compose project; anything else is standalone. */
function stackOf(c: FleetContainer) {
  return isSystem(c) ? SYSTEM : c.project_name ? `Project · ${c.project_name}` : c.compose_project ? `Compose · ${c.compose_project}` : "Standalone containers";
}

export default function ContainersPage() {
  useCrumbs([{ label: "Workers" }]);
  const { data, reload } = usePoll(api.containers, 5000);
  const [q, setQ] = useState("");
  const [state, setState] = useState("all");
  const [group, setGroup] = useState(true);
  const isAdmin = useCan("admin");
  const rows = (data ?? []).filter(
    (c) =>
      (state === "all" || (state === "running" ? c.state === "running" : c.state !== "running")) &&
      `${c.name} ${c.image} ${c.device_name} ${c.project_name ?? ""} ${c.compose_project ?? ""} ${c.device_addresses.join(" ")}`
        .toLowerCase()
        .includes(q.toLowerCase()),
  );
  const leftovers = (data ?? []).filter(cleanable);
  const cleanUp = async () => {
    const list = leftovers.map((c) => `• ${c.name} on ${c.device_name}`).join("\n");
    const ok = await confirmDialog({
      title: `Remove ${leftovers.length} exited ${leftovers.length === 1 ? "container" : "containers"}?`,
      message: `Exited containers made by Orchestrator that no project owns, and old blue/green copies. Volumes are kept.\n${list}`,
      confirmLabel: "Remove",
      danger: true,
    });
    if (!ok) return;
    let failed = 0;
    for (const c of leftovers) {
      try {
        if ((await waitFor((await api.command(c.device_id, "container.remove", c.id)).id)).status === "failed") failed++;
      } catch (e) {
        failed++;
        notify(`${c.name}: ${e instanceof Error ? e.message : String(e)}`);
      }
    }
    if (failed) notify(`${failed} could not be removed.`);
    reload();
  };
  const stacks = new Map<string, FleetContainer[]>();
  for (const c of rows) stacks.set(stackOf(c), [...(stacks.get(stackOf(c)) ?? []), c]);
  const ordered = [...stacks.entries()].sort(([a], [b]) => (a === SYSTEM ? 1 : b === SYSTEM ? -1 : a.startsWith("Standalone") ? 1 : b.startsWith("Standalone") ? -1 : a.localeCompare(b)));

  return (
    <>
      <PageHeader
        title="Workers & stacks"
        description="Every container in the fleet: which device it runs on, its ports, and where to reach it."
        actions={
          isAdmin &&
          data && (
            <>
              {leftovers.length > 0 && (
                <Button icon={<Trash2 className="size-4" />} onClick={cleanUp}>
                  Clean up {leftovers.length} exited
                </Button>
              )}
              <ImportContainers containers={data} onDone={reload} />
            </>
          )
        }
      />
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <input className={`${inputClass} max-w-xs`} placeholder="Filter by name, image, device or IP" value={q} onChange={(e) => setQ(e.target.value)} />
        <select className={inputClass.replace("w-full", "w-auto")} value={state} onChange={(e) => setState(e.target.value)}>
          <option value="all">All states</option>
          <option value="running">Running</option>
          <option value="stopped">Stopped</option>
        </select>
        <label className="ml-1 flex items-center gap-2 text-sm text-neutral-700">
          <input type="checkbox" className="size-4 accent-primary" checked={group} onChange={(e) => setGroup(e.target.checked)} />
          Group by stack
        </label>
      </div>
      {!data ? (
        <Loading />
      ) : group && rows.length ? (
        <div className="space-y-4">
          {ordered.map(([name, list]) => (
            <Card key={name}>
              <CardHeader title={name} subtitle={`${name === SYSTEM ? "Orchestrator's own containers. Kept apart so they aren't removed by accident; only restart and logs are offered. " : ""}${list.filter((c) => c.state === "running").length} of ${list.length} running on ${new Set(list.map((c) => c.device_name)).size} device(s)`} />
              <ContainerTable containers={list} showDevice onChanged={reload} />
            </Card>
          ))}
        </div>
      ) : (
        <Card>
          <ContainerTable containers={rows} showDevice onChanged={reload} />
        </Card>
      )}
    </>
  );
}
