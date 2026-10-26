// tests/test_wake_context.test.ts — regression tests for the wake-context
// extraction defect (2026-10-26).
//
// THE DEFECT
// ----------
// `mpm_context / read_wake_context` with `format: "system-prompt"` returns
//
//     {"content": "<prose>", "format": "system-prompt", "success": true}
//
// at the top level of the `mpm call` envelope. `callMpm` stores that whole
// envelope in `CallResult.payload`, and the adapter used to read
//
//     payload?.context ?? payload?.text ?? ""
//
// Every real response therefore extracted "" — the hook's `if (context)`
// guard skipped the push, the wake context never reached the agent's system
// prompt, and the per-session cache never populated (so each `context` hook
// re-spawned `mpm` for a context it discarded).
//
// These tests pin the fix and the behaviour it must preserve:
//   1. the real `content` response shape
//   2. the previously-supported string shapes (`context`, `text`)
//   3. empty / malformed responses (never coerced, never "[object Object]")
//   4. per-session caching after a successful extraction
//   5. failed retrieval does not poison the session — retry still injects
//   6. duplicate injection prevention (sequential AND concurrent)
//   7. a non-empty context actually reaches the hook's `event.system`
//
// A final wiring test guards `src/index.v2.ts` itself, because the plugin
// entry imports `@opencode/plugin` (only resolvable inside a running host)
// and therefore cannot be imported by a unit test.
//
// @ts-nocheck — see the rationale on test_resolve_mpm_binary.test.ts: the
// canonical Makefile gate compiles this file via `tsc -p
// tsconfig.tests.json` (strict:false); production code under src/ remains
// under the strict tsconfig.json that anchors `npm run build`.

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const ADAPTER_DIR = path.resolve(__dirname, "..");
const DIST_MODULE = path.join(ADAPTER_DIR, "dist", "wake-context.js");
const SRC_ENTRY = path.join(ADAPTER_DIR, "src", "index.v2.ts");
const SRC_MODULE = path.join(ADAPTER_DIR, "src", "wake-context.ts");

// The exact envelope shape measured from
//   mpm call mpm_context --payload \
//     '{"action":"read_wake_context","params":{"format":"system-prompt"}}'
// (dev build 0.1.0-dev+g70deac2b and installed build 0.1.0-dev+g3df72315
// both return this shape; `payload` is absent entirely).
const REAL_SYSTEM_PROMPT_ENVELOPE = {
  content: [
    "**Modes:** <none> [empty]",
    "**Persona:** <none> [empty]",
    "**Recent Memories:**",
    "  - On every session start, call read_wake_context before responding.",
    "",
    "**Next Move:** review the open work items.",
  ].join("\n"),
  format: "system-prompt",
  success: true,
};

let extractWakeContextText: (payload: unknown) => string;
let createWakeContextInjector: (deps: any) => any;

before(async () => {
  // The compiled module must exist before import. The Makefile gate compiles
  // tests, not src, so build dist here — same pattern as
  // test_resolve_mpm_binary.test.ts.
  const bin = path.join(ADAPTER_DIR, "node_modules", ".bin", "tsc");
  const r = existsSync(bin)
    ? spawnSync(bin, [], { cwd: ADAPTER_DIR, encoding: "utf8" })
    : spawnSync("npx", ["tsc"], { cwd: ADAPTER_DIR, encoding: "utf8" });
  assert.equal(
    r.status,
    0,
    `tsc build failed (status=${r.status})\nstdout=${r.stdout || ""}\nstderr=${r.stderr || ""}`,
  );
  assert.ok(existsSync(DIST_MODULE), `missing compiled module: ${DIST_MODULE}`);

  const mod = await import(pathToFileURL(DIST_MODULE).href);
  extractWakeContextText = mod.extractWakeContextText;
  createWakeContextInjector = mod.createWakeContextInjector;
  assert.equal(typeof extractWakeContextText, "function");
  assert.equal(typeof createWakeContextInjector, "function");
});

/** A counting fetch stub; `responses` are returned in order, last repeats. */
function fetchStub(responses: Array<string | Error | (() => Promise<string>)>) {
  let calls = 0;
  const fn = async () => {
    const r = responses[Math.min(calls, responses.length - 1)];
    calls++;
    if (r instanceof Error) throw r;
    if (typeof r === "function") return r();
    return r;
  };
  return {
    fetch: fn,
    get calls() {
      return calls;
    },
  };
}

// ---------------------------------------------------------------------------
// 1. The real `payload.content` response shape
// ---------------------------------------------------------------------------
test("1a: extracts the real system-prompt envelope from its `content` key", () => {
  const text = extractWakeContextText(REAL_SYSTEM_PROMPT_ENVELOPE);
  assert.equal(text, REAL_SYSTEM_PROMPT_ENVELOPE.content);
});

test("1b: the extracted text is non-empty and preserves multiline formatting verbatim", () => {
  const text = extractWakeContextText(REAL_SYSTEM_PROMPT_ENVELOPE);
  assert.ok(text.length > 0);
  assert.ok(text.includes("\n"), "newlines must survive");
  assert.equal(text, text.trim(), "no leading/trailing whitespace introduced");
  // The defect's observable symptom: this used to be "".
  assert.notEqual(text, "");
});

test("1c: an envelope without a `payload` wrapper is handled (the real wire shape)", () => {
  // Regression guard: the pre-fix code assumed a nested `payload` object.
  assert.equal("payload" in REAL_SYSTEM_PROMPT_ENVELOPE, false);
  assert.equal(extractWakeContextText(REAL_SYSTEM_PROMPT_ENVELOPE),
    REAL_SYSTEM_PROMPT_ENVELOPE.content);
});

// ---------------------------------------------------------------------------
// 2. Existing supported response shapes
// ---------------------------------------------------------------------------
test("2a: legacy `context` string still extracts", () => {
  assert.equal(extractWakeContextText({ context: "legacy ctx", success: true }), "legacy ctx");
});

test("2b: legacy `text` string still extracts", () => {
  assert.equal(extractWakeContextText({ text: "legacy text", success: true }), "legacy text");
});

test("2c: `content` wins when several string shapes are present", () => {
  assert.equal(
    extractWakeContextText({ content: "canonical", context: "legacy", text: "older" }),
    "canonical",
  );
});

test("2d: `context` wins over `text` when `content` is absent", () => {
  assert.equal(extractWakeContextText({ context: "ctx", text: "txt" }), "ctx");
});

test("2e: a whitespace-only higher-priority key does not shadow a usable lower one", () => {
  assert.equal(extractWakeContextText({ content: "   ", context: "real" }), "real");
});

// ---------------------------------------------------------------------------
// 3. Empty or malformed responses
// ---------------------------------------------------------------------------
test("3a: the compact projection's `context` DICT is rejected, not stringified", () => {
  // Measured shape of {projection: "compact"}:
  //   {"context": {…}, "projection": "compact", "success": true}
  const compact = {
    context: { mpm_session_id: "", open_work_ids: [], recent_artifact_ids: ["a"] },
    projection: "compact",
    success: true,
  };
  const text = extractWakeContextText(compact);
  assert.equal(text, "");
  assert.notEqual(text, "[object Object]");
});

test("3b: null / undefined / non-object envelopes yield empty", () => {
  for (const v of [null, undefined, 42, true, "a string", [], ["content"]]) {
    assert.equal(extractWakeContextText(v), "", `input ${JSON.stringify(v)}`);
  }
});

test("3c: empty and whitespace-only bodies yield empty", () => {
  for (const body of ["", "   ", "\n\t "]) {
    assert.equal(extractWakeContextText({ content: body }), "", `body ${JSON.stringify(body)}`);
  }
});

test("3d: non-string bodies are rejected for every candidate key", () => {
  assert.equal(extractWakeContextText({ content: 123 }), "");
  assert.equal(extractWakeContextText({ content: { nested: true } }), "");
  assert.equal(extractWakeContextText({ content: ["a"] }), "");
  assert.equal(extractWakeContextText({ context: { a: 1 } }), "");
  assert.equal(extractWakeContextText({ text: null }), "");
});

test("3e: an empty object and an unrelated envelope yield empty", () => {
  assert.equal(extractWakeContextText({}), "");
  assert.equal(extractWakeContextText({ success: true, format: "system-prompt" }), "");
});

// ---------------------------------------------------------------------------
// 4. Per-session caching after a successful extraction
// ---------------------------------------------------------------------------
test("4a: one successful injection caches, and the second call does not fetch", async () => {
  const stub = fetchStub(["CACHED CONTEXT"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });
  const system: any[] = [];

  assert.equal(await inj.inject("ses_a", system), true);
  assert.equal(stub.calls, 1);
  assert.equal(system.length, 1);
  assert.equal(inj.cache.get("ses_a"), "CACHED CONTEXT");

  assert.equal(await inj.inject("ses_a", system), false);
  assert.equal(stub.calls, 1, "second call must not spawn mpm again");
  assert.equal(system.length, 1, "second call must not push again");
});

test("4b: the cache is per session, not global", async () => {
  const stub = fetchStub(["CTX"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });
  const a: any[] = [];
  const b: any[] = [];

  assert.equal(await inj.inject("ses_a", a), true);
  assert.equal(await inj.inject("ses_b", b), true);
  assert.equal(stub.calls, 2, "each session fetches once");
  assert.equal(a.length, 1);
  assert.equal(b.length, 1);
});

test("4c: clear() drops the cache so the next session cycle re-injects", async () => {
  const stub = fetchStub(["CTX"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });
  const system: any[] = [];

  assert.equal(await inj.inject("ses_a", system), true);
  inj.clear();
  assert.equal(inj.cache.size, 0);
  assert.equal(await inj.inject("ses_a", system), true);
  assert.equal(stub.calls, 2);
});

test("4d: a caller-supplied cache map is the one used (injectable state)", async () => {
  const cache = new Map<string, string>();
  const inj = createWakeContextInjector({ fetch: fetchStub(["CTX"]).fetch, cache });
  await inj.inject("ses_x", []);
  assert.equal(cache.get("ses_x"), "CTX");
});

// ---------------------------------------------------------------------------
// 5. Failed retrieval and retry behaviour
// ---------------------------------------------------------------------------
test("5a: an empty fetch is not cached, and a later attempt injects", async () => {
  const stub = fetchStub(["", "SECOND TRY"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });
  const system: any[] = [];

  assert.equal(await inj.inject("ses_r", system), false);
  assert.equal(system.length, 0);
  assert.equal(inj.cache.has("ses_r"), false, "a failed fetch must not poison the session");

  assert.equal(await inj.inject("ses_r", system), true);
  assert.equal(stub.calls, 2);
  assert.equal(system.length, 1);
  assert.equal(system[0].text, "SECOND TRY");
});

test("5b: a throwing fetch is not cached, and a later attempt injects", async () => {
  const stub = fetchStub([new Error("mpm spawn failed"), "RECOVERED"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });
  const system: any[] = [];

  assert.equal(await inj.inject("ses_t", system), false);
  assert.equal(inj.cache.has("ses_t"), false);
  assert.equal(await inj.inject("ses_t", system), true);
  assert.equal(system[0].text, "RECOVERED");
});

test("5c: a missing `system` array does not cache, so the next hook can still inject", async () => {
  const stub = fetchStub(["CTX"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });

  assert.equal(await inj.inject("ses_s", undefined), false);
  assert.equal(inj.cache.has("ses_s"), false, "nothing was pushed, so nothing may be cached");

  const system: any[] = [];
  assert.equal(await inj.inject("ses_s", system), true);
  assert.equal(system.length, 1);
});

test("5d: an absent sessionID never fetches (unattributable observation)", async () => {
  const stub = fetchStub(["CTX"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });
  assert.equal(await inj.inject(undefined, []), false);
  assert.equal(await inj.inject("", []), false);
  assert.equal(stub.calls, 0);
});

// ---------------------------------------------------------------------------
// 6. Duplicate injection prevention
// ---------------------------------------------------------------------------
test("6a: repeated sequential hooks produce exactly one injection", async () => {
  const stub = fetchStub(["ONCE"]);
  const inj = createWakeContextInjector({ fetch: stub.fetch });
  const system: any[] = [];

  const results = [];
  for (let i = 0; i < 5; i++) results.push(await inj.inject("ses_seq", system));

  assert.deepEqual(results, [true, false, false, false, false]);
  assert.equal(stub.calls, 1);
  assert.equal(system.length, 1);
});

test("6b: concurrent hooks for one session produce exactly one injection and one fetch", async () => {
  // Two context builds racing before the cache is populated must not both
  // fetch and both push.
  let release: (v: string) => void = () => {};
  const gate = new Promise<string>((res) => (release = res));
  let calls = 0;
  const inj = createWakeContextInjector({
    fetch: async () => {
      calls++;
      return gate;
    },
  });
  const system: any[] = [];

  const p1 = inj.inject("ses_conc", system);
  const p2 = inj.inject("ses_conc", system);
  release("RACED");

  const [r1, r2] = await Promise.all([p1, p2]);
  assert.deepEqual([r1, r2], [true, false], "exactly one caller wins");
  assert.equal(calls, 1, "only one spawn for a concurrent pair");
  assert.equal(system.length, 1);
});

test("6c: a failed in-flight attempt does not block the next attempt", async () => {
  let calls = 0;
  const inj = createWakeContextInjector({
    fetch: async () => {
      calls++;
      if (calls === 1) throw new Error("first fails");
      return "SECOND";
    },
  });
  const system: any[] = [];
  assert.equal(await inj.inject("ses_f", system), false);
  assert.equal(await inj.inject("ses_f", system), true);
  assert.equal(system.length, 1);
});

// ---------------------------------------------------------------------------
// 7. A non-empty context actually reaches the OpenCode hook
// ---------------------------------------------------------------------------
test("7a: the hook event receives a non-empty text part with the exact body", async () => {
  // Mirrors the real hook signature: ctx.session.hook("context", event => …)
  // where event = { sessionID, system: SystemPromptPart[] }.
  const inj = createWakeContextInjector({ fetch: fetchStub([REAL_SYSTEM_PROMPT_ENVELOPE.content]).fetch });
  const event = { sessionID: "ses_hook", system: [] as any[] };

  const pushed = await inj.inject(event.sessionID, event.system);

  assert.equal(pushed, true);
  assert.equal(event.system.length, 1, "the hook's system prompt must gain exactly one part");
  const part = event.system[0];
  assert.equal(part.type, "text");
  assert.equal(part.text, REAL_SYSTEM_PROMPT_ENVELOPE.content);
  assert.ok(part.text.length > 0, "the injected part must be non-empty (the original defect)");
});

test("7b: the second context build for the same session leaves the system prompt untouched", async () => {
  const inj = createWakeContextInjector({ fetch: fetchStub(["BODY"]).fetch });
  const first = { sessionID: "ses_hook2", system: [] as any[] };
  const second = { sessionID: "ses_hook2", system: [] as any[] };

  await inj.inject(first.sessionID, first.system);
  const pushedAgain = await inj.inject(second.sessionID, second.system);

  assert.equal(pushedAgain, false);
  assert.equal(first.system.length, 1);
  assert.equal(second.system.length, 0, "no duplicate injection into a later build");
});

// ---------------------------------------------------------------------------
// Wiring guard for src/index.v2.ts (not importable without a live host)
// ---------------------------------------------------------------------------
test("7c: the plugin entry delegates the context hook to the injector", () => {
  const raw = readFileSync(SRC_ENTRY, "utf8");
  const code = raw
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");

  assert.match(code, /from "\.\/wake-context\.js"/, "the entry must import the wake-context module");
  assert.match(
    code,
    /ctx\.session\.hook\("context"/,
    "the context hook must still be registered",
  );
  assert.match(
    code,
    /wakeContext\.inject\(\s*event\.sessionID\s*,\s*event\.system\s*\)/,
    "the hook must delegate to the injector with the hook's sessionID and system array",
  );
  // The defect itself must not reappear.
  assert.ok(
    !/payload\?\.context\s*\?\?\s*payload\?\.text/.test(code),
    "the pre-fix `payload?.context ?? payload?.text` extraction must not return",
  );
  // And the fetch path must go through the tested extractor.
  assert.match(
    code,
    /extractWakeContextText\(\s*r\.payload\s*\)/,
    "fetchWakeContext must use extractWakeContextText on the parsed envelope",
  );
});

test("7d: the extractor module documents no silent coercion path", () => {
  const src = readFileSync(SRC_MODULE, "utf8");
  assert.match(src, /typeof\s+value\s*===\s*"string"/, "only strings may be returned");
  assert.ok(
    !/String\(/.test(src.replace(/\/\*[\s\S]*?\*\//g, "")),
    "the extractor must not coerce values through String()",
  );
});
