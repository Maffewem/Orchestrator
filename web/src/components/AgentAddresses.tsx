import { useState } from "react";
import { api } from "../api";
import { Button, inputClass } from "./ui";

/** Settings > Server addresses: the list of URLs every agent saves as fallbacks. */
export function AgentAddresses({ urls, onSaved }: { urls: string[]; onSaved: () => void }) {
  const [text, setText] = useState<string | null>(null);
  const [msg, setMsg] = useState<string | null>(null);
  const here = window.location.origin;
  const value = text ?? urls.join("\n");
  const lines = value.split("\n").map((l) => l.trim()).filter(Boolean);

  const save = async (next: string[]) => {
    setMsg(null);
    try {
      const r = await api.setAgentURLs(next);
      setText(r.agent_urls.join("\n"));
      setMsg(r.agent_urls.length ? "Saved. Devices pick this up on their next heartbeat." : "Saved. Devices use only the address they were installed with.");
      onSaved();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="space-y-3 p-5 text-sm">
      <p className="text-neutral-600">
        Each device remembers the address it was installed with. List every address this server can be reached at, one per line, and
        devices save them as fallbacks. Before a planned move, add the new address here first and devices switch over on their own.
      </p>
      <textarea
        className={inputClass + " h-24 font-mono text-xs"}
        placeholder={`${here}\nhttp://orchestrator.local:8080`}
        value={value}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="flex flex-wrap gap-2">
        <Button variant="primary" onClick={() => save(lines)}>
          Save
        </Button>
        {!lines.includes(here) && <Button onClick={() => setText([...lines, here].join("\n"))}>Add the address you are using now</Button>}
      </div>
      {msg && <p className="text-neutral-600">{msg}</p>}
      <p className="text-xs text-neutral-500">
        If none of these answer, a device looks for this server on its own local network and checks its identity before reconnecting,
        so an unplanned change of IP address heals without SSH. Devices on another network need a name or address that does not change.
      </p>
    </div>
  );
}
