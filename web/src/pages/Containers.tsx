import { useState } from "react";
import { api, type FleetContainer } from "../api";
import { useCrumbs } from "../components/Layout";
import ContainerTable from "../components/ContainerTable";
import { Card, CardHeader, Loading, PageHeader, inputClass } from "../components/ui";
import { usePoll } from "../lib";

/** A stack is an Orchestrator project or a Docker Compose project; anything else is standalone. */
function stackOf(c: FleetContainer) {
  return c.project_name ? `Project · ${c.project_name}` : c.compose_project ? `Compose · ${c.compose_project}` : "Standalone containers";
}

export default function ContainersPage() {
  useCrumbs([{ label: "Workers" }]);
  const { data, reload } = usePoll(api.containers, 5000);
  const [q, setQ] = useState("");
  const [state, setState] = useState("all");
  const [group, setGroup] = useState(true);
  const rows = (data ?? []).filter(
    (c) =>
      (state === "all" || (state === "running" ? c.state === "running" : c.state !== "running")) &&
      `${c.name} ${c.image} ${c.device_name} ${c.project_name ?? ""} ${c.compose_project ?? ""} ${c.device_addresses.join(" ")}`
        .toLowerCase()
        .includes(q.toLowerCase()),
  );
  const stacks = new Map<string, FleetContainer[]>();
  for (const c of rows) stacks.set(stackOf(c), [...(stacks.get(stackOf(c)) ?? []), c]);
  const ordered = [...stacks.entries()].sort(([a], [b]) => (a.startsWith("Standalone") ? 1 : b.startsWith("Standalone") ? -1 : a.localeCompare(b)));

  return (
    <>
      <PageHeader
        title="Workers & stacks"
        description="Every container in the fleet: which device it runs on, its ports, and where to reach it."
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
              <CardHeader title={name} subtitle={`${list.filter((c) => c.state === "running").length} of ${list.length} running on ${new Set(list.map((c) => c.device_name)).size} device(s)`} />
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
