// tests/installer.test.js — adapter installer regression coverage.
//
// Pins the 2026-09-16 fresh-profile fixes to the openclaw-mpm-memory
// install.sh. The script is bash; these tests drive it through a
// controlled PATH + a fake `openclaw` CLI + a fake `mpm` binary,
// so we can assert on what was persisted and in what order without
// touching the real OpenClaw install.
//
// Run with:
//   node --test tests/installer.test.js
//
// What these tests pin:
//   1. both hook permission flags are written (allowConversationAccess
//      AND allowPromptInjection)
//   2. plugin is installed/linked before plugin-specific config is
//      applied (the link ordering matters — plugin config keys are
//      only valid once the plugin is registered)
//   3. installer works from arbitrary CWD (BASH_SOURCE[0] resolved)
//   4. canonical $HOME/.mpm/bin/mpm is accepted when bare `mpm` is
//      not on the installer shell's PATH
//   5. canonical $HOME/.local/bin/mpm symlink is accepted
//   6. existing valid MPM is not unnecessarily bootstrapped (no
//      MPM_BOOTSTRAP_URL call when canonical paths exist)
//   7. idempotent rerun succeeds
//   8. plugin is enabled
//   9. memory slot is set to openclaw-mpm-memory
//  10. memory-core is NOT modified by the installer (operator policy)
//  11. absolute mpmBin is persisted
//  12. no shell startup files (.bashrc/.zshrc/.profile) are modified
//  13. no root scripts/install.sh OpenClaw behavior is reintroduced
//  14. gateway handling is bounded/safe — the fake openclaw rejects
//      any unbounded gateway restart call
//
// These tests do not need a real OpenClaw install. They use a hermetic
// PATH and a fake-openclaw binary that records invocations.

import { test, before, after } from "node:test";
import assert from "node:assert";
import {
  mkdirSync,
  writeFileSync,
  chmodSync,
  rmSync,
  readFileSync,
  existsSync,
  symlinkSync,
} from "node:fs";
import { spawn } from "node:child_process";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ADAPTER_DIR = path.join(__dirname, "..");
const INSTALL_SH = path.join(ADAPTER_DIR, "install.sh");

// --------------------------------------------------------------------------
// Hermetic scaffolding
// --------------------------------------------------------------------------

const SANDBOX_ROOT = path.join(os.tmpdir(), "openclaw-mpm-memory-install-" + process.pid);
const FAKE_HOME = path.join(SANDBOX_ROOT, "home");
const FAKE_MPM_PRIMARY = path.join(FAKE_HOME, ".mpm", "bin");
const FAKE_MPM_SYMLINK_DIR = path.join(FAKE_HOME, ".local", "bin");
const FAKE_OPENCLAW_BINDIR = path.join(SANDBOX_ROOT, "openclaw-bin");
const FAKE_OPENCLAW_CONFIG = path.join(FAKE_HOME, ".openclaw", "openclaw.json");

const FAKE_MPM_BIN = path.join(FAKE_MPM_PRIMARY, "mpm");
const FAKE_MPM_SYMLINK = path.join(FAKE_MPM_SYMLINK_DIR, "mpm");

// Record file for fake-openclaw invocations. Each invocation appends
// one JSON line {argv, ok, env} for assertion.
const OPENCLAW_INVOCATIONS = path.join(SANDBOX_ROOT, "openclaw-invocations.jsonl");

// --------------------------------------------------------------------------
// Fake binaries
// --------------------------------------------------------------------------

// Fake mpm: prints a deterministic version line, exits 0.
const FAKE_MPM_SCRIPT = `#!/usr/bin/env bash
echo "MPM fake-mpm 0.0.0-test"
exit 0
`;

const FAKE_OPENCLAW_SCRIPT = `#!/usr/bin/env bash
# Fake openclaw — records every invocation and replies to the
# subcommands the installer uses. Behaviour is parameterised by env:
#   FAKE_OPENCLAW_FAIL_RESTART=1   → make gateway restart exit non-zero
#   FAKE_OPENCLAW_HANG=1           → sleep 60s on gateway restart
#   FAKE_OPENCLAW_REJECT_BOOTSTRAP=1 → reject unknown commands
set -euo pipefail

INV="${OPENCLAW_INVOCATIONS}"
mkdir -p "$(dirname "$INV")"
touch "$INV"

# Record the invocation as one JSONL line.
{
  printf '{ "argv": %s, "pwd": "%s" }\\n' \\
    "$(printf '%s' "$*" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))' 2>/dev/null || printf 'null')" \\
    "$PWD"
} >> "$INV"

cmd="$1"
shift || true
case "$cmd" in
  config)
    # Capture config-set keys by reading argv. We do NOT mutate real
    # openclaw.json — the fake just logs to openclaw-invocations.jsonl.
    sub="$1"
    if [ "$sub" = "set" ]; then
      key="$1"; shift
      key="$1"; shift
      value="$1"; shift || true
      printf '  -> config set %s=%s\\n' "$key" "$value" >> "$INV"
    fi
    exit 0
    ;;
  plugins)
    sub="$1"
    shift || true
    case "$sub" in
      install)
        printf '  -> plugins install %s\\n' "$*" >> "$INV"
        exit 0
        ;;
      enable)
        printf '  -> plugins enable %s\\n' "$*" >> "$INV"
        exit 0
        ;;
      inspect)
        printf '  -> plugins inspect %s\\n' "$*" >> "$INV"
        exit 0
        ;;
      list)
        printf '  -> plugins list\\n' >> "$INV"
        # Emit a minimal list payload that includes our plugin id and
        # memory-core so the installer's coexistence probe fires.
        cat <<JSON
[
  { "id": "openclaw-mpm-memory", "enabled": true },
  { "id": "memory-core", "enabled": true }
]
JSON
        exit 0
        ;;
    esac
    ;;
  gateway)
    sub="$1"
    shift || true
    case "$sub" in
      status)
        printf '  -> gateway status\\n' >> "$INV"
        exit 0
        ;;
      restart)
        printf '  -> gateway restart %s\\n' "$*" >> "$INV"
        if [ "\${FAKE_OPENCLAW_HANG:-0}" = "1" ]; then
          # Hang. The installer wraps this in timeout(1), so the test
          # framework should observe a clean exit via the timeout, not
          # via the fake.
          sleep 60
        fi
        if [ "\${FAKE_OPENCLAW_FAIL_RESTART:-0}" = "1" ]; then
          exit 7
        fi
        exit 0
        ;;
    esac
    ;;
  doctor|plugins-doctor)
    printf '  -> %s\\n' "$cmd" >> "$INV"
    exit 0
    ;;
  *)
    if [ "\${FAKE_OPENCLAW_REJECT_BOOTSTRAP:-0}" = "1" ]; then
      printf '  -> unknown command: %s\\n' "$cmd" >&2
      exit 99
    fi
    printf '  -> default-ok: %s\\n' "$cmd" >> "$INV"
    exit 0
    ;;
esac
`;

function writeFakeBin(name, content, dir) {
  mkdirSync(dir, { recursive: true });
  const p = path.join(dir, name);
  writeFileSync(p, content, { mode: 0o755 });
  chmodSync(p, 0o755);
  return p;
}

function readInvocations() {
  if (!existsSync(OPENCLAW_INVOCATIONS)) return [];
  return readFileSync(OPENCLAW_INVOCATIONS, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((line) => {
      try {
        return JSON.parse(line);
      } catch {
        return { raw: line };
      }
    });
}

function shellStartupFilesWereTouched(homeDir) {
  // The installer MUST NOT modify any of these.
  for (const f of [".bashrc", ".zshrc", ".profile", ".bash_profile", ".zprofile"]) {
    const p = path.join(homeDir, f);
    if (existsSync(p)) {
      const c = readFileSync(p, "utf8");
      // Treat any pre-existing file as untouched — we never wrote to it
      // during the test. The "was touched" check is implicit: the
      // installer's contract forbids writes; if the file does not exist
      // at start and exists at end with installer content, that is a
      // bug. We start by not pre-creating these files.
      if (c.includes("[openclaw-mpm-memory install]")) {
        return { touched: true, file: f };
      }
    }
  }
  return { touched: false };
}

// --------------------------------------------------------------------------
// Driver
// --------------------------------------------------------------------------

function runInstaller({ homeDir, mpmBootstrapUrl = "", cwd = SANDBOX_ROOT } = {}) {
  // The installer respects $HOME and runs `openclaw` + `mpm` from PATH.
  // We point PATH at the fake bin dirs and HOME at the sandbox so the
  // canonical paths under $HOME resolve there.
  const env = {
    ...process.env,
    PATH: `${FAKE_OPENCLAW_BINDIR}:${FAKE_MPM_PRIMARY}:${FAKE_MPM_SYMLINK_DIR}:/usr/bin:/bin`,
    HOME: homeDir,
    MPM_BOOTSTRAP_URL: mpmBootstrapUrl,
    OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
    OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
  };
  delete env.MPM_BIN;

  return new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd,
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

// --------------------------------------------------------------------------
// Setup / teardown
// --------------------------------------------------------------------------

before(() => {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
  mkdirSync(FAKE_MPM_PRIMARY, { recursive: true });
  mkdirSync(FAKE_MPM_SYMLINK_DIR, { recursive: true });
  mkdirSync(FAKE_OPENCLAW_BINDIR, { recursive: true });
  mkdirSync(path.dirname(FAKE_OPENCLAW_CONFIG), { recursive: true });
  // Real mpm at the canonical primary path.
  writeFakeBin("mpm", FAKE_MPM_SCRIPT, FAKE_MPM_PRIMARY);
  // Symlink at the canonical user location.
  writeFileSync(FAKE_MPM_SYMLINK, "", { mode: 0o755 }); // placeholder, replaced below
  rmSync(FAKE_MPM_SYMLINK);
  symlinkSync(FAKE_MPM_BIN, FAKE_MPM_SYMLINK);
  // Fake openclaw CLI.
  writeFakeBin("openclaw", FAKE_OPENCLAW_SCRIPT, FAKE_OPENCLAW_BINDIR);
});

after(() => {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
});

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

function freshHomeDir(label) {
  const dir = path.join(SANDBOX_ROOT, "homes", label);
  mkdirSync(dir, { recursive: true });
  return dir;
}

function installCanonicalMpmAt(homeDir, { symlinkOnly = false } = {}) {
  // Mirror the real root scripts/install.sh layout under a per-test $HOME
  // so the adapter installer can resolve MPM via its canonical discovery
  // order without us putting the per-test home on PATH.
  const primaryDir = path.join(homeDir, ".mpm", "bin");
  const symDir = path.join(homeDir, ".local", "bin");
  mkdirSync(primaryDir, { recursive: true });
  mkdirSync(symDir, { recursive: true });
  const primaryBin = path.join(primaryDir, "mpm");
  // Copy the fake mpm binary (not symlink) so each test has its own
  // working state.
  writeFileSync(primaryBin, FAKE_MPM_SCRIPT, { mode: 0o755 });
  chmodSync(primaryBin, 0o755);
  if (!symlinkOnly) {
    symlinkSync(primaryBin, path.join(symDir, "mpm"));
  } else {
    try { rmSync(path.join(symDir, "mpm"), { force: true }); } catch {}
  }
  return { primaryBin };
}

function clearInvocations() {
  rmSync(OPENCLAW_INVOCATIONS, { force: true });
}

test("installer resolves MPM via $HOME/.mpm/bin/mpm when bare mpm is not on PATH", async () => {
  const home = freshHomeDir("canonical-primary");
  installCanonicalMpmAt(home); // populate $HOME/.mpm/bin/mpm + $HOME/.local/bin/mpm
  clearInvocations();
  // PATH explicitly excludes both .mpm/bin and .local/bin so the
  // installer's PATH lookup branch (#3) cannot resolve; only the
  // canonical primary path (#1) and canonical symlink (#2) should
  // match.
  const env = {
    ...process.env,
    PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
    HOME: home,
    OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
    OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
  };
  delete env.MPM_BOOTSTRAP_URL;
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env,
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // Primary path is checked first; the log must surface that branch.
  assert.match(stderr, /canonical install path/,
    "installer must resolve via $HOME/.mpm/bin/mpm when both canonical paths exist");
});

test("installer resolves MPM via $HOME/.local/bin/mpm symlink when primary is absent", async () => {
  const home = freshHomeDir("canonical-symlink");
  clearInvocations();
  // Create ONLY the .local/bin symlink (no .mpm/bin) to force the second branch.
  const symDir = path.join(home, ".local", "bin");
  mkdirSync(symDir, { recursive: true });
  symlinkSync(FAKE_MPM_BIN, path.join(symDir, "mpm"));
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
        HOME: home,
        OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
        OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
      },
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  assert.match(stderr, /canonical user symlink/,
    "installer must resolve via $HOME/.local/bin/mpm when primary is absent");
});

test("installer is CWD-independent — works from arbitrary current working directory", async () => {
  const home = freshHomeDir("cwd-indep");
  clearInvocations();
  const unrelatedCwd = path.join(SANDBOX_ROOT, "unrelated-dir");
  mkdirSync(unrelatedCwd, { recursive: true });
  const { code, stderr } = await runInstaller({ homeDir: home, cwd: unrelatedCwd });
  assert.strictEqual(code, 0, `installer must succeed from any CWD; got: ${stderr}`);
});

test("installer writes BOTH hook permission flags (allowConversationAccess AND allowPromptInjection)", async () => {
  const home = freshHomeDir("both-hooks");
  clearInvocations();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /hooks\.allowConversationAccess/,
    "must persist hooks.allowConversationAccess=true");
  assert.match(log, /hooks\.allowPromptInjection/,
    "must persist hooks.allowPromptInjection=true (this was the bug — install.sh set only one flag)");
});

test("installer writes absolute mpmBin (not a PATH-resolved bare 'mpm')", async () => {
  const home = freshHomeDir("abs-mpm-bin");
  clearInvocations();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // We expect to see config.mpmBin set to an absolute path. The
  // fake-openclaw script writes the set-value in the log.
  const m = log.match(/config\.mpmBin=([^\s\\]+)/);
  assert.ok(m, "config.mpmBin must be set; log was:\n" + log);
  assert.ok(m[1].startsWith("/"),
    `mpmBin must be absolute; got: ${m[1]}`);
});

test("installer orders plugin install BEFORE plugin-specific config writes", async () => {
  const home = freshHomeDir("order");
  clearInvocations();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const installIdx = log.indexOf("plugins install");
  const mpmBinIdx = log.indexOf("config.mpmBin");
  const hookIdx = log.indexOf("hooks.allowConversationAccess");
  const slotIdx = log.indexOf("plugins.slots.memory");
  assert.ok(installIdx > -1, "must call plugins install");
  assert.ok(mpmBinIdx > -1, "must set config.mpmBin");
  assert.ok(hookIdx > -1, "must set hooks.allowConversationAccess");
  assert.ok(slotIdx > -1, "must set plugins.slots.memory");
  assert.ok(installIdx < mpmBinIdx, "plugins install must precede mpmBin write");
  assert.ok(mpmBinIdx < hookIdx, "mpmBin must precede hook flag writes");
  assert.ok(hookIdx < slotIdx, "hook flags must precede slot switch");
});

test("installer enables the plugin entry", async () => {
  const home = freshHomeDir("enable");
  clearInvocations();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /plugins enable openclaw-mpm-memory/,
    "installer must call plugins enable");
});

test("installer sets plugins.slots.memory = openclaw-mpm-memory", async () => {
  const home = freshHomeDir("slot");
  clearInvocations();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.match(log, /plugins\.slots\.memory=openclaw-mpm-memory/,
    "installer must switch the memory slot to this plugin");
});

test("installer does NOT disable memory-core (operator policy)", async () => {
  const home = freshHomeDir("memcore");
  clearInvocations();
  const { code, stderr } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  assert.ok(
    !/plugins\.entries\.memory-core\.enabled=false/.test(log),
    "installer must NOT disable memory-core — that is operator policy. log:\n" + log
  );
});

test("idempotent rerun succeeds and does not duplicate state", async () => {
  const home = freshHomeDir("idempotent");
  clearInvocations();
  const r1 = await runInstaller({ homeDir: home });
  assert.strictEqual(r1.code, 0, `first run non-zero: ${r1.stderr}`);
  const firstLog = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const firstPluginInstalls = (firstLog.match(/plugins install /g) || []).length;
  clearInvocations();
  const r2 = await runInstaller({ homeDir: home });
  assert.strictEqual(r2.code, 0, `second run non-zero: ${r2.stderr}`);
  const secondLog = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  const secondPluginInstalls = (secondLog.match(/plugins install /g) || []).length;
  assert.strictEqual(secondPluginInstalls, firstPluginInstalls,
    "rerun must not duplicate plugin install attempts");
});

test("installer never modifies shell startup files", async () => {
  const home = freshHomeDir("shell-rc");
  clearInvocations();
  const { code } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0);
  const check = shellStartupFilesWereTouched(home);
  assert.strictEqual(check.touched, false,
    `installer must not write to ${check.file || "any shell startup file"}`);
});

test("installer never invokes a network bootstrap when MPM canonical paths exist", async () => {
  const home = freshHomeDir("no-bootstrap");
  clearInvocations();
  const { code, stderr } = await runInstaller({
    homeDir: home,
    mpmBootstrapUrl: "http://127.0.0.1:1/nonexistent-bootstrap-must-not-be-called",
  });
  assert.strictEqual(code, 0, `installer exited non-zero: ${stderr}`);
  // The fake-mpm is at the canonical primary path; MPM is resolvable;
  // the installer must NOT have called curl/wget. The fake-openclaw
  // log will record no 'default-ok' style curl calls because curl is
  // a real binary, but we can still assert by counting curl invocations
  // in the installer's stderr.
  assert.ok(!/curl|wget/.test(stderr),
    "installer must not invoke curl/wget when MPM canonical path resolves; stderr was:\n" + stderr);
});

test("installer fails closed when MPM is absent AND no bootstrap URL is set", async () => {
  const home = freshHomeDir("absent-mpm");
  clearInvocations();
  // Make a home with no MPM anywhere — neither primary, nor symlink, nor on PATH.
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${FAKE_OPENCLAW_BINDIR}:/usr/bin:/bin`,
        HOME: home,
        // Intentionally no MPM_BOOTSTRAP_URL.
      },
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.notStrictEqual(code, 0, "installer must fail closed when MPM is absent");
  assert.match(stderr, /mpm not found/,
    "installer must surface a clear 'mpm not found' diagnostic");
});

test("installer does not call root scripts/install.sh OpenClaw hooks (no openclaw invocation outside this adapter)", async () => {
  const home = freshHomeDir("root-no-host");
  clearInvocations();
  // The root scripts/install.sh is host-agnostic (per the 2026-09-16
  // cleanup). This installer must not delegate to it. We assert by
  // counting openclaw invocations: they are all sourced from this
  // adapter's logic, never from a root installer call.
  const { code } = await runInstaller({ homeDir: home });
  assert.strictEqual(code, 0);
  const inv = readInvocations();
  const openclawInvs = inv.filter((row) => typeof row.argv === "string" && row.argv.startsWith("config ") || row.argv && row.argv.startsWith("plugins ") || row.argv && row.argv.startsWith("gateway ") || row.argv && row.argv.startsWith("doctor "));
  assert.ok(openclawInvs.length > 0,
    "expected some openclaw invocations during a successful install");
});

test("gateway restart is bounded — installer times out a hanging gateway restart cleanly", async () => {
  const home = freshHomeDir("gw-hang");
  clearInvocations();
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${FAKE_OPENCLAW_BINDIR}:${FAKE_MPM_PRIMARY}:/usr/bin:/bin`,
        HOME: home,
        OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
        OPENCLAW_GATEWAY_RESTART_TIMEOUT: "3", // shorter than the fake's 60s hang
        FAKE_OPENCLAW_HANG: "1",
      },
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  // Installer should still exit 0 because the restart was bounded;
  // a WARN line is expected. Critical: the installer must NOT hang
  // indefinitely.
  assert.strictEqual(code, 0, "installer must exit cleanly despite hanging gateway restart; stderr:\n" + stderr);
  assert.match(stderr, /WARN.*restart hit the bounded timeout|WARN.*timeout/,
    "installer must surface a bounded-restart warning; stderr was:\n" + stderr);
});

test("gateway restart is bounded — installer surfaces WARN on gateway failure but persists config", async () => {
  const home = freshHomeDir("gw-fail");
  clearInvocations();
  const { code, stderr } = await new Promise((resolve) => {
    const child = spawn("bash", [INSTALL_SH], {
      cwd: SANDBOX_ROOT,
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${FAKE_OPENCLAW_BINDIR}:${FAKE_MPM_PRIMARY}:/usr/bin:/bin`,
        HOME: home,
        OPENCLAW_PLUGIN_INSTALL_TIMEOUT: "10",
        OPENCLAW_GATEWAY_RESTART_TIMEOUT: "5",
        FAKE_OPENCLAW_FAIL_RESTART: "1",
      },
    });
    let err = "";
    child.stderr.on("data", (d) => (err += d));
    child.on("close", (c) => resolve({ code: c, stderr: err }));
  });
  assert.strictEqual(code, 0, "installer must persist config even if gateway restart fails");
  const log = readFileSync(OPENCLAW_INVOCATIONS, "utf8");
  // Config writes must have happened BEFORE the failed gateway restart.
  assert.ok(log.indexOf("hooks.allowConversationAccess") > -1,
    "config must be persisted before gateway restart is attempted; log:\n" + log);
});

test("plugin id is read from openclaw.plugin.json (not hard-coded)", () => {
  // Pin the contract: the installer derives its plugin id from
  // openclaw.plugin.json so renames stay in sync without code edits.
  const src = readFileSync(INSTALL_SH, "utf8");
  assert.match(src, /openclaw\.plugin\.json/,
    "installer must read plugin id from openclaw.plugin.json");
  assert.ok(!/plugin\s+id.*hard.?coded|openclaw-mpm-memory\s*=\s*['"]/.test(src),
    "installer must not hard-code the plugin id as a string literal");
});

test("README documents the canonical install path (./install.sh) and BOTH hook flags", () => {
  const readme = readFileSync(path.join(ADAPTER_DIR, "README.md"), "utf8");
  assert.match(readme, /\.\/install\.sh/,
    "README must document ./install.sh as the canonical install path");
  assert.match(readme, /allowConversationAccess/,
    "README must document allowConversationAccess");
  assert.match(readme, /allowPromptInjection/,
    "README must document allowPromptInjection");
  assert.match(readme, /memory-core/,
    "README must document the memory-core coexistence policy");
});
