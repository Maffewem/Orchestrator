import test from "node:test";
import assert from "node:assert/strict";
import { mergeEnv, parseEnv, serializeEnv } from "./env.ts";

test("parses comments, export and quotes", () => {
  const rows = parseEnv('# c\nexport A=1\nB="two words"\nC=\'x\'\nD=v # note\n\nE\n');
  assert.deepEqual(rows, [
    { key: "A", value: "1" },
    { key: "B", value: "two words" },
    { key: "C", value: "x" },
    { key: "D", value: "v" },
    { key: "E", value: "" },
  ]);
});

test("keeps = inside values and round-trips", () => {
  const rows = parseEnv("URL=postgres://u:p@h/db?a=b");
  assert.equal(rows[0].value, "postgres://u:p@h/db?a=b");
  assert.deepEqual(serializeEnv(rows), ["URL=postgres://u:p@h/db?a=b"]);
});

test("merge overrides and appends", () => {
  const m = mergeEnv([{ key: "A", value: "1" }], [{ key: "A", value: "2" }, { key: "B", value: "3" }]);
  assert.deepEqual(m, [{ key: "A", value: "2" }, { key: "B", value: "3" }]);
});

test("comment-only lines never become rows", () => {
  const rows = parseEnv("# Phone push alerts (treat it like a password)\nNTFY_TOPIC=\n\n# Or Claude instead:\nANTHROPIC_API_KEY=");
  assert.deepEqual(serializeEnv(rows), ["NTFY_TOPIC=", "ANTHROPIC_API_KEY="]);
});
