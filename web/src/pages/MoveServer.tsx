import { useEffect, useState } from "react";
import { Link } from "react-router";
import { CheckCircle2, Circle, Loader2, Server } from "lucide-react";
import { api, type MoveCandidate, type MoveView, type ServerMove } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, Loading, PageHeader, inputClass } from "../components/ui";
import { confirmDialog, notify } from "../components/dialogs";
import { usePoll } from "../lib";

const working = ["preparing", "installing", "verifying"];

// The five things that happen, in the order the person sees them.
const journey = [
  ["pick", "Choose the device"],
  ["copy", "Copy and start"],
  ["check", "Check it"],
  ["switch", "Switch devices"],
  ["confirm", "Confirm"],
] as const;

function stage(v: MoveView): number {
  const p = v.move?.phase;
  if (!p || p === "failed") return 0;
  if (working.includes(p)) return p === "verifying" ? 2 : 1;
  if (p === "ready") return 2;
  if (p === "switched") return 3;
  return 4;
}

function Journey({ at, done }: { at: number; done?: boolean }) {
  return (
    <ol className="flex flex-wrap gap-x-6 gap-y-2 text-sm">
      {journey.map(([k, label], i) => (
        <li key={k} className={i > at ? "text-neutral-400" : i === at && !done ? "font-medium" : "text-neutral-600"}>
          <span className="mr-1.5 inline-block align-text-bottom">
            {i < at || done ? <CheckCircle2 className="size-4 text-ok" /> : i === at ? <Loader2 className="size-4 animate-spin text-neutral-500" /> : <Circle className="size-4" />}
          </span>
          {label}
        </li>
      ))}
    </ol>
  );
}

function Lines({ m }: { m: ServerMove }) {
  return (
    <ul className="space-y-1 text-sm text-neutral-700">
      {m.steps.map((s, i) => (
        <li key={i}>✓ {s}</li>
      ))}
    </ul>
  );
}

export default function MoveServerPage() {
  useCrumbs([{ label: "Main server" }]);
  const { data: v, error, reload } = usePoll(() => api.serverMove(), 2000);
  const [busy, setBusy] = useState(false);
  if (error && !v) return <ErrorNote>{error}</ErrorNote>;
  if (!v) return <Loading />;

  const run = async (f: () => Promise<unknown>, note?: (r: unknown) => string | undefined) => {
    setBusy(true);
    try {
      const r = await f();
      const n = note?.(r);
      if (n) notify(n, "info");
      await reload();
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const noteOf = (r: unknown) => (r as { note?: string })?.note;

  const m = v.move;
  return (
    <div className="space-y-6">
      <PageHeader
        title="Main server"
        description="Promote another device to be the main server. Your projects, accounts, tokens and settings are copied across, devices switch over by themselves, and this server stays untouched until you say the move worked."
      />
      <Card className="p-5">
        <Journey at={stage(v)} done={m?.phase === "done"} />
      </Card>

      {v.moved_from && !v.standby_to && (
        <Card>
          <CardHeader title="This is the new main server" subtitle={`It took over from ${v.moved_from.old_url}`} />
          <div className="space-y-4 p-5 text-sm">
            <p>Check that your devices show as online and your projects look right. Then confirm the move. Until you do, the previous server is kept exactly as it was, so you can go back.</p>
            <div className="flex flex-wrap gap-2">
              <Button
                variant="primary"
                loading={busy}
                onClick={async () => {
                  if (await confirmDialog({ title: "Confirm the move?", message: "This server becomes the main server for good. The previous server is told, and stays as a standby that only points devices here.", confirmLabel: "Confirm the move" })) run(() => api.acceptServerMove(), noteOf);
                }}
              >
                Everything works: confirm
              </Button>
              <Button
                variant="danger"
                disabled={busy}
                onClick={async () => {
                  if (await confirmDialog({ title: "Move back to the previous server?", message: `Devices go back to ${v.moved_from!.old_url}. Anything you changed on this server since the move is not carried back.`, confirmLabel: "Move back", danger: true })) run(() => api.moveServerBack(), noteOf);
                }}
              >
                Move back to the previous server
              </Button>
            </div>
          </div>
        </Card>
      )}

      {v.standby_to && (
        <Card>
          <CardHeader title="This server has handed over" subtitle={`Devices are being sent to ${v.standby_to}`} />
          <div className="space-y-4 p-5 text-sm">
            <p>This server keeps its data but gives devices no work and pauses changes, so there is only ever one server in charge.</p>
            <div className="flex flex-wrap gap-2">
              <a href={v.standby_to} className="inline-flex"><Button variant="primary">Open the new dashboard</Button></a>
              {m && (
                <Button
                  variant="danger"
                  disabled={busy}
                  onClick={async () => {
                    if (await confirmDialog({ title: "Take this server back?", message: "Devices return to this server. Anything changed on the new server since the move is not carried back.", confirmLabel: "Take this server back", danger: true })) run(() => api.cancelServerMove(), noteOf);
                  }}
                >
                  Take this server back
                </Button>
              )}
            </div>
            {!m && <p className="text-neutral-500">To use this server again, open Main server on the new dashboard and choose Move back, or run the install command for this server again.</p>}
          </div>
        </Card>
      )}

      {m && working.includes(m.phase) && (
        <Card>
          <CardHeader title={`Setting up the new server on ${m.device_name}`} subtitle="Dashboard changes are paused on this server while this runs. Nothing else is touched." />
          <div className="space-y-3 p-5">
            <Lines m={m} />
            <p className="flex items-center gap-2 text-sm text-neutral-500">
              <Loader2 className="size-4 animate-spin" /> Working… this page updates by itself.
            </p>
          </div>
        </Card>
      )}

      {m?.phase === "ready" && (
        <Card>
          <CardHeader title={`${m.device_name} is ready to take over`} subtitle={`New server: ${m.new_url}`} />
          <div className="space-y-4 p-5 text-sm">
            <Lines m={m} />
            <div className="rounded-lg bg-blue-50 px-4 py-3">
              <p className="font-medium">Before you switch</p>
              <ul className="mt-1 list-disc space-y-1 pl-5 text-neutral-700">
                <li>You can open the new dashboard now at <a className="underline" href={m.new_url} target="_blank" rel="noreferrer">{m.new_url}</a> and sign in with the same accounts.</li>
                <li>If you reach the dashboard by a name (a domain, or a Cloudflare Tunnel), point it at {m.device_name} when you switch, so bookmarks and your GitHub webhook keep working.</li>
                <li>Switching sends every device to the new server. This server keeps its data and now only points devices there.</li>
              </ul>
            </div>
            <div className="flex flex-wrap gap-2">
              <Button
                variant="primary"
                loading={busy}
                onClick={async () => {
                  if (await confirmDialog({ title: `Switch ${v.on_this_server} device${v.on_this_server === 1 ? "" : "s"} to ${m.device_name}?`, message: "Devices connect to the new server within a few seconds. You can still take this server back afterwards.", confirmLabel: "Switch devices" })) run(() => api.switchServerMove());
                }}
              >
                Switch devices to {m.device_name}
              </Button>
              <Button disabled={busy} onClick={async () => { if (await confirmDialog({ title: "Cancel the move?", message: "This server stays in charge. The new server on the other device keeps running; remove its container there if you do not want it.", confirmLabel: "Cancel the move" })) run(() => api.cancelServerMove(), noteOf); }}>
                Cancel and keep this server
              </Button>
            </div>
          </div>
        </Card>
      )}

      {m?.phase === "switched" && (
        <Card>
          <CardHeader title="Devices are moving to the new server" subtitle={`New server: ${m.new_url}`} />
          <div className="space-y-4 p-5 text-sm">
            <p>
              {v.on_this_server === 0 ? "Every device has moved." : `${v.on_this_server} device${v.on_this_server === 1 ? " is" : "s are"} still connected to this server. Devices that are switched off move when they next start.`}
            </p>
            <ol className="list-decimal space-y-1 pl-5 text-neutral-700">
              <li>Open the new dashboard and check that your devices are online.</li>
              <li>There, choose <b>Everything works: confirm</b> (or <b>Move back</b> if something is wrong).</li>
              <li>You can also finish the move from here.</li>
            </ol>
            <div className="flex flex-wrap gap-2">
              <a href={m.new_url} target="_blank" rel="noreferrer" className="inline-flex"><Button variant="primary">Open the new dashboard</Button></a>
              <Button disabled={busy} onClick={() => run(() => api.finishServerMove())}>Finish the move from here</Button>
              <Button
                variant="danger"
                disabled={busy}
                onClick={async () => {
                  if (await confirmDialog({ title: "Take this server back?", message: "Devices return to this server. Anything changed on the new server since the switch is not carried back.", confirmLabel: "Take this server back", danger: true })) run(() => api.cancelServerMove(), noteOf);
                }}
              >
                Take this server back
              </Button>
            </div>
          </div>
        </Card>
      )}

      {m?.phase === "done" && (
        <Card>
          <CardHeader title="The move is complete" subtitle={`${m.device_name} is the main server`} />
          <div className="space-y-4 p-5 text-sm">
            <p>This server is now a standby: it pauses changes and points devices at the new server. Once you are happy, you can shut it down:</p>
            <pre className="overflow-x-auto rounded-lg bg-neutral-900 p-3 text-xs text-neutral-100">docker stop orchestrator && docker update --restart no orchestrator</pre>
            <p className="text-neutral-500">Keep its data volume for a while as a backup. To go back later, use Take this server back here, or Move back on the new dashboard.</p>
            <div className="flex flex-wrap gap-2">
              <a href={m.new_url} className="inline-flex"><Button variant="primary">Open the new dashboard</Button></a>
              <Button
                variant="danger"
                disabled={busy}
                onClick={async () => {
                  if (await confirmDialog({ title: "Take this server back?", message: "Devices return to this server. Anything changed on the new server since the move is not carried back.", confirmLabel: "Take this server back", danger: true })) run(() => api.cancelServerMove(), noteOf);
                }}
              >
                Take this server back
              </Button>
            </div>
          </div>
        </Card>
      )}

      {(!m || m.phase === "failed") && v.can_move && !v.moved_from && !v.standby_to && <Start v={v} failed={m?.phase === "failed" ? m : undefined} onStart={reload} />}
      {(!m || m.phase === "failed") && !v.can_move && !v.moved_from && !v.standby_to && <ErrorNote>{v.cannot_why}</ErrorNote>}

      {!m && !v.moved_from && !v.standby_to && <ManualMove />}
    </div>
  );
}

function Start({ v, failed, onStart }: { v: MoveView; failed?: ServerMove; onStart: () => void }) {
  const eligible = v.devices.filter((d) => d.eligible);
  const [pick, setPick] = useState<string>("");
  const [port, setPort] = useState("8080");
  const [addr, setAddr] = useState("");
  const [oldURL, setOldURL] = useState(window.location.origin);
  const [busy, setBusy] = useState(false);
  const dev: MoveCandidate | undefined = v.devices.find((d) => d.id === pick);
  useEffect(() => {
    if (dev) setAddr(dev.addresses[0] ?? "");
  }, [dev?.id]); // eslint-disable-line react-hooks/exhaustive-deps

  const go = async () => {
    if (!dev) return;
    if (!(await confirmDialog({
      title: `Make ${dev.name} the main server?`,
      message: "This server copies its data to the device and starts a new server there. Dashboard changes are paused here until you finish or cancel, and nothing is switched yet.",
      confirmLabel: "Start",
    }))) return;
    setBusy(true);
    try {
      await api.startServerMove({ device_id: dev.id, port: Number(port) || 8080, address: addr, old_url: oldURL });
      onStart();
    } catch (e) {
      notify(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <CardHeader title="1. Choose the device to promote" subtitle="It needs Docker and an up-to-date agent. Not in the list? Add it first." action={<Link to="/devices"><Button size="sm">Add a device</Button></Link>} />
      <div className="space-y-5 p-5">
        {failed && (
          <ErrorNote>
            The last attempt did not finish: {failed.error} Nothing changed on this server, and it is accepting changes again.
          </ErrorNote>
        )}
        <div className="space-y-2">
          {v.devices.length === 0 && <p className="text-sm text-neutral-500">No devices yet. Add one with Add a device, then come back.</p>}
          {v.devices.map((d) => (
            <label key={d.id} className={`flex items-start gap-3 rounded-lg border px-4 py-3 text-sm ${d.eligible ? "cursor-pointer border-neutral-200 hover:bg-neutral-50" : "border-neutral-100 text-neutral-400"} ${pick === d.id ? "!border-primary bg-blue-50/50" : ""}`}>
              <input type="radio" name="device" className="mt-1" disabled={!d.eligible} checked={pick === d.id} onChange={() => setPick(d.id)} />
              <Server className="mt-0.5 size-4 shrink-0" />
              <div className="min-w-0 flex-1">
                <p className="font-medium">{d.name} {d.arch && <span className="font-normal text-neutral-500">· {d.arch}</span>}</p>
                <p className="text-xs">{d.eligible ? d.addresses.join(", ") : d.reason}</p>
              </div>
              {!d.online && <Badge tone="bad">Offline</Badge>}
            </label>
          ))}
          {eligible.length === 0 && v.devices.length > 0 && <p className="text-sm text-neutral-500">None of your devices can take over yet. The reason is shown on each one.</p>}
        </div>
        {dev && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Address of the new server" hint="How your devices will reach it on the network">
              {dev.addresses.length > 1 ? (
                <select className={inputClass} value={addr} onChange={(e) => setAddr(e.target.value)}>
                  {dev.addresses.map((a) => <option key={a}>{a}</option>)}
                </select>
              ) : (
                <input className={inputClass} value={addr} onChange={(e) => setAddr(e.target.value)} />
              )}
            </Field>
            <Field label="Port" hint="8080 unless something else uses it there">
              <input className={inputClass} inputMode="numeric" value={port} onChange={(e) => setPort(e.target.value)} />
            </Field>
            <Field label="Address devices use for this server today" hint="Filled in from your browser; it is where devices fall back to if the move is cancelled">
              <input className={inputClass} value={oldURL} onChange={(e) => setOldURL(e.target.value)} />
            </Field>
          </div>
        )}
        <div className="flex items-center gap-3">
          <Button variant="primary" loading={busy} disabled={!dev || busy} onClick={go}>Promote {dev ? dev.name : "device"} to main server</Button>
          <p className="text-xs text-neutral-500">Nothing is switched yet. You check the new server first.</p>
        </div>
      </div>
    </Card>
  );
}

function ManualMove() {
  const [pw, setPw] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <Card>
      <CardHeader title="Moving by hand" subtitle="For a machine that has no agent yet, or when you would rather run the commands yourself" />
      <div className="space-y-3 p-5 text-sm text-neutral-700">
        <p>Download a password-protected copy of this server (it contains the key that seals your secrets, so it is encrypted). Anything changed after the download is not in it.</p>
        <div className="flex max-w-md gap-2">
          <input type="password" className={inputClass} placeholder="Choose a password (8+ characters)" value={pw} onChange={(e) => setPw(e.target.value)} />
          <Button
            loading={busy}
            disabled={pw.length < 8 || busy}
            onClick={async () => {
              setBusy(true);
              try {
                await api.exportServerCopy(pw);
              } catch (e) {
                notify(e instanceof Error ? e.message : String(e));
              } finally {
                setBusy(false);
              }
            }}
          >
            Download copy
          </Button>
        </div>
        <p>On the new machine, import it while installing the server:</p>
        <pre className="overflow-x-auto rounded-lg bg-neutral-900 p-3 text-xs text-neutral-100">{`curl -fsSL https://raw.githubusercontent.com/Maffewem/Orchestrator/main/scripts/install-server.sh -o install-server.sh
sudo sh install-server.sh --import orchestrator-move.db.enc`}</pre>
        <p>Then, here, add that address under Settings &gt; Server connectivity so devices learn it, or use the guided promotion above which does this for you.</p>
      </div>
    </Card>
  );
}
