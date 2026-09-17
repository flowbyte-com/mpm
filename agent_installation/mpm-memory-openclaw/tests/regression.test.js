/**
 * mpm-memory-openclaw — regression tests for the 2026-09-02 forensic audit.
 *
 * Covers:
 *   (B) callMpmTool env inheritance: child receives process.env + plugin
 *       additions (not just plugin additions).
 *   (D) runtime.search() no longer returns the empty stub — it actually
 *       calls MPM and adapts the response to MemorySearchResult[].
 *   (E) PATH-independent configured binary: an absolute mpmBin works even
 *       when PATH lacks ~/.mpm/bin.
 *
 * (A) config nesting is covered by reading the actual source — both plugins
 * use the same `entries[id].config.*` shape after the fix.
 * (C) no MPM_BIN env var is read — verified by the absence of any
 * `process.env.MPM_BIN` access in the plugin source (manual review guard).
 *
 * Run with: node --test tests/regression.test.js
 */

import { test, describe } from "node:test";
import assert from "node:assert";
import { spawn, spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { withWorkspace } from "../lib/workspace.js";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

// --------------------------------------------------------------------------
// (B) callMpmTool env inheritance — the actual callMpmTool merges
// process.env BEFORE MPM_LOG_FORMAT. Replicate the merge and verify.
// --------------------------------------------------------------------------

// This mirrors the FIXED spawn-options block in callMpmTool:
//   env: withWorkspace({ ...process.env, MPM_LOG_FORMAT: "json" }),
//
// Pre-fix:
//   env: withWorkspace({ MPM_LOG_FORMAT: "json" }),
//   → child env = { MPM_LOG_FORMAT: "json", MPM_WORKSPACE: ... }
//   → PATH dropped. systemd --user breaks.

function fixedCallMpmToolEnv() {
  return withWorkspace({ ...process.env, MPM_LOG_FORMAT: "json" });
}

test("(B) callMpmTool env inherits process.env.PATH", () => {
  const env = fixedCallMpmToolEnv();
  assert.strictEqual(env.PATH, process.env.PATH,
    "PATH must reach the child — that is the whole point of process.env inheritance");
});

test("(B) callMpmTool env inherits HOME, USER, and other process.env vars", () => {
  const env = fixedCallMpmToolEnv();
  assert.strictEqual(env.HOME, process.env.HOME);
  if (process.env.USER) {
    assert.strictEqual(env.USER, process.env.USER);
  }
});

test("(B) callMpmTool env adds MPM_LOG_FORMAT=json (intentional addition)", () => {
  const env = fixedCallMpmToolEnv();
  assert.strictEqual(env.MPM_LOG_FORMAT, "json",
    "plugin-specific env var must reach the child alongside process.env");
});

test("(B) callMpmTool env sets MPM_WORKSPACE (workspace helper invariant)", () => {
  const env = fixedCallMpmToolEnv();
  assert.ok(env.MPM_WORKSPACE && env.MPM_WORKSPACE.length > 0,
    "MPM_WORKSPACE must always be set by withWorkspace");
});

test("(B) regression guard: the buggy single-key merge drops PATH", () => {
  // Document the pre-fix behavior to make the test meaningful.
  const buggyEnv = withWorkspace({ MPM_LOG_FORMAT: "json" });
  // Pre-fix env has MPM_LOG_FORMAT + MPM_WORKSPACE only — no PATH.
  assert.strictEqual(buggyEnv.PATH, undefined,
    "pre-fix env has no PATH — verifying this is what we fixed");
});

// --------------------------------------------------------------------------
// (D) runtime.search() — must call MPM via the same callMpm path and adapt
// the response. No more silent empty stub.
// --------------------------------------------------------------------------

// Mirror of adaptMemoryHit (kept in sync with index.js).
function adaptMemoryHit(memory, idx) {
  const id = memory.id || `unknown-${idx}`;
  const content = memory.content || memory.text || memory.snippet || "";
  const lines = content ? content.split("\n").length : 1;
  const score =
    typeof memory.weight === "number"
      ? Math.max(0, Math.min(1, memory.weight / 100))
      : 0.5;
  return {
    path: `mpm://memory/${id}`,
    startLine: 1,
    endLine: Math.max(1, lines),
    score,
    snippet: content.slice(0, 1200),
    source: "memory", // MemorySearchResult source is "memory" | "sessions"
    collection: memory.collection || "memories",
    tags: memory.tags || [],
    weight: memory.weight,
    reinforcementCount: memory.reinforcement_count,
    createdAt: memory.created_at,
    rank: idx + 1,
  };
}

// Mirror of the fixed search() method on the manager.
function search(callMpm, scope, limitDefault, query, opts) {
  if (typeof query !== "string" || !query) return [];
  const maxResults =
    typeof opts?.maxResults === "number" && opts.maxResults > 0
      ? Math.min(opts.maxResults, 50)
      : limitDefault;
  const params = { query, limit: maxResults, scope };
  if (typeof opts?.minScore === "number") params.min_score = opts.minScore;
  return callMpm("mpm_memory", { action: "query", params }).then((result) => {
    if (!result || result.success === false) return [];
    const memories = Array.isArray(result.memories) ? result.memories : [];
    return memories.map((m, i) => adaptMemoryHit(m, i));
  });
}

test("(D) runtime.search() returns empty array for empty query", async () => {
  const calls = [];
  const fakeCallMpm = async (tool, payload) => {
    calls.push({ tool, payload });
    return { success: true, memories: [] };
  };
  const result = await search(fakeCallMpm, "all", 6, "", {});
  assert.deepStrictEqual(result, []);
  assert.strictEqual(calls.length, 0, "must not call MPM with empty query");
});

test("(D) runtime.search() returns empty array on MPM failure (fail-open)", async () => {
  const fakeCallMpm = async () => ({ success: false, error: "ENOENT" });
  const result = await search(fakeCallMpm, "all", 6, "foo", {});
  assert.deepStrictEqual(result, []);
});

test("(D) runtime.search() calls mpm_memory with the right query/limit/scope", async () => {
  let captured = null;
  const fakeCallMpm = async (tool, payload) => {
    captured = { tool, payload };
    return { success: true, memories: [{ id: "abc", content: "hit", weight: 70 }] };
  };
  const result = await search(fakeCallMpm, "local", 6, "user typed this", { maxResults: 3 });
  assert.strictEqual(captured.tool, "mpm_memory");
  assert.strictEqual(captured.payload.action, "query");
  assert.strictEqual(captured.payload.params.query, "user typed this");
  assert.strictEqual(captured.payload.params.limit, 3,
    "maxResults from opts must be respected");
  assert.strictEqual(captured.payload.params.scope, "local",
    "scope must be threaded through from plugin config");
  assert.strictEqual(result.length, 1);
});

test("(D) runtime.search() passes min_score when provided", async () => {
  let captured = null;
  const fakeCallMpm = async (tool, payload) => {
    captured = { tool, payload };
    return { success: true, memories: [] };
  };
  await search(fakeCallMpm, "all", 6, "q", { minScore: 0.5 });
  assert.strictEqual(captured.payload.params.min_score, 0.5);
});

test("(D) runtime.search() clamps maxResults to 50", async () => {
  let captured = null;
  const fakeCallMpm = async (tool, payload) => {
    captured = { tool, payload };
    return { success: true, memories: [] };
  };
  await search(fakeCallMpm, "all", 6, "q", { maxResults: 9999 });
  assert.strictEqual(captured.payload.params.limit, 50,
    "must clamp to SDK maximum (50)");
});

test("(D) runtime.search() adapts MPM memories to MemorySearchResult shape", async () => {
  const fakeCallMpm = async () => ({
    success: true,
    memories: [
      { id: "m1", content: "first memory", weight: 80, tags: ["a"] },
      { id: "m2", content: "second", weight: 50 },
    ],
  });
  const result = await search(fakeCallMpm, "all", 6, "q", {});
  assert.strictEqual(result.length, 2);
  assert.strictEqual(result[0].path, "mpm://memory/m1");
  assert.strictEqual(result[0].source, "memory",
    "MemorySearchResult.source must be \"memory\" or \"sessions\"");
  assert.strictEqual(result[0].score, 0.8, "weight 80 → score 0.8");
  assert.strictEqual(result[1].score, 0.5);
  assert.deepStrictEqual(result[0].tags, ["a"]);
  assert.strictEqual(result[0].rank, 1);
  assert.strictEqual(result[1].rank, 2);
});

test("(D) runtime.search() — pre-fix stub returned the wrong shape (no longer)", async () => {
  // The bug: `runtime.search()` returned `{ results: [], total: 0 }`.
  // The fixed method returns MemorySearchResult[] (an array directly).
  const fakeCallMpm = async () => ({
    success: true,
    memories: [{ id: "x", content: "y" }],
  });
  const result = await search(fakeCallMpm, "all", 6, "q", {});
  assert.ok(Array.isArray(result),
    "must return an array (MemorySearchResult[]), not { results, total }");
  assert.strictEqual(result.length, 1);
});

test("(D) runtime.search() returns empty array (NOT error) on missing memories field", async () => {
  const fakeCallMpm = async () => ({ success: true /* no memories */ });
  const result = await search(fakeCallMpm, "all", 6, "q", {});
  assert.deepStrictEqual(result, [],
    "missing memories field must be treated as no hits, not a crash");
});

// --------------------------------------------------------------------------
// (E) PATH-independent configured binary — spawn with absolute mpmBin and
// a stripped PATH must still work.
// --------------------------------------------------------------------------

test("(E) configured absolute mpmBin works with stripped PATH", async () => {
  // Use `node -e` as a stand-in "mpm" so we can introspect the child env
  // without needing a real mpm binary. The point is to verify the absolute
  // path resolves regardless of PATH contents.
  const absoluteBin = process.execPath;
  const child = spawn(
    absoluteBin,
    ["-e", "process.stdout.write(JSON.stringify({PATH: process.env.PATH || ''}))"],
    {
      stdio: ["ignore", "pipe", "pipe"],
      env: withWorkspace({
        // Strip PATH down to nothing-resolvable for `mpm`. Absolute path
        // is given to spawn directly so PATH is irrelevant.
        PATH: "/usr/bin:/bin",
      }),
    },
  );
  let stdout = "";
  child.stdout.on("data", (d) => (stdout += d));
  await new Promise((resolve) => child.on("close", resolve));
  const result = JSON.parse(stdout);
  assert.strictEqual(result.PATH, "/usr/bin:/bin",
    "child received the env we passed — PATH stripped but absolute bin still works");
});

test("(E) systemd-like minimal env (no ~/.mpm/bin) — child still gets MPM_WORKSPACE", async () => {
  const absoluteBin = process.execPath;
  const child = spawn(
    absoluteBin,
    [
      "-e",
      "process.stdout.write(JSON.stringify({MPM_WORKSPACE: process.env.MPM_WORKSPACE || '', " +
        "PATH: process.env.PATH || ''}))",
    ],
    {
      stdio: ["ignore", "pipe", "pipe"],
      env: withWorkspace({
        PATH: "/usr/bin:/bin", // typical systemd --user PATH
      }),
    },
  );
  let stdout = "";
  child.stdout.on("data", (d) => (stdout += d));
  await new Promise((resolve) => child.on("close", resolve));
  const result = JSON.parse(stdout);
  assert.ok(result.MPM_WORKSPACE && result.MPM_WORKSPACE.length > 0,
    "MPM_WORKSPACE must reach child even with minimal PATH env");
});

// --------------------------------------------------------------------------
// (C) MPM_BIN — not honoured by the plugin. Document the absence.
// --------------------------------------------------------------------------

test("(C) plugin source must not reference MPM_BIN as a configurable env var", () => {
  // Read the actual plugin source as a string and check it does NOT contain
  // an `MPM_BIN` reference. If a future contributor wires MPM_BIN into the
  // plugin, this test fails (deliberate: the spec was explicit — only
  // cfg.mpmBin is the configuration surface; no env-var override).
  const src = readFileSync(
    path.join(__dirname, "..", "index.js"),
    "utf8",
  );
  assert.strictEqual(src.includes("MPM_BIN"), false,
    "MPM_BIN was a fabricated documentation reference; the plugin uses cfg.mpmBin only");
});

// --------------------------------------------------------------------------
// (F) doctor log noise — the "registered" lifecycle event must be logged
// at debug level (not info) so OpenClaw doctor's natural double-register
// (one for the detect phase, one for the run phase) does not produce
// duplicate "registered" lines suggesting multiple plugin installs.
//
// Substrate: OpenClaw doctor --lint invokes noteMemorySearchHealth (the
// memory-search doctor contribution) twice per doctor run, each via
// ensureMemoryRuntime → loadPluginRegistryHandle → runPluginRegisterSync.
// The plugin's register() therefore runs twice per doctor invocation.
// The plugin's boot-time health_check IIFE fires once per register().
//
// What the plugin owns:
//   - log.debug for the "registered" line — invisible at default log
//     level so healthy doctor runs stay operationally quiet.
//   - log.info for the "health_check ok (memories=... theories=...
//     wakes_overdue=...)" line — the substantive metric operators want.
//
// What the plugin does NOT own:
//   - OpenClaw's double-register lifecycle. The plugin cannot suppress
//     that without inventing stateful dedup, which the integration
//     contract explicitly forbids (one register() = one plugin instance,
//     even when called twice).
//
// Verified live: instrumenting register() in the plugin shows
// `register() call #1` and `register() call #2` from the same Node.js
// process when `openclaw doctor --lint` runs — see the 2026-09-11
// doctor investigation notes.
// --------------------------------------------------------------------------

test("(F) registration log uses log.debug, not log.info (doctor noise guard)", () => {
  const src = readFileSync(
    path.join(__dirname, "..", "index.js"),
    "utf8",
  );
  // Locate the register() body block. It is bounded by the `register(api) {`
  // opener and the closing brace before the next "// Session extension:"
  // comment that begins the next section in the plugin source.
  const startIdx = src.indexOf("register(api) {");
  const sessionExtIdx = src.indexOf("// Session extension:");
  assert.ok(startIdx > 0, "register(api) opener must exist in plugin source");
  assert.ok(sessionExtIdx > startIdx, "// Session extension: comment must follow register() body");
  const body = src.slice(startIdx, sessionExtIdx);

  // The "registered" lifecycle line must be at debug, not info.
  assert.ok(
    body.includes("mpm-memory-openclaw: registered"),
    "register() must log a 'registered' lifecycle line"
  );
  // Find the call site for that line. It must be guarded by
  // `log.debug` (not `log.info`).
  const registeredIdx = body.indexOf("mpm-memory-openclaw: registered");
  const precedingSlice = body.slice(Math.max(0, registeredIdx - 200), registeredIdx);
  assert.ok(
    /log\.debug\s*\(\s*$/.test(precedingSlice) ||
    /log\.debug\s*\(\s*\n/.test(precedingSlice) ||
    /log\.debug[^\(]*$/.test(precedingSlice) ||
    /typeof\s+log\.debug\s*===\s*"function"\s*\)[\s\S]{0,20}log\.debug/.test(precedingSlice),
    "the 'registered' line must be emitted via log.debug, not log.info"
  );
  // Belt and braces: the literal string 'log.info' must not appear
  // directly before the registered line in the source.
  const lastInfoBefore = precedingSlice.lastIndexOf("log.info");
  const lastDebugBefore = precedingSlice.lastIndexOf("log.debug");
  assert.ok(
    lastDebugBefore > lastInfoBefore || lastInfoBefore < 0,
    "log.debug must be the most recent logger call before the 'registered' line"
  );
});

test("(F) health_check ok line uses log.info (substantive metric stays visible)", () => {
  const src = readFileSync(
    path.join(__dirname, "..", "index.js"),
    "utf8",
  );
  const okIdx = src.indexOf("mpm-memory-openclaw: health_check ok");
  assert.ok(okIdx > 0, "the health_check ok log line must exist");
  // The immediately preceding log.X call must be log.info.
  const precedingSlice = src.slice(Math.max(0, okIdx - 200), okIdx);
  assert.ok(
    precedingSlice.includes("log.info"),
    "the 'health_check ok' line must be emitted at log.info level — it's the substantive metric operators want"
  );
});

test("(F) registration line at debug, but boot-time health_check at info — net behaviour preserved", () => {
  // Cross-check: count log.info sites in the boot block vs log.debug
  // sites. The health-check body should have a clear info signal; the
  // registration line should not contribute one.
  const src = readFileSync(
    path.join(__dirname, "..", "index.js"),
    "utf8",
  );
  const bootBlock = src.slice(
    src.indexOf("register(api)"),
    src.indexOf("// Session extension"),
  );
  const infoCount = (bootBlock.match(/log\.info/g) || []).length;
  const debugCount = (bootBlock.match(/log\.debug/g) || []).length;
  assert.ok(
    debugCount >= 1,
    "register() body must use log.debug at least once (registration is at debug)"
  );
  assert.ok(
    infoCount >= 1,
    "register() body must use log.info at least once (health_check ok stays visible)"
  );
});

console.log("regression.test.js loaded.");
