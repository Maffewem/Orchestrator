import { useEffect, useRef, useState } from "react";
import { RefreshCw } from "lucide-react";
import { api, type UpdateSchedule, type UpdateStatus } from "../api";
import { useCrumbs } from "../components/Layout";
import { Badge, Button, Card, CardHeader, ErrorNote, Field, Loading, PageHeader, Table, Td, inputClass } from "../components/ui";
import { ago, usePoll } from "../lib";

const days = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

function ServerCard({ st, reload }: { st: UpdateStatus; reload: () => void }) {
  const [error, setError] = useState<string | null>(null);
  const run = async (f: () => Promise<unknown>) => {
    setError(null);
    try {
      await f();
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  const busy = !!st.busy || st.updating;
  const newer = st.check?.newer;
  return (
    <Card className="mb-6">
      <CardHeader title="Control plane" subtitle="Updates pull the newest published image and restart the server on it. The previous version is kept and restored automatically if the new one doesn't come up healthy." />
      <div className="space-y-4 p-5">
        <dl className="grid gap-4 text-sm sm:grid-cols-3">
          <div>
            <dt className="text-neutral-500">Running</dt>
            <dd className="font-mono font-medium">{st.version || "unknown"}</dd>
          </div>
          <div>
            <dt className="text-neutral-500">Latest build</dt>
            <dd className="font-medium">
              {!st.check ? "Not checked yet" : st.check.error ? <span className="text-bad">Check failed</span> : newer ? <span className="font-mono">{st.check.version || "newer build"}</span> : "You're up to date"}
            </dd>
          </div>
          <div>
            <dt className="text-neutral-500">Last checked</dt>
            <dd>{st.check ? ago(st.check.checked_at) : "never"}</dd>
          </div>
        </dl>
        {st.check?.error && <p className="text-sm text-bad">{st.check.error}</p>}
        {st.updating && (
          <p className="rounded-lg bg-blue-50 px-4 py-2 text-sm">Updating. The dashboard goes offline for a few seconds while the server restarts, then reconnects by itself.</p>
        )}
        {st.result && !st.updating && (
          <p className={`rounded-lg px-4 py-2 text-sm ${st.result.ok ? "bg-green-50" : "bg-red-50 text-bad"}`}>
            {st.result.message} <span className="text-neutral-500">({ago(st.result.at)})</span>
          </p>
        )}
        {!st.can_update && (
          <p className="rounded-lg bg-amber-50 px-4 py-2 text-sm">
            This server can't update itself: {st.reason}. Updating from the dashboard needs the server to run as a Docker container with the Docker socket mounted, which gives that container control of Docker on this machine. See the README ("Updating") for how to enable it.
          </p>
        )}
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex flex-wrap gap-2">
          <Button loading={st.busy === "checking"} disabled={busy || !st.can_update} icon={<RefreshCw className="size-4" />} onClick={() => run(api.checkUpdate)}>
            Check now
          </Button>
          <Button
            variant="primary"
            loading={st.busy === "updating" || st.updating}
            disabled={busy || !st.can_update || newer === false}
            onClick={() => {
              if (window.confirm("Update the server now? The dashboard will be unavailable for a few seconds.")) run(() => api.applyUpdate(st.schedule.agents));
            }}
          >
            {newer ? "Update now" : "Update to latest"}
          </Button>
        </div>
      </div>
    </Card>
  );
}

function ScheduleCard({ st, reload }: { st: UpdateStatus; reload: () => void }) {
  const [s, setS] = useState<UpdateSchedule>(st.schedule);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const first = useRef(true);
  useEffect(() => {
    if (!first.current) return;
    first.current = false;
    // A server that never had a schedule saved shows the browser's time zone as the default.
    const tz = st.schedule.mode === "off" && st.schedule.tz === "UTC" ? Intl.DateTimeFormat().resolvedOptions().timeZone : st.schedule.tz;
    setS({ ...st.schedule, tz });
  }, [st.schedule]);
  const set = (p: Partial<UpdateSchedule>) => {
    setSaved(false);
    setS({ ...s, ...p });
  };
  const save = async () => {
    setError(null);
    try {
      await api.setUpdateSchedule(s);
      setSaved(true);
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  return (
    <Card className="mb-6">
      <CardHeader title="Automatic updates" subtitle="Check for the latest build and install it on a schedule." />
      <div className="space-y-4 p-5">
        <div className="grid gap-4 sm:grid-cols-4">
          <Field label="Check">
            <select className={inputClass} value={s.mode} onChange={(e) => set({ mode: e.target.value as UpdateSchedule["mode"] })}>
              <option value="off">Never (manual only)</option>
              <option value="daily">Every day</option>
              <option value="weekly">Every week</option>
            </select>
          </Field>
          {s.mode === "weekly" && (
            <Field label="Day">
              <select className={inputClass} value={s.weekday} onChange={(e) => set({ weekday: Number(e.target.value) })}>
                {days.map((d, i) => (
                  <option key={d} value={i}>
                    {d}
                  </option>
                ))}
              </select>
            </Field>
          )}
          {s.mode !== "off" && (
            <>
              <Field label="At">
                <input type="time" className={inputClass} value={s.time} onChange={(e) => set({ time: e.target.value })} />
              </Field>
              <Field label="Time zone">
                <input className={inputClass} value={s.tz} onChange={(e) => set({ tz: e.target.value })} placeholder="Europe/London" />
              </Field>
            </>
          )}
        </div>
        {s.mode !== "off" && (
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" className="size-4 accent-primary" checked={s.agents} onChange={(e) => set({ agents: e.target.checked })} />
            Also update the agents on every device to match the server
          </label>
        )}
        {st.next_run && st.schedule.mode !== "off" && (
          <p className="text-sm text-neutral-600">Next run: {new Date(st.next_run).toLocaleString()}</p>
        )}
        {error && <ErrorNote>{error}</ErrorNote>}
        <div className="flex items-center gap-3">
          <Button variant="primary" onClick={save}>
            Save schedule
          </Button>
          {saved && <span className="text-sm text-neutral-600">Saved</span>}
        </div>
      </div>
    </Card>
  );
}

function AgentsCard({ st, reload }: { st: UpdateStatus; reload: () => void }) {
  const [msg, setMsg] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const outdated = st.agents.filter((a) => a.outdated && a.online && !a.in_container);
  const go = async (device?: string) => {
    setError(null);
    try {
      const r = await api.updateAgents(device);
      setMsg(r.queued ? `Update queued for ${r.queued} device(s). They restart their agent when done.` : "Nothing to update right now.");
      reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };
  return (
    <Card>
      <CardHeader
        title="Device agents"
        subtitle={`Each agent downloads the build this server carries (${st.version || "unknown"}), checks it, and keeps its old binary to fall back on.`}
        action={
          <Button disabled={outdated.length === 0} onClick={() => go()}>
            Update {outdated.length || ""} outdated
          </Button>
        }
      />
      {(msg || error) && <div className="px-5 pt-4">{error ? <ErrorNote>{error}</ErrorNote> : <p className="text-sm">{msg}</p>}</div>}
      <Table head={["Device", "Agent version", "Status", ""]}>
        {st.agents.map((a) => (
          <tr key={a.device_id}>
            <Td className="font-medium">{a.name}</Td>
            <Td className="font-mono text-xs">{a.version || "unknown"}</Td>
            <Td>
              {a.in_container ? <Badge>Container: pull a new image</Badge> : !a.online ? <Badge>Offline</Badge> : a.outdated ? <Badge tone="warn">Update available</Badge> : <Badge tone="ok">Up to date</Badge>}
            </Td>
            <Td>
              <div className="flex justify-end">
                {a.outdated && a.online && !a.in_container && (
                  <Button size="sm" onClick={() => go(a.device_id)}>
                    Update
                  </Button>
                )}
              </div>
            </Td>
          </tr>
        ))}
      </Table>
    </Card>
  );
}

export default function UpdatesPage() {
  useCrumbs([{ label: "Updates" }]);
  const [interval, setInterval_] = useState(5000);
  const { data, error, reload } = usePoll(api.updateStatus, interval);
  const wasUpdating = useRef(false);
  useEffect(() => {
    if (data) wasUpdating.current = data.updating || !!data.busy;
    setInterval_(data && (data.updating || data.busy) ? 2000 : 5000);
  }, [data]);

  return (
    <>
      <PageHeader title="Updates" description="Keep the control plane and the device agents on the latest build, by hand or on a schedule." />
      {!data ? (
        error ? <ErrorNote>{error}</ErrorNote> : <Loading />
      ) : (
        <>
          {error && wasUpdating.current && <p className="mb-4 rounded-lg bg-blue-50 px-4 py-2 text-sm">Waiting for the server to come back after its restart…</p>}
          <ServerCard st={data} reload={reload} />
          <ScheduleCard st={data} reload={reload} />
          <AgentsCard st={data} reload={reload} />
        </>
      )}
    </>
  );
}
