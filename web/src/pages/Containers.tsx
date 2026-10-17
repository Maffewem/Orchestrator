import { useState } from "react";
import { api } from "../api";
import { useCrumbs } from "../components/Layout";
import ContainerTable from "../components/ContainerTable";
import { Card, Loading, PageHeader, inputClass } from "../components/ui";
import { usePoll } from "../lib";

export default function ContainersPage() {
  useCrumbs([{ label: "Containers" }]);
  const { data, reload } = usePoll(api.containers, 5000);
  const [q, setQ] = useState("");
  const [state, setState] = useState("all");
  const rows = (data ?? []).filter(
    (c) =>
      (state === "all" || (state === "running" ? c.state === "running" : c.state !== "running")) &&
      `${c.name} ${c.image} ${c.device_name}`.toLowerCase().includes(q.toLowerCase()),
  );
  return (
    <>
      <PageHeader title="Containers" description="Every container across your fleet." />
      <div className="mb-4 flex flex-wrap gap-2">
        <input className={`${inputClass} max-w-xs`} placeholder="Filter by name, image or device" value={q} onChange={(e) => setQ(e.target.value)} />
        <select className={`${inputClass} w-auto`} value={state} onChange={(e) => setState(e.target.value)}>
          <option value="all">All states</option>
          <option value="running">Running</option>
          <option value="stopped">Stopped</option>
        </select>
      </div>
      <Card>{!data ? <Loading /> : <ContainerTable containers={rows} showDevice onChanged={reload} />}</Card>
    </>
  );
}
