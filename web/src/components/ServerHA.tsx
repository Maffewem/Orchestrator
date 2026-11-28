import { useState } from "react";
import { api } from "../api";
import { Badge, Button, Card, CardHeader, CodeBlock, ErrorNote, Field, inputClass } from "./ui";
import { ago, usePoll } from "../lib";
import { confirmDialog } from "./dialogs";

/** Server high availability (Beta): pair a standby server, see its state, promote it. */
export default function ServerHA() {
  const { data, error, reload } = usePoll(api.serverHA, 5000);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [join, setJoin] = useState({ active_url: "", token: "", self_url: "" });

  const run = async (f: () => Promise<unknown>) => {
    setErr("");
    setBusy(true);
    try {
      await f();
      reload();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  const promote = async () => {
    const down = await confirmDialog({
      title: "Promote this standby?",
      message: "If the active server is stopped or unreachable, choose \"It is down\". Otherwise choose \"Switch over\" to ask it to step down first and take a last copy, so nothing is lost. This server restarts and becomes the active one.",
      confirmLabel: "It is down: promote now",
      danger: true,
    });
    if (down) return run(() => api.haPromote(true));
    if (await confirmDialog({ title: "Switch over from the active server?", message: "The active server is asked to step down, then this one takes a last copy and becomes active.", confirmLabel: "Switch over" })) return run(() => api.haPromote(false));
  };

  return (
    <Card className="mb-6">
      <CardHeader title={<span className="flex items-center gap-2">Server high availability <Badge tone="info">Beta</Badge></span>} subtitle="Keep a second server up to date and take over from it by hand. Both servers must be the same version." />
      <div className="space-y-3 p-4 text-sm">
        {(error || err) && <ErrorNote>{error || err}</ErrorNote>}
        {!data ? null : data.role === "standby" ? (
          <>
            <p>
              This server is a <Badge tone={data.ready ? "ok" : "warn"}>standby{data.ready ? ", ready" : ", not ready"}</Badge> of <code className="font-mono">{data.peer_url}</code>. It is read-only and devices do not use it.
            </p>
            <p className="text-neutral-600">
              {data.last_sync ? `Last copy ${ago(data.last_sync)}${data.sync_version ? ` (version ${data.sync_version})` : ""}.` : "No copy yet."}
              {data.sync_error && <span className="text-red-700"> Last attempt failed: {data.sync_error}</span>}
            </p>
            {data.sync_version && data.sync_version !== data.version && <ErrorNote>The active server runs {data.sync_version} and this one {data.version}. Update the standby before promoting.</ErrorNote>}
            <div className="flex gap-2">
              <Button variant="danger" disabled={busy || !data.ready || !data.can_restart} onClick={promote}>Promote to active</Button>
              <Button disabled={busy} onClick={async () => { if (await confirmDialog({ title: "Leave the pair?", message: "This server becomes a standalone server again and keeps its own data. It restarts.", confirmLabel: "Leave" })) run(() => api.haLeave()); }}>Leave the pair</Button>
            </div>
            <div className="rounded-md border border-neutral-200 p-3">
              <label className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={!!data.auto_promote}
                  disabled={busy}
                  onChange={async (e) => {
                    const on = e.target.checked;
                    if (on && !(await confirmDialog({ title: "Promote automatically?", message: "If the active server stops answering for the wait below and a majority of at least 3 devices also cannot reach it, this server takes over by itself and restarts. With fewer than 3 devices it never promotes by itself.", confirmLabel: "Turn on", danger: true }))) return;
                    run(() => api.haAuto(on, data.auto_after || 45));
                  }}
                />
                Promote automatically when the active server is down
              </label>
              <p className="mt-1 text-xs text-neutral-500">Waits {data.auto_after || 45} s without a copy, then needs a majority of devices (at least 3) to confirm they cannot reach the active server. It never promotes when only the link between the two servers is broken.</p>
              {data.auto_note && <p className="mt-1 text-xs text-amber-700">{data.auto_note}</p>}
            </div>
            <p className="text-xs text-neutral-500">After promotion, devices need this server's address to follow it: add it under Settings &gt; Server addresses on the active server beforehand, and repoint the dashboard address (DNS or tunnel) by hand.</p>
          </>
        ) : (
          <>
            <p>This is the <Badge tone="ok">active</Badge> server (epoch {data.epoch}).{data.previous && ` It took over from ${data.previous} and is waiting for it to step down.`}</p>
            {data.token ? (
              <>
                <p>Pairing token. On the standby server, open this page and enter this server's address and the token:</p>
                <CodeBlock code={data.token} />
                <p className="text-neutral-600">{data.standby_last_pull ? `A standby last fetched a copy ${ago(data.standby_last_pull)}.` : "No standby has connected yet."} Use https or a trusted network: the copy contains this server's secret key (it is encrypted with the token in transit).</p>
              </>
            ) : null}
            <Button disabled={busy} onClick={() => run(() => api.haPair())}>{data.token ? "Create a new token" : "Create a pairing token"}</Button>
            {!data.token && (
              <div className="mt-4 space-y-3 border-t border-neutral-100 pt-4">
                <p className="font-medium">Or make this server a standby of another one</p>
                <p className="text-neutral-600">Use a fresh server. Its own data is replaced by the active server's if it is ever promoted.</p>
                <Field label="Active server address"><input className={inputClass} placeholder="http://192.168.0.184:8080" value={join.active_url} onChange={(e) => setJoin({ ...join, active_url: e.target.value })} /></Field>
                <Field label="Pairing token"><input className={inputClass} value={join.token} onChange={(e) => setJoin({ ...join, token: e.target.value })} /></Field>
                <Field label="This server's address" hint="How the active server reaches this one"><input className={inputClass} placeholder="http://192.168.0.125:8080" value={join.self_url} onChange={(e) => setJoin({ ...join, self_url: e.target.value })} /></Field>
                <Button variant="primary" disabled={busy || !join.active_url || !join.token} onClick={async () => { if (await confirmDialog({ title: "Make this server a standby?", message: "It becomes read-only and stops serving devices until promoted.", confirmLabel: "Make standby", danger: true })) run(() => api.haJoin(join)); }}>Make this server a standby</Button>
              </div>
            )}
          </>
        )}
      </div>
    </Card>
  );
}
