import { Link } from "react-router";
import { ArrowRight } from "lucide-react";
import { api } from "../api";
import { Badge } from "./ui";
import { ago, duration, usePoll } from "../lib";

// A timeline of automatic moves: what failed, where the worker went and how long it took.
export default function FailoverHistory({ project, limit = 20 }: { project?: string; limit?: number }) {
  const { data } = usePoll(() => api.clusterHistory(project), 10000);
  if (!data) return null;
  if (data.length === 0) return <p className="p-4 text-sm text-neutral-500">No automatic failovers or fail backs yet.</p>;
  return (
    <ol className="divide-y divide-neutral-100 text-sm">
      {data.slice(0, limit).map((e) => (
        <li key={e.id} className="space-y-1 px-4 py-3">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone={e.status === "done" ? "ok" : e.status === "failed" ? "bad" : "info"}>
              {e.status === "running" ? "moving" : e.status === "done" ? (e.kind === "failback" ? "moved back" : "failed over") : "failed"}
            </Badge>
            {!project && <Link to={`/projects/${e.project_id}`} className="font-medium hover:underline">{e.worker}</Link>}
            <span className="inline-flex items-center gap-1.5">
              {e.from_name}
              {e.to_name && (<><ArrowRight className="size-3.5 text-neutral-400" />{e.to_name}</>)}
            </span>
            <span className="text-xs text-neutral-500" title={new Date(e.at).toLocaleString()}>{ago(e.at)}</span>
            {e.status !== "running" && e.duration_seconds ? <span className="text-xs text-neutral-500">took {duration(e.duration_seconds * 1000)}</span> : null}
          </div>
          <p className="text-xs text-neutral-500">
            {e.kind === "failback" ? "Fail back: " : "Failover: "}
            {e.reason}
            {e.kind === "failover" && e.to_name ? (e.with_data ? "; restored the newest backup" : "; started without data") : ""}.
          </p>
          {e.error && <p className="text-xs text-red-700">{e.error}</p>}
          {(e.warnings ?? []).map((w) => (
            <p key={w} className="text-xs text-amber-700">Capacity warning: {w}</p>
          ))}
        </li>
      ))}
    </ol>
  );
}
