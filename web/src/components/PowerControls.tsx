import { useState } from "react";
import { Play, RotateCw, Square } from "lucide-react";
import { api } from "../api";
import { Button } from "./ui";
import { useCan } from "./auth";
import { waitFor } from "./ContainerTable";

/** Start/stop and restart for one container; renders nothing for viewers. */
export default function PowerControls({
  deviceId,
  containerId,
  state,
  size = "md",
  onDone,
  onError,
}: {
  deviceId: string;
  containerId: string;
  state: string;
  size?: "sm" | "md";
  onDone?: () => void;
  onError?: (message: string | null) => void;
}) {
  const canOperate = useCan("operator");
  const [busy, setBusy] = useState<string | null>(null);
  if (!canOperate) return null;
  const running = state === "running";
  const run = async (action: string) => {
    setBusy(action);
    onError?.(null);
    try {
      const r = await waitFor((await api.command(deviceId, action, containerId)).id);
      if (r.status === "failed") onError?.(r.error ?? "Failed");
      onDone?.();
    } catch (e) {
      onError?.(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(null);
    }
  };
  const small = size === "sm";
  const cls = small ? "size-3.5" : "size-4";
  const props = small ? ({ size: "sm", variant: "ghost" } as const) : {};
  return (
    <>
      {running ? (
        <Button {...props} title="Stop" loading={busy === "container.stop"} icon={<Square className={cls} />} onClick={() => run("container.stop")}>
          {small ? undefined : "Stop"}
        </Button>
      ) : (
        <Button {...props} title="Start" loading={busy === "container.start"} icon={<Play className={cls} />} onClick={() => run("container.start")}>
          {small ? undefined : "Start"}
        </Button>
      )}
      <Button {...props} title="Restart" loading={busy === "container.restart"} icon={<RotateCw className={cls} />} onClick={() => run("container.restart")}>
        {small ? undefined : "Restart"}
      </Button>
    </>
  );
}
