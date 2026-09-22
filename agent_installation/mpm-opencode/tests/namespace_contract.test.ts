/**
 * mpm-opencode — namespace contract test for the 2026-09-22 audit.
 *
 * Asserts every `tools.<name> = tool(...)` and every `name: "<x>"` inside
 * a `pi.registerTool({...})` (wait — this is opencode, but the pattern is
 * the same: scan the source for tool-registration identifiers) starts with
 * "mpm_".
 *
 * Run with: npx tsx tests/namespace_contract.test.ts
 */

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const SRC = path.join(__dirname, "..", "src", "index.ts");

const src = readFileSync(SRC, "utf8");

const BANNED = ["memory_search", "memory_get", "log_to_changelog", "request_review"];

test("every registered OpenCode tool name is mpm_-prefixed", () => {
  const names: string[] = [];

  // Pattern 1: tools.<name> = tool({ ... })  (standalone tools)
  {
    const re = /tools\.([a-z_][a-z_0-9]*)\s*=\s*tool\(\{/g;
    let match: RegExpExecArray | null;
    while ((match = re.exec(src)) !== null) {
      names.push(match[1]);
    }
  }

  // Pattern 2: registerDomainTool(bin, tools, { name: "<x>", ... })  (Fat RPC)
  {
    const re = /registerDomainTool\([\s\S]*?name:\s*"([^"]+)"/g;
    let match: RegExpExecArray | null;
    while ((match = re.exec(src)) !== null) {
      names.push(match[1]);
    }
  }

  assert.ok(names.length >= 17, `expected at least 17 tool registrations; found ${names.length}`);
  for (const n of names) {
    assert.ok(
      n.startsWith("mpm_"),
      `OpenCode tool "${n}" must be prefixed with mpm_ to coexist with sibling plugins`,
    );
    assert.ok(
      !BANNED.includes(n),
      `OpenCode tool "${n}" is banned — it is the original collision point`,
    );
  }
});

test("no banned bare names remain in src/index.ts", () => {
  for (const banned of BANNED) {
    const re = new RegExp(`\\b${banned}\\b`, "g");
    const matches = src.match(re);
    assert.ok(
      matches === null,
      `OpenCode src/index.ts must not reference "${banned}" (matches: ${matches?.length ?? 0}). ` +
      `If this is in a comment, update the comment; if in a tool name, rename.`,
    );
  }
});