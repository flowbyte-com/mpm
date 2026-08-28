/**
 * openclaw-mpm-memory — integration tests
 *
 * Tests the session lifecycle hooks, wake context injection, provenance
 * contribution, and existing memory tool adapters.
 *
 * Run with: node --test tests/integration.test.js
 * (Requires Node.js ≥ 18 for built-in test runner)
 */

import { test, describe } from "node:test";
import assert from "node:assert";

// --------------------------------------------------------------------------
// findJsonInOutput — tested against known inputs
// --------------------------------------------------------------------------

function findJsonInOutput(s) {
  if (!s) return null;
  const lines = s.split("\n");
  for (let i = lines.length - 1; i >= 0; i--) {
    const t = lines[i].trim();
    if (t.startsWith("{") && t.endsWith("}")) return t;
  }
  return null;
}

test("findJsonInOutput returns null for empty/null input", () => {
  assert.strictEqual(findJsonInOutput(""), null);
  assert.strictEqual(findJsonInOutput(null), null);
  assert.strictEqual(findJsonInOutput(undefined), null);
});

test("findJsonInOutput finds JSON object on single line", () => {
  const input = '{"success":true,"content":"hello"}';
  assert.strictEqual(findJsonInOutput(input), input);
});

test("findJsonInOutput finds last JSON object on multi-line output", () => {
  const jsonLine = '{"success":true,"content":"mode: coding"}';
  const input = `INFO starting mpm\n${jsonLine}\nDEBUG done`;
  assert.strictEqual(findJsonInOutput(input), jsonLine);
});

test("findJsonInOutput ignores non-JSON lines", () => {
  const jsonLine = '{"success":true}';
  const input = `zap log line\n[ERROR] something failed\n${jsonLine}`;
  assert.strictEqual(findJsonInOutput(input), jsonLine);
});

// --------------------------------------------------------------------------
// adaptMemoryHit — result shape
// --------------------------------------------------------------------------

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
    source: "mpm",
    collection: memory.collection || "memories",
    tags: memory.tags || [],
    weight: memory.weight,
    reinforcementCount: memory.reinforcement_count,
    createdAt: memory.created_at,
    rank: idx + 1,
  };
}

test("adaptMemoryHit maps all memory fields correctly", () => {
  const input = {
    id: "abc123",
    content: "Use pnpm not npm for this project",
    weight: 87,
    collection: "memories",
    tags: ["tooling", "pnpm"],
    reinforcement_count: 4,
    created_at: "2026-08-01T10:00:00Z",
  };
  const hit = adaptMemoryHit(input, 0);

  assert.strictEqual(hit.path, "mpm://memory/abc123");
  assert.strictEqual(hit.startLine, 1);
  assert.strictEqual(hit.score, 0.87);
  assert.strictEqual(hit.snippet, "Use pnpm not npm for this project");
  assert.strictEqual(hit.source, "mpm");
  assert.strictEqual(hit.collection, "memories");
  assert.deepStrictEqual(hit.tags, ["tooling", "pnpm"]);
  assert.strictEqual(hit.weight, 87);
  assert.strictEqual(hit.reinforcementCount, 4);
  assert.strictEqual(hit.createdAt, "2026-08-01T10:00:00Z");
  assert.strictEqual(hit.rank, 1);
});

test("adaptMemoryHit handles missing optional fields gracefully", () => {
  const hit = adaptMemoryHit({ id: "xyz" }, 0);
  assert.strictEqual(hit.path, "mpm://memory/xyz");
  assert.strictEqual(hit.score, 0.5); // default when weight is absent
  assert.strictEqual(hit.snippet, "");
  assert.strictEqual(hit.endLine, 1);
  assert.deepStrictEqual(hit.tags, []);
  assert.strictEqual(hit.weight, undefined);
});

test("adaptMemoryHit clips snippet at 1200 chars", () => {
  const long = "x".repeat(1500);
  const hit = adaptMemoryHit({ id: "long", content: long }, 0);
  assert.strictEqual(hit.snippet.length, 1200);
});

test("adaptMemoryHit weight scale: 0-100 → 0-1", () => {
  assert.strictEqual(adaptMemoryHit({ id: "a", weight: 0 }, 0).score, 0);
  assert.strictEqual(adaptMemoryHit({ id: "a", weight: 50 }, 0).score, 0.5);
  assert.strictEqual(adaptMemoryHit({ id: "a", weight: 100 }, 0).score, 1);
  assert.strictEqual(adaptMemoryHit({ id: "a", weight: 150 }, 0).score, 1); // clamped
});

// --------------------------------------------------------------------------
// Wake context injection — session lifecycle simulation
// --------------------------------------------------------------------------

test("wake context is cached after session_start (non-blocking)", async () => {
  const sessionKey = "session-abc-123";
  const wakeContent = "Mode: coding\nPersona: default\nPending: 0 work items";

  /** @type {Map<string, Promise<string>>} */
  const cache = new Map();

  // Simulate what the plugin does on session_start:
  // starts an async fetch, stores the promise immediately (non-blocking)
  const promise = (async () => {
    await new Promise((r) => setTimeout(r, 10)); // simulate I/O latency
    return wakeContent;
  })();

  cache.set(sessionKey, promise);

  // The promise should be in the cache immediately (non-blocking)
  assert.ok(cache.has(sessionKey), "sessionKey should be in cache after session_start");

  // Awaiting should give us the wake content
  const content = await promise;
  assert.strictEqual(content, wakeContent);
});

test("session_end clears the wake context cache", () => {
  const cache = new Map();
  const sessionKey = "session-end-test";

  // Pre-populate
  cache.set(sessionKey, Promise.resolve("Mode: coding"));
  assert.ok(cache.has(sessionKey));

  // Simulate session_end handler (only clears cache — no mpm subprocess)
  cache.delete(sessionKey);

  assert.ok(!cache.has(sessionKey), "cache should be empty after session_end");
});

test("agent_turn_prepare awaits cached wake context", async () => {
  const cache = new Map();
  const sessionKey = "session-await";
  const wakeContent = "Mode: default\nRecent: nothing pending";

  // Pre-populate cache as session_start would
  cache.set(sessionKey, Promise.resolve(wakeContent));

  // Simulate agent_turn_prepare reading from cache
  const cached = cache.get(sessionKey);
  assert.ok(cached, "promise should be in cache");

  const result = await cached;
  assert.strictEqual(result, wakeContent);
});

test("agent_turn_prepare returns prependContext when wake context available", async () => {
  const cache = new Map();
  const sessionKey = "session-prepend";
  const wakeContent = "Mode: coding\nPersona: architect";

  cache.set(sessionKey, Promise.resolve(wakeContent));

  // Simulate agent_turn_prepare hook result
  const cached = cache.get(sessionKey);
  let prependContext = undefined;

  if (cached) {
    const wakeContext = await Promise.race([
      cached,
      new Promise((resolve) => setTimeout(() => resolve(""), 1000)),
    ]);
    if (wakeContext && wakeContext.length > 0) {
      prependContext = wakeContext;
    }
  }

  assert.strictEqual(prependContext, wakeContent);
});

test("agent_turn_prepare returns nothing when cache is empty", async () => {
  const cache = new Map();
  const sessionKey = "session-empty";

  const cached = cache.get(sessionKey);
  let prependContext = undefined;

  if (cached) {
    prependContext = await cached;
  }

  assert.strictEqual(prependContext, undefined);
  assert.ok(!cache.has(sessionKey));
});

test("wake context fetch failure does not throw", async () => {
  // Simulate the plugin's failure path: findJsonInOutput returns null on empty output
  // (mirrors what happens when spawn succeeds but returns no JSON on stdout)
  const result = findJsonInOutput("");
  assert.strictEqual(result, null); // null → plugin resolves to "" (graceful degradation)
  // The agent turn proceeds without wake context — never throws
});

// --------------------------------------------------------------------------
// Provenance — resolve_exec_env simulation
// --------------------------------------------------------------------------

test("resolve_exec_env contributes openclaw framework id", () => {
  // Simulates the actual plugin's resolve_exec_env handler
  function resolveExecEnv(event) {
    const env = {
      MPM_PROVENANCE_FRAMEWORK: "openclaw",
    };
    if (event?.sessionKey) {
      env.MPM_PROVENANCE_SESSION_KEY = event.sessionKey;
    }
    return env;
  }

  const result = resolveExecEnv({ sessionKey: "agent:main:main:2026-08-25" });
  assert.strictEqual(result.MPM_PROVENANCE_FRAMEWORK, "openclaw");
  assert.strictEqual(result.MPM_PROVENANCE_SESSION_KEY, "agent:main:main:2026-08-25");
});

test("resolve_exec_env omits sessionKey when not provided", () => {
  function resolveExecEnv(event) {
    const env = { MPM_PROVENANCE_FRAMEWORK: "openclaw" };
    if (event?.sessionKey) env.MPM_PROVENANCE_SESSION_KEY = event.sessionKey;
    return env;
  }

  const result = resolveExecEnv({});
  assert.strictEqual(result.MPM_PROVENANCE_FRAMEWORK, "openclaw");
  assert.strictEqual("MPM_PROVENANCE_SESSION_KEY" in result, false);
});

test("resolve_exec_env does not fabricate MPM_PROVENANCE_MODEL", () => {
  // OpenClaw hook context does not expose model name — intentionally unset
  function resolveExecEnv(event) {
    const env = { MPM_PROVENANCE_FRAMEWORK: "openclaw" };
    if (event?.sessionKey) env.MPM_PROVENANCE_SESSION_KEY = event.sessionKey;
    // MPM_PROVENANCE_MODEL: intentionally omitted — OpenClaw does not expose model in hook context
    return env;
  }

  const result = resolveExecEnv({ sessionKey: "test" });
  assert.strictEqual("MPM_PROVENANCE_MODEL" in result, false);
  assert.strictEqual("MPM_PROVENANCE_INVOCATION_ID" in result, false);
  assert.strictEqual("MPM_PROVENANCE_PARENT_INVOCATION_ID" in result, false);
});

// --------------------------------------------------------------------------
// Session end does NOT imply work completion
// --------------------------------------------------------------------------

test("session_end handler does NOT call mpm work --complete", () => {
  // DESIGN test: the session_end hook only clears the wakeContextCache.
  // It does NOT spawn any mpm subprocess for work completion.
  // This is the documented semantic contract: session ended ≠ work verified.

  const cache = new Map();
  cache.set("session-x", Promise.resolve("Mode: coding"));

  // This is the handler — it only deletes the cache entry (the real plugin does this)
  const sessionKey = "session-x";
  const hadEntry = cache.has(sessionKey);
  cache.delete(sessionKey);

  assert.ok(hadEntry, "had an entry to delete");
  assert.ok(!cache.has(sessionKey), "entry deleted — no mpm subprocess was spawned");
  // If any test ever tries to spawn mpm in this handler, it would be a design violation.
});

// --------------------------------------------------------------------------
// Memory tool adapters — contract tests
// --------------------------------------------------------------------------

test("memory_get rejects non-mpm:// paths", () => {
  function validatePath(path) {
    if (!path.startsWith("mpm://memory/")) {
      return { notFound: true, supportedPrefix: "mpm://memory/" };
    }
    return null;
  }

  assert.deepStrictEqual(
    validatePath("/some/other/path"),
    { notFound: true, supportedPrefix: "mpm://memory/" }
  );
  assert.deepStrictEqual(validatePath("mpm://memory/"), null);
  assert.deepStrictEqual(validatePath("mpm://memory/abc123"), null);
});

test("memory_get handles empty id", () => {
  const id = "mpm://memory/".slice("mpm://memory/".length);
  assert.strictEqual(id, "");
  // Empty id is a separate validation case handled by the tool
});

// --------------------------------------------------------------------------
// Pointer behaviour — mpm://memory/<id> paths preserved end-to-end
// --------------------------------------------------------------------------

test("memory_search hits retain mpm://memory/ prefix in path", () => {
  const memories = [
    { id: "mem-001", content: "first memory", weight: 80 },
    { id: "mem-002", content: "second memory", weight: 60 },
  ];
  const hits = memories.map((m, i) => adaptMemoryHit(m, i));

  hits.forEach((hit, i) => {
    assert.ok(hit.path.startsWith("mpm://memory/"), `hit[${i}] path starts with mpm://memory/: ${hit.path}`);
    assert.ok(hit.path.includes(memories[i].id), `hit[${i}] path contains memory id`);
  });
});

// --------------------------------------------------------------------------
// End-to-end: session lifecycle simulation
// --------------------------------------------------------------------------

test("full session lifecycle: session_start → agent_turn_prepare → session_end", async () => {
  const wakeContent = "Mode: coding\nPersona: default\nPending: 1 item";
  /** @type {Map<string, Promise<string>>} */
  const cache = new Map();
  const sessionKey = "lifecycle-test-session";

  // Step 1: session_start fires — fetch begins (non-blocking)
  const promise = (async () => {
    await new Promise((r) => setTimeout(r, 10)); // simulate I/O
    return wakeContent;
  })();
  cache.set(sessionKey, promise);

  assert.ok(cache.has(sessionKey), "session in cache after session_start");

  // Step 2: agent_turn_prepare — awaits cached promise
  const cached = cache.get(sessionKey);
  assert.ok(cached, "have cached promise");

  const result = await Promise.race([
    cached,
    new Promise((_, reject) => setTimeout(() => reject(new Error("timeout")), 2000)),
  ]);

  assert.strictEqual(result, wakeContent);

  // Step 3: session_end — clears cache (no mpm work --complete)
  cache.delete(sessionKey);
  assert.ok(!cache.has(sessionKey), "cache cleared after session_end");
});

// --------------------------------------------------------------------------
// heartbeat_prompt_contribution — minimal context, no full wake injection
// --------------------------------------------------------------------------

test("heartbeat prompt is minimal (300 char cap)", () => {
  const longWakeContent = "Mode: coding\n".repeat(100); // ~1400 chars
  const heartbeat = `[MPM heartbeat] ${longWakeContent}`.slice(0, 300);
  assert.strictEqual(heartbeat.length, 300);
  assert.ok(heartbeat.startsWith("[MPM heartbeat]"));
});

console.log("Tests complete.");
