export type EnvRow = { key: string; value: string; /** Comment and blank lines that sat directly above this variable. */ before?: string[] };

/** Parses one .env line; null for blank and comment lines. */
function parseLine(raw: string): EnvRow | null {
  let line = raw.trim();
  if (!line || line.startsWith("#")) return null;
  if (line.startsWith("export ")) line = line.slice(7).trim();
  const eq = line.indexOf("=");
  if (eq < 0) return line ? { key: line, value: "" } : null;
  const key = line.slice(0, eq).trim();
  let value = line.slice(eq + 1).trim();
  const q = value[0];
  if ((q === '"' || q === "'") && value.length > 1) {
    const end = value.indexOf(q, 1);
    if (end > 0) {
      value = value.slice(1, end);
      if (q === '"') value = value.replace(/\\n/g, "\n").replace(/\\"/g, '"');
    }
  } else {
    const hash = value.search(/\s#/);
    if (hash >= 0) value = value.slice(0, hash).trimEnd();
  }
  return key ? { key, value } : null;
}

/** Parses .env-style text into variables: comments, blank lines, `export KEY=v` and quoted values. */
export function parseEnv(text: string): EnvRow[] {
  const rows: EnvRow[] = [];
  for (const raw of text.split(/\r?\n/)) {
    const r = parseLine(raw);
    if (r) rows.push(r);
  }
  return rows;
}

export type EnvDoc = { rows: EnvRow[]; tail: string[] };

/** Like parseEnv, but keeps comments and blank lines: each rides along with the variable below it. */
export function parseEnvDoc(text: string): EnvDoc {
  const rows: EnvRow[] = [];
  let pending: string[] = [];
  for (const raw of text.split(/\r?\n/)) {
    const r = parseLine(raw);
    if (r) {
      rows.push(pending.length ? { ...r, before: pending } : r);
      pending = [];
    } else pending.push(raw.trim() === "" ? "" : raw.trimEnd());
  }
  while (pending.length && pending[pending.length - 1] === "") pending.pop();
  return { rows, tail: pending };
}

/** The lines to store for a document, comments and blank lines included. */
export function serializeDoc(doc: EnvDoc): string[] {
  const out: string[] = [];
  let carry: string[] = [];
  for (const r of doc.rows) {
    carry = carry.concat(r.before ?? []);
    if (!r.key.trim()) continue;
    out.push(...carry, `${r.key.trim()}=${r.value}`);
    carry = [];
  }
  return out.concat(carry, doc.tail);
}

/** Normalises raw text to storable lines: assignments tidied to KEY=value, comments and blank lines kept. */
export function normalizeEnv(text: string): string[] {
  const out: string[] = [];
  for (const raw of text.split(/\r?\n/)) {
    const t = raw.trim();
    const r = parseLine(raw);
    if (r) out.push(`${r.key}=${r.value}`);
    else if (t.startsWith("#")) out.push(raw.trimEnd());
    else if (!t) out.push("");
  }
  while (out.length && out[0] === "") out.shift();
  while (out.length && out[out.length - 1] === "") out.pop();
  return out;
}

/** Serialises rows to the KEY=value lines the API stores (no quoting). */
export function serializeEnv(rows: EnvRow[]): string[] {
  return rows.filter((r) => r.key.trim()).map((r) => `${r.key.trim()}=${r.value}`);
}

/** Merges imported rows into existing ones; imported keys win. */
export function mergeEnv(existing: EnvRow[], incoming: EnvRow[]): EnvRow[] {
  const out = existing.map((r) => ({ ...r }));
  for (const r of incoming) {
    const i = out.findIndex((o) => o.key === r.key);
    if (i >= 0) out[i].value = r.value;
    else out.push({ ...r });
  }
  return out;
}

export const SECRET_RE = /(pass|secret|token|key|credential|private)/i;
