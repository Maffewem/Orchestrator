import { useEffect, useState } from "react";
import { Link } from "react-router";
import { Network } from "lucide-react";
import { api, ApiError, type ClusterPolicy, type ClusterView, type ReplicaInfo } from "../api";
import { confirmDialog, confirmWithOption } from "../components/dialogs";
import { useCrumbs } from "../components/Layout";
import { Badge, Card, CardHeader, Empty, ErrorNote, Loading, PageHeader, Toggle, inputClass } from "../components/ui";
import { ago, bytes, usePoll } from "../lib";
import FailoverHistory from "../components/FailoverHistory";
import ServerHA from "../components/ServerHA";

export function ReplicaBadge({ st, onRetry }: { st: ReplicaInfo; onRetry?: () => void }) {
  const retry = onRetry ? <button type="button" onClick={onRetry} className="ml-2 text-xs text-primary hover:underline">Retry</button> : null;
  if (st.running) return <Badge tone="ok">running</Badge>;
  if (st.maintenance) return <Badge tone="warn">in maintenance: will deploy when it ends</Badge>;
  if (!st.online) return <Badge tone="bad">offline: will deploy when it is back</Badge>;
  const link = st.deployment_id ? <Link to={`/deployments/${st.deployment_id}`} className="ml-1 text-xs text-primary hover:underline">deployment</Link> : null;
  if (st.deployment_status === "failed") return <span><Badge tone="bad">deployment failed</Badge>{link}{retry}{st.deployment_error && <span className="mt-1 block text-xs text-red-700">{st.deployment_error}</span>}</span>;
  if (st.deployment_status === "queued" || st.deployment_status === "running") return <span><Badge tone="info">deploying</Badge>{link}</span>;
  if (st.deployment_status === "succeeded") return <span><Badge tone="warn">{st.state ? st.state : "container not found"}</Badge>{link}{retry}</span>;
  return <span><Badge tone="warn">waiting for the server to deploy it</Badge>{retry}</span>;
}

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

  // Ticking warns when the device already uses the worker's published ports; unticking says what
  // happens to the copy that is already running there and lets you stop it or remove it.
  const toggleReplica = async (w: ClusterView["workers"][number], deviceId: string, deviceName: string, on: boolean) => {
    const p = w.policy;
    const replicas = on ? [...(p.replicas ?? []), deviceId] : (p.replicas ?? []).filter((x) => x !== deviceId);
    const next = { ...p, replicas };
    if (on) {
      if (devices.find((d) => d.id === deviceId)?.maintenance && !(await confirmDialog({ title: `${deviceName} is in maintenance`, message: `Nothing is deployed to a device in maintenance. The copy of ${w.name} is queued and starts by itself when maintenance ends.`, confirmLabel: "Add and wait" }))) return;
      return run(async () => {
        try {
          await api.setClusterPolicy(w.project_id, next);
        } catch (e) {
          if (!(e instanceof ApiError && e.status === 409)) throw e;
          if (!(await confirmDialog({ title: `${deviceName} can't run ${w.name} yet`, message: `${e.message}. You can add it anyway and fix this later, then use Retry.`, confirmLabel: "Add anyway" }))) return;
          await api.setClusterPolicy(w.project_id, next, { confirm: true });
        }
      });
    }
    const st = w.replicas?.find((r) => r.device_id === deviceId);
    if (!st?.container_id) return run(() => api.setClusterPolicy(w.project_id, next));
    const remove = await confirmWithOption({
      title: `Take ${deviceName} off ${w.name}?`,
      message: `A copy of ${w.name} is running on ${deviceName}. Stopping keeps its container and data there, so it can be started again. Tick the box to remove the container instead.`,
      confirmLabel: "Continue",
      danger: true,
      option: { label: "Remove the container instead of only stopping it", checked: false },
    });
    if (remove === null) return;
    if (!remove) return run(() => api.setClusterPolicy(w.project_id, next, { on_remove: "stop" }));
    const volumes = await confirmWithOption({
      title: `Remove ${w.name} from ${deviceName}?`,
      message: "The container is deleted. Data in bind-mounted folders stays on the device; named volumes are kept unless you tick the box below.",
      confirmLabel: "Remove",
      danger: true,
      option: { label: "Also delete its volumes (data is lost)", checked: false },
    });
    if (volumes === null) return;
    return run(() => api.setClusterPolicy(w.project_id, next, { on_remove: "remove", delete_volumes: volumes }));
  };

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
          {data.capacity.enabled && (
            <Card className="mb-6">
              <CardHeader
                title="Capacity"
                subtitle="Whether the other devices have room to take over if one is lost, using what each worker uses now."
                action={<Badge tone={data.capacity.survives_one ? "ok" : "warn"}>{data.capacity.survives_one ? "Can survive losing any one device" : "Some device could not be lost safely"}</Badge>}
              />
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="text-left text-xs text-neutral-500">
                      <th className="px-4 py-2 font-medium">Device</th>
                      <th className="px-4 py-2 font-medium">Free</th>
                      <th className="px-4 py-2 font-medium">Failover workers</th>
                      <th className="px-4 py-2 font-medium">If this device is lost</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.capacity.devices.map((d) => (
                      <tr key={d.device_id} className="border-t border-neutral-100 align-top">
                        <td className="px-4 py-2 font-medium">{d.device}{!d.online && <span className="ml-2"><Badge tone="bad">offline</Badge></span>}</td>
                        <td className="px-4 py-2 text-neutral-600">{d.cores ? `${d.cores} cores, ` : ""}{d.mem_free ? `${bytes(d.mem_free)} RAM, ` : ""}{d.disk_free ? `${bytes(d.disk_free)} disk` : "not reported"}</td>
                        <td className="px-4 py-2">{d.workers}</td>
                        <td className="px-4 py-2">
                          {d.workers === 0 ? <span className="text-neutral-500">nothing to move</span> : d.can_lose ? <Badge tone="ok">everything fits elsewhere</Badge> : <Badge tone="warn">{(d.stranded ?? []).join(", ")} would not fit</Badge>}
                          {(d.reasons ?? []).map((r) => <p key={r} className="mt-1 text-xs text-neutral-500">{r}</p>)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <p className="px-4 py-3 text-xs text-neutral-500">Estimates come from each worker's latest memory, CPU and volume size, and a target is treated as full at 90%. A worker with no usage reading yet is not counted, so the answer is only as good as the data.</p>
            </Card>
          )}
          {data.workers.length === 0 ? (
            <Empty icon={<Network className="size-8" />} title="No workers yet">Create a project on <Link to="/projects" className="text-primary hover:underline">Projects</Link> first.</Empty>
          ) : (
            <div className="space-y-3">
              {data.workers.map((w) => {
                const p = w.policy;
                return (
                  <Card key={w.project_id}>
                    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 p-3">
                      <div className="flex min-w-0 flex-1 flex-wrap items-center gap-2 text-sm">
                        <Link to={`/projects/${w.project_id}`} className="mr-1 font-medium hover:underline">{w.name}</Link>
                        <span className="inline-flex items-center gap-1.5 rounded-full border border-neutral-200 px-2.5 py-0.5">
                          {w.device || "(unknown device)"} <span className="text-neutral-500">(main)</span>
                          {w.url && <a href={w.url} target="_blank" rel="noreferrer" className="text-xs text-primary hover:underline">{w.url.replace("http://", "")}</a>}
                        </span>
                        {(p.replicas ?? []).map((id) => {
                          const d = devices.find((x) => x.id === id);
                          const name = d?.name ?? "(removed device)";
                          const st = w.replicas?.find((r) => r.device_id === id);
                          return (
                            <span key={id} className="inline-flex flex-wrap items-center gap-1.5 rounded-full border border-neutral-200 px-2.5 py-0.5">
                              {name}
                              {st?.url && st.running && <a href={st.url} target="_blank" rel="noreferrer" className="text-xs text-primary hover:underline">{st.url.replace("http://", "")}</a>}
                              {st ? <ReplicaBadge st={st} onRetry={() => run(() => api.retryReplica(w.project_id, id))} /> : d?.maintenance ? <Badge tone="warn">in maintenance</Badge> : null}
                              <button type="button" aria-label={`Remove the copy on ${name}`} title={`Remove the copy on ${name}`} onClick={() => toggleReplica(w, id, name, false)} className="text-neutral-400 hover:text-red-600">×</button>
                            </span>
                          );
                        })}
                        {(() => {
                          const free = devices.filter((d) => d.id !== w.device_id && !(p.replicas ?? []).includes(d.id));
                          if (free.length === 0) return null;
                          return (
                            <details className="relative">
                              <summary className="inline-flex cursor-pointer list-none items-center rounded-full border border-dashed border-neutral-300 px-2.5 py-0.5 text-neutral-600 hover:bg-neutral-50" title="Add a copy on another device">+</summary>
                              <div className="absolute left-0 z-10 mt-1 min-w-40 rounded-md border border-neutral-200 bg-white p-1 shadow-md">
                                {free.map((d) => (
                                  <button key={d.id} type="button" className="block w-full rounded px-2 py-1 text-left hover:bg-neutral-100" onClick={(e) => { (e.currentTarget.closest("details") as HTMLDetailsElement).open = false; toggleReplica(w, d.id, d.name, true); }}>{d.name}</button>
                                ))}
                              </div>
                            </details>
                          );
                        })()}
                        <span className="cursor-help text-neutral-400" title={`Copies: each device added with + runs an extra, independent copy of ${w.name}, deployed together with the main one. Nothing is synced or moved, so the app must cope with several copies.`}>ⓘ</span>
                        {p.failover && (w.targets ?? []).filter((t) => !t.fits).map((t) => (
                          <p key={t.device_id} className="basis-full text-xs text-amber-700">Capacity warning: {t.device} {(t.problems ?? []).join(", ")}</p>
                        ))}
                        {w.last_failover && (
                          <p className="basis-full text-xs text-neutral-500">
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
                        <div className="flex flex-wrap items-center gap-2">
                          <label className="flex items-center gap-2"><input type="checkbox" checked={p.fail_back} onChange={(e) => setPolicy(w.project_id, p, { fail_back: e.target.checked })} /> Move back to {w.device || "the original device"} automatically once it has stayed online for</label>
                          <input type="number" min={60} max={86400} disabled={!p.fail_back} defaultValue={p.fail_back_seconds || 300} key={`${w.project_id}-${p.fail_back_seconds}`} onBlur={(e) => { const v = Number(e.target.value); if (v !== (p.fail_back_seconds || 300)) setPolicy(w.project_id, p, { fail_back_seconds: v }); }} className={inputClass + " w-24"} />
                          seconds
                        </div>
                        <label className="flex items-center gap-2"><input type="checkbox" checked={p.start_empty} onChange={(e) => setPolicy(w.project_id, p, { start_empty: e.target.checked })} /> Start with no data if the server has no backup of it (only for workers that keep nothing worth saving)</label>
                      </div>
                    )}
                  </Card>
                );
              })}
            </div>
          )}
          <Card className="mt-6">
            <CardHeader title="Failover history" subtitle="Each automatic failover and fail back: what failed, where it went and how long the move took." />
            <FailoverHistory />
          </Card>
        </>
      )}
    </>
  );
}
