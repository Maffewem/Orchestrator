export type EnvRow = { key: string; value: string };

/** Parses .env-style text: comments, blank lines, `export KEY=v` and quoted values. */
export function parseEnv(text: string): EnvRow[] {
  const rows: EnvRow[] = [];
  for (const raw of text.split(/\r?\n/)) {
    let line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    if (line.startsWith("export ")) line = line.slice(7).trim();
    const eq = line.indexOf("=");
    if (eq < 0) {
      rows.push({ key: line, value: "" });
      continue;
    }
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
    if (key) rows.push({ key, value });
  }
  return rows;
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
