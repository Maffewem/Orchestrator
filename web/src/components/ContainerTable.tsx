import { useEffect, useState } from "react";
import { Link } from "react-router";
import { FileText, Play, RotateCw, Square } from "lucide-react";
import { api, type Container, type FleetContainer } from "../api";
import { Button, Empty, Loading, Modal, StatusDot, Table, Td } from "./ui";

function ports(c: Container) {
  const seen = new Set<string>();
  for (const p of c.ports ?? []) {
    seen.add(p.public_port ? `${p.public_port}→${p.private_port}/${p.type}` : `${p.private_port}/${p.type}`);
  }
  return [...seen].join(", ");
}

async function waitFor(id: string) {
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
}: {
  containers: (Container | FleetContainer)[];
  deviceId?: string;
  showDevice?: boolean;
  onChanged?: () => void;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  const [logs, setLogs] = useState<{ deviceId: string; c: Container } | null>(null);
  const [error, setError] = useState<string | null>(null);

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

  return (
    <>
      {error && <p className="border-b border-neutral-200 bg-red-50 px-5 py-2 text-sm text-bad">{error}</p>}
      <Table head={["Name", ...(showDevice ? ["Device"] : []), "Image", "Status", "Ports", ""]}>
        {containers.map((c) => {
          const dev = "device_id" in c ? c.device_id : deviceId!;
          const running = c.state === "running";
          return (
            <tr key={dev + c.id} className="hover:bg-neutral-50">
              <Td>
                <span className="flex items-center gap-2 font-medium">
                  <StatusDot status={c.state} />
                  {c.name}
                </span>
                {c.compose_project && <span className="ml-4 text-xs text-neutral-500">compose: {c.compose_project}</span>}
              </Td>
              {showDevice && "device_name" in c && (
                <Td>
                  <Link to={`/devices/${c.device_id}`} className="text-primary hover:underline">
                    {c.device_name}
                  </Link>
                </Td>
              )}
              <Td className="max-w-64 truncate font-mono text-xs">{c.image}</Td>
              <Td className="whitespace-nowrap text-neutral-600">{c.status}</Td>
              <Td className="font-mono text-xs whitespace-nowrap text-neutral-600">{ports(c) || "—"}</Td>
              <Td>
                <div className="flex justify-end gap-1">
                  {running ? (
                    <Button size="sm" variant="ghost" title="Stop" loading={busy === c.id + "container.stop"} icon={<Square className="size-3.5" />} onClick={() => run(dev, c, "container.stop")} />
                  ) : (
                    <Button size="sm" variant="ghost" title="Start" loading={busy === c.id + "container.start"} icon={<Play className="size-3.5" />} onClick={() => run(dev, c, "container.start")} />
                  )}
                  <Button size="sm" variant="ghost" title="Restart" loading={busy === c.id + "container.restart"} icon={<RotateCw className="size-3.5" />} onClick={() => run(dev, c, "container.restart")} />
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
