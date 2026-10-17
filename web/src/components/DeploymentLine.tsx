import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
import { Check, Copy, GitBranch, MoreHorizontal } from "lucide-react";
import { api, type Deployment } from "../api";
import { StatusIcon } from "./ui";
import { ago, cx, duration, span } from "../lib";
import { useCan } from "./auth";
import { deploymentTitle } from "../pages/Projects";

export function Sparkline({ values, color = "#0051c3" }: { values: number[]; color?: string }) {
  if (values.length < 2) return <div className="h-8 w-full rounded bg-neutral-100" />;
  const max = Math.max(1, ...values), w = 200, h = 32;
  const pts = values.map((v, i) => `${(i / (values.length - 1)) * w},${h - 2 - (v / max) * (h - 4)}`);
  return (
    <svg viewBox={`0 0 ${w} ${h}`} preserveAspectRatio="none" className="h-8 w-full">
      <defs>
        <linearGradient id="spark" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor={color} stopOpacity="0.3" />
          <stop offset="1" stopColor={color} stopOpacity="0" />
        </linearGradient>
      </defs>
      <polygon points={`0,${h} ${pts.join(" ")} ${w},${h}`} fill="url(#spark)" />
      <polyline points={pts.join(" ")} fill="none" stroke={color} strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

export function versionId(d: Deployment) {
  return d.commit ? d.commit.sha.slice(0, 8) : d.id.replace(/^[a-z]+_/, "").slice(0, 8);
}

export function DeploymentLine({ d, live, onChanged, compact }: { d: Deployment; live: boolean; onChanged: () => void; compact?: boolean }) {
  const nav = useNavigate();
  const [copied, setCopied] = useState(false);
  const menu = useRef<HTMLDetailsElement>(null);
  const canRollback = useCan("operator") && d.status === "succeeded" && !live && !d.preview_id;
  const took = span(d.started_at ?? d.created_at, d.finished_at);
  const hash = d.commit?.sha;
  useEffect(() => {
    const close = (e: MouseEvent) => {
      if (menu.current && !menu.current.contains(e.target as Node)) menu.current.open = false;
    };
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, []);
  const copy = async () => {
    if (!hash) return;
    try {
      await navigator.clipboard.writeText(hash);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard blocked */
    }
  };
  const label = { queued: "Queued", running: "Building", succeeded: "Ready", failed: "Failed" }[d.status];
  return (
    <div
      className={cx(
        "grid grid-cols-[7rem_1fr_auto] items-center gap-x-4 gap-y-2 px-5 py-3 text-sm",
        compact ? "lg:grid-cols-[7rem_minmax(0,1fr)_8rem_6rem_4rem_2rem]" : "lg:grid-cols-[7rem_minmax(0,1fr)_9rem_6rem_7rem_9rem_5rem_2rem]",
      )}
    >
      <div className="flex items-center gap-2 font-mono text-neutral-500">
        {hash ? (
          <>
            <span>{hash.slice(0, 8)}</span>
            <button onClick={copy} aria-label="Copy commit hash" className="rounded p-0.5 hover:bg-neutral-100">
              {copied ? <Check className="size-3.5 text-ok" /> : <Copy className="size-3.5" />}
            </button>
          </>
        ) : (
          "—"
        )}
      </div>
      <Link to={`/deployments/${d.id}`} className="min-w-0 truncate hover:underline" title={deploymentTitle(d)}>
        {deploymentTitle(d)}
      </Link>
      <div className="flex items-center gap-2 whitespace-nowrap">
        {live ? <span className="h-4 w-1 rounded bg-primary" title="Active deployment" /> : <StatusIcon status={d.status} size="sm" />}
        <span className={cx(d.status === "failed" && "text-bad")}>{label}</span>
        {d.status !== "queued" && <span className="text-neutral-500">{duration(took)}</span>}
      </div>
      {!compact && (
        <Link to={`/deployments/${d.id}`} className="hidden w-fit rounded-full border border-neutral-300 px-3 py-1 text-xs hover:bg-neutral-50 lg:block">
          Build ↗
        </Link>
      )}
      <span className="hidden w-fit items-center gap-1 rounded-full border border-neutral-300 px-3 py-1 text-xs lg:inline-flex">
        <GitBranch className="size-3" />
        {d.commit?.branch ?? "image"}
      </span>
      {!compact && <span className="hidden truncate text-neutral-600 lg:block">{d.commit?.author ? `by ${d.commit.author}` : d.trigger === "manual" ? "by you" : ""}</span>}
      <span className="hidden whitespace-nowrap text-neutral-500 lg:block">{ago(d.created_at)}</span>
      <details ref={menu} className="relative">
        <summary className="flex cursor-pointer list-none justify-end rounded p-1 hover:bg-neutral-100" aria-label="Actions">
          <MoreHorizontal className="size-4" />
        </summary>
        <div className="absolute right-0 z-10 mt-1 w-48 rounded-lg border border-neutral-200 bg-white py-1 text-sm shadow-lg">
          <Link to={`/deployments/${d.id}`} className="block px-3 py-1.5 hover:bg-neutral-50">View build log</Link>
          {hash && <button onClick={copy} className="block w-full px-3 py-1.5 text-left hover:bg-neutral-50">Copy commit hash</button>}
          {d.commit?.url && <a href={d.commit.url} target="_blank" rel="noreferrer" className="block px-3 py-1.5 hover:bg-neutral-50">Open commit on GitHub</a>}
          <button
            className="block w-full px-3 py-1.5 text-left hover:bg-neutral-50"
            onClick={async () => {
              const r = await api.retryDeployment(d.id);
              onChanged();
              nav(`/deployments/${r.id}`);
            }}
          >
            Redeploy this version
          </button>
          {canRollback && (
            <button
              className="block w-full px-3 py-1.5 text-left hover:bg-neutral-50"
              onClick={async () => {
                if (!window.confirm(`Roll back to ${versionId(d)}? It becomes the live version.`)) return;
                try {
                  const r = await api.rollbackDeployment(d.id);
                  onChanged();
                  nav(`/deployments/${r.id}`);
                } catch (e) {
                  window.alert(e instanceof Error ? e.message : String(e));
                }
              }}
            >
              Roll back to this version
            </button>
          )}
        </div>
      </details>
    </div>
  );
}

