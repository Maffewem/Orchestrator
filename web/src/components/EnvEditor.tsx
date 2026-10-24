import { useRef, useState } from "react";
import { Eye, EyeOff, Plus, Trash2, Upload } from "lucide-react";
import { mergeEnv, parseEnv, SECRET_RE, serializeEnv, type EnvRow } from "../env";
import { cx } from "../lib";
import { inputClass as baseInput } from "./ui";

const inputClass = baseInput.replace("w-full", "min-w-0");

/**
 * Environment variables as a line-by-line table or one raw box. `value` holds
 * KEY=value lines; a .env file can be imported (merged into what's there).
 */
export default function EnvEditor({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const [raw, setRaw] = useState(false);
  const [shown, setShown] = useState<Set<number>>(new Set());
  const file = useRef<HTMLInputElement>(null);
  // Rows are local so blank, half-typed rows survive; only complete rows are emitted.
  const [rows, setLocal] = useState<EnvRow[]>(() => parseEnv(value));

  const setRows = (r: EnvRow[]) => {
    setLocal(r);
    onChange(serializeEnv(r).join("\n"));
  };
  const edit = (i: number, patch: Partial<EnvRow>) => setRows(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const setMode = (r: boolean) => {
    if (!r) setLocal(parseEnv(value)); // leaving the raw box: re-read what was typed
    setRaw(r);
  };
  const toggle = (i: number) =>
    setShown((s) => {
      const n = new Set(s);
      if (n.has(i)) n.delete(i);
      else n.add(i);
      return n;
    });

  const [drag, setDrag] = useState(false);
  const [note, setNote] = useState("");

  async function importFile(f: File | undefined) {
    if (!f) return;
    const cur = parseEnv(value);
    const inc = parseEnv(await f.text());
    const old = new Map(cur.map((r) => [r.key, r.value]));
    const added = inc.filter((r) => !old.has(r.key)).length;
    const changed = inc.filter((r) => old.has(r.key) && old.get(r.key) !== r.value).length;
    setRows(mergeEnv(cur, inc));
    setNote(`${f.name}: ${added} added, ${changed} changed, ${inc.length - added - changed} unchanged. Review, then save.`);
    if (file.current) file.current.value = "";
  }

  return (
    <div
      className={cx("space-y-2 rounded-lg", drag && "outline-dashed outline-2 outline-primary")}
      onDragOver={(e) => {
        e.preventDefault();
        setDrag(true);
      }}
      onDragLeave={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node)) setDrag(false);
      }}
      onDrop={(e) => {
        e.preventDefault();
        setDrag(false);
        importFile(e.dataTransfer.files?.[0]);
      }}
    >
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <div className="inline-flex rounded-lg border border-neutral-300 p-0.5">
          {[false, true].map((r) => (
            <button
              type="button"
              key={String(r)}
              onClick={() => setMode(r)}
              className={cx("rounded-md px-2.5 py-1", raw === r ? "bg-neutral-900 text-white" : "text-neutral-600 hover:bg-neutral-100")}
            >
              {r ? "Raw" : "Table"}
            </button>
          ))}
        </div>
        <button type="button" onClick={() => file.current?.click()} className="inline-flex items-center gap-1.5 rounded-lg border border-neutral-300 px-2.5 py-1.5 hover:bg-neutral-50">
          <Upload className="size-3.5" /> Import .env
        </button>
        <input ref={file} type="file" className="hidden" onChange={(e) => importFile(e.target.files?.[0])} />
        <span className="text-xs text-neutral-500">{serializeEnv(rows).length} variable{serializeEnv(rows).length === 1 ? "" : "s"}</span>
      </div>

      {note && <p className="text-xs text-neutral-600">{note}</p>}

      {raw ? (
        <textarea
          className={cx(inputClass, "min-h-48 w-full max-w-full resize whitespace-pre font-mono text-xs")}
          wrap="off"
          rows={12}
          placeholder={"KEY=value\n# paste a whole .env file here, or drop one on this box"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
        />
      ) : (
        <div className="space-y-1.5">
          {rows.map((r, i) => {
            const secret = SECRET_RE.test(r.key) && !shown.has(i);
            return (
              <div key={i} className="flex items-center gap-1.5">
                <input className={cx(inputClass, "w-2/5 font-mono")} placeholder="KEY" value={r.key} onChange={(e) => edit(i, { key: e.target.value })} />
                <input
                  className={cx(inputClass, "flex-1 font-mono")}
                  placeholder="value"
                  type={secret ? "password" : "text"}
                  value={r.value}
                  onChange={(e) => edit(i, { value: e.target.value })}
                />
                {SECRET_RE.test(r.key) && (
                  <button type="button" onClick={() => toggle(i)} className="rounded p-1.5 text-neutral-500 hover:bg-neutral-100" aria-label="Show or hide value">
                    {shown.has(i) ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
                  </button>
                )}
                <button type="button" onClick={() => setRows(rows.filter((_, j) => j !== i))} className="rounded p-1.5 text-neutral-500 hover:bg-neutral-100 hover:text-red-600" aria-label="Remove variable">
                  <Trash2 className="size-4" />
                </button>
              </div>
            );
          })}
          <button
            type="button"
            onClick={() => setLocal([...rows, { key: "", value: "" }])}
            className="inline-flex items-center gap-1.5 rounded-lg px-2 py-1.5 text-sm text-neutral-600 hover:bg-neutral-100"
          >
            <Plus className="size-4" /> Add variable
          </button>
        </div>
      )}
    </div>
  );
}
