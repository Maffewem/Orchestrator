import { useEffect, useState } from "react";
import { Link } from "react-router";
import { Network } from "lucide-react";
import { api, type ClusterPolicy } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Card, CardHeader, Empty, ErrorNote, Loading, PageHeader, Toggle, inputClass } from "../components/ui";
import { ago, usePoll } from "../lib";
import ServerHA from "../components/ServerHA";

export default function ClusterPage() {
  useCrumbs([{ label: "Cluster" }]);
  const { data, error, reload } = usePoll(api.cluster, 5000);
  const devices = usePoll(api.devices, 10000).data ?? [];
  const [err, setErr] = useState("");
  const [grace, setGrace] = useState(120);
  useEffect(() => {
    if (data) setGrace(data.config.grace_seconds);
  }, [data?.config.grace_seconds]); // eslint-disable-line react-hooks/exhaustive-deps

  const run = async (f: () => Promise<unknown>) => {
    setErr("");
    try {
      await f();
      reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };
  const setPolicy = (id: string, p: ClusterPolicy, patch: Partial<ClusterPolicy>) => run(() => api.setClusterPolicy(id, { ...p, ...patch }));

  return (
    <>
      <PageHeader
        title={<span className="flex items-center gap-2">Cluster <Badge tone="info">Beta</Badge></span>}
        description="Let workers move to another device when their own goes offline. A failover restores the newest backup on the server into a new container on the chosen device, so data written since that backup is lost. Failover never runs on its own until you turn it on here and choose the workers."
      />
      {(error || err) && <ErrorNote>{error || err}</ErrorNote>}
      {!data ? (
        <Loading />
      ) : (
        <>
          <ServerHA />
          <Card className="mb-6">
            <CardHeader title="Automatic failover" subtitle="Applies to the workers you switch on below." />
            <div className="flex flex-wrap items-center gap-6 p-4 text-sm">
              <label className="flex items-center gap-3">
                <Toggle checked={data.config.enabled} label="Automatic failover" onChange={(v) => run(() => api.setCluster({ enabled: v, grace_seconds: grace }))} />
                {data.config.enabled ? "On" : "Off"}
              </label>
              <label className="flex items-center gap-2">
                Move workers after a device has been offline for
                <input type="number" min={30} max={3600} value={grace} onChange={(e) => setGrace(Number(e.target.value))} onBlur={() => run(() => api.setCluster({ enabled: data.config.enabled, grace_seconds: grace }))} className={inputClass + " w-24"} />
                seconds
              </label>
            </div>
            <p className="px-4 pb-4 text-xs text-neutral-500">
              Nothing moves while every device is offline (the server may be the one cut off), while a device is in maintenance mode, or within the wait after the server restarts. When the original device comes back, the stale copy there is stopped so two instances never run at once.
            </p>
          </Card>
          {data.workers.length === 0 ? (
            <Empty icon={<Network className="size-8" />} title="No workers yet">Create a project on <Link to="/projects" className="text-primary hover:underline">Projects</Link> first.</Empty>
          ) : (
            <div className="space-y-3">
              {data.workers.map((w) => {
                const p = w.policy;
                return (
                  <Card key={w.project_id}>
                    <div className="flex flex-wrap items-center justify-between gap-3 p-4">
                      <div>
                        <Link to={`/projects/${w.project_id}`} className="font-medium hover:underline">{w.name}</Link>
                        <span className="ml-2 text-sm text-neutral-500">on {w.device || "(unknown device)"}</span>
                        {w.last_failover && (
                          <p className="mt-1 text-xs text-neutral-500">
                            Failed over from {w.last_failover.from_name} to {w.last_failover.to_name} {ago(w.last_failover.at)}
                            {w.last_failover.with_data ? " with the newest backup" : " without data"}
                            {w.last_failover.home ? "" : "; waiting for the original device to return"}.
                          </p>
                        )}
                      </div>
                      <label className="flex items-center gap-2 text-sm">
                        <Toggle checked={p.failover} label={`Failover for ${w.name}`} onChange={(v) => setPolicy(w.project_id, p, { failover: v })} />
                        Fail over
                      </label>
                    </div>
                    <div className="space-y-2 border-t border-neutral-100 p-4 text-sm">
                      <p className="font-medium">Replicas <span className="font-normal text-neutral-500">(an extra, independent copy on each device ticked; deployed together with the main one. Nothing is synced or moved, so the app must cope with several copies)</span></p>
                      <div className="flex flex-wrap gap-x-5 gap-y-1">
                        {devices.filter((d) => d.id !== w.device_id).map((d) => {
                          const st = w.replicas?.find((r) => r.device_id === d.id);
                          return (
                            <label key={d.id} className="flex items-center gap-1.5">
                              <input type="checkbox" checked={(p.replicas ?? []).includes(d.id)} onChange={(e) => setPolicy(w.project_id, p, { replicas: e.target.checked ? [...(p.replicas ?? []), d.id] : (p.replicas ?? []).filter((x) => x !== d.id) })} />
                              {d.name}
                              {st && <Badge tone={st.running ? "ok" : st.online ? "warn" : "bad"}>{st.running ? "running" : st.online ? "starting" : "offline"}</Badge>}
                            </label>
                          );
                        })}
                      </div>
                    </div>
                    {p.failover && (
                      <div className="space-y-3 border-t border-neutral-100 p-4 text-sm">
                        <div>
                          <p className="mb-1 font-medium">Allowed devices <span className="font-normal text-neutral-500">(none ticked means any online device)</span></p>
                          <div className="flex flex-wrap gap-x-5 gap-y-1">
                            {devices.filter((d) => d.id !== w.device_id).map((d) => (
                              <label key={d.id} className="flex items-center gap-1.5">
                                <input type="checkbox" checked={(p.allowed ?? []).includes(d.id)} onChange={(e) => setPolicy(w.project_id, p, { allowed: e.target.checked ? [...(p.allowed ?? []), d.id] : (p.allowed ?? []).filter((x) => x !== d.id) })} />
                                {d.name}
                              </label>
                            ))}
                          </div>
                        </div>
                        <label className="flex items-center gap-2"><input type="checkbox" checked={p.fail_back} onChange={(e) => setPolicy(w.project_id, p, { fail_back: e.target.checked })} /> Move back automatically once the original device has been online for the wait above</label>
                        <label className="flex items-center gap-2"><input type="checkbox" checked={p.start_empty} onChange={(e) => setPolicy(w.project_id, p, { start_empty: e.target.checked })} /> Start with no data if the server has no backup of it (only for workers that keep nothing worth saving)</label>
                      </div>
                    )}
                  </Card>
                );
              })}
            </div>
          )}
        </>
      )}
    </>
  );
}
