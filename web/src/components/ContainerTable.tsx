import { useState, type ReactNode } from "react";
import { Link } from "react-router";
import { Archive, FileText, PackagePlus, Play, RotateCw, Trash2, Square, SquareTerminal } from "lucide-react";
import { api, type Container, type FleetContainer } from "../api";
import { Button, Empty, StatusDot, Table, Td } from "./ui";
import { useCan } from "./auth";
import { LiveLogs, Shell } from "./Streams";
import { AdoptDialog, adoptable, isSystem, isUnowned } from "./Adopt";
import { confirmWithOption } from "./dialogs";

/** Reachable URLs for a container's published ports on its device. */
export function endpoints(c: Container, addresses: string[]): string[] {
  const host = addresses[0];
  const out = new Set<string>();
  for (const p of c.ports ?? []) {
    if (!p.public_port || p.type !== "tcp") continue;
    const ip = p.ip && p.ip !== "0.0.0.0" && p.ip !== "::" ? p.ip : host;
    if (ip) out.add(`${ip.includes(":") ? `[${ip}]` : ip}:${p.public_port}`);
  }
  return [...out];
}

function ports(c: Container) {
  const seen = new Set<string>();
  for (const p of c.ports ?? []) {
    seen.add(p.public_port ? `${p.public_port}→${p.private_port}/${p.type}` : `${p.private_port}/${p.type}`);
  }
  return [...seen].join(", ");
}

export async function waitFor(id: string) {
  for (let i = 0; i < 120; i++) {
    const c = await api.getCommand(id);
    if (c.status === "succeeded" || c.status === "failed") return c;
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error("The device did not answer in time");
}

export default function ContainerTable({
  containers,
  deviceId,
  showDevice,
  onChanged,
  addresses,
}: {
  containers: (Container | FleetContainer)[];
  deviceId?: string;
  /** Device addresses, for per-device tables. */
  addresses?: string[];
  showDevice?: boolean;
  onChanged?: () => void;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  const [logs, setLogs] = useState<{ deviceId: string; c: Container } | null>(null);
  const [shell, setShell] = useState<{ deviceId: string; c: Container } | null>(null);
  const isAdmin = useCan("admin");
  const canOperate = useCan("operator");
  const [adopting, setAdopting] = useState<{ deviceId: string; deviceName: string; id: string } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<ReactNode>(null);

  if (containers.length === 0) return <Empty title="No containers">Containers on this device will show up here.</Empty>;

  const run = async (dev: string, c: Container, action: string, args?: Record<string, string>) => {
    setBusy(c.id + action);
    setError(null);
    try {
      const r = await waitFor((await api.command(dev, action, c.id, args)).id);
      if (r.status === "failed") setError(`${c.name}: ${r.error}`);
      else if (r.output && action === "container.remove") setNotice(`${c.name}: ${r.output}`);
      onChanged?.();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };

  const backup = async (dev: string, c: Container) => {
    setBusy(c.id + "backup");
    setError(null);
    try {
      await api.createBackup(dev, c.id);
      setNotice(
        <>
          Backup of {c.name} started. Follow it on the <Link to="/backups" className="font-medium underline">Backups</Link> page.
        </>,
      );
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };

  const endpointLinks = (c: Container | FleetContainer) =>
    endpoints(c, addresses ?? ("device_addresses" in c ? c.device_addresses : [])).map((e) => (
      <a key={e} href={`http://${e}`} target="_blank" rel="noreferrer" className="block font-mono text-primary hover:underline">
        {e}
      </a>
    ));

  const nameCell = (c: Container | FleetContainer, dev: string) => (
    <>
      <Link to={`/workers/${dev}/${encodeURIComponent(c.name)}`} className="flex items-center gap-2 font-medium text-neutral-900 hover:text-primary hover:underline">
        <StatusDot status={c.state} />
        {c.name}
        {c.name === "orchestrator-previous" && <span className="rounded bg-neutral-100 px-1.5 py-0.5 text-xs font-normal text-neutral-600" title="Kept by the last server update so it can be rolled back">rollback copy</span>}
      </Link>
      {"project_name" in c && c.project_name ? (
        <Link to={`/projects/${c.project_id}`} className="ml-4 text-xs text-primary hover:underline">
          project: {c.project_name}
        </Link>
      ) : (
        <span className="ml-4 inline-flex flex-wrap items-center gap-1.5 align-middle">
          {c.compose_project && <span className="text-xs text-neutral-500">stack: {c.compose_project}</span>}
          {isUnowned(c) && (
            <span
              className="rounded bg-neutral-100 px-1.5 py-0.5 text-xs text-neutral-600"
              title="Labelled by Orchestrator, but no existing project owns it (its project may have been deleted, or it was created separately)"
            >
              Orchestrator
            </span>
          )}
        </span>
      )}
    </>
  );

  const deviceCell = (c: FleetContainer | Container) =>
    "device_name" in c && (
      <>
        <Link to={`/devices/${c.device_id}`} className="flex items-center gap-1.5 text-primary hover:underline">
          <StatusDot status={c.device_status} />
          {c.device_name}
        </Link>
        <span className="mt-0.5 block font-mono text-xs text-neutral-500">{c.device_addresses.join(", ") || "no address reported"}</span>
      </>
    );

  const actionButtons = (c: Container | FleetContainer, dev: string) => {
    const running = c.state === "running";
    const system = isSystem(c);
    return (
      <>
        {canOperate && !system && (running ? (
          <Button size="sm" variant="ghost" title="Stop" loading={busy === c.id + "container.stop"} icon={<Square className="size-3.5" />} onClick={() => run(dev, c, "container.stop")} />
        ) : (
          <Button size="sm" variant="ghost" title="Start" loading={busy === c.id + "container.start"} icon={<Play className="size-3.5" />} onClick={() => run(dev, c, "container.start")} />
        ))}
        {canOperate && <Button size="sm" variant="ghost" title="Restart" loading={busy === c.id + "container.restart"} icon={<RotateCw className="size-3.5" />} onClick={() => run(dev, c, "container.restart")} />}
        {canOperate && <Button size="sm" variant="ghost" title="Back up" loading={busy === c.id + "backup"} icon={<Archive className="size-3.5" />} onClick={() => backup(dev, c)} />}
        {canOperate && <Button size="sm" variant="ghost" title="Logs" icon={<FileText className="size-3.5" />} onClick={() => setLogs({ deviceId: dev, c })} />}
        {isAdmin && adoptable(c) && (
          <Button size="sm" variant="ghost" title="Adopt as a project" icon={<PackagePlus className="size-3.5" />} onClick={() => setAdopting({ deviceId: dev, deviceName: "device_name" in c ? c.device_name : "this device", id: c.id })}>
            Adopt
          </Button>
        )}
        {isAdmin && adoptable(c) && (
          <Button
            size="sm"
            variant="ghost"
            title="Remove container"
            loading={busy === c.id + "container.remove"}
            icon={<Trash2 className="size-3.5 text-bad" />}
            onClick={async () => {
              const vols = c.volumes ?? [];
              const del = await confirmWithOption({
                title: `Remove ${c.name}?`,
                message: `The container is stopped and deleted from the device.${vols.length ? " Its volumes are kept unless you tick the box below." : ""}`,
                confirmLabel: "Remove",
                danger: true,
                option: vols.length ? { label: `Also delete its volumes (permanent): ${vols.join(", ")}`, checked: false } : undefined,
              });
              if (del !== null) run(dev, c, "container.remove", del && vols.length ? { volumes: "delete" } : undefined);
            }}
          />
        )}
        {isAdmin && running && !system && <Button size="sm" variant="ghost" title="Shell" icon={<SquareTerminal className="size-3.5" />} onClick={() => setShell({ deviceId: dev, c })} />}
      </>
    );
  };

  return (
    <>
      {notice && <p className="border-b border-neutral-200 bg-blue-50 px-5 py-2 text-sm text-neutral-800">{notice}</p>}
      {error && <p className="border-b border-neutral-200 bg-red-50 px-5 py-2 text-sm text-bad">{error}</p>}
      <div className="hidden md:block">
        <Table head={["Name", ...(showDevice ? ["Device"] : []), "Image", "Status", "Ports", "Endpoints", ""]}>
          {containers.map((c) => {
            const dev = "device_id" in c ? c.device_id : deviceId!;
            const running = c.state === "running";
            return (
              <tr key={dev + c.id} className="hover:bg-neutral-50">
                <Td>{nameCell(c, dev)}</Td>
                {showDevice && "device_name" in c && <Td>{deviceCell(c)}</Td>}
                <Td className="max-w-48 truncate font-mono text-xs">
                  <span title={c.image}>{c.image}</span>
                </Td>
                <Td className="min-w-24 text-neutral-600">{c.status}</Td>
                <Td className="font-mono text-xs text-neutral-600">{ports(c) || "—"}</Td>
                <Td className="text-xs break-all">{running && endpointLinks(c)}</Td>
                <Td>
                  <div className="flex max-w-40 flex-wrap justify-end gap-1 lg:max-w-52">{actionButtons(c, dev)}</div>
                </Td>
              </tr>
            );
          })}
        </Table>
      </div>
      <ul className="divide-y divide-neutral-100 md:hidden">
        {containers.map((c) => {
          const dev = "device_id" in c ? c.device_id : deviceId!;
          const eps = c.state === "running" ? endpointLinks(c) : null;
          return (
            <li key={dev + c.id} className="px-4 py-4">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0 flex-1">{nameCell(c, dev)}</div>
                <span className="shrink-0 text-xs text-neutral-500">{c.status}</span>
              </div>
              <dl className="mt-3 grid grid-cols-[4.5rem_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-sm">
                {showDevice && "device_name" in c && (
                  <>
                    <dt className="text-neutral-500">Device</dt>
                    <dd>{deviceCell(c)}</dd>
                  </>
                )}
                <dt className="text-neutral-500">Image</dt>
                <dd className="break-all font-mono text-xs leading-5">{c.image}</dd>
                <dt className="text-neutral-500">Ports</dt>
                <dd className="font-mono text-xs leading-5 text-neutral-700">{ports(c) || "—"}</dd>
                {eps && (
                  <>
                    <dt className="text-neutral-500">Open</dt>
                    <dd className="text-xs break-all leading-5">{eps}</dd>
                  </>
                )}
              </dl>
              <div className="mt-3 flex flex-wrap gap-1.5 border-t border-neutral-100 pt-3">{actionButtons(c, dev)}</div>
            </li>
          );
        })}
      </ul>
      {logs && <LiveLogs deviceId={logs.deviceId} container={logs.c.id} name={logs.c.name} onClose={() => setLogs(null)} />}
      {adopting && (
        <AdoptDialog
          deviceId={adopting.deviceId}
          deviceName={adopting.deviceName}
          containers={containers.filter((c) => ("device_id" in c ? c.device_id : deviceId) === adopting.deviceId)}
          selected={[adopting.id]}
          onClose={() => setAdopting(null)}
          onDone={() => onChanged?.()}
        />
      )}
      {shell && <Shell deviceId={shell.deviceId} container={shell.c.id} name={shell.c.name} onClose={() => setShell(null)} />}
    </>
  );
}
