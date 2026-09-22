/**
 * mpm-pi — namespace contract test for the 2026-09-22 audit.
 *
 * Scans the plugin source for every `name: "<x>"` literal inside a
 * `pi.registerTool({...})` invocation. Every tool name MUST begin
 * with "mpm_".
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
const SRC = path.join(__dirname, "..", "index.ts");

const src = readFileSync(SRC, "utf8");

const BANNED = ["memory_search", "memory_get", "log_to_changelog", "request_review"];

test("every Pi tool name is mpm_-prefixed", () => {
  const names: string[] = [];

  // Pattern 1: direct pi.registerTool({ name: "<x>", ... }) invocations
  {
    const re = /pi\.registerTool\(\s*\{[\s\S]*?name:\s*"([^"]+)"/g;
    let match: RegExpExecArray | null;
    while ((match = re.exec(src)) !== null) {
      names.push(match[1]);
    }
  }

  // Pattern 2: registerDomainTool(pi, { name: "<x>", ... }) Fat RPC
  {
    const re = /registerDomainTool\(\s*pi\s*,\s*\{[\s\S]*?name:\s*"([^"]+)"/g;
    let match: RegExpExecArray | null;
    while ((match = re.exec(src)) !== null) {
      names.push(match[1]);
    }
  }

  assert.ok(names.length >= 17, `expected at least 17 tool registrations; found ${names.length}`);
  for (const n of names) {
    if (n === "spec.name") continue; // dynamic aggregator name
    assert.ok(
      n.startsWith("mpm_"),
      `Pi tool "${n}" must be prefixed with mpm_ to coexist with sibling plugins`,
    );
    assert.ok(
      !BANNED.includes(n),
      `Pi tool "${n}" is banned — original collision point`,
    );
  }
});

test("no banned bare names remain in index.ts", () => {
  for (const banned of BANNED) {
    const re = new RegExp(`\\b${banned}\\b`, "g");
    const matches = src.match(re);
    assert.ok(
      matches === null,
      `Pi index.ts must not reference "${banned}" (matches: ${matches?.length ?? 0})`,
    );
  }
});