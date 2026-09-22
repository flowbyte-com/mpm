/**
 * mpm-memory-openclaw — namespace contract test for the 2026-09-22 audit.
 *
 * Asserts:
 *   - Every tool name exposed via api.registerTool(...) in index.js begins with mpm_.
 *   - Every name listed in openclaw.plugin.json contracts.tools begins with mpm_.
 *   - The plugin manifest does NOT list memory_search / memory_get as
 *     contract tool names (those collide with the memory-core plugin).
 *
 * Run with: node --test tests/namespace_contract.test.js
 */

import { test, describe } from "node:test";
import assert from "node:assert";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const PLUGIN_DIR = path.join(__dirname, "..");

const pluginSrc = readFileSync(
  path.join(PLUGIN_DIR, "index.js"),
  "utf8",
);
const pluginManifest = JSON.parse(
  readFileSync(path.join(PLUGIN_DIR, "openclaw.plugin.json"), "utf8"),
);

const BANNED = ["memory_search", "memory_get", "log_to_changelog", "request_review"];

describe("namespace contract", () => {
  test("every registered tool name in index.js is mpm_-prefixed", () => {
    // Find every api.registerTool(...) invocation in the source and
    // extract the `name: "..."` field. We look for the literal block
    // pattern that the plugin uses; this is a contract test on the
    // plugin source so we don't need to load the plugin SDK.
    const re = /api\.registerTool\(\s*\(\)\s*=>\s*\(\{[\s\S]*?name:\s*"([^"]+)"/g;
    const names = [];
    let match;
    while ((match = re.exec(pluginSrc)) !== null) {
      names.push(match[1]);
    }
    assert.ok(names.length >= 2,
      `expected at least 2 api.registerTool invocations in index.js; found ${names.length}`);
    for (const n of names) {
      assert.ok(n.startsWith("mpm_"),
        `registerTool name "${n}" must be prefixed with mpm_ to coexist with sibling plugins`);
      assert.ok(!BANNED.includes(n),
        `registerTool name "${n}" is banned — it is the original collision point with memory-core`);
    }
  });

  test("every name in contracts.tools is mpm_-prefixed", () => {
    const tools = pluginManifest.contracts && pluginManifest.contracts.tools;
    if (!Array.isArray(tools) || tools.length === 0) {
      // The plugin is allowed to omit contracts.tools (no globally-
      // exposed tools); the test still pins the prefix rule for when
      // the field is present.
      return;
    }
    for (const n of tools) {
      assert.ok(n.startsWith("mpm_"),
        `openclaw.plugin.json contracts.tools entry "${n}" must be prefixed with mpm_`);
      assert.ok(!BANNED.includes(n),
        `openclaw.plugin.json contracts.tools entry "${n}" is banned`);
    }
  });

  test("plugin manifest does not declare the original collision names", () => {
    const tools = (pluginManifest.contracts && pluginManifest.contracts.tools) || [];
    for (const banned of BANNED) {
      assert.ok(!tools.includes(banned),
        `openclaw.plugin.json contracts.tools must NOT list "${banned}" — ` +
        "that is the original memory-core collision point");
    }
  });
});

console.log("namespace_contract.test.js loaded.");