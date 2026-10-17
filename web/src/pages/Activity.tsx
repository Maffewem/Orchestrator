import { Link } from "react-router";
import { AlertCircle, AlertTriangle, Info } from "lucide-react";
import { api, type Device, type Event } from "../api";
import { useCrumbs } from "../components/Layout";
import { Card, Empty, Loading, PageHeader } from "../components/ui";
import { ago, cx, usePoll } from "../lib";

export function EventList({ events, devices, compact }: { events: Event[]; devices?: Device[]; compact?: boolean }) {
  if (events.length === 0) return <Empty title="No activity yet" />;
  const name = (id?: string) => devices?.find((d) => d.id === id)?.name;
  return (
    <ul className="divide-y divide-neutral-100">
      {events.map((e) => {
        const Icon = e.level === "error" ? AlertCircle : e.level === "warning" ? AlertTriangle : Info;
        return (
          <li key={e.id} className={cx("flex gap-3 px-5", compact ? "py-2.5" : "py-3")}>
            <Icon className={cx("mt-0.5 size-4 shrink-0", e.level === "error" ? "text-bad" : e.level === "warning" ? "text-warn" : "text-neutral-400")} />
            <div className="min-w-0 flex-1">
              <p className={cx("text-sm", compact && "truncate")}>{e.message}</p>
              <p className="mt-0.5 text-xs text-neutral-500">
                {ago(e.created_at)}
                {!compact && name(e.device_id) && (
                  <>
                    {" · "}
                    <Link to={`/devices/${e.device_id}`} className="hover:underline">
                      {name(e.device_id)}
                    </Link>
                  </>
                )}
                {!compact && <span className="ml-2 font-mono">{e.kind}</span>}
              </p>
            </div>
          </li>
        );
      })}
    </ul>
  );
}

export default function ActivityPage() {
  useCrumbs([{ label: "Activity" }]);
  const events = usePoll(() => api.events(undefined, 200), 5000);
  const devices = usePoll(api.devices, 30000);
  return (
    <>
      <PageHeader title="Activity" description="Registrations, deployments, container changes and alerts from across the fleet." />
      <Card>{!events.data ? <Loading /> : <EventList events={events.data} devices={devices.data ?? []} />}</Card>
    </>
  );
}
