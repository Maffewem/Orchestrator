import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router";
import { Archive, FileText, Play, RotateCw, Square } from "lucide-react";
import { api, type Container, type FleetContainer } from "../api";
import { Button, Empty, Loading, Modal, StatusDot, Table, Td } from "./ui";

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

function LogsModal({ deviceId, c, onClose }: { deviceId: string; c: Container; onClose: () => void }) {
  const [output, setOutput] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    api
      .command(deviceId, "container.logs", c.id, { tail: "300" })
      .then((cmd) => waitFor(cmd.id))
      .then((r) => {
        if (cancelled) return;
        if (r.status === "succeeded") setOutput(r.output ?? "");
        else setError(r.error ?? "Failed");
      })
      .catch((e) => !cancelled && setError(String(e.message ?? e)));
    return () => {
      cancelled = true;
    };
  }, [deviceId, c.id]);
  return (
    <Modal title={`Logs: ${c.name}`} onClose={onClose} wide>
      {error ? (
        <p className="text-sm text-bad">{error}</p>
      ) : output === null ? (
        <Loading />
      ) : (
        <pre className="max-h-[60vh] overflow-auto rounded-lg bg-neutral-900 p-4 font-mono text-xs leading-relaxed text-neutral-100">
          {output || "(no output)"}
        </pre>
      )}
    </Modal>
  );
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
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<ReactNode>(null);

  if (containers.length === 0) return <Empty title="No containers">Containers on this device will show up here.</Empty>;

  const run = async (dev: string, c: Container, action: string) => {
    setBusy(c.id + action);
    setError(null);
    try {
      const r = await waitFor((await api.command(dev, action, c.id)).id);
      if (r.status === "failed") setError(`${c.name}: ${r.error}`);
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

  return (
    <>
      {notice && <p className="border-b border-neutral-200 bg-blue-50 px-5 py-2 text-sm text-neutral-800">{notice}</p>}
      {error && <p className="border-b border-neutral-200 bg-red-50 px-5 py-2 text-sm text-bad">{error}</p>}
      <Table head={["Name", ...(showDevice ? ["Device"] : []), "Image", "Status", "Ports", "Endpoints", ""]}>
        {containers.map((c) => {
          const dev = "device_id" in c ? c.device_id : deviceId!;
          const running = c.state === "running";
          return (
            <tr key={dev + c.id} className="hover:bg-neutral-50">
              <Td>
                <Link to={`/workers/${dev}/${encodeURIComponent(c.name)}`} className="flex items-center gap-2 font-medium text-neutral-900 hover:text-primary hover:underline">
                  <StatusDot status={c.state} />
                  {c.name}
                </Link>
                {"project_name" in c && c.project_name ? (
                  <Link to={`/projects/${c.project_id}`} className="ml-4 text-xs text-primary hover:underline">
                    project: {c.project_name}
                  </Link>
                ) : (
                  c.compose_project && <span className="ml-4 text-xs text-neutral-500">stack: {c.compose_project}</span>
                )}
              </Td>
              {showDevice && "device_name" in c && (
                <Td>
                  <Link to={`/devices/${c.device_id}`} className="flex items-center gap-1.5 text-primary hover:underline">
                    <StatusDot status={c.device_status} />
                    {c.device_name}
                  </Link>
                  <span className="mt-0.5 block font-mono text-xs text-neutral-500">{c.device_addresses.join(", ") || "no address reported"}</span>
                </Td>
              )}
              <Td className="max-w-64 truncate font-mono text-xs">{c.image}</Td>
              <Td className="whitespace-nowrap text-neutral-600">{c.status}</Td>
              <Td className="font-mono text-xs whitespace-nowrap text-neutral-600">{ports(c) || "—"}</Td>
              <Td className="text-xs whitespace-nowrap">
                {running && endpoints(c, addresses ?? ("device_addresses" in c ? c.device_addresses : [])).map((e) => (
                  <a key={e} href={`http://${e}`} target="_blank" rel="noreferrer" className="block font-mono text-primary hover:underline">
                    {e}
                  </a>
                ))}
              </Td>
              <Td>
                <div className="flex justify-end gap-1">
                  {running ? (
                    <Button size="sm" variant="ghost" title="Stop" loading={busy === c.id + "container.stop"} icon={<Square className="size-3.5" />} onClick={() => run(dev, c, "container.stop")} />
                  ) : (
                    <Button size="sm" variant="ghost" title="Start" loading={busy === c.id + "container.start"} icon={<Play className="size-3.5" />} onClick={() => run(dev, c, "container.start")} />
                  )}
                  <Button size="sm" variant="ghost" title="Restart" loading={busy === c.id + "container.restart"} icon={<RotateCw className="size-3.5" />} onClick={() => run(dev, c, "container.restart")} />
                  <Button size="sm" variant="ghost" title="Back up" loading={busy === c.id + "backup"} icon={<Archive className="size-3.5" />} onClick={() => backup(dev, c)} />
                  <Button size="sm" variant="ghost" title="Logs" icon={<FileText className="size-3.5" />} onClick={() => setLogs({ deviceId: dev, c })} />
                </div>
              </Td>
            </tr>
          );
        })}
      </Table>
      {logs && <LogsModal deviceId={logs.deviceId} c={logs.c} onClose={() => setLogs(null)} />}
    </>
  );
}
