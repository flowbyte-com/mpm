/**
 * openclaw-mpm-memory — RUNTIME INJECTION CONTRACT TEST
 *
 * Proves the documented architecture end-to-end:
 *
 *   OpenClaw turn begins
 *       ↓
 *   session_start hook fires → plugin starts async mpm call
 *       ↓
 *   agent_turn_prepare hook fires → plugin returns { prependContext: <wake> }
 *       ↓
 *   OpenClaw runtime concatenates prependContext into the agent prompt
 *       ↓
 *   LLM sees MPM wake context WITHOUT any persistent SOUL.md/AGENTS.md block.
 *
 * This is the contract the manual integration test threatened: if the
 * persistent MPM instruction block is removed from SOUL.md/AGENTS.md,
 * wake context must STILL arrive because it is delivered by the
 * plugin's agent_turn_prepare hook, not by a behavioural instruction
 * in a markdown file.
 *
 * Implementation note (this test loads the real plugin in a child
 * process so the SDK imports run normally):
 *
 *   PATH=<fake-mpm-bin>:... MPM_BIN=<fake-mpm> \
 *     node --experimental-loader tests/loader.mjs \
 *     tests/runtime_injection.test.js
 *
 * The fake-mpm binary returns canned wake content for action:
 * read_wake_context and a healthy response for action: health_check.
 *
 * Tests run with node --test. The loader.mjs file in this directory
 * stubs the three openclaw/plugin-sdk subpath imports so we don't
 * need the real SDK at runtime.
 */

import { test, describe } from "node:test";
import assert from "node:assert";
import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync, chmodSync, rmSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PLUGIN_DIR = path.join(__dirname, "..");
const LOADER = path.join(__dirname, "loader.mjs");

const FAKE_WAKE = "FAKE_WAKE_PAYLOAD_runtime_injection_test";
const MOCK_MPM_DIR = "/tmp/openclaw-mpm-fake-" + process.pid;

// --------------------------------------------------------------------------
// Fake mpm binary
// --------------------------------------------------------------------------

const FAKE_MPM_SCRIPT = `#!/usr/bin/env node
const argv = process.argv.slice(2);
const idx = argv.indexOf("--payload");
const payload = idx >= 0 ? argv[idx + 1] : "{}";
let parsed = {};
try { parsed = JSON.parse(payload); } catch {}
const action = parsed.action || "";
let body;
if (action === "read_wake_context") {
  body = JSON.stringify({
    success: true,
    content: ${JSON.stringify(FAKE_WAKE)},
    format: "system-prompt",
  });
} else if (action === "health_check") {
  body = JSON.stringify({ ok: true, memories_active: 1, theories_pending: 0, wakes_overdue: 0 });
} else {
  body = JSON.stringify({ success: false, error: "unknown action " + action });
}
process.stdout.write(body + "\\n");
process.exit(0);
`;

function setupFakeMpm() {
  mkdirSync(MOCK_MPM_DIR, { recursive: true });
  const mpmPath = path.join(MOCK_MPM_DIR, "mpm");
  writeFileSync(mpmPath, FAKE_MPM_SCRIPT, { mode: 0o755 });
  chmodSync(mpmPath, 0o755);
  return mpmPath;
}

function teardownFakeMpm() {
  try {
    rmSync(MOCK_MPM_DIR, { recursive: true, force: true });
  } catch {}
}

// --------------------------------------------------------------------------
// Driver script: loads the real plugin and exercises the lifecycle
// --------------------------------------------------------------------------

const DRIVER_SCRIPT = `
import { default as pluginEntry } from ${JSON.stringify("file://" + path.join(PLUGIN_DIR, "index.js"))};

const entry = pluginEntry;
const handlers = {};
const api = {
  logger: { info: () => {}, debug: () => {}, warn: (m) => console.log("WARN", m), error: (m) => console.log("ERR", m) },
  config: {
    plugins: { entries: { "openclaw-mpm-memory": { config: { enabled: true, mpmBin: process.env.MPM_BIN, timeoutMs: 2000, scope: "all", limitDefault: 6 } } } },
    agents: { list: [], defaults: { workspace: "/tmp" } },
  },
  session: { state: { registerSessionExtension: () => {} } },
  on(hookName, fn) { handlers[hookName] = fn; },
  registerTool() {},
  registerMemoryCapability() {},
};

await entry.register(api);
// Allow boot-time health_check to settle.
await new Promise((r) => setTimeout(r, 100));

// Realistic OpenClaw shape: sessionKey is in the ctx argument, NOT in
// the event payload for agent_turn_prepare (per PluginAgentTurnPrepareEvent
// definition in openclaw/agent-harness-runtime-*.d.ts).
const sessionKey = "test:ri:" + Date.now();
handlers.session_start(
  { sessionId: "sess-1", sessionKey },
  { sessionKey, sessionId: "sess-1", agentId: "main" }
);
await new Promise((r) => setTimeout(r, 200));

const turnResult = await handlers.agent_turn_prepare(
  { prompt: "hello", messages: [], queuedInjections: [] },
  { sessionKey, sessionId: "sess-1", agentId: "main" }
);
console.log("TURN_RESULT:" + JSON.stringify(turnResult));

const heartbeatResult = await handlers.heartbeat_prompt_contribution(
  { heartbeatName: "test" },
  { sessionKey, sessionId: "sess-1", agentId: "main" }
);
console.log("HEARTBEAT_RESULT:" + JSON.stringify(heartbeatResult));

const execEnv = handlers.resolve_exec_env({}, { sessionKey });
console.log("EXEC_ENV:" + JSON.stringify(execEnv));

await handlers.session_end(
  { sessionId: "sess-1", sessionKey },
  { sessionKey, sessionId: "sess-1", agentId: "main" }
);
console.log("DONE");
`;

function runDriver() {
  const driverPath = path.join(__dirname, ".runtime-injection-driver.mjs");
  writeFileSync(driverPath, DRIVER_SCRIPT, { mode: 0o644 });

  const env = {
    ...process.env,
    PATH: MOCK_MPM_DIR + ":" + (process.env.PATH || ""),
    MPM_BIN: path.join(MOCK_MPM_DIR, "mpm"),
    MPM_WORKSPACE: "/tmp",
  };

  return new Promise((resolve, reject) => {
    const child = spawn(
      process.execPath,
      ["--experimental-loader", LOADER, driverPath],
      { stdio: ["ignore", "pipe", "pipe"], env }
    );
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("error", reject);
    child.on("close", (code) => {
      try {
        rmSync(driverPath, { force: true });
      } catch {}
      resolve({ code, stdout, stderr });
    });
  });
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

let mpmBin;

test("setup: install fake mpm binary", () => {
  mpmBin = setupFakeMpm();
  assert.ok(mpmBin);
});

test("runtime injection: plugin loads and registers", async () => {
  const { code, stdout, stderr } = await runDriver();
  if (code !== 0) {
    console.log("STDOUT:", stdout);
    console.log("STDERR:", stderr);
  }
  assert.strictEqual(code, 0, "driver exited cleanly");
});

test("runtime injection: agent_turn_prepare returns prependContext (no SOUL.md block needed)", async () => {
  const { stdout } = await runDriver();
  const lines = stdout.split("\n").reverse();
  const resultLine = lines.find((l) => l.startsWith("TURN_RESULT:"));
  assert.ok(resultLine, "driver must emit a TURN_RESULT: line; stdout was:\n" + stdout);
  const result = JSON.parse(resultLine.slice("TURN_RESULT:".length));
  assert.ok(result && typeof result === "object", "result is an object");
  assert.ok(
    typeof result.prependContext === "string",
    "prependContext must be a string (the OpenClaw runtime contract); got: " + JSON.stringify(result)
  );
  assert.strictEqual(
    result.prependContext,
    FAKE_WAKE,
    "prependContext must contain the wake payload fetched from mpm"
  );
});

test("runtime injection: works WITHOUT any persistent SOUL.md/AGENTS.md block", async () => {
  // The contract test: same plugin, same session lifecycle, no markdown
  // file. If wake context arrives in prependContext, runtime injection
  // holds and the agent does not depend on a managed instruction block.
  const { stdout } = await runDriver();
  const lines = stdout.split("\n").reverse();
  const resultLine = lines.find((l) => l.startsWith("TURN_RESULT:"));
  const result = JSON.parse(resultLine.slice("TURN_RESULT:".length));
  assert.ok(
    result.prependContext && result.prependContext.length > 0,
    "wake context must be delivered without any persistent file"
  );
  assert.strictEqual(result.prependContext, FAKE_WAKE);
});

test("runtime injection: heartbeat_prompt_contribution also delivers wake", async () => {
  const { stdout } = await runDriver();
  const lines = stdout.split("\n").reverse();
  const resultLine = lines.find((l) => l.startsWith("HEARTBEAT_RESULT:"));
  assert.ok(resultLine);
  const result = JSON.parse(resultLine.slice("HEARTBEAT_RESULT:".length));
  assert.ok(result && typeof result.prependContext === "string");
  assert.ok(result.prependContext.includes(FAKE_WAKE));
});

test("runtime injection: resolve_exec_env attaches MPM_PROVENANCE_SESSION_KEY", async () => {
  const { stdout } = await runDriver();
  const lines = stdout.split("\n").reverse();
  const resultLine = lines.find((l) => l.startsWith("EXEC_ENV:"));
  assert.ok(resultLine);
  const env = JSON.parse(resultLine.slice("EXEC_ENV:".length));
  assert.strictEqual(env.MPM_PROVENANCE_FRAMEWORK, "openclaw");
  assert.ok(
    typeof env.MPM_PROVENANCE_SESSION_KEY === "string" &&
      env.MPM_PROVENANCE_SESSION_KEY.length > 0,
    "MPM_PROVENANCE_SESSION_KEY must be populated from ctx.sessionKey; got: " +
      JSON.stringify(env)
  );
});

test("runtime injection: session_end completes the lifecycle without errors", async () => {
  const { stdout, code } = await runDriver();
  assert.strictEqual(code, 0);
  assert.ok(stdout.includes("DONE"), "driver completed end-to-end");
});

// --------------------------------------------------------------------------
// 2026-09-08 hardening: fallback fetch when no cached promise exists
// --------------------------------------------------------------------------

const FALLBACK_DRIVER = `
import { default as pluginEntry } from ${JSON.stringify("file://" + path.join(PLUGIN_DIR, "index.js"))};

const entry = pluginEntry;
const handlers = {};
const api = {
  logger: { info: () => {}, debug: () => {}, warn: (m) => console.log("WARN", m), error: (m) => console.log("ERR", m) },
  config: {
    plugins: { entries: { "openclaw-mpm-memory": { config: { enabled: true, mpmBin: process.env.MPM_BIN, timeoutMs: 2000, scope: "all", limitDefault: 6 } } } },
    agents: { list: [], defaults: { workspace: "/tmp" } },
  },
  session: { state: { registerSessionExtension: () => {} } },
  on(hookName, fn) { handlers[hookName] = fn; },
  registerTool() {},
  registerMemoryCapability() {},
};

await entry.register(api);
await new Promise((r) => setTimeout(r, 50));

// CRITICAL: do NOT call session_start — the cache must be empty when
// agent_turn_prepare fires. This simulates the OpenClaw lifecycle race where
// agent_turn_prepare runs before session_start lands.
const sessionKey = "test:fallback:" + Date.now();
const turnResult = await handlers.agent_turn_prepare(
  { prompt: "hello", messages: [], queuedInjections: [] },
  { sessionKey, sessionId: "sess-fb", agentId: "main" }
);
console.log("TURN_RESULT:" + JSON.stringify(turnResult));

// Subsequent turn with cache populated should still emit wake (cache wins).
const turnResult2 = await handlers.agent_turn_prepare(
  { prompt: "hello2", messages: [], queuedInjections: [] },
  { sessionKey, sessionId: "sess-fb", agentId: "main" }
);
console.log("TURN_RESULT2:" + JSON.stringify(turnResult2));

console.log("DONE_FALLBACK");
`;

function runFallbackDriver() {
  const driverPath = path.join(__dirname, ".runtime-fallback-driver.mjs");
  writeFileSync(driverPath, FALLBACK_DRIVER, { mode: 0o644 });
  const env = {
    ...process.env,
    PATH: MOCK_MPM_DIR + ":" + (process.env.PATH || ""),
    MPM_BIN: path.join(MOCK_MPM_DIR, "mpm"),
    MPM_WORKSPACE: "/tmp",
  };
  return new Promise((resolve, reject) => {
    const child = spawn(
      process.execPath,
      ["--experimental-loader", LOADER, driverPath],
      { stdio: ["ignore", "pipe", "pipe"], env }
    );
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("error", reject);
    child.on("close", (code) => {
      try { rmSync(driverPath, { force: true }); } catch {}
      resolve({ code, stdout, stderr });
    });
  });
}

test("hardening: agent_turn_prepare fetches wake when no cached promise exists (race fallback)", async () => {
  const { stdout } = await runFallbackDriver();
  const lines = stdout.split("\n").reverse();
  const resultLine = lines.find((l) => l.startsWith("TURN_RESULT:"));
  assert.ok(resultLine, "driver must emit a TURN_RESULT: line; stdout was:\n" + stdout);
  const result = JSON.parse(resultLine.slice("TURN_RESULT:".length));
  assert.ok(result && typeof result.prependContext === "string",
    "prependContext must be a string even without a prior session_start; got: " + JSON.stringify(result));
  assert.strictEqual(
    result.prependContext, FAKE_WAKE,
    "fallback fetch must inject the wake payload; got: " + result.prependContext
  );
});

test("hardening: subsequent agent_turn_prepare reuses the cache populated by fallback", async () => {
  const { stdout } = await runFallbackDriver();
  const lines = stdout.split("\n").reverse();
  const resultLine = lines.find((l) => l.startsWith("TURN_RESULT2:"));
  assert.ok(resultLine, "driver must emit TURN_RESULT2:");
  const result = JSON.parse(resultLine.slice("TURN_RESULT2:".length));
  assert.strictEqual(
    result.prependContext, FAKE_WAKE,
    "cache hit on subsequent turn must still inject wake; got: " + JSON.stringify(result)
  );
});

test("hardening: source-level guard — classifyWorkspaceMemoryPaths exists", () => {
  // The 2026-09-08 OpenClaw audit found that the gateway logs
  //   "excluding automatic memory context: selected memory runtime does not
  //    support provenance classification"
  // for any plugin whose runtime does not implement classifyWorkspaceMemoryPaths.
  // This guards against accidental removal of the (empty-array) implementation
  // that lets the gateway treat the plugin as a first-class runtime.
  const src = readFileSync(path.join(PLUGIN_DIR, "index.js"), "utf8");
  assert.ok(
    /classifyWorkspaceMemoryPaths\s*\(/.test(src),
    "classifyWorkspaceMemoryPaths must be implemented on the runtime object"
  );
});

test("hardening: source-level guard — lifecycle hooks use log.info, not log.debug", () => {
  // Production log level suppresses log.debug, so hook firing was invisible.
  // This guard prevents reverting to log.debug on session_start, agent_turn_prepare,
  // or session_end.
  const src = readFileSync(path.join(PLUGIN_DIR, "index.js"), "utf8");
  // Find the session_start handler and verify it logs at info level.
  const ssMatch = src.match(/api\.on\(\s*"session_start"[\s\S]*?\n\s*\}\);/);
  assert.ok(ssMatch, "session_start handler block must exist");
  assert.ok(
    /log\.info\s*\(/.test(ssMatch[0]),
    "session_start handler must use log.info (debug is invisible in production)"
  );
  const atpMatch = src.match(/api\.on\(\s*"agent_turn_prepare"[\s\S]*?\n\s*\}\);/);
  assert.ok(atpMatch, "agent_turn_prepare handler block must exist");
  assert.ok(
    /log\.info\s*\(/.test(atpMatch[0]),
    "agent_turn_prepare handler must use log.info"
  );
});



test("regression guard: plugin must read sessionKey from ctx, not just event", () => {
  // The 2026-09-08 forensic audit found that the plugin's
  // agent_turn_prepare hook handler was registered as
  // `api.on("agent_turn_prepare", async (event) => ...)`, reading
  // `event.sessionKey`. But OpenClaw's PluginAgentTurnPrepareEvent has
  // no `sessionKey` field — that field lives on the ctx argument.
  // session_start stored under the real sessionKey, agent_turn_prepare
  // looked up under "default", cache miss → no prependContext → no
  // wake context. The fix reads from ctx first. This guard catches a
  // regression that drops the ctx argument or sessionKeyFor helper.
  const src = readFileSync(path.join(PLUGIN_DIR, "index.js"), "utf8");
  assert.ok(
    src.includes("function sessionKeyFor(event, ctx)"),
    "sessionKeyFor(event, ctx) helper must exist"
  );
  assert.ok(
    /api\.on\(\s*"session_start"\s*,\s*\(event,\s*ctx\)\s*=>/.test(src),
    "session_start handler must accept (event, ctx)"
  );
  assert.ok(
    /api\.on\(\s*"agent_turn_prepare"\s*,\s*async\s*\(event,\s*ctx\)\s*=>/.test(src),
    "agent_turn_prepare handler must accept (event, ctx) — this is the bug class"
  );
  assert.ok(
    /api\.on\(\s*"session_end"\s*,\s*\(event,\s*ctx\)\s*=>/.test(src),
    "session_end handler must accept (event, ctx)"
  );
  assert.ok(
    /api\.on\(\s*"heartbeat_prompt_contribution"\s*,\s*async\s*\(event,\s*ctx\)\s*=>/.test(src),
    "heartbeat_prompt_contribution handler must accept (event, ctx)"
  );
});

test("teardown: remove fake mpm binary", () => {
  teardownFakeMpm();
});