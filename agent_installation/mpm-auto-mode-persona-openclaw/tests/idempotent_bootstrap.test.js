/**
 * mpm-auto-mode-persona-openclaw — idempotent bootstrap regression test.
 *
 * Symptom that triggered this test: in fresh OpenClaw sessions, the persona
 * block appeared THREE times in the assembled SOUL.md content.
 *
 * Root cause: agent:bootstrap fires multiple times per turn (retry / queue /
 * partial-assembly paths). The plugin's bootstrap handler APPENDED the
 * reminder to bootstrapFiles[soulIdx].content on every fire, producing
 * one duplicate copy per fire.
 *
 * Fix: per-session fingerprint Set. Each reminder carries a djb2 hash of
 * its content; once a hash is recorded for the session, subsequent bootstrap
 * firings for the same reminder are skipped.
 *
 * Pattern: load the real plugin via a SDK-stubbing loader at import time
 * (same approach as mpm-memory-openclaw/tests/runtime_injection.test.js),
 * then exercise the registered hooks directly in the test process.
 */

import { test, describe, before, after } from "node:test";
import assert from "node:assert";
import { register } from "node:module";
import { pathToFileURL } from "node:url";
import { writeFileSync, mkdirSync, rmSync, chmodSync } from "node:fs";
import path from "node:path";
import os from "os";
import { fileURLToPath } from "url";
import { spawn } from "node:child_process";

// Register the SDK-stubbing loader BEFORE importing the plugin.
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PLUGIN_DIR = path.join(__dirname, "..");
const loaderPath = path.join(__dirname, "loader.mjs");
register(loaderPath, pathToFileURL(PLUGIN_DIR));

// Now load the plugin (loader intercepts openclaw/plugin-sdk/* imports).
const pluginEntry = await import(pathToFileURL(path.join(PLUGIN_DIR, "index.js")).href);
const plugin = pluginEntry.default;

// Stub the loader's hook-runtime to record handlers in globalThis.__MPM_HOOKS.
// (The loader.mjs already does this via `globalThis.__MPM_HOOKS = HOOKS`.)
const HOOKS = globalThis.__MPM_HOOKS;

// --------------------------------------------------------------------------
// Fake mpm binary: prints a fixed reminder on stdout.
// --------------------------------------------------------------------------

const FAKE_REMINDER = "PERSONA_BLOCK_UNDER_TEST_reminder_fingerprint_a1b2c3";
const FAKE_MPM_DIR = path.join(os.tmpdir(), "mpm-persona-idem-fake-" + process.pid);
const fakeMpmPath = path.join(FAKE_MPM_DIR, "mpm");

mkdirSync(FAKE_MPM_DIR, { recursive: true });
writeFileSync(
  fakeMpmPath,
  `#!/usr/bin/env node
process.stdout.write(${JSON.stringify(FAKE_REMINDER)});
process.exit(0);
`,
  { mode: 0o755 },
);
chmodSync(fakeMpmPath, 0o755);

// --------------------------------------------------------------------------
// Build the api object the plugin expects.
// --------------------------------------------------------------------------

let api;
before(() => {
  api = {
    config: {
      plugins: {
        entries: {
          "mpm-auto-mode-persona-openclaw": {
            enabled: true,
            config: { mpmBin: fakeMpmPath, timeoutMs: 5000 },
          },
        },
      },
    },
    log: { info: () => {}, warn: () => {} },
  };
});

after(() => {
  try { rmSync(FAKE_MPM_DIR, { recursive: true, force: true }); } catch {}
});

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

describe("agent:bootstrap idempotency", () => {
  test("3 firings of the same reminder produce exactly 1 occurrence", async () => {
    await plugin.register(api);
    const sessionKey = "agent:main:idempotent-test-3";

    await HOOKS.get("message:received")[0]({
      sessionKey,
      context: { content: "test prompt" },
    });
    // Allow the spawned mpm process to produce stdout.
    await new Promise((r) => setTimeout(r, 200));

    let soul = "EXISTING_SOUL_CONTENT\n";
    for (let i = 0; i < 3; i++) {
      const files = [
        { name: "SOUL.md", path: "/x", content: soul, missing: false },
        { name: "AGENTS.md", path: "/x", content: "agent stuff", missing: false },
      ];
      await HOOKS.get("agent:bootstrap")[0]({
        sessionKey,
        context: { bootstrapFiles: files },
      });
      soul = files[0].content;
    }

    const occurrences = (soul.match(new RegExp(FAKE_REMINDER.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g")) || []).length;
    assert.strictEqual(
      occurrences,
      1,
      `expected 1 occurrence after 3 bootstrap firings, got ${occurrences} (soul=${JSON.stringify(soul.slice(0, 200))})`,
    );
  });

  test("1 firing still works (regression guard)", async () => {
    await plugin.register(api);
    const sessionKey = "agent:main:idempotent-test-1";

    await HOOKS.get("message:received")[0]({
      sessionKey,
      context: { content: "test prompt" },
    });
    await new Promise((r) => setTimeout(r, 200));

    let soul = "EXISTING_SOUL_CONTENT\n";
    const files = [
      { name: "SOUL.md", path: "/x", content: soul, missing: false },
      { name: "AGENTS.md", path: "/x", content: "agent stuff", missing: false },
    ];
    await HOOKS.get("agent:bootstrap")[0]({
      sessionKey,
      context: { bootstrapFiles: files },
    });
    soul = files[0].content;

    const occurrences = (soul.match(new RegExp(FAKE_REMINDER.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g")) || []).length;
    assert.strictEqual(occurrences, 1, `expected 1 occurrence after a single fire, got ${occurrences}`);
  });

  test("5 firings produce exactly 1 occurrence (worst case)", async () => {
    await plugin.register(api);
    const sessionKey = "agent:main:idempotent-test-5";

    await HOOKS.get("message:received")[0]({
      sessionKey,
      context: { content: "test prompt" },
    });
    await new Promise((r) => setTimeout(r, 200));

    let soul = "EXISTING_SOUL_CONTENT\n";
    for (let i = 0; i < 5; i++) {
      const files = [
        { name: "SOUL.md", path: "/x", content: soul, missing: false },
      ];
      await HOOKS.get("agent:bootstrap")[0]({
        sessionKey,
        context: { bootstrapFiles: files },
      });
      soul = files[0].content;
    }

    const occurrences = (soul.match(new RegExp(FAKE_REMINDER.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g")) || []).length;
    assert.strictEqual(occurrences, 1, `expected 1 occurrence after 5 bootstrap firings, got ${occurrences}`);
  });

  test("session_end clears fingerprint (next session can inject again)", async () => {
    await plugin.register(api);
    const sessionKey = "agent:main:idempotent-test-reset";

    // First session: 2 firings -> 1 occurrence
    await HOOKS.get("message:received")[0]({
      sessionKey,
      context: { content: "first" },
    });
    await new Promise((r) => setTimeout(r, 200));
    let soul = "EXISTING\n";
    for (let i = 0; i < 2; i++) {
      const files = [{ name: "SOUL.md", path: "/x", content: soul, missing: false }];
      await HOOKS.get("agent:bootstrap")[0]({ sessionKey, context: { bootstrapFiles: files } });
      soul = files[0].content;
    }
    const s1Count = (soul.match(new RegExp(FAKE_REMINDER.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g")) || []).length;
    assert.strictEqual(s1Count, 1, `session 1: expected 1, got ${s1Count}`);

    // session_end should clear the fingerprint
    await HOOKS.get("session_end")[0]({ sessionKey });

    // Second session: should inject again (1 occurrence)
    await HOOKS.get("message:received")[0]({
      sessionKey,
      context: { content: "second" },
    });
    await new Promise((r) => setTimeout(r, 200));
    soul = "EXISTING\n";
    {
      const files = [{ name: "SOUL.md", path: "/x", content: soul, missing: false }];
      await HOOKS.get("agent:bootstrap")[0]({ sessionKey, context: { bootstrapFiles: files } });
      soul = files[0].content;
    }
    const s2Count = (soul.match(new RegExp(FAKE_REMINDER.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g")) || []).length;
    assert.strictEqual(s2Count, 1, `session 2 (after session_end): expected 1, got ${s2Count}`);
  });

  test("overlapping sessions are independent (Map<sessionKey, Set<fingerprint>>)", async () => {
    await plugin.register(api);
    const sessionA = "agent:main:overlap-A";
    const sessionB = "agent:main:overlap-B";
    const reminderRegex = new RegExp(
      FAKE_REMINDER.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"),
      "g",
    );

    // Real OpenClaw hook order: message:received primes the per-session
    // reminder, bootstrap consumes it. To exercise the fingerprint
    // dedupe (not just the consume-on-read), we re-prime the reminder
    // before each bootstrap. The dedupe Set must block the second
    // injection even though the reminder is freshly available.
    async function primeReminder(sessionKey, prompt) {
      await HOOKS.get("message:received")[0]({ sessionKey, context: { content: prompt } });
      await new Promise((r) => setTimeout(r, 200));
    }
    async function bootstrap(sessionKey) {
      const files = [
        { name: "SOUL.md", path: "/x", content: "EXISTING\n", missing: false },
      ];
      await HOOKS.get("agent:bootstrap")[0]({ sessionKey, context: { bootstrapFiles: files } });
      return files[0].content;
    }
    const count = (text) => (text.match(reminderRegex) || []).length;

    // 1. session A first bootstrap: reminder X inserted.
    await primeReminder(sessionA, "a-1");
    const aSoul1 = await bootstrap(sessionA);
    assert.strictEqual(count(aSoul1), 1,
      `session A first bootstrap: expected 1, got ${count(aSoul1)}`);

    // 2. session B first bootstrap (overlapping in time with A): same
    //    reminder X must STILL be inserted for B. A's fingerprint
    //    must NOT block B's injection — this is the cross-session
    //    isolation invariant.
    await primeReminder(sessionB, "b-1");
    const bSoul1 = await bootstrap(sessionB);
    assert.strictEqual(count(bSoul1), 1,
      `session B first bootstrap: expected 1 (independent of A), got ${count(bSoul1)}`);

    // 3. session A re-primed and bootstrapped again: dedupe must
    //    block the re-injection. Same reminder content, but the
    //    fingerprint Set for A already contains the hash. Each
    //    bootstrap creates a fresh files array, so aSoul2's
    //    occurrence count starts at 0; if dedupe works, the second
    //    bootstrap appends nothing and aSoul2 stays at 0.
    await primeReminder(sessionA, "a-2");
    const aSoul2 = await bootstrap(sessionA);
    assert.strictEqual(count(aSoul2), 0,
      `session A re-primed bootstrap: expected 0 (dedupe blocks re-injection), got ${count(aSoul2)}`);

    // 4. session A ends: must not clear B's dedupe state.
    await HOOKS.get("session_end")[0]({ sessionKey: sessionA });

    // 5. session B re-primed and bootstrapped again: B's fingerprint
    //    Set must be intact. A's session_end did not affect B. Same
    //    semantics as step 3 — bootstrap creates a fresh files array,
    //    dedupe must block the re-injection, so bSoul2 stays at 0.
    await primeReminder(sessionB, "b-2");
    const bSoul2 = await bootstrap(sessionB);
    assert.strictEqual(count(bSoul2), 0,
      `session B re-primed bootstrap after A ended: expected 0 (B's dedupe intact), got ${count(bSoul2)}`);
  });
});
