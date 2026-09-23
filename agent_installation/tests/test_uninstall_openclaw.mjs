// tests/test_uninstall_openclaw.mjs — regression coverage for the
// OpenClaw MPM uninstaller.
//
// Pins the safety contract:
//   * Two canonical plugins present → both uninstalled
//   * Neither present → idempotent success
//   * Memory slot == canonical → reset (and legacy slot reset too)
//   * Memory slot == unrelated plugin → untouched
//   * Canonical plugin source matches expected adapter → uninstall
//   * Canonical plugin id points at unexpected source → conflict, do
//     NOT delete (don't seize another install)
//   * Legacy plugin ids present → safely cleaned
//   * Unrelated plugin entries → byte/semantic equivalent after cleanup
//   * Dry-run → zero mutation
//   * MPM substrate tree → untouched
//   * Capability-policy state → only plugin-scoped MPM state removed
//
// Mirrors the hermetic fake-openclaw pattern from
// agent_installation/mpm-memory-openclaw/tests/installer.test.js: a
// fake `openclaw` binary records invocations and emits plausible
// JSON for `plugins inspect --json`, `plugins registry --json`, and
// `plugins list --json` based on env-var-driven state.
//
// Run with: node --test tests/test_uninstall_openclaw.mjs

import { test, before, after, beforeEach } from "node:test";
import assert from "node:assert";
import {
  mkdirSync,
  writeFileSync,
  chmodSync,
  rmSync,
  readFileSync,
  existsSync,
  symlinkSync,
  readlinkSync,
  statSync,
  copyFileSync,
} from "node:fs";
import { spawn, spawnSync } from "node:child_process";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const AGENT_INSTALLATION = path.join(__dirname, "..");
const UNINSTALL_SH = path.join(AGENT_INSTALLATION, "uninstall-openclaw.sh");

// --------------------------------------------------------------------------
// Sandbox layout
// --------------------------------------------------------------------------

const SANDBOX_ROOT = path.join(
  os.tmpdir(),
  "mpm-openclaw-uninstall-" + process.pid + "-" + Math.random().toString(36).slice(2, 8),
);
const FAKE_HOME = path.join(SANDBOX_ROOT, "home");
const FAKE_OPENCLAW_BINDIR = path.join(SANDBOX_ROOT, "openclaw-bin");
const FAKE_OPENCLAW_CONFIG = path.join(FAKE_HOME, ".openclaw", "openclaw.json");

// OpenClaw state lives under $HOME/.openclaw/ in the real CLI.
// We use $FAKE_HOME/.openclaw/ in the test sandbox.
const FAKE_OPENCLAW_DIR = path.join(FAKE_HOME, ".openclaw");

// MPM substrate: $HOME/.mpm/bin/mpm. We DO NOT touch the real one
// — the test sandbox provides its own empty mpm directory and
// asserts the uninstaller never reads or writes anything under it
// except for the verification probe at the very end (which is
// strictly read-only).
const FAKE_MPM_DIR = path.join(FAKE_HOME, ".mpm");
const FAKE_MPM_BIN = path.join(FAKE_MPM_DIR, "bin", "mpm");

// Recorded invocations for assertion.
const OPENCLAW_INVOCATIONS = path.join(SANDBOX_ROOT, "openclaw-invocations.jsonl");

// --------------------------------------------------------------------------
// Adapter directory copies
// --------------------------------------------------------------------------
//
// We need real `openclaw.plugin.json` files to drive the manifest
// parser. Each test gets a fresh sandbox copy of the canonical
// adapter directories so renames/manifests from the repo don't
// pollute the assertions.

const CANONICAL_ADAPTER_MEMORY = path.join(AGENT_INSTALLATION, "mpm-memory-openclaw");
const CANONICAL_ADAPTER_AUTO = path.join(AGENT_INSTALLATION, "mpm-auto-mode-persona-openclaw");

// We copy the adapter directories into the sandbox so the uninstaller
// resolves them via $SCRIPT_DIR (the agent_installation directory).
function installFakeAdapters(sandboxAiRoot) {
  mkdirSync(path.join(sandboxAiRoot, "mpm-memory-openclaw"), { recursive: true });
  mkdirSync(path.join(sandboxAiRoot, "mpm-auto-mode-persona-openclaw"), { recursive: true });
  copyFileSync(
    path.join(CANONICAL_ADAPTER_MEMORY, "openclaw.plugin.json"),
    path.join(sandboxAiRoot, "mpm-memory-openclaw", "openclaw.plugin.json"),
  );
  copyFileSync(
    path.join(CANONICAL_ADAPTER_AUTO, "openclaw.plugin.json"),
    path.join(sandboxAiRoot, "mpm-auto-mode-persona-openclaw", "openclaw.plugin.json"),
  );
}

// --------------------------------------------------------------------------
// Fake openclaw binary
// --------------------------------------------------------------------------
//
// This is a bash script that records every invocation and replies
// to the subcommands the uninstaller uses. Behaviour is parameterised
// by env vars:
//   FAKE_OPENCLAW_CANONICAL_MEMORY_STATE
//   FAKE_OPENCLAW_CANONICAL_AUTO_STATE
//   FAKE_OPENCLAW_LEGACY_MEMORY_STATE
//   FAKE_OPENCLAW_LEGACY_AUTO_STATE
//     absent         — inspect returns ok:false (no plugin record).
//     linked         — inspect returns plugin.rootDir == adapter path.
//     linked_at_alt  — inspect returns plugin.rootDir == some
//                      other path (conflict case).
//     unresolvable   — plugin field present, rootDir missing.
//
//   FAKE_OPENCLAW_MEMORY_SLOT
//     unset|empty    — slot is unset (config get returns "").
//     mpm-memory-openclaw / openclaw-mpm-memory / memory-core / <other>
//
//   FAKE_OPENCLAW_FAIL_UNINSTALL=1 — make uninstall return non-zero.
//   FAKE_OPENCLAW_HANG_UNINSTALL=1 — sleep 60s on uninstall.
//
// The fake also has to handle the uninspected state: `plugins
// uninstall <id>` is the primary mutation, and the fake just records
// it. We don't actually mutate any registry — tests rely on the
// uninstaller to call `plugins uninstall` and to read the post-state
// from a subsequent inspect. To keep the post-state realistic, the
// fake flags a "post-uninstall" mode by checking the FAKE_OPENCLAW_
// UNINSTALLED_LIST env var (a colon-separated list of plugin ids
// that have already been uninstalled). After uninstall, inspect
// for those ids returns ok:false.

// The fake openclaw binary lives in a separate fixture file
// (tests/fixtures/fake-openclaw.sh) to avoid JS string-escaping
// problems with bash parameter expansions like `${VAR}`. We read
// the fixture, apply sentinel substitutions, and write it to the
// per-test bin dir.

const FAKE_OPENCLAW_FIXTURE = path.join(__dirname, "fixtures", "fake-openclaw.sh");

function writeFakeOpenclaw(binDir) {
  mkdirSync(binDir, { recursive: true });
  const p = path.join(binDir, "openclaw");
  const script = readFileSync(FAKE_OPENCLAW_FIXTURE, "utf8")
    .replace(/__BPE_ADAPTER_MEMORY__/g, path.join(AGENT_INSTALLATION, "mpm-memory-openclaw"))
    .replace(/__BPE_ADAPTER_AUTO__/g, path.join(AGENT_INSTALLATION, "mpm-auto-mode-persona-openclaw"));
  writeFileSync(p, script, { mode: 0o755 });
  chmodSync(p, 0o755);
  return p;
}

function writeFakeMpm(homeDir) {
  const dir = path.join(homeDir, ".mpm", "bin");
  mkdirSync(dir, { recursive: true });
  const p = path.join(dir, "mpm");
  writeFileSync(
    p,
    [
      "#!/usr/bin/env bash",
      'echo "MPM fake-mpm 0.0.0-test"',
      "exit 0",
      "",
    ].join("\n"),
    { mode: 0o755 },
  );
  chmodSync(p, 0o755);
  return p;
}

function clearInvocations() {
  rmSync(OPENCLAW_INVOCATIONS, { force: true });
}

function readInvocations() {
  if (!existsSync(OPENCLAW_INVOCATIONS)) return [];
  return readFileSync(OPENCLAW_INVOCATIONS, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      try { return JSON.parse(line); } catch { return { raw: line }; }
    });
}

function readInvocationsRaw() {
  if (!existsSync(OPENCLAW_INVOCATIONS)) return "";
  return readFileSync(OPENCLAW_INVOCATIONS, "utf8");
}

// --------------------------------------------------------------------------
// Setup / teardown
// --------------------------------------------------------------------------

before(() => {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
  mkdirSync(FAKE_OPENCLAW_BINDIR, { recursive: true });
  writeFakeOpenclaw(FAKE_OPENCLAW_BINDIR);
  mkdirSync(FAKE_OPENCLAW_DIR, { recursive: true });
  // Seed an empty openclaw.json so the real uninstaller doesn't
  // error trying to read a non-existent file. (The uninstaller
  // doesn't actually require this file — config is queried via
  // `openclaw config get` — but we seed it for completeness.)
  writeFileSync(FAKE_OPENCLAW_CONFIG, "{}\n", { mode: 0o600 });
});

beforeEach(() => {
  clearInvocations();
  // Reset uninstalled-list between tests so post-state is
  // reproducible regardless of order.
  delete process.env.FAKE_OPENCLAW_UNINSTALLED_LIST;
});

after(() => {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
});

// --------------------------------------------------------------------------
// Driver
// --------------------------------------------------------------------------

function runUninstaller({
  homeDir,
  dryRun = false,
  yes = true,
  extraEnv = {},
} = {}) {
  const env = {
    ...process.env,
    PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
    HOME: homeDir,
    OPENCLAW_PLUGIN_UNINSTALL_TIMEOUT: "10",
    OPENCLAW_PLUGIN_INSPECT_TIMEOUT: "5",
    OPENCLAW_CONFIG_TIMEOUT: "5",
    OPENCLAW_GATEWAY_STATUS_TIMEOUT: "3",
    OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
    FAKE_OPENCLAW_INV_FILE: OPENCLAW_INVOCATIONS,
    FAKE_OPENCLAW_UNINSTALLED_LIST: "",
    ...extraEnv,
  };
  const args = [UNINSTALL_SH];
  if (dryRun) args.push("--dry-run");
  if (yes) args.push("--yes");

  return new Promise((resolve) => {
    const child = spawn("bash", args, {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env,
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => (stdout += d));
    child.stderr.on("data", (d) => (stderr += d));
    child.on("close", (code) => resolve({ code, stdout, stderr }));
    child.on("error", (err) => resolve({ code: -1, stdout, stderr: stderr + err.message }));
  });
}

function freshHome(label) {
  const aiRoot = path.join(SANDBOX_ROOT, "homes", label, "agent_installation");
  const home = path.dirname(aiRoot);  // parent of agent_installation
  mkdirSync(aiRoot, { recursive: true });
  installFakeAdapters(aiRoot);
  // Set up MPM substrate stubs (proof that the uninstaller never
  // touches them).
  writeFakeMpm(home);
  return { home, aiRoot };
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

// (A) Both canonical plugins present → both removed.
test("A. both canonical plugins present → both uninstalled", async () => {
  const { home, aiRoot } = freshHome("A-both-present");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.match(log, /plugins uninstall (--force )?mpm-memory-openclaw/,
    "must uninstall mpm-memory-openclaw; log:\n" + log);
  assert.match(log, /plugins uninstall (--force )?mpm-auto-mode-persona-openclaw/,
    "must uninstall mpm-auto-mode-persona-openclaw; log:\n" + log);
});

// (B) Neither present → idempotent success.
test("B. neither present → idempotent success", async () => {
  const { home } = freshHome("B-neither");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "absent",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "absent",
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "absent",
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "absent",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall/.test(log),
    "must NOT call plugins uninstall when nothing is installed; log:\n" + log);
});

// (C) Memory slot == mpm-memory-openclaw → reset.
test("C. memory slot == canonical → reset to memory-core", async () => {
  const { home } = freshHome("C-slot-canonical");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
      FAKE_OPENCLAW_MEMORY_SLOT: "mpm-memory-openclaw",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  // The plugin uninstall should have happened (so the slot would
  // be reset by the CLI itself). If not, our defensive config
  // set resets it explicitly.
  const hasReset =
    /config set plugins\.slots\.memory memory-core/.test(log) ||
    /plugins uninstall (--force )?mpm-memory-openclaw/.test(log);
  assert.ok(hasReset,
    "must reset memory slot to memory-core (either via config set or via plugins uninstall); log:\n" + log);
});

// (D) Memory slot == unrelated plugin → untouched.
test("D. memory slot == unrelated plugin → untouched", async () => {
  const { home } = freshHome("D-slot-unrelated");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "absent",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "absent",
      FAKE_OPENCLAW_MEMORY_SLOT: "some-other-plugin",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.ok(!/config set plugins\.slots\.memory/.test(log),
    "must NOT mutate the memory slot when it points at an unrelated plugin; log:\n" + log);
});

// (E) Canonical plugin source matches expected adapter → uninstall.
test("E. canonical source matches expected adapter → uninstall allowed", async () => {
  const { home } = freshHome("E-source-match");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_MEMORY_PATH: path.join(AGENT_INSTALLATION, "mpm-memory-openclaw"),
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_PATH: path.join(AGENT_INSTALLATION, "mpm-auto-mode-persona-openclaw"),
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.match(log, /plugins uninstall (--force )?mpm-memory-openclaw/,
    "must uninstall when rootDir matches; log:\n" + log);
  assert.match(log, /plugins uninstall (--force )?mpm-auto-mode-persona-openclaw/,
    "must uninstall when rootDir matches; log:\n" + log);
});

// (F) Canonical plugin id points at unexpected source → conflict, do not delete.
test("F. canonical plugin id at unrelated rootDir → refuse (do not seize)", async () => {
  const { home } = freshHome("F-conflict");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked_at_alt",
      FAKE_OPENCLAW_CANONICAL_MEMORY_PATH: "/opt/unrelated/mpm-memory-openclaw",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked_at_alt",
      FAKE_OPENCLAW_CANONICAL_AUTO_PATH: "/opt/unrelated/mpm-auto-mode-persona-openclaw",
    },
  });
  // Exit 0 is fine — the script exits 0 because it doesn't fail
  // closed on a conflict; it WARNS and skips.
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  assert.match(r.stderr, /refusing to uninstall.*unrelated rootDir/,
    "must surface a refusal warning for unrelated source; stderr:\n" + r.stderr);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall (--force )?mpm-memory-openclaw/.test(log),
    "must NOT call plugins uninstall when rootDir is unrelated; log:\n" + log);
  assert.ok(!/plugins uninstall (--force )?mpm-auto-mode-persona-openclaw/.test(log),
    "must NOT call plugins uninstall when rootDir is unrelated; log:\n" + log);
});

// (G) Legacy plugin ids present → safely cleaned.
test("G. legacy plugin ids present → safely cleaned", async () => {
  const { home } = freshHome("G-legacy");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "absent",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "absent",
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.match(log, /plugins uninstall (--force )?openclaw-mpm-memory/,
    "must uninstall legacy mpm-memory id; log:\n" + log);
  assert.match(log, /plugins uninstall (--force )?openclaw-mpm-auto-mode-persona/,
    "must uninstall legacy auto-mode id; log:\n" + log);
});

// (H) Unrelated plugin entries → byte/semantic equivalent after cleanup.
test("H. unrelated plugin entries → no writes against unrelated plugin ids", async () => {
  const { home } = freshHome("H-unrelated");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  // The uninstaller must not touch unrelated config. We assert by
  // checking that the only `plugins uninstall` calls are for OUR
  // ids. Use [a-z0-9-]+ to bound the match (plugin ids never
  // contain whitespace or punctuation), avoiding trailing JSON
  // characters. Allow an optional `--force` flag before the id.
  const uninstallMatches = [...log.matchAll(/plugins uninstall (?:--force )?([a-z0-9-]+)/g)].map((m) => m[1]);
  for (const id of uninstallMatches) {
    assert.ok(
      id === "mpm-memory-openclaw" ||
      id === "mpm-auto-mode-persona-openclaw" ||
      id === "openclaw-mpm-memory" ||
      id === "openclaw-mpm-auto-mode-persona",
      `uninstall called for non-owned id: ${id}; log:\n` + log,
    );
  }
  // No config get/set/unset against unrelated paths.
  assert.ok(!/config (get|set|unset) plugins\.entries\.anthropic/.test(log),
    "must not touch unrelated plugins.entries.anthropic; log:\n" + log);
  assert.ok(!/config (get|set|unset) plugins\.entries\.memory-core/.test(log),
    "must not touch unrelated plugins.entries.memory-core; log:\n" + log);
});

// (I) Dry-run → zero mutation.
test("I. dry-run → zero mutation", async () => {
  const { home } = freshHome("I-dry-run");
  const r = await runUninstaller({
    homeDir: home,
    dryRun: true,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
      FAKE_OPENCLAW_MEMORY_SLOT: "mpm-memory-openclaw",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  // CRITICAL: in dry-run mode, no plugins uninstall may be issued.
  assert.ok(!/plugins uninstall/.test(log),
    "dry-run must NOT call plugins uninstall; log:\n" + log);
  // And no config set either.
  assert.ok(!/config set/.test(log),
    "dry-run must NOT call config set; log:\n" + log);
});

// (J) MPM substrate tree → untouched.
test("J. MPM substrate tree → untouched", async () => {
  const { home } = freshHome("J-mpm-substrate");
  // Resolve the per-test mpm binary path; freshHome() creates one
  // per test sandbox.
  const mpmBin = path.join(home, ".mpm", "bin", "mpm");
  assert.ok(existsSync(mpmBin), "MPM binary must exist before uninstall");
  // Snapshot the MPM substrate BEFORE uninstall.
  const mpmBinBefore = readFileSync(mpmBin, "utf8");
  const mpmBinStatBefore = statSync(mpmBin);
  const mtimeBefore = mpmBinStatBefore.mtimeMs;

  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);

  // Substrate tree untouched.
  assert.ok(existsSync(mpmBin), "MPM binary must still exist");
  assert.strictEqual(
    readFileSync(mpmBin, "utf8"),
    mpmBinBefore,
    "MPM binary contents must not change",
  );
  const mtimeAfter = statSync(mpmBin).mtimeMs;
  assert.strictEqual(mtimeAfter, mtimeBefore,
    "MPM binary mtime must not change");
});

// (K) Capability-policy state → only plugin-scoped MPM state removed.
test("K. capability-policy residue (no capability writes outside plugin scope)", async () => {
  const { home } = freshHome("K-capabilities");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  // We assert: no `config get/set/unset` against capability/policy
  // namespaces outside our plugin scope. The uninstaller must rely
  // entirely on `plugins uninstall` for capability cleanup (which
  // is the documented 2026.9.5 contract).
  assert.ok(!/config (get|set|unset) capabilities/.test(log),
    "must NOT touch capabilities namespace; log:\n" + log);
  assert.ok(!/config (get|set|unset) policy/.test(log),
    "must NOT touch policy namespace; log:\n" + log);
  assert.ok(!/--accept-capabilities/.test(log),
    "must NOT re-issue --accept-capabilities (we are removing, not granting); log:\n" + log);
});

// (L) Idempotent: second run on a clean state is a no-op.
test("L. second run on already-clean state is a no-op", async () => {
  const { home } = freshHome("L-idempotent");
  // First run: clean state with all canonical plugins present.
  const r1 = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r1.code, 0, `first run failed: ${r1.stderr}`);
  clearInvocations();
  // Second run: all states are now "absent" (the fake tracks
  // uninstalls via UNINSTALLED_LIST, which is propagated via env).
  const r2 = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_UNINSTALLED_LIST: "mpm-memory-openclaw:mpm-auto-mode-persona-openclaw",
    },
  });
  assert.strictEqual(r2.code, 0, `second run failed: ${r2.stderr}`);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall/.test(log),
    "second run on clean state must NOT call plugins uninstall; log:\n" + log);
});

// (M) Hook permission flags are explicitly cleared (covered by
// `plugins uninstall` which removes the entire entries subtree).
test("H-hook. hook permission flags must be removed (no residual flag writes)", async () => {
  const { home } = freshHome("M-hooks");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  // The uninstaller must call plugins uninstall for the memory
  // plugin (which removes the entries subtree including hooks).
  assert.match(log, /plugins uninstall (--force )?mpm-memory-openclaw/,
    "must uninstall the memory plugin (removes entries subtree incl. hook flags); log:\n" + log);
});

// (N) The slot is NOT reset to anything other than the documented
// default 'memory-core' (we must not impose a policy choice).
test("N. slot is reset to 'memory-core' (the documented default), not invented", async () => {
  const { home } = freshHome("N-slot-default");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_MEMORY_SLOT: "mpm-memory-openclaw",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  // If the uninstaller had to reset the slot explicitly (because
  // plugins uninstall didn't), the value must be memory-core. Use
  // [a-z-]+ to bound the match.
  const slotSetMatch = log.match(/config set plugins\.slots\.memory ([a-z-]+)/);
  if (slotSetMatch) {
    assert.strictEqual(slotSetMatch[1], "memory-core",
      `slot reset must be to 'memory-core', got: ${slotSetMatch[1]}`);
  }
});

// (O) Uninstaller works from arbitrary CWD.
test("O. uninstaller is CWD-independent", async () => {
  const { home } = freshHome("O-cwd-indep");
  const unrelatedCwd = path.join(SANDBOX_ROOT, "unrelated-cwd");
  mkdirSync(unrelatedCwd, { recursive: true });
  const r = await new Promise((resolve) => {
    const child = spawn("bash", [UNINSTALL_SH, "--yes"], {
      cwd: unrelatedCwd,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
        HOME: home,
        OPENCLAW_PLUGIN_UNINSTALL_TIMEOUT: "10",
        OPENCLAW_PLUGIN_INSPECT_TIMEOUT: "5",
        OPENCLAW_CONFIG_TIMEOUT: "5",
        OPENCLAW_GATEWAY_STATUS_TIMEOUT: "3",
        OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
        FAKE_OPENCLAW_INV_FILE: OPENCLAW_INVOCATIONS,
        FAKE_OPENCLAW_UNINSTALLED_LIST: "",
        FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
        FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
      },
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.strictEqual(r.code, 0, `uninstaller must succeed from any CWD; stderr:\n` + r.stderr);
});

// (P) --help flag works and exits 0.
test("P. --help exits 0 and prints usage", async () => {
  const { home } = freshHome("P-help");
  const r = await new Promise((resolve) => {
    const child = spawn("bash", [UNINSTALL_SH, "--help"], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `\$\FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
        HOME: home,
      },
    });
    let out = "";
    child.stdout.on("data", (d) => (out += d));
    child.on("close", (c) => resolve({ code: c, stdout: out }));
  });
  assert.strictEqual(r.code, 0, `--help should exit 0; got: ${r.code}`);
  assert.match(r.stdout, /Usage:/);
  assert.match(r.stdout, /--dry-run/);
  assert.match(r.stdout, /--yes/);
});

// (Q) Manifest-driven plugin id (not hard-coded).
test("Q. plugin ids are read from openclaw.plugin.json manifests", async () => {
  // The uninstaller resolves plugin ids from openclaw.plugin.json
  // via the read_plugin_id_from_manifest function. We pin this by
  // reading the uninstaller source directly.
  const src = readFileSync(UNINSTALL_SH, "utf8");
  assert.match(src, /read_plugin_id_from_manifest/,
    "uninstaller must define a manifest-driven id reader");
  assert.match(src, /openclaw\.plugin\.json/,
    "uninstaller must reference openclaw.plugin.json");
  assert.ok(!/PLUGIN_ID\s*=\s*["']mpm-memory-openclaw["']/.test(src),
    "uninstaller must not hard-code the canonical plugin id");
});

// (R) Does not modify shell startup files.
test("R. does not modify shell startup files", async () => {
  const { home } = freshHome("R-shell-rc");
  // Pre-create the typical shell startup files with sentinel content.
  const rcFiles = [".bashrc", ".zshrc", ".profile", ".bash_profile", ".zprofile"];
  const sentinels = {};
  for (const f of rcFiles) {
    const p = path.join(home, f);
    writeFileSync(p, `# original ${f}\n`);
    sentinels[f] = readFileSync(p, "utf8");
  }
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "linked",
    },
  });
  assert.strictEqual(r.code, 0);
  for (const f of rcFiles) {
    const p = path.join(home, f);
    if (existsSync(p)) {
      const c = readFileSync(p, "utf8");
      assert.strictEqual(c, sentinels[f],
        `shell startup file ${f} must be untouched`);
    }
  }
});

// (S) Plugin ids are derived correctly even when one adapter is missing.
test("S. missing adapter directory is tolerated (graceful skip)", async () => {
  // We construct a sandbox WITHOUT the auto adapter. The memory
  // adapter is present.
  const label = "S-missing-adapter";
  const aiRoot = path.join(SANDBOX_ROOT, "homes", label, "agent_installation");
  const home = path.dirname(aiRoot);
  mkdirSync(aiRoot, { recursive: true });
  // Only install the memory adapter; skip the auto adapter.
  mkdirSync(path.join(aiRoot, "mpm-memory-openclaw"), { recursive: true });
  copyFileSync(
    path.join(CANONICAL_ADAPTER_MEMORY, "openclaw.plugin.json"),
    path.join(aiRoot, "mpm-memory-openclaw", "openclaw.plugin.json"),
  );
  // No mpm-auto-mode-persona-openclaw directory.

  writeFakeMpm(home);
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_CANONICAL_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_CANONICAL_AUTO_STATE: "absent",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller must tolerate missing adapter; stderr:\n` + r.stderr);
  const log = readInvocationsRaw();
  assert.match(log, /plugins uninstall (--force )?mpm-memory-openclaw/,
    "must still uninstall memory plugin; log:\n" + log);
  assert.ok(!/plugins uninstall (--force )?mpm-auto-mode-persona-openclaw/.test(log),
    "must NOT attempt uninstall of missing auto adapter; log:\n" + log);
});

// --------------------------------------------------------------------------
// Phase 1 (2026-09-19) — legacy ownership precedence.
// Pins the strict evidence model for legacy plugin ids:
//   A. inspect rootDir matches historical legacy adapter path → owned
//   B. inspect rootDir is a DIFFERENT existing path       → conflict
//   C. registry installRecords[id].sourcePath matches      → owned
//   D. registry sourcePath is a DIFFERENT existing path    → conflict
//   E. slot points to legacy id (only when no A/B/C/D verdict) → owned
//   F. legacy entries have non-empty value (only when no A/B/C/D) → owned
//   G. no source metadata AND no E/F evidence               → ambiguous
// Conflicting source MUST override E/F fallbacks (B/D win).
// --------------------------------------------------------------------------

// LEG-1: legacy id + old canonical MPM source → removed
test("LEG-1. legacy id with inspect rootDir matching historical MPM source → removed", async () => {
  const { home } = freshHome("LEG-1");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      // Inspect returns rootDir = the legacy adapter path. The
      // uninstaller computes its expected legacy path from
      // $SCRIPT_DIR/openclaw-mpm-memory which realpath-resolves
      // to the same path → inspect_root_match → owned.
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "linked",
      FAKE_OPENCLAW_LEGACY_MEMORY_PATH:
        path.join(AGENT_INSTALLATION, "openclaw-mpm-memory"),
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "linked",
      FAKE_OPENCLAW_LEGACY_AUTO_PATH:
        path.join(AGENT_INSTALLATION, "openclaw-mpm-auto-mode-persona"),
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.match(log,
    /plugins uninstall (--force )?openclaw-mpm-memory\b/,
    "must uninstall legacy memory id (inspect_root_match evidence); log:\n" + log);
  assert.match(log,
    /plugins uninstall (--force )?openclaw-mpm-auto-mode-persona\b/,
    "must uninstall legacy auto id (inspect_root_match evidence); log:\n" + log);
});

// LEG-2: legacy id + registry old canonical MPM source → removed
test("LEG-2. legacy id with registry installRecords pointing at historical MPM source → removed", async () => {
  const { home } = freshHome("LEG-2");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      // Inspect fails (returns ok:false); registry has the legacy
      // install record pointing at the canonical legacy adapter
      // path → registry_path_match → owned.
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "vanished",
      FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH:
        path.join(AGENT_INSTALLATION, "openclaw-mpm-memory"),
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "vanished",
      FAKE_OPENCLAW_LEGACY_AUTO_REGISTRY_PATH:
        path.join(AGENT_INSTALLATION, "openclaw-mpm-auto-mode-persona"),
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.match(log,
    /plugins uninstall (--force )?openclaw-mpm-memory\b/,
    "must uninstall legacy memory id via registry_path_match; log:\n" + log);
  assert.match(log,
    /plugins uninstall (--force )?openclaw-mpm-auto-mode-persona\b/,
    "must uninstall legacy auto id via registry_path_match; log:\n" + log);
});

// LEG-3: legacy id + unexpected inspect source → conflict, untouched
test("LEG-3. legacy id with inspect rootDir at UNRELATED existing path → conflict, untouched", async () => {
  const { home, aiRoot } = freshHome("LEG-3");
  // Create an EXISTING unrelated directory for the conflict path.
  // The uninstaller only classifies as conflict when the
  // rootDir/sourcePath resolves to an existing path; a non-
  // existent path falls through to the ambiguous verdict.
  const unrelMemory = path.join(aiRoot, "unrelated-memory");
  const unrelAuto = path.join(aiRoot, "unrelated-auto");
  mkdirSync(unrelMemory, { recursive: true });
  mkdirSync(unrelAuto, { recursive: true });
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "linked_at_alt",
      FAKE_OPENCLAW_LEGACY_MEMORY_PATH: unrelMemory,
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "linked_at_alt",
      FAKE_OPENCLAW_LEGACY_AUTO_PATH: unrelAuto,
    },
  });
  // Conflict is a non-fatal refusal; the uninstaller still exits 0
  // but logs a WARN and skips the uninstall.
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  assert.match(r.stderr, /refusing to uninstall.*unrelated rootDir/,
    "must surface the conflict refusal; stderr:\n" + r.stderr);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-memory\b/.test(log),
    "must NOT call plugins uninstall for legacy id with conflicting rootDir; log:\n" + log);
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-auto-mode-persona\b/.test(log),
    "must NOT call plugins uninstall for legacy auto id with conflicting rootDir; log:\n" + log);
});

// LEG-4: legacy id + unexpected registry source → conflict, untouched
test("LEG-4. legacy id with registry install record at UNRELATED existing path → conflict, untouched", async () => {
  const { home, aiRoot } = freshHome("LEG-4");
  // Existing unrelated directories required for the conflict path.
  const unrelMemory = path.join(aiRoot, "unrelated-memory");
  const unrelAuto = path.join(aiRoot, "unrelated-auto");
  mkdirSync(unrelMemory, { recursive: true });
  mkdirSync(unrelAuto, { recursive: true });
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "vanished",
      FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH: unrelMemory,
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "vanished",
      FAKE_OPENCLAW_LEGACY_AUTO_REGISTRY_PATH: unrelAuto,
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  assert.match(r.stderr, /refusing to uninstall.*unrelated/,
    "must surface the conflict refusal from registry evidence; stderr:\n" + r.stderr);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-memory\b/.test(log),
    "must NOT call plugins uninstall when registry shows conflict; log:\n" + log);
});

// LEG-5: legacy id + slot fallback, no source verdict → removable
test("LEG-5. legacy id with no source verdict AND slot points to legacy → removable (slot fallback)", async () => {
  const { home } = freshHome("LEG-5");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      // Inspect + registry have no source verdict. The memory
      // slot currently points to the legacy memory id → owned
      // via slot_points_to_legacy.
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "ambiguous",
      FAKE_OPENCLAW_MEMORY_SLOT: "openclaw-mpm-memory",
      // Auto legacy has neither slot nor entries; should be ambiguous.
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "ambiguous",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.match(log,
    /plugins uninstall (--force )?openclaw-mpm-memory\b/,
    "must uninstall legacy memory id via slot fallback; log:\n" + log);
  // The auto legacy has no slot/config fallback → ambiguous, no uninstall.
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-auto-mode-persona\b/.test(log),
    "must NOT uninstall auto legacy when no fallback evidence; log:\n" + log);
});

// LEG-6: legacy id + entries fallback, no source verdict → removable
test("LEG-6. legacy id with no source verdict AND entries have non-empty value → removable (config fallback)", async () => {
  const { home } = freshHome("LEG-6");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      // No inspect / registry verdict. Memory slot does NOT point
      // to legacy. The legacy entries subtree has a non-empty
      // config.mpmBin (or enabled) value → owned via
      // entry_key_present.
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "entries_only",
      FAKE_OPENCLAW_LEGACY_MEMORY_ENTRY_VALUE: path.join(os.homedir(), ".local", "bin", "mpm"),
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "entries_only",
      FAKE_OPENCLAW_LEGACY_AUTO_ENTRY_VALUE: path.join(os.homedir(), ".local", "bin", "mpm"),
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  const log = readInvocationsRaw();
  assert.match(log,
    /plugins uninstall (--force )?openclaw-mpm-memory\b/,
    "must uninstall legacy memory id via entries fallback; log:\n" + log);
  assert.match(log,
    /plugins uninstall (--force )?openclaw-mpm-auto-mode-persona\b/,
    "must uninstall legacy auto id via entries fallback; log:\n" + log);
});

// LEG-7: legacy id + no ownership evidence → ambiguous, preserved
test("LEG-7. legacy id with NO ownership evidence at all → ambiguous, preserved", async () => {
  const { home } = freshHome("LEG-7");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      // Inspect fails. Registry has no install record. Slot does
      // NOT point to legacy. Entries are empty. → ambiguous.
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "ambiguous",
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "ambiguous",
    },
  });
  assert.strictEqual(r.code, 0, `uninstaller exited non-zero: ${r.stderr}`);
  assert.match(r.stderr, /no ownership evidence/,
    "must surface the ambiguous verdict; stderr:\n" + r.stderr);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-memory\b/.test(log),
    "must NOT call plugins uninstall for ambiguous legacy id; log:\n" + log);
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-auto-mode-persona\b/.test(log),
    "must NOT call plugins uninstall for ambiguous legacy auto id; log:\n" + log);
});

// LEG-8: conflicting inspect rootDir MUST override slot fallback (B > E)
test("LEG-8. conflicting inspect rootDir overrides slot fallback (B wins over E)", async () => {
  const { home, aiRoot } = freshHome("LEG-8");
  // Existing unrelated directory required for the conflict path.
  const unrelMemory = path.join(aiRoot, "unrelated-memory");
  mkdirSync(unrelMemory, { recursive: true });
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      // Inspect returns a DIFFERENT existing rootDir → conflict (B).
      // Slot ALSO points to the legacy id (E). Per the precedence,
      // B wins; we must NOT uninstall.
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "linked_at_alt",
      FAKE_OPENCLAW_LEGACY_MEMORY_PATH: unrelMemory,
      FAKE_OPENCLAW_MEMORY_SLOT: "openclaw-mpm-memory",
    },
  });
  assert.strictEqual(r.code, 0);
  assert.match(r.stderr, /refusing to uninstall/,
    "must surface conflict refusal despite slot fallback evidence; stderr:\n" + r.stderr);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-memory\b/.test(log),
    "must NOT uninstall legacy id when conflict overrides slot fallback; log:\n" + log);
});

// LEG-9: conflicting registry source MUST override entries fallback (D > F)
test("LEG-9. conflicting registry sourcePath overrides entries fallback (D wins over F)", async () => {
  const { home, aiRoot } = freshHome("LEG-9");
  // Existing unrelated directory required for the conflict path.
  const unrelMemory = path.join(aiRoot, "unrelated-memory");
  mkdirSync(unrelMemory, { recursive: true });
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      // Inspect fails. Registry has install record at a DIFFERENT
      // existing path → conflict (D). Entries also have a
      // non-empty value (F). Per precedence, D wins; refuse.
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "vanished",
      FAKE_OPENCLAW_LEGACY_MEMORY_REGISTRY_PATH: unrelMemory,
      FAKE_OPENCLAW_LEGACY_MEMORY_ENTRY_VALUE: path.join(os.homedir(), ".local", "bin", "mpm"),
    },
  });
  assert.strictEqual(r.code, 0);
  assert.match(r.stderr, /refusing to uninstall/,
    "must surface conflict refusal despite entries fallback evidence; stderr:\n" + r.stderr);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall (--force )?openclaw-mpm-memory\b/.test(log),
    "must NOT uninstall when registry conflict overrides entries fallback; log:\n" + log);
});

// LEG-10: unrelated plugins remain unchanged across legacy cases
test("LEG-10. unrelated plugin entries remain untouched across legacy scenarios", async () => {
  const { home } = freshHome("LEG-10");
  const r = await runUninstaller({
    homeDir: home,
    extraEnv: {
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "ambiguous",
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "ambiguous",
    },
  });
  assert.strictEqual(r.code, 0);
  const log = readInvocationsRaw();
  assert.ok(!/config (get|set|unset) plugins\.entries\.anthropic/.test(log),
    "must NOT touch unrelated entries; log:\n" + log);
  assert.ok(!/config (get|set|unset) plugins\.entries\.memory-core/.test(log),
    "must NOT touch unrelated memory-core entry; log:\n" + log);
});

// LEG-11: dry-run with legacy ambiguity remains mutation-free
test("LEG-11. dry-run remains mutation-free when legacy ids are ambiguous", async () => {
  const { home } = freshHome("LEG-11");
  const r = await runUninstaller({
    homeDir: home,
    dryRun: true,
    extraEnv: {
      FAKE_OPENCLAW_LEGACY_MEMORY_STATE: "ambiguous",
      FAKE_OPENCLAW_LEGACY_AUTO_STATE: "linked",
      FAKE_OPENCLAW_LEGACY_AUTO_PATH:
        path.join(AGENT_INSTALLATION, "openclaw-mpm-auto-mode-persona"),
    },
  });
  assert.strictEqual(r.code, 0);
  const log = readInvocationsRaw();
  assert.ok(!/plugins uninstall/.test(log),
    "dry-run must NOT call plugins uninstall; log:\n" + log);
  assert.ok(!/config set/.test(log),
    "dry-run must NOT call config set; log:\n" + log);
});

console.log("test_uninstall_openclaw.mjs loaded.");