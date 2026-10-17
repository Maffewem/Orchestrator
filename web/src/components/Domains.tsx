import { useState } from "react";
import { Link } from "react-router";
import { Globe, Plus, Search, Trash2 } from "lucide-react";
import { api, type DomainRow, type DomainsView } from "../api";
import { Badge, Button, Card, CardHeader, CopyButton, Empty, ErrorNote, Field, Loading, Modal, Table, Td, inputClass } from "./ui";
import { cx, usePoll } from "../lib";

function Toggle({ on, onChange, label, disabled }: { on: boolean; onChange: (v: boolean) => void; label: string; disabled?: boolean }) {
  return (
    <button
      role="switch"
      aria-checked={on}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!on)}
      className={cx("relative h-6 w-11 shrink-0 rounded-full transition-colors disabled:opacity-50", on ? "bg-primary" : "bg-neutral-300")}
    >
      <span className={cx("absolute top-0.5 size-5 rounded-full bg-white shadow transition-all", on ? "left-[22px]" : "left-0.5")} />
    </button>
  );
}

function StatusBadge({ s }: { s: DomainRow["status"] }) {
  if (s === "active") return <Badge tone="ok">Active</Badge>;
  if (s === "mismatch") return <Badge tone="warn">Points elsewhere</Badge>;
  return <Badge>Waiting for DNS</Badge>;
}

function Records({ rows }: { rows: { type: string; name: string; value: string }[] }) {
  if (rows.length === 0)
    return <p className="text-sm text-neutral-500">This device has no IPv4 address on record yet, so there is nothing to point DNS at.</p>;
  return (
    <div className="space-y-1 font-mono text-xs">
      {rows.map((r) => (
        <div key={r.name + r.value} className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md bg-neutral-50 px-3 py-2">
          <span className="font-semibold">{r.type}</span>
          <span>{r.name}</span>
          <span className="text-neutral-400">→</span>
          <span>{r.value}</span>
          <CopyButton text={r.value} label="Copy" />
        </div>
      ))}
    </div>
  );
}

function UrlLine({ label, host, enabled, row, onToggle, disabled }: { label: string; host: string; enabled: boolean; row?: DomainRow["status"]; onToggle: (v: boolean) => void; disabled?: boolean }) {
  const dot = host.lastIndexOf(".", host.lastIndexOf(".") - 1);
  return (
    <div className="flex items-start justify-between gap-4 px-5 py-4">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-3">
          <span className="rounded-md bg-emerald-50 px-2 py-0.5 text-sm font-medium text-emerald-800">{label}</span>
          <span className="break-all text-base">
            {host.slice(0, dot)}
            <span className="text-neutral-400">{host.slice(dot)}</span>
          </span>
          {enabled && row && <StatusBadge s={row} />}
        </div>
        <p className="mt-2 flex items-center gap-2 text-sm text-neutral-500">
          <Globe className="size-3.5" />
          {enabled ? "Anyone who can reach this device can visit." : "Switched off. This URL does not serve the worker."}
        </p>
      </div>
      <Toggle on={enabled} onChange={onToggle} label={`${label} URL`} disabled={disabled} />
    </div>
  );
}

export default function DomainsPanel({ projectId }: { projectId: string }) {
  const q = usePoll(() => api.projectDomains(projectId), 15000, [projectId]);
  const [view, setView] = useState<DomainsView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [adding, setAdding] = useState<"domain" | "route" | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  const v = view ?? q.data;
  if (!v) return q.error ? <ErrorNote>{q.error}</ErrorNote> : <Loading />;

  const act = async (fn: () => Promise<DomainsView | void>) => {
    setError(null);
    try {
      const r = await fn();
      if (r) setView(r);
      else setView(await api.projectDomains(projectId));
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  const rows = v.domains.filter((d) => (d.hostname + (d.path ?? "")).includes(filter.trim().toLowerCase()));
  const prod = v.production;
  const prev = v.preview;

  return (
    <div className="space-y-8">
      {error && <ErrorNote>{error}</ErrorNote>}
      {v.proxy === "agent_too_old" && (
        <div className="rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900">
          The agent on this device is too old to serve domains. Update it from the <Link to="/updates" className="underline">Updates page</Link>, or re-run the install command on the device.
        </div>
      )}
      <Card>
        <CardHeader title="Worker URL" />
        {prod && prev ? (
          <div className="divide-y divide-neutral-100">
            <UrlLine label="Production" host={prod.hostname} enabled={prod.enabled} row={prod.status} onToggle={(on) => act(() => api.setProjectUrls(projectId, { production: on, preview: prev.enabled }))} />
            {prod.enabled && prod.status !== "active" && prod.records && (
              <div className="space-y-2 bg-neutral-50/60 px-5 py-4">
                <p className="text-sm text-neutral-600">{prod.detail}</p>
                <Records rows={prod.records} />
              </div>
            )}
            <UrlLine label="Preview" host={prev.hostname} enabled={prev.enabled} onToggle={(on) => act(() => api.setProjectUrls(projectId, { production: prod.enabled, preview: on }))} />
            {prev.enabled && <p className="px-5 pb-4 text-xs text-neutral-500">Needs a wildcard DNS record, *.{prod.hostname.split(".").slice(1).join(".")}, pointing at the same address. Preview URLs are served over plain HTTP.</p>}
          </div>
        ) : (
          <div className="p-5 text-sm text-neutral-600">
            Set a base domain in <Link to="/settings" className="text-primary hover:underline">Settings</Link> and every worker gets <code>name.&lt;your-domain&gt;</code> automatically.
          </div>
        )}
      </Card>

      <section>
        <h2 className="mb-3 text-lg font-medium">Custom Domains and Routes</h2>
        <div className="mb-3 flex flex-wrap gap-2">
          <div className="relative min-w-48 flex-1">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-neutral-400" />
            <input className={cx(inputClass, "pl-9")} placeholder="Search domains" value={filter} onChange={(e) => setFilter(e.target.value)} />
          </div>
          <Button icon={<Plus className="size-4" />} onClick={() => setAdding("route")}>Add Route</Button>
          <Button variant="primary" icon={<Plus className="size-4" />} onClick={() => setAdding("domain")}>Add Domain</Button>
        </div>
        <Card>
          {rows.length === 0 ? (
            <Empty icon={<Globe className="size-8" />} title="No custom domains">
              Custom domains and routes you add to this worker will appear here.
            </Empty>
          ) : (
            <Table head={["Name", "Type", "Zone", "Status", ""]}>
              {rows.flatMap((d) => [
                <tr key={d.id} className="cursor-pointer hover:bg-neutral-50" onClick={() => setOpen(open === d.id ? null : (d.id ?? null))}>
                  <Td className="font-medium">{d.hostname}{d.path}</Td>
                  <Td>{d.type === "route" ? "Route" : "Domain"}</Td>
                  <Td className="text-neutral-500">{d.zone}</Td>
                  <Td><StatusBadge s={d.status} /></Td>
                  <Td className="text-right">
                    <button
                      aria-label={`Remove ${d.hostname}`}
                      className="rounded-md p-1.5 text-neutral-500 hover:bg-neutral-100 hover:text-bad"
                      onClick={(e) => { e.stopPropagation(); if (confirm(`Remove ${d.hostname}${d.path ?? ""}?`)) act(async () => { await api.deleteDomain(projectId, d.id!); }); }}
                    >
                      <Trash2 className="size-4" />
                    </button>
                  </Td>
                </tr>,
                open === d.id && (
                  <tr key={d.id + "-dns"}>
                    <td colSpan={5} className="space-y-2 bg-neutral-50/60 px-5 py-4">
                      <p className="text-sm text-neutral-600">{d.detail}</p>
                      {d.status !== "active" && <Records rows={d.records} />}
                    </td>
                  </tr>
                ),
              ])}
            </Table>
          )}
        </Card>
      </section>

      {adding && (
        <AddDialog
          kind={adding}
          onClose={() => setAdding(null)}
          onAdd={async (hostname, path) => {
            const r = await api.addDomain(projectId, { hostname, type: adding, path });
            setView(r);
            setOpen(r.domains.find((d) => d.hostname === hostname.toLowerCase().trim())?.id ?? null);
            setAdding(null);
          }}
        />
      )}
    </div>
  );
}

function AddDialog({ kind, onClose, onAdd }: { kind: "domain" | "route"; onClose: () => void; onAdd: (host: string, path: string) => Promise<void> }) {
  const [host, setHost] = useState("");
  const [path, setPath] = useState("/*");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const submit = async () => {
    setBusy(true);
    setError(null);
    try {
      await onAdd(host, kind === "route" ? path : "");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };
  return (
    <Modal title={kind === "route" ? "Add route" : "Add custom domain"} onClose={onClose}>
      <div className="space-y-4">
        <Field label="Hostname" hint="For example app.example.com. You will be shown the DNS record to create.">
          <input className={inputClass} autoFocus value={host} onChange={(e) => setHost(e.target.value)} placeholder="app.example.com" />
        </Field>
        {kind === "route" && (
          <Field label="Path" hint="Only requests under this path go to this worker, e.g. /api/*">
            <input className={inputClass} value={path} onChange={(e) => setPath(e.target.value)} />
          </Field>
        )}
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!host.trim()} onClick={submit}>
            {kind === "route" ? "Add route" : "Add domain"}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
