import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { X } from "lucide-react";
import { api, ApiError } from "../api";
import { Modal } from "./ui";

/** Follows a stream's output until it ends or the signal aborts, calling onData for each chunk. */
async function follow(id: string, signal: AbortSignal, onData: (b: Uint8Array) => void): Promise<string | null> {
  let from = 0;
  let failures = 0;
  while (!signal.aborted) {
    try {
      const c = await api.streamOutput(id, from, signal);
      failures = 0;
      from = c.next;
      if (c.data.length) onData(c.data);
      if (c.closed) return c.error ?? null;
    } catch (e) {
      if (signal.aborted) return null;
      if (e instanceof ApiError && e.status !== 0 && e.status < 500) return e.message;
      if (++failures > 5) return "Lost contact with the server";
      await new Promise((r) => setTimeout(r, 1000 * failures));
    }
  }
  return null;
}

const MAX_LOG_CHARS = 1_000_000;

/** Live, following logs of a container. */
export function LiveLogs({ deviceId, container, name, onClose }: { deviceId: string; container: string; name: string; onClose: () => void }) {
  const [text, setText] = useState("");
  const [status, setStatus] = useState<"connecting" | "live" | "ended">("connecting");
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(false);
  const held = useRef("");
  const box = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  pausedRef.current = paused;

  useEffect(() => {
    const ac = new AbortController();
    const dec = new TextDecoder();
    let id = "";
    const add = (s: string) => {
      if (pausedRef.current) {
        held.current += s;
        return;
      }
      setText((t) => (t + s).slice(-MAX_LOG_CHARS));
    };
    (async () => {
      try {
        id = (await api.openStream(deviceId, { kind: "logs", container, tail: 200 })).id;
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setStatus("ended");
        return;
      }
      setStatus("live");
      const err = await follow(id, ac.signal, (b) => add(dec.decode(b, { stream: true })));
      if (err) setError(err);
      if (!ac.signal.aborted) setStatus("ended");
    })();
    return () => {
      ac.abort();
      if (id) api.closeStream(id).catch(() => {});
    };
  }, [deviceId, container]);

  useEffect(() => {
    if (!paused && held.current) {
      const h = held.current;
      held.current = "";
      setText((t) => (t + h).slice(-MAX_LOG_CHARS));
    }
  }, [paused]);

  useEffect(() => {
    const el = box.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [text, filter]);

  const shown = filter ? text.split("\n").filter((l) => l.toLowerCase().includes(filter.toLowerCase())).join("\n") : text;
  return (
    <Modal title={`Logs: ${name}`} onClose={onClose} wide>
      <div className="mb-3 flex flex-wrap items-center gap-3 text-sm">
        <span className="inline-flex items-center gap-1.5">
          <span className={`size-2 rounded-full ${status === "live" ? "animate-pulse bg-ok" : "bg-neutral-400"}`} />
          {status === "connecting" ? "Connecting…" : status === "live" ? (paused ? "Paused" : "Live") : "Ended"}
        </span>
        <button className="rounded-md border border-neutral-300 px-2.5 py-1 hover:bg-neutral-50" onClick={() => setPaused(!paused)} disabled={status !== "live"}>
          {paused ? "Resume" : "Pause"}
        </button>
        <button className="rounded-md border border-neutral-300 px-2.5 py-1 hover:bg-neutral-50" onClick={() => setText("")}>
          Clear
        </button>
        <input
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter lines"
          aria-label="Filter lines"
          className="ml-auto h-8 w-48 rounded-md border border-neutral-300 px-2.5"
        />
      </div>
      {error && <p className="mb-3 text-sm text-bad">{error}</p>}
      <pre
        ref={box}
        onScroll={(e) => {
          const el = e.currentTarget;
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
        className="h-[60vh] overflow-auto whitespace-pre-wrap break-all rounded-lg bg-neutral-900 p-4 font-mono text-xs leading-relaxed text-neutral-100"
      >
        {shown || (status === "live" ? "Waiting for output…" : "(no output)")}
      </pre>
    </Modal>
  );
}

/** An interactive shell inside a container. Administrators only; the server enforces that. */
export function Shell({ deviceId, container, name, onClose }: { deviceId: string; container: string; name: string; onClose: () => void }) {
  const host = useRef<HTMLDivElement>(null);
  const [state, setState] = useState<"connecting" | "live" | "ended">("connecting");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!host.current) return;
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 13,
      theme: { background: "#171717" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current);
    fit.fit();
    term.focus();

    const ac = new AbortController();
    let id = "";
    let queue: Promise<unknown> = Promise.resolve();
    const send = (input: { data?: string; cols?: number; rows?: number }) => {
      if (!id) return;
      queue = queue.then(() => api.streamInput(id, input)).catch(() => {});
    };
    term.onData((d) => send({ data: d }));
    term.onResize(({ cols, rows }) => send({ cols, rows }));
    const ro = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        /* host not laid out yet */
      }
    });
    ro.observe(host.current);

    (async () => {
      try {
        id = (await api.openStream(deviceId, { kind: "shell", container, cols: term.cols, rows: term.rows })).id;
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setState("ended");
        return;
      }
      setState("live");
      const err = await follow(id, ac.signal, (b) => term.write(b));
      if (ac.signal.aborted) return;
      if (err) setError(err);
      term.write("\r\n\x1b[2m[session ended]\x1b[0m\r\n");
      setState("ended");
    })();
    return () => {
      ac.abort();
      ro.disconnect();
      if (id) api.closeStream(id).catch(() => {});
      term.dispose();
    };
  }, [deviceId, container]);

  // Not a Modal: Escape and clicks outside belong to the program in the terminal.
  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/40 p-4 sm:p-10">
      <div className="w-full max-w-4xl overflow-hidden rounded-xl bg-neutral-900 shadow-xl">
        <div className="flex items-center justify-between border-b border-neutral-700 px-4 py-2.5 text-sm text-neutral-200">
          <span className="flex items-center gap-2 font-medium">
            <span className={`size-2 rounded-full ${state === "live" ? "bg-ok" : "bg-neutral-500"}`} />
            Shell: {name}
            {state === "connecting" && <span className="font-normal text-neutral-400">connecting…</span>}
          </span>
          <button onClick={onClose} className="rounded-md p-1 text-neutral-300 hover:bg-neutral-700" aria-label="Close shell">
            <X className="size-4" />
          </button>
        </div>
        {error && <p className="bg-red-950 px-4 py-2 text-sm text-red-200">{error}</p>}
        <div className="p-2">
          <div ref={host} className="h-[calc(min(60vh,100dvh-9rem))]" />
        </div>
      </div>
    </div>
  );
}
