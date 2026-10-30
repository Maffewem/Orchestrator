import { useState } from "react";
import { Link } from "react-router";
import { Plus, Trash2 } from "lucide-react";
import { api, type ProjectTunnelView } from "../api";
import { Badge, Button, Card, CardHeader, ErrorNote, Loading, inputClass } from "./ui";
import { useCan } from "./auth";
import { notify } from "./dialogs";
import { usePoll } from "../lib";

type Row = { hostname: string; port: string };

/** Publish this project on a public hostname through its device's Cloudflare Tunnel. */
export default function TunnelPanel({ projectId }: { projectId: string }) {
  const isAdmin = useCan("admin");
  const t = usePoll(() => api.projectTunnel(projectId), 5000, [projectId]);
  const [rows, setRows] = useState<Row[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warnings, setWarnings] = useState<string[]>([]);
  if (t.error && !t.data) return <ErrorNote>{t.error}</ErrorNote>;
  const v: ProjectTunnelView | null = t.data && { ...t.data, ports: t.data.ports ?? [], routes: t.data.routes ?? [] };
  if (!v) return <Loading />;
  const saved: Row[] = v.routes.map((r) => ({ hostname: r.hostname, port: r.service.split(":").pop() ?? "" }));
  const shown = rows ?? saved;

  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      const res = await api.setProjectTunnel(
        projectId,
        shown.filter((r) => r.hostname.trim()).map((r) => ({ hostname: r.hostname.trim(), port: Number(r.port) })),
      );
      const warns = res.warnings ?? [];
      setWarnings(warns);
      if (warns.length) notify(`Saved, but not fully applied: ${warns.join(" ")}`);
      else notify(`Tunnel routes applied: ${res.routes.length ? res.routes.map((r) => r.hostname).join(", ") : "none left for this project"}.`, "info");
      setRows(null);
      t.reload();
    } catch (e) {
      const m = e instanceof Error ? e.message : String(e);
      setError(m);
      notify(`Couldn't apply the tunnel routes: ${m}`);
    } finally {
      setBusy(false);
    }
  };
  const edit = (i: number, patch: Partial<Row>) => setRows(shown.map((r, j) => (j === i ? { ...r, ...patch } : r)));

  return (
    <Card>
      <CardHeader title="Cloudflare Tunnel" subtitle="Serve this project on a public hostname through its device's tunnel, without opening ports." />
      <div className="space-y-4 p-5 text-sm">
        {!v.supported ? (
          <p className="text-neutral-500">This device's agent is too old to run a tunnel. Update it from the Updates page.</p>
        ) : !v.enabled ? (
          <p className="text-neutral-500">
            The device has no tunnel yet. Set one up once on the{" "}
            <Link className="text-primary underline" to={`/devices/${v.device_id}`}>
              device page
            </Link>{" "}
            and every project on it can use it.
          </p>
        ) : (
          <>
            <p className="flex items-center gap-2">
              <Badge>{v.state ?? "starting"}</Badge>
              <Link className="text-xs text-neutral-500 underline" to={`/devices/${v.device_id}`}>
                Tunnel settings on the device
              </Link>
            </p>
            {v.ports.length === 0 && <p className="text-neutral-500">This project doesn't publish a port yet. Deploy it first, or add a port mapping, then pick it here.</p>}
            {shown.map((r, i) => (
              <div key={i} className="flex flex-wrap gap-2">
                <input className={inputClass + " min-w-48 flex-1"} placeholder="app.example.com" value={r.hostname} disabled={!isAdmin} onChange={(e) => edit(i, { hostname: e.target.value })} />
                <select className={inputClass + " w-40"} value={r.port} disabled={!isAdmin} onChange={(e) => edit(i, { port: e.target.value })}>
                  <option value="">Port…</option>
                  {[...new Set([...v.ports.map(String), ...(r.port ? [r.port] : [])])].map((p) => (
                    <option key={p} value={p}>
                      {p}
                    </option>
                  ))}
                </select>
                {isAdmin && (
                  <Button icon={<Trash2 className="size-4" />} onClick={() => setRows(shown.filter((_, j) => j !== i))}>
                    Remove
                  </Button>
                )}
              </div>
            ))}
            {isAdmin && (
              <div className="flex gap-2">
                <Button icon={<Plus className="size-4" />} onClick={() => setRows([...shown, { hostname: "", port: v.ports[0] ? String(v.ports[0]) : "" }])}>
                  Add hostname
                </Button>
                {rows && (
                  <Button variant="primary" loading={busy} onClick={save}>
                    Save and apply
                  </Button>
                )}
              </div>
            )}
            <p className="text-xs text-neutral-500">
              {v.can_route
                ? "Applied through the Cloudflare API connected in Settings, which also creates the DNS record."
                : "Connect Cloudflare in Settings to apply these from here; until then add the same hostnames in the Cloudflare dashboard."}
            </p>
          </>
        )}
        {error && <ErrorNote>{error}</ErrorNote>}
        {warnings.map((w) => (
          <p key={w} className="text-xs text-amber-700">
            {w}
          </p>
        ))}
      </div>
    </Card>
  );
}
