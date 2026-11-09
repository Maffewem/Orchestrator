import { useState } from "react";
import { Link } from "react-router";
import { AlertTriangle, Plus, X } from "lucide-react";
import { api, type ConnAddress } from "../api";
import { ago, usePoll } from "../lib";
import { Badge, Button, ErrorNote, Loading, StatusDot, inputClass } from "./ui";

const VIA: Record<string, string> = {
  configured: "Installed address",
  announced: "Fallback from this list",
  discovered: "Found by searching the network",
  "": "Installed address",
};

function Kind({ a }: { a: ConnAddress }) {
  if (a.kind === "loopback") return <Badge>Same machine</Badge>;
  if (a.kind === "name") return <Badge tone="ok">Hostname</Badge>;
  if (a.assigned === "dhcp") return <Badge tone="warn">DHCP, may change</Badge>;
  if (a.assigned === "static") return <Badge tone="ok">Fixed IP</Badge>;
  return <Badge>IP address</Badge>;
}

/** Settings > Server connectivity: which addresses devices use, warnings, and recent changes. */
export function AgentAddresses({ urls, onSaved }: { urls: string[]; onSaved: () => void }) {
  const conn = usePoll(api.connectivity, 5000);
  const [draft, setDraft] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const here = window.location.origin;

  const save = async (next: string[]) => {
    setErr(null);
    try {
      await api.setAgentURLs(next);
      setDraft("");
      onSaved();
      conn.reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  if (!conn.data) return conn.error ? <div className="p-5"><ErrorNote>{conn.error}</ErrorNote></div> : <Loading />;
  const v = conn.data;
  const suggestions = [...new Set([...v.suggestions, here])].filter((u) => !urls.includes(u) && !u.includes("//localhost") && !u.includes("//127."));

  return (
    <div className="divide-y divide-neutral-100 text-sm">
      {v.warnings.length > 0 && (
        <ul className="space-y-2 bg-amber-50 px-5 py-4">
          {v.warnings.map((w) => (
            <li key={w} className="flex gap-2 text-warn">
              <AlertTriangle className="mt-0.5 size-4 shrink-0" />
              <span>{w}</span>
            </li>
          ))}
        </ul>
      )}

      <section className="p-5">
        <h4 className="font-medium">Addresses for this server</h4>
        <p className="mt-0.5 text-xs text-neutral-500">
          Listed addresses are sent to every device as fallbacks. Before a planned move, add the new address here first.
        </p>
        <ul className="mt-3 divide-y divide-neutral-100 rounded-lg border border-neutral-200">
          {v.addresses.length === 0 && <li className="px-3 py-3 text-neutral-500">No device has connected yet.</li>}
          {v.addresses.map((a) => (
            <li key={a.url} className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2.5">
              <code className="font-mono text-xs">{a.url}</code>
              <Kind a={a} />
              {a.url === here && <Badge tone="info">You are here</Badge>}
              <span className="text-xs text-neutral-500">
                {a.devices.length ? `Used by ${a.devices.join(", ")}` : "Not in use"}
              </span>
              <span className="ml-auto flex items-center gap-2">
                {a.listed ? (
                  <>
                    <Badge tone="ok">Listed</Badge>
                    <Button size="sm" variant="ghost" aria-label={`Remove ${a.url}`} icon={<X className="size-3.5" />} onClick={() => save(urls.filter((u) => u !== a.url))} />
                  </>
                ) : a.kind !== "loopback" ? (
                  <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => save([...urls, a.url])}>
                    List
                  </Button>
                ) : null}
              </span>
            </li>
          ))}
        </ul>
        <form
          className="mt-3 flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (draft.trim()) save([...urls, draft.trim()]);
          }}
        >
          <input className={inputClass + " font-mono text-xs"} placeholder="http://orchestrator.local:8080" value={draft} onChange={(e) => setDraft(e.target.value)} />
          <Button variant="primary" type="submit" disabled={!draft.trim()}>
            Add
          </Button>
        </form>
        {suggestions.length > 0 && (
          <p className="mt-2 flex flex-wrap items-center gap-2 text-xs text-neutral-500">
            Suggested:
            {suggestions.map((u) => (
              <button key={u} type="button" className="rounded-md border border-neutral-200 px-2 py-1 font-mono hover:bg-neutral-100" onClick={() => save([...urls, u])}>
                + {u}
              </button>
            ))}
          </p>
        )}
        {err && <p className="mt-2 text-bad">{err}</p>}
      </section>

      <section className="p-5">
        <h4 className="font-medium">How each device connects</h4>
        <ul className="mt-3 divide-y divide-neutral-100 rounded-lg border border-neutral-200">
          {v.devices.map((d) => (
            <li key={d.id} className="grid gap-x-4 gap-y-1 px-3 py-2.5 sm:grid-cols-[10rem_1fr_auto] sm:items-center">
              <Link to={`/devices/${d.id}`} className="flex items-center gap-2 font-medium hover:underline">
                <StatusDot status={d.status} /> {d.name}
              </Link>
              <div className="min-w-0">
                <code className="font-mono text-xs break-all">{d.server || "unknown"}</code>
                <p className="text-xs text-neutral-500">
                  {VIA[d.via] ?? d.via}
                  {d.network.length > 0 && ` · device is at ${d.network.map((n) => n.address + (n.assigned === "dhcp" ? " (DHCP)" : n.assigned === "static" ? " (fixed)" : "")).join(", ")}`}
                </p>
              </div>
              {d.recovers ? <Badge tone="ok">Self-recovery ready</Badge> : <Badge tone="warn">Agent needs update</Badge>}
            </li>
          ))}
        </ul>
        <p className="mt-2 text-xs text-neutral-500">
          When no known address answers, a device with self-recovery searches its own local network for this server and checks its
          identity before reconnecting. Devices on another network need an address that does not change.
        </p>
      </section>

      <section className="p-5">
        <h4 className="font-medium">Recent network changes</h4>
        {v.changes.length === 0 ? (
          <p className="mt-2 text-neutral-500">Nothing has changed.</p>
        ) : (
          <ul className="mt-2 space-y-1.5">
            {v.changes.map((c) => (
              <li key={c.id} className="flex gap-2">
                <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warn" />
                <span>
                  {c.message} <span className="text-xs text-neutral-500">{ago(c.created_at)}</span>
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
