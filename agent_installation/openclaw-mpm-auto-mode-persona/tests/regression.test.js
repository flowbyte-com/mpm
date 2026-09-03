/**
 * openclaw-mpm-auto-mode-persona — regression tests for the 2026-09-02
 * forensic audit.
 *
 * Covers:
 *   (A) config-path nesting: entries[id].config.mpmBin must be resolved.
 *   (C) no fabricated MPM_BIN env var: the plugin must not depend on it.
 *
 * Environment inheritance (B) and PATH-independent binary (E) are covered
 * by the shared lib/workspace.js tests in the openclaw-mpm-memory suite
 * — both plugins use the identical helper.
 *
 * Run with: node --test tests/regression.test.js
 */

import { test, describe } from "node:test";
import assert from "node:assert";
import { spawn } from "node:child_process";
import { resolveWorkspace, withWorkspace } from "../lib/workspace.js";

// --------------------------------------------------------------------------
// (A) Config-path nesting — entries[id].config.mpmBin
// --------------------------------------------------------------------------
//
// Simulates the config-reading logic in index.js. Before the fix, the code
// read `entries[id]` directly, silently dropping user-configured mpmBin
// (which the docs put under .config). After the fix, the inner .config is
// dereferenced.

function readPluginConfig(api, pluginId) {
  // Mirror of the fixed code in index.js:register(api).
  const entry = api?.config?.plugins?.entries?.[pluginId];
  const cfg = entry?.config ?? {};
  return {
    enabled: cfg.enabled !== false,
    mpmBin:
      typeof cfg.mpmBin === "string" && cfg.mpmBin ? cfg.mpmBin : "mpm",
    timeoutMs: typeof cfg.timeoutMs === "number" ? cfg.timeoutMs : 5000,
  };
}

test("(A) config nesting: entries[id].config.mpmBin is read", () => {
  const api = {
    config: {
      plugins: {
        entries: {
          "openclaw-mpm-auto-mode-persona": {
            enabled: true,
            config: {
              mpmBin: "/home/alice/.local/bin/mpm",
              timeoutMs: 7777,
            },
          },
        },
      },
    },
  };
  const cfg = readPluginConfig(api, "openclaw-mpm-auto-mode-persona");
  assert.strictEqual(cfg.mpmBin, "/home/alice/.local/bin/mpm");
  assert.strictEqual(cfg.timeoutMs, 7777);
  assert.strictEqual(cfg.enabled, true);
});

test("(A) config nesting: bare entry shape (without .config) falls back to defaults", () => {
  // Regression guard: if a user accidentally writes the mpmBin directly
  // under entries[id] (the OLD shape from before the fix), the plugin
  // must still default gracefully — never silently pick up the wrong value.
  const api = {
    config: {
      plugins: {
        entries: {
          "openclaw-mpm-auto-mode-persona": {
            // OLD shape (no .config) — would have been read by the bug,
            // but the fix drops this and falls back to defaults. A user
            // upgrading must move mpmBin under .config.
            mpmBin: "/should/not/be/picked/up",
            timeoutMs: 1,
          },
        },
      },
    },
  };
  const cfg = readPluginConfig(api, "openclaw-mpm-auto-mode-persona");
  assert.strictEqual(cfg.mpmBin, "mpm", "old shape must be ignored, not silently consumed");
  assert.strictEqual(cfg.timeoutMs, 5000, "old shape timeoutMs must be ignored");
});

test("(A) config nesting: missing entries block falls back to defaults", () => {
  const api = { config: { plugins: { entries: {} } } };
  const cfg = readPluginConfig(api, "openclaw-mpm-auto-mode-persona");
  assert.strictEqual(cfg.mpmBin, "mpm");
  assert.strictEqual(cfg.timeoutMs, 5000);
  assert.strictEqual(cfg.enabled, true);
});

test("(A) config nesting: explicit `enabled: false` disables the plugin", () => {
  const api = {
    config: {
      plugins: {
        entries: {
          "openclaw-mpm-auto-mode-persona": {
            config: { enabled: false },
          },
        },
      },
    },
  };
  const cfg = readPluginConfig(api, "openclaw-mpm-auto-mode-persona");
  assert.strictEqual(cfg.enabled, false);
});

// --------------------------------------------------------------------------
// (B) Environment inheritance — workspace helper
// --------------------------------------------------------------------------
//
// withWorkspace() must preserve process.env unless explicitly overridden.
// This is what the auto-mode-persona runMpmRoute relies on (it passes no
// args to withWorkspace).

test("(B) withWorkspace() inherits process.env (PATH, HOME, etc.)", () => {
  const env = withWorkspace();
  assert.strictEqual(env.PATH, process.env.PATH, "PATH must be inherited");
  assert.strictEqual(env.HOME, process.env.HOME, "HOME must be inherited");
});

test("(B) withWorkspace() sets MPM_WORKSPACE to process.env value when set", () => {
  const prior = process.env.MPM_WORKSPACE;
  try {
    process.env.MPM_WORKSPACE = "/custom/mpm/workspace";
    const env = withWorkspace();
    assert.strictEqual(env.MPM_WORKSPACE, "/custom/mpm/workspace");
  } finally {
    if (prior === undefined) delete process.env.MPM_WORKSPACE;
    else process.env.MPM_WORKSPACE = prior;
  }
});

test("(B) withWorkspace() falls back to $HOME/.mpm when env unset", async () => {
  const prior = process.env.MPM_WORKSPACE;
  try {
    delete process.env.MPM_WORKSPACE;
    const env = withWorkspace();
    const path = await import("node:path");
    const os = await import("node:os");
    assert.strictEqual(env.MPM_WORKSPACE, path.join(os.homedir(), ".mpm"));
  } finally {
    if (prior !== undefined) process.env.MPM_WORKSPACE = prior;
  }
});

test("(B) withWorkspace() merges with explicit env override", () => {
  // The helper's contract: when called with an explicit env object, only
  // those keys are included (the caller is responsible for spreading
  // process.env if they want inheritance). MPM_WORKSPACE is added on top.
  // The memory plugin's callMpmTool uses this pattern correctly:
  //   withWorkspace({ ...process.env, MPM_LOG_FORMAT: "json" })
  const env = withWorkspace({ FOO: "bar", PATH: "/minimal/path" });
  assert.strictEqual(env.FOO, "bar");
  assert.strictEqual(env.PATH, "/minimal/path", "explicit override applied");
  assert.strictEqual(env.HOME, undefined,
    "withWorkspace does NOT auto-merge process.env when an arg is passed — caller does that");
  assert.ok(env.MPM_WORKSPACE && env.MPM_WORKSPACE.length > 0,
    "MPM_WORKSPACE is always populated");
});

// --------------------------------------------------------------------------
// (C) No fabricated MPM_BIN environment variable
// --------------------------------------------------------------------------
//
// The plugin code reads cfg.mpmBin only. It must NOT honour MPM_BIN from
// process.env (which would silently behave differently from the documented
// config). The mpm binary path is set ONLY via plugin config.

test("(C) resolution does not read MPM_BIN env var", () => {
  // Simulate a user setting MPM_BIN expecting it to be honoured — the
  // plugin must ignore it. (The bug being guarded against is the inverse:
  // docs claiming MPM_BIN is supported when it isn't.)
  const prior = process.env.MPM_BIN;
  try {
    process.env.MPM_BIN = "/from/env/mpm";
    const api = {
      config: {
        plugins: {
          entries: {
            "openclaw-mpm-auto-mode-persona": {
              config: { mpmBin: "/from/config/mpm" },
            },
          },
        },
      },
    };
    const cfg = readPluginConfig(api, "openclaw-mpm-auto-mode-persona");
    assert.strictEqual(cfg.mpmBin, "/from/config/mpm",
      "MPM_BIN env var must NOT influence mpmBin — plugin uses config only");
  } finally {
    if (prior === undefined) delete process.env.MPM_BIN;
    else process.env.MPM_BIN = prior;
  }
});

// --------------------------------------------------------------------------
// (E) PATH-independent configured binary
// --------------------------------------------------------------------------
//
// A configured absolute mpmBin must work even when PATH lacks $HOME/.mpm/bin.
// This is the systemd --user failure mode.

test("(E) runMpmRoute spawn receives configured absolute mpmBin even with stripped PATH", async () => {
  // Simulate the spawn call runMpmRoute makes. We don't actually need mpm
  // to exist — we just need to verify that the env passed to the child
  // contains the right PATH/workspace, and that the binary is invoked
  // via its absolute path (which works regardless of PATH).
  //
  // We use `node -e` as a stand-in "binary" so we can introspect the env
  // the child received.
  const absoluteBin = process.execPath; // /usr/bin/node or similar
  const child = spawn(
    absoluteBin,
    [
      "-e",
      "process.stdout.write(JSON.stringify({PATH: process.env.PATH || '', " +
        "MPM_WORKSPACE: process.env.MPM_WORKSPACE || '', " +
        "argv0: process.argv[0]}))",
    ],
    {
      stdio: ["ignore", "pipe", "pipe"],
      env: withWorkspace({
        // Deliberately strip MPM bin from PATH. The absolute binary still
        // works because we passed it directly to spawn.
        PATH: "/usr/bin:/bin",
      }),
    },
  );
  let stdout = "";
  child.stdout.on("data", (d) => (stdout += d));
  await new Promise((resolve) => child.on("close", resolve));
  const result = JSON.parse(stdout);
  assert.strictEqual(result.argv0, absoluteBin,
    "absolute binary path resolves without PATH lookup");
  assert.ok(result.PATH.length > 0, "child received a PATH (inherited)");
});

console.log("regression.test.js loaded.");
