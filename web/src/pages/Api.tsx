import { useEffect, useState } from "react";
import { KeyRound, Plug, Send, Trash2, Webhook as WebhookIcon } from "lucide-react";
import { api, type ApiKey, type Delivery, type OpenApiDoc, type Role, type Webhook } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, CodeBlock, CopyButton, ErrorNote, Field, Loading, Modal, PageHeader, Table, Td, Toggle, inputClass } from "../components/ui";
import { confirmDialog } from "../components/dialogs";
import { ago, usePoll } from "../lib";

type Tab = "keys" | "webhooks" | "reference";

const roleHelp: Record<Role, string> = {
  viewer: "Read-only.",
  operator: "Deploy, restart, back up, manage domains and projects.",
  admin: "Everything except managing keys, webhooks and accounts.",
};

// Event groups offered when adding a webhook; each entry is a pattern the server matches.
const eventGroups: { label: string; hint: string; events: string[] }[] = [
  { label: "Deployments", hint: "queued, succeeded, failed, cancelled", events: ["deployment.*"] },
  { label: "Devices", hint: "offline, back online, removed", events: ["device.*"] },
  { label: "Backups", hint: "backup and server backup results", events: ["backup.*", "serverbackup.*"] },
  { label: "Projects & domains", hint: "created, deleted, migrated, DNS", events: ["project.*", "domain.*", "dns.*"] },
  { label: "Everything", hint: "every activity event", events: ["*"] },
];

export default function ApiPage() {
  useCrumbs([{ label: "API & webhooks" }]);
  const [tab, setTab] = useState<Tab>("keys");
  return (
    <div className="max-w-5xl">
      <PageHeader title="API & webhooks" description="Let scripts and other tools use Orchestrator, and have it tell them when something happens." />
      <div className="mb-4 flex gap-1" role="tablist">
        {([["keys", "API keys"], ["webhooks", "Webhooks"], ["reference", "Reference"]] as [Tab, string][]).map(([t, label]) => (
          <Button key={t} role="tab" aria-selected={tab === t} variant={tab === t ? "primary" : "secondary"} onClick={() => setTab(t)}>{label}</Button>
        ))}
      </div>
      {tab === "keys" && <Keys />}
      {tab === "webhooks" && <Webhooks />}
      {tab === "reference" && <Reference />}
    </div>
  );
}

function useAct(reload: () => void) {
  const [error, setError] = useState<string | null>(null);
  const act = async (fn: () => Promise<unknown>) => {
    setError(null);
    try {
      await fn();
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  return { error, act };
}

function Secret({ title, value, note, onClose }: { title: string; value: string; note: string; onClose: () => void }) {
  return (
    <Modal title={title} onClose={onClose}>
      <div className="space-y-4">
        <p className="text-sm text-neutral-600">{note}</p>
        <CodeBlock code={value} />
        <div className="flex justify-end"><Button variant="primary" onClick={onClose}>I've copied it</Button></div>
      </div>
    </Modal>
  );
}

function Keys() {
  const keys = usePoll(api.apiKeys, 0);
  const { error, act } = useAct(keys.reload);
  const [adding, setAdding] = useState(false);
  const [shown, setShown] = useState<string | null>(null);
  if (!keys.data) return keys.error ? <ErrorNote>{keys.error}</ErrorNote> : <Loading />;
  return (
    <>
      {error && <div className="mb-4"><ErrorNote>{error}</ErrorNote></div>}
      <Card>
        <CardHeader
          title="API keys"
          subtitle="Send a key as Authorization: Bearer <key>. A key acts with its role, but can never manage keys, webhooks or accounts."
          action={<Button variant="primary" icon={<KeyRound className="size-4" />} onClick={() => setAdding(true)}>Create key</Button>}
        />
        <Table head={["Name", "Key", "Role", "Last used", "Expires", ""]}>
          {keys.data.map((k: ApiKey) => (
            <tr key={k.id}>
              <Td><span className="font-medium">{k.name}</span><div className="text-xs text-neutral-500">by {k.created_by}, {ago(k.created_at)}</div></Td>
              <Td className="font-mono text-xs">{k.prefix}…</Td>
              <Td><Badge tone={k.role === "admin" ? "warn" : "neutral"}>{k.role}</Badge></Td>
              <Td className="text-neutral-500">{k.last_used ? ago(k.last_used) : "never"}</Td>
              <Td className="text-neutral-500">{k.expires_at ? new Date(k.expires_at).toLocaleDateString() : "never"}</Td>
              <Td className="text-right">
                <Button variant="danger" aria-label={`Revoke ${k.name}`} icon={<Trash2 className="size-4" />}
                  onClick={async () => (await confirmDialog({ title: `Revoke ${k.name}?`, message: "Anything using this key stops working immediately.", confirmLabel: "Revoke", danger: true })) && act(() => api.deleteApiKey(k.id))} />
              </Td>
            </tr>
          ))}
        </Table>
        {keys.data.length === 0 && <p className="p-5 text-sm text-neutral-500">No keys yet.</p>}
      </Card>
      {adding && (
        <AddKey onClose={() => setAdding(false)} onDone={(token) => { setAdding(false); setShown(token); keys.reload(); }} />
      )}
      {shown && <Secret title="Your new API key" value={shown} note="This is the only time the key is shown. Store it somewhere safe." onClose={() => setShown(null)} />}
    </>
  );
}

function AddKey({ onClose, onDone }: { onClose: () => void; onDone: (token: string) => void }) {
  const [name, setName] = useState("");
  const [role, setRole] = useState<Role>("viewer");
  const [days, setDays] = useState(90);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  return (
    <Modal title="Create API key" onClose={onClose}>
      <form
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          try {
            onDone((await api.createApiKey({ name, role, expiry_days: days })).token);
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            setBusy(false);
          }
        }}
      >
        <Field label="Name" hint="What will use it, such as GitHub Actions or Home Assistant.">
          <input className={inputClass} autoFocus value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Role" hint={roleHelp[role]}>
          <select className={inputClass} value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="viewer">Viewer</option>
            <option value="operator">Operator</option>
            <option value="admin">Admin</option>
          </select>
        </Field>
        <Field label="Expires" hint="Short-lived keys limit the damage of a leak.">
          <select className={inputClass} value={days} onChange={(e) => setDays(Number(e.target.value))}>
            <option value={30}>In 30 days</option>
            <option value={90}>In 90 days</option>
            <option value={365}>In a year</option>
            <option value={0}>Never</option>
          </select>
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!name.trim()}>Create key</Button>
        </div>
      </form>
    </Modal>
  );
}

function Webhooks() {
  const hooks = usePoll(api.webhooks, 0);
  const { error, act } = useAct(hooks.reload);
  const [adding, setAdding] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const [open, setOpen] = useState<Webhook | null>(null);
  if (!hooks.data) return hooks.error ? <ErrorNote>{hooks.error}</ErrorNote> : <Loading />;
  return (
    <>
      {error && <div className="mb-4"><ErrorNote>{error}</ErrorNote></div>}
      <Card>
        <CardHeader
          title="Webhooks"
          subtitle="Orchestrator POSTs a JSON event to the address, signed with the webhook's secret, and retries failures twice. Works with ntfy, Discord and Slack relays, Home Assistant, n8n and your own code."
          action={<Button variant="primary" icon={<WebhookIcon className="size-4" />} onClick={() => setAdding(true)}>Add webhook</Button>}
        />
        <Table head={["Name", "Address", "Events", "On", ""]}>
          {hooks.data.map((h) => (
            <tr key={h.id}>
              <Td><button className="font-medium hover:underline" onClick={() => setOpen(h)}>{h.name}</button><div className="font-mono text-xs text-neutral-500">secret {h.secret_hint}</div></Td>
              <Td className="max-w-64 truncate font-mono text-xs" >{h.url}</Td>
              <Td className="text-xs text-neutral-600">{h.events.join(", ")}</Td>
              <Td><Toggle label={`Enable ${h.name}`} checked={h.enabled} onChange={(v) => act(() => api.updateWebhook(h.id, { enabled: v }))} /></Td>
              <Td className="text-right">
                <div className="flex justify-end gap-1">
                  <Button onClick={() => setOpen(h)}>Deliveries</Button>
                  <Button variant="danger" aria-label={`Delete ${h.name}`} icon={<Trash2 className="size-4" />}
                    onClick={async () => (await confirmDialog({ title: `Delete ${h.name}?`, confirmLabel: "Delete", danger: true })) && act(() => api.deleteWebhook(h.id))} />
                </div>
              </Td>
            </tr>
          ))}
        </Table>
        {hooks.data.length === 0 && <p className="p-5 text-sm text-neutral-500">No webhooks yet.</p>}
      </Card>
      {adding && <AddWebhook onClose={() => setAdding(false)} onDone={(s) => { setAdding(false); setSecret(s); hooks.reload(); }} />}
      {secret && <Secret title="Webhook secret" value={secret} note="Use this to check the X-Orchestrator-Signature header. It is only shown now; you can rotate it later." onClose={() => setSecret(null)} />}
      {open && <Deliveries hook={open} onClose={() => setOpen(null)} onRotated={(s) => { setSecret(s); hooks.reload(); }} />}
    </>
  );
}

function AddWebhook({ onClose, onDone }: { onClose: () => void; onDone: (secret: string) => void }) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [picked, setPicked] = useState<Set<number>>(new Set([0, 1, 2]));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const events = [...new Set([...picked].flatMap((i) => eventGroups[i].events))];
  return (
    <Modal title="Add webhook" onClose={onClose}>
      <form
        className="space-y-4"
        onSubmit={async (e) => {
          e.preventDefault();
          setBusy(true);
          setError(null);
          try {
            onDone((await api.createWebhook({ name, url, events })).secret);
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
            setBusy(false);
          }
        }}
      >
        <Field label="Name"><input className={inputClass} autoFocus value={name} onChange={(e) => setName(e.target.value)} /></Field>
        <Field label="Address" hint="Where to POST events, https:// or http://."><input className={inputClass} placeholder="https://example.com/hooks/orchestrator" value={url} onChange={(e) => setUrl(e.target.value)} /></Field>
        <Field label="Events" as="div">
          <div className="space-y-2">
            {eventGroups.map((g, i) => (
              <label key={g.label} className="flex items-start gap-2 text-sm">
                <input type="checkbox" className="mt-1" checked={picked.has(i)} onChange={(e) => setPicked((p) => { const n = new Set(p); e.target.checked ? n.add(i) : n.delete(i); return n; })} />
                <span><span className="font-medium">{g.label}</span> <span className="text-neutral-500">{g.hint}</span></span>
              </label>
            ))}
          </div>
        </Field>
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex justify-end gap-2">
          <Button type="button" onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={busy} disabled={!name.trim() || !url.trim() || events.length === 0}>Add webhook</Button>
        </div>
      </form>
    </Modal>
  );
}

function Deliveries({ hook, onClose, onRotated }: { hook: Webhook; onClose: () => void; onRotated: (secret: string) => void }) {
  const list = usePoll(() => api.deliveries(hook.id), 0);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [sel, setSel] = useState<Delivery | null>(null);
  const run = async (fn: () => Promise<void>) => {
    setBusy(true);
    setError(null);
    try {
      await fn();
      list.reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
    setBusy(false);
  };
  return (
    <Modal title={`${hook.name}: deliveries`} onClose={onClose} wide>
      <div className="space-y-4">
        <div className="flex flex-wrap gap-2">
          <Button loading={busy} icon={<Send className="size-4" />} onClick={() => run(async () => { await api.testWebhook(hook.id); })}>Send test event</Button>
          <Button onClick={async () => (await confirmDialog({ title: "Rotate the secret?", message: "The old secret stops working at once; update the receiver.", confirmLabel: "Rotate" })) && run(async () => { const r = await api.updateWebhook(hook.id, { rotate_secret: true }); if (r.secret) onRotated(r.secret); })}>Rotate secret</Button>
        </div>
        {error && <ErrorNote>{error}</ErrorNote>}
        {!list.data ? <Loading /> : list.data.length === 0 ? <p className="text-sm text-neutral-500">Nothing has been sent yet. The last 50 deliveries are kept.</p> : (
          <Table head={["When", "Event", "Result", "Tries", "Time"]} compact>
            {list.data.map((d) => (
              <tr key={d.id} className="cursor-pointer hover:bg-neutral-50" onClick={() => setSel(d)}>
                <Td className="text-neutral-500">{ago(d.created_at)}</Td>
                <Td className="font-mono text-xs">{d.event}</Td>
                <Td>{d.ok ? <Badge tone="ok">{d.http_status}</Badge> : <Badge tone="bad">{d.http_status || "no answer"}</Badge>}</Td>
                <Td>{d.attempts}</Td>
                <Td className="text-neutral-500">{d.duration_ms} ms</Td>
              </tr>
            ))}
          </Table>
        )}
        {sel && (
          <div>
            {sel.error && <p className="mb-2 text-sm text-red-600">{sel.error}</p>}
            <CodeBlock code={sel.payload ?? ""} />
          </div>
        )}
      </div>
    </Modal>
  );
}

function Reference() {
  const [doc, setDoc] = useState<OpenApiDoc | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [q, setQ] = useState("");
  useEffect(() => { api.openapi().then(setDoc, (e) => setError(String(e))); }, []);
  if (error) return <ErrorNote>{error}</ErrorNote>;
  if (!doc) return <Loading />;
  const base = window.location.origin;
  const rows = Object.entries(doc.paths)
    .flatMap(([path, ms]) => Object.entries(ms).map(([m, op]) => ({ path, method: m.toUpperCase(), role: op["x-required-role"], key: op["x-api-key-access"] })))
    .filter((r) => !q || (r.method + " " + r.path).toLowerCase().includes(q.toLowerCase()));
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader title="Using the API" subtitle="Every route in the dashboard is available. The machine-readable spec is at /api/openapi.json (OpenAPI 3)." action={<CopyButton text={`${base}/api/openapi.json`} label="Copy spec URL" />} />
        <div className="space-y-3 p-5">
          <CodeBlock code={`curl -H "Authorization: Bearer $ORCH_KEY" ${base}/api/devices\ncurl -X POST -H "Authorization: Bearer $ORCH_KEY" ${base}/api/projects/<id>/deployments`} />
          <p className="text-sm text-neutral-600">Verify a webhook by computing <code>sha256=</code> + hex HMAC-SHA256 of <code>{"<X-Orchestrator-Timestamp>.<raw body>"}</code> with the webhook secret and comparing it to <code>X-Orchestrator-Signature</code>. Reject old timestamps to stop replays.</p>
        </div>
      </Card>
      <Card>
        <CardHeader title={<span className="inline-flex items-center gap-2"><Plug className="size-4" /> Routes</span>} action={<input className={inputClass + " w-56"} placeholder="Filter" aria-label="Filter routes" value={q} onChange={(e) => setQ(e.target.value)} />} />
        <Table head={["Method", "Path", "Role", "API key"]} compact>
          {rows.map((r) => (
            <tr key={r.method + r.path}>
              <Td className="font-mono text-xs">{r.method}</Td>
              <Td className="font-mono text-xs">{r.path}</Td>
              <Td><Badge tone={r.role === "admin" ? "warn" : "neutral"}>{r.role}</Badge></Td>
              <Td className="text-neutral-500">{r.key ? "yes" : "no"}</Td>
            </tr>
          ))}
        </Table>
      </Card>
    </div>
  );
}
