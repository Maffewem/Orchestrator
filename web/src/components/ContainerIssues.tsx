import { useState } from "react";
import { Link } from "react-router";
import { Check, FileText, Settings2, Square, Undo2 } from "lucide-react";
import { api, type ContainerIssue, type Device } from "../api";
import { bytes } from "../lib";
import { useCan } from "./auth";
import { Button, ErrorNote } from "./ui";
import { waitFor } from "./ContainerTable";
import { LiveLogs } from "./Streams";

const prefixes = ["Containers killed", "Containers restarting repeatedly", "Containers dead"];

/** True for the one-line device issue that ContainerIssues explains in full. */
export function isContainerIssueText(s: string): boolean {
  return prefixes.some((p) => s.startsWith(p));
}

/** Each failing container on a device: what happened, the memory figures, and one-click next steps. */
export default function ContainerIssues({ d, onChanged }: { d: Device; onChanged: () => void }) {
  const canOperate = useCan("operator");
  const isAdmin = useCan("admin");
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [logs, setLogs] = useState<ContainerIssue | null>(null);
  const issues = d.container_issues ?? [];
  if (issues.length === 0) return null;

  const attempt = async (key: string, fn: () => Promise<unknown>) => {
    setBusy(key);
    setError(null);
    try {
      await fn();
      onChanged();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };
  const stopRestarting = (i: ContainerIssue) =>
    attempt(i.id + "stop", async () => {
      const r = await waitFor((await api.command(d.id, "container.update", i.id, { restart: "no", stop: "1" })).id);
      if (r.status === "failed") throw new Error(`${i.name}: ${r.error}`);
    });

  return (
    <div className="divide-y divide-amber-200 border-t border-amber-200">
      {issues.map((i) => {
        const c = d.containers.find((x) => x.id === i.id);
        const canStop = canOperate && (i.kind === "loop" || c?.state === "running" || c?.state === "restarting");
        return (
          <div key={i.id} className={`space-y-3 px-5 py-4 text-sm ${i.acked ? "opacity-70" : ""}`}>
            <div className="flex flex-wrap items-baseline justify-between gap-x-3">
              <p className="font-medium">
                {i.name}
                <span className="ml-2 font-normal text-neutral-600">
                  {i.kind === "oom" ? "killed: out of memory" : i.kind === "killed" ? "stopped with exit 137 (killed)" : i.kind === "loop" ? "restarting repeatedly" : "dead"}
                </span>
              </p>
              {i.acked && <span className="text-xs text-neutral-500">Acknowledged{i.acked_by ? ` by ${i.acked_by}` : ""}. It comes back if it fails again.</span>}
            </div>
            <p className="text-neutral-700">{i.cause}</p>
            {(i.mem_limit !== undefined || i.host_mem_total) && (
              <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs sm:grid-cols-4">
                <Fact label="Container limit" value={i.mem_limit ? bytes(i.mem_limit) : "None"} />
                <Fact label="Last memory use" value={i.mem_used ? bytes(i.mem_used) : "Unknown"} />
                <Fact label="Device RAM" value={i.host_mem_total ? `${bytes(i.host_mem_used ?? 0)} of ${bytes(i.host_mem_total)}` : "Unknown"} />
                <Fact label="Device swap" value={i.swap_total ? `${bytes(i.swap_used ?? 0)} of ${bytes(i.swap_total)}` : "None"} />
              </dl>
            )}
            {!i.acked && i.tips && i.tips.length > 0 && (
              <ul className="list-disc space-y-0.5 pl-5 text-xs text-neutral-600">
                {i.tips.map((t) => (
                  <li key={t}>{t}</li>
                ))}
              </ul>
            )}
            {!i.acked && i.suggest_why && <p className="text-xs text-neutral-600">{i.suggest_why}</p>}
            <div className="flex flex-wrap gap-2">
              {isAdmin && i.project_id && (i.kind === "oom" || (i.kind === "killed" && !!i.suggest_mb)) && (
                <Link
                  to={`/projects/${i.project_id}/settings`}
                  className="inline-flex min-h-8 items-center gap-1.5 rounded-lg border border-neutral-200 bg-white px-3 text-sm font-medium hover:bg-neutral-50"
                  title="Opens the project's settings, where the memory limit is set"
                >
                  <Settings2 className="size-3.5" />
                  {i.suggest_mb ? `Set container memory limit to ${i.suggest_mb} MiB` : "Open project settings"}
                </Link>
              )}
              {canStop && (
                <Button size="sm" loading={busy === i.id + "stop"} icon={<Square className="size-3.5" />} onClick={() => stopRestarting(i)} title="Sets the restart policy to Never and stops the container. Start it again or redeploy to bring it back.">
                  Stop restarting
                </Button>
              )}
              {canOperate && (
                <Button size="sm" icon={<FileText className="size-3.5" />} onClick={() => setLogs(i)}>
                  View logs
                </Button>
              )}
              {canOperate &&
                (i.acked ? (
                  <Button size="sm" variant="ghost" loading={busy === i.id + "ack"} icon={<Undo2 className="size-3.5" />} onClick={() => attempt(i.id + "ack", () => api.ackContainerIssue(d.id, i.name, true))}>
                    Show again
                  </Button>
                ) : (
                  <Button size="sm" variant="ghost" loading={busy === i.id + "ack"} icon={<Check className="size-3.5" />} onClick={() => attempt(i.id + "ack", () => api.ackContainerIssue(d.id, i.name))} title="Hides this warning until the container runs again and fails again">
                    Acknowledge
                  </Button>
                ))}
            </div>
          </div>
        );
      })}
      {error && (
        <div className="px-5 py-3">
          <ErrorNote>{error}</ErrorNote>
        </div>
      )}
      {logs && <LiveLogs deviceId={d.id} container={logs.id} name={logs.name} onClose={() => setLogs(null)} />}
    </div>
  );
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-neutral-500">{label}</dt>
      <dd className="font-medium text-neutral-800">{value}</dd>
    </div>
  );
}
