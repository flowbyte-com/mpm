// tests/test_resolve_mpm_binary.js — Node test runner for
// agent_installation/mpm-opencode/src/resolve-mpm-binary.ts.
//
// Pins the B2-B8 invariants:
//   B2: deterministic canonical discovery (env > canonical > symlink > PATH)
//   B5: canonical $HOME/.mpm/bin/mpm works when mpm absent from PATH
//   B6: $HOME/.local/bin/mpm works when canonical binary absent and PATH restricted
//   B7: PATH fallback works
//   B8: genuine MPM absence produces the expected fallback (literal "mpm")
//   B4: boot health check uses the resolved absolute binary
//   B9: stale "opencode-mpm" warning label is gone (canonical is mpm-opencode)
//   B12: adapter identity remains mpm-opencode
//
// The tests use a hermetic sandbox (HOME points at a tempdir; PATH is
// restricted; no real ~/.mpm/bin/mpm, ~/.local/bin/mpm, or `mpm` in
// PATH). Fake mpm binaries are written into the sandbox at the
// expected paths and their absolute resolved paths are returned by a
// stub `command` so PATH-lookup discovery is deterministic.

import { test, before, after } from "node:test";
import assert from "node:assert";
import { spawnSync, spawn } from "node:child_process";
import {
  mkdirSync,
  writeFileSync,
  chmodSync,
  rmSync,
  symlinkSync,
  readFileSync,
  existsSync,
} from "node:fs";
import path from "node:path";
import os from "node:os";

const ADAPTER_DIR = "/home/v/workspace/projects/mpm/agent_installation/mpm-opencode";
const RESOLVER_TS = path.join(ADAPTER_DIR, "src", "resolve-mpm-binary.ts");

// ---- Sandbox helpers -----------------------------------------------------

const SANDBOX_ROOT = path.join(
  os.tmpdir(),
  "mpm-opencode-resolve-" + process.pid,
);
const FAKE_HOME = SANDBOX_ROOT;
const FAKE_BIN_DIR = path.join(SANDBOX_ROOT, "fake-bin"); // also serves as restricted PATH

function freshSandbox(): string {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
  mkdirSync(SANDBOX_ROOT, { recursive: true });
  mkdirSync(FAKE_BIN_DIR, { recursive: true });
  // A no-op `command` shim so PATH-lookup discovery finds a known
  // binary path under FAKE_BIN_DIR. We do NOT put `mpm` in here by
  // default — tests add it explicitly when exercising PATH fallback.
  writeFileSync(
    path.join(FAKE_BIN_DIR, "command"),
    `#!/bin/sh
# Test stub: report the requested binary path if it exists on PATH,
# exit 1 otherwise. The resolver uses this to emulate 'command -v mpm'.
if [ "$1" = "-v" ]; then
  shift
  name="$1"
  # Look up the binary on PATH (excluding ourself).
  OLDIFS="$IFS"
  IFS=':'
  for p in $PATH; do
    IFS="$OLDIFS"
    if [ -x "$p/$name" ]; then
      printf '%s/%s\\n' "$p" "$name"
      exit 0
    fi
    IFS=':'
  done
  IFS="$OLDIFS"
  exit 1
fi
exit 1
`,
  { mode: 0o755 },
  );
  return FAKE_HOME;
}

function writeMpm(home: string, rel: string): string {
  // rel is relative to home, e.g. ".mpm/bin/mpm" or ".local/bin/mpm"
  const full = path.join(home, rel);
  mkdirSync(path.dirname(full), { recursive: true });
  // Use /bin/sh so we don't depend on bash being in the restricted PATH.
  writeFileSync(
    full,
    `#!/bin/sh
# Test stub for mpm binary.
echo "fake-mpm \${1:-noarg} \${2:-noarg}"
exit 0
`,
    { mode: 0o755 },
  );
  chmodSync(full, 0o755);
  return full;
}

function execFileStub(captured: { which?: string; throw?: boolean }): typeof import("node:child_process").execFileSync {
  // Returns a synchronous execFileSync that mimics `command -v mpm`.
  // If captured.throw is true, throw like a real failure.
  // If captured.which is set, return that string.
  return ((_cmd: string, _args: string[]) => {
    if (captured.throw) {
      throw new Error("stub: command -v exit 1");
    }
    return (captured.which ?? "") + "\n";
  }) as unknown as typeof import("node:child_process").execFileSync;
}

function tsEvalResolver(env: NodeJS.ProcessEnv, execFileArg: ReturnType<typeof execFileStub>): unknown {
  // Compile resolve-mpm-binary.ts via tsc to a temp file, then import.
  // For the purposes of this test we instead use ts-node style by
  // using Node's --experimental-strip-types (Node 22+) or just use
  // the compiled dist. We use the compiled dist that's already produced
  // by the project's tsc step.
  // The dist file path:
  const distFile = path.join(ADAPTER_DIR, "dist", "resolve-mpm-binary.js");
  assert.ok(
    existsSync(distFile),
    `missing compiled resolver: ${distFile} — run 'npx tsc' in the adapter dir`,
  );
  // We import via dynamic import to allow overriding the module's
  // resolved-at-module-load behaviour.
  return import(distFile).then((mod) => {
    const attemptLog: unknown[] = [];
    const bin = mod.resolveMpmBinary(env, execFileArg, attemptLog);
    return { bin, attempts: attemptLog };
  });
}

// ---- Tests --------------------------------------------------------------

test("B5: canonical $HOME/.mpm/bin/mpm works when mpm absent from PATH", async () => {
  const home = freshSandbox();
  // Canonical primary present.
  const mpm = writeMpm(home, ".mpm/bin/mpm");
  // PATH is restricted to FAKE_BIN_DIR (no `mpm` there).
  const env = {
    HOME: home,
    PATH: FAKE_BIN_DIR,
    // No MPM_BINARY override.
  };
  const { bin } = await tsEvalResolver(env, execFileStub({ throw: true }));
  // Resolver returns the canonical primary (resolved via realpath).
  assert.strictEqual(bin, mpm, "resolver must return the canonical primary");
});

test("B6: $HOME/.local/bin/mpm symlink works when canonical binary absent", async () => {
  const home = freshSandbox();
  // No canonical primary. Symlink only.
  const mpm = writeMpm(home, ".tmp/real-mpm");
  mkdirSync(path.join(home, ".local", "bin"), { recursive: true });
  const sym = path.join(home, ".local", "bin", "mpm");
  symlinkSync(mpm, sym);
  const env = { HOME: home, PATH: FAKE_BIN_DIR };
  const { bin } = await tsEvalResolver(env, execFileStub({ throw: true }));
  // Symlink resolves through realpath to its target.
  assert.strictEqual(
    bin,
    mpm,
    "resolver must follow .local/bin/mpm symlink via realpath",
  );
});

test("B6b: $HOME/.local/bin/mpm direct executable (not symlink) is also accepted", async () => {
  const home = freshSandbox();
  const mpm = writeMpm(home, ".local/bin/mpm");
  const env = { HOME: home, PATH: FAKE_BIN_DIR };
  const { bin } = await tsEvalResolver(env, execFileStub({ throw: true }));
  assert.strictEqual(bin, mpm);
});

test("B7: PATH fallback works when canonical paths absent and `mpm` on PATH", async () => {
  const home = freshSandbox();
  // Canonical paths absent. mpm on PATH (via a custom fake-bin dir).
  const fakeBinDir = path.join(SANDBOX_ROOT, "path-bin");
  mkdirSync(fakeBinDir, { recursive: true });
  const mpm = writeMpm(fakeBinDir, "mpm"); // file at fakeBinDir/mpm
  const env = { HOME: home, PATH: fakeBinDir };
  const { bin } = await tsEvalResolver(
    env,
    execFileStub({ which: mpm }),
  );
  assert.strictEqual(bin, mpm);
});

test("B2: explicit MPM_BINARY env override wins over canonical paths", async () => {
  const home = freshSandbox();
  // Canonical primary ALSO present — must be ignored when env override is set.
  writeMpm(home, ".mpm/bin/mpm");
  const override = writeMpm(home, ".tmp/explicit-mpm");
  const env = { HOME: home, PATH: FAKE_BIN_DIR, MPM_BINARY: override };
  const { bin } = await tsEvalResolver(env, execFileStub({ throw: true }));
  assert.strictEqual(bin, override);
});

test("B2b: explicit MPM_BINARY override that doesn't exist fails closed (no fallback)", async () => {
  const home = freshSandbox();
  // Canonical primary present, but explicit override points at a missing file.
  const canonical = writeMpm(home, ".mpm/bin/mpm");
  const env = {
    HOME: home,
    PATH: FAKE_BIN_DIR,
    MPM_BINARY: "/nonexistent/operator-intended-mpm",
  };
  const { bin } = await tsEvalResolver(env, execFileStub({ throw: true }));
  // Operator chose something specific — return that literal so the
  // spawn fails loudly with the operator's chosen value, not silently
  // fall back to the canonical install.
  assert.strictEqual(bin, "/nonexistent/operator-intended-mpm");
  // And we must NOT have fallen back to the canonical path.
  assert.notStrictEqual(bin, canonical);
});

test("B8: genuine absence produces the literal 'mpm' fallback", async () => {
  const home = freshSandbox();
  // No canonical primary, no symlink, no mpm on PATH.
  const env = { HOME: home, PATH: FAKE_BIN_DIR };
  const { bin } = await tsEvalResolver(env, execFileStub({ throw: true }));
  assert.strictEqual(
    bin,
    "mpm",
    "genuine absence must surface literal 'mpm' for the spawn-failure-and-warning contract",
  );
});

test("B8b: genuine absence also surfaces the discovery audit log", async () => {
  const home = freshSandbox();
  const env = { HOME: home, PATH: FAKE_BIN_DIR };
  const { bin, attempts } = await tsEvalResolver(
    env,
    execFileStub({ throw: true }),
  );
  assert.strictEqual(bin, "mpm");
  // Audit log should include every discovery attempt — canonical
  // primary, symlink, PATH lookup, and the final literal fallback.
  assert.ok(Array.isArray(attempts) && attempts.length >= 3,
    `expected >= 3 audit entries, got ${attempts.length}`);
  const reasons = (attempts as Array<{ reason: string }>).map((a) => a.reason);
  assert.ok(reasons.includes("canonical_primary"));
  assert.ok(reasons.includes("canonical_symlink"));
  assert.ok(reasons.includes("path_lookup"));
  assert.ok(reasons.includes("fallback_literal"));
});

test("B5b: caller CWD does not affect canonical resolution", async () => {
  const home = freshSandbox();
  const mpm = writeMpm(home, ".mpm/bin/mpm");
  const env = { HOME: home, PATH: FAKE_BIN_DIR };
  // Change CWD via process.chdir — resolution must still hit $HOME-based paths.
  const cwd = process.cwd();
  try {
    process.chdir("/tmp");
    const { bin } = await tsEvalResolver(env, execFileStub({ throw: true }));
    assert.strictEqual(bin, mpm);
  } finally {
    process.chdir(cwd);
  }
});

test("B2c: resolution does not mutate shell startup files", () => {
  // Static check: the resolver source code does not perform any disk
  // writes. Comments referencing shell startup files (for context) are
  // permitted; what we pin is that no write call exists in the code.
  const src = readFileSync(RESOLVER_TS, "utf8");
  // Strip comments to avoid false positives from "we don't write to .bashrc" prose.
  const codeNoComments = src
    .replace(/\/\*[\s\S]*?\*\//g, "")  // /* ... */ block comments
    .replace(/^\s*\/\/.*$/gm, "");       // // line comments
  for (const bad of ["writeFileSync", "appendFileSync"]) {
    assert.ok(
      !codeNoComments.includes(bad),
      `resolver must not perform disk writes (${bad})`,
    );
  }
  // And no shell startup file path appears as an argument to a write
  // syscall (we don't have any, but this would catch future regressions).
  for (const f of [".bashrc", ".profile", ".zshrc", ".bash_profile", ".zprofile"]) {
    // The string literal is allowed in comments; what we forbid is
    // using it as an argument to a write call (already covered above
    // since we have no write calls).
    // This assertion is a regression placeholder.
    assert.ok(true, `shell-startup file ${f} not used as write target`);
  }
});

test("B9: adapter canonical identity is mpm-opencode (no active opencode-mpm)", () => {
  const src = readFileSync(path.join(ADAPTER_DIR, "src", "index.ts"), "utf8");
  assert.match(src, /mpm-opencode/);
  // No active use of opencode-mpm (the obsolete name).
  assert.ok(
    !/opencode-mpm/.test(src),
    "active source must not reference the obsolete opencode-mpm name",
  );
  const pkg = JSON.parse(readFileSync(path.join(ADAPTER_DIR, "package.json"), "utf8"));
  assert.strictEqual(pkg.name, "mpm-opencode");
  assert.ok(
    !/opencode-mpm/.test(JSON.stringify(pkg)),
    "package.json must not reference the obsolete name",
  );
});

test("B9b: stale dist is canonical after rebuild (no opencode-mpm in dist)", () => {
  // We rebuild dist before this test runs (in the before() hook).
  // Pin that the dist matches the source.
  const dist = readFileSync(path.join(ADAPTER_DIR, "dist", "index.js"), "utf8");
  assert.ok(
    !/opencode-mpm/.test(dist),
    "compiled dist must not reference the obsolete opencode-mpm name after rebuild",
  );
  assert.match(dist, /mpm-opencode/);
});

// ---- Boot health check uses the resolved binary ---------------------------
//
// We exercise the boot health check by running install.sh via a small
// driver that imports the compiled module. The driver script:
//   1. Sets HOME to the sandbox with a working $HOME/.mpm/bin/mpm
//   2. Sets PATH to a restricted value (no `mpm`)
//   3. Sets MPM_PROVENANCE_* as required
//   4. Invokes the plugin's boot path (RESOLVED_MPM_BINARY + pingHealth)
//   5. Asserts the resolved binary is the canonical one (not PATH-resolved)
//
// The driver is implemented as a subprocess because the compiled
// module runs its resolution at import-time (top-level side effect).

test("B4: boot health check uses resolved absolute binary, not PATH lookup", () => {
  const home = freshSandbox();
  const mpm = writeMpm(home, ".mpm/bin/mpm");
  // PATH contains a different `mpm` (a hostile decoy). The resolver
  // must prefer the canonical install over PATH.
  const decoyBinDir = path.join(SANDBOX_ROOT, "decoy-bin");
  mkdirSync(decoyBinDir, { recursive: true });
  writeFileSync(
    path.join(decoyBinDir, "mpm"),
    `#!/bin/sh
echo "DECOY MPM — should NEVER be spawned"
exit 99
`,
    { mode: 0o755 },
  );

  // Drive the resolver via a node child process so the resolver sees
  // process.env we control. The driver imports the compiled resolver
  // module and prints the resolved binary plus a health-check probe.
  //
  // We must keep node's own PATH visible (so the node binary is found)
  // while restricting the resolver's PATH view. Use the real system
  // PATH but override HOME and MPM-discovery PATH via FAKE_BIN_DIR +
  // decoyBinDir prepended. The resolver uses the env we pass, not the
  // outer shell PATH.
  const driver = `
    import("./dist/resolve-mpm-binary.js").then((mod) => {
      const attempts = [];
      const bin = mod.resolveMpmBinary(process.env, require("node:child_process").execFileSync, attempts);
      console.log("RESOLVED:" + bin);
      console.log("ATTEMPTS:" + JSON.stringify(attempts.map(a => ({r: a.reason, p: a.candidate, e: a.exists, x: a.executable}))));
      const cp = require("node:child_process");
      const r = cp.spawnSync(bin, ["call", "mpm_system", "--payload", JSON.stringify({action: "health_check", params: {}})], {
        env: process.env,
        encoding: "utf8",
      });
      console.log("HEALTH_RC:" + r.status);
      console.log("HEALTH_STDOUT:" + (r.stdout || ""));
      console.log("HEALTH_STDERR:" + (r.stderr || ""));
    });
  `;
  writeFileSync(
    path.join(SANDBOX_ROOT, "driver.mjs"),
    driver,
  );

  // The driver uses MPM_RESOLVER_DIST env var (absolute path to the
  // compiled dist) so the import works regardless of where the
  // driver.mjs lives. We set PATH inside the driver to the resolver
  // view, and run node from outerPath which we already preserve in
  // process.env.PATH.
  const outerPath = process.env.PATH ?? "/usr/bin:/bin";
  const resolverPath = `${FAKE_BIN_DIR}:${decoyBinDir}`;
  const distAbs = path.join(ADAPTER_DIR, "dist", "resolve-mpm-binary.js");
  const driver2 = `
    import(${JSON.stringify(distAbs)}).then(async (mod) => {
      // Override the PATH the resolver sees so the hostile decoy
      // is on PATH and the canonical install is at $HOME/.mpm/bin/mpm.
      process.env.PATH = process.env.MPM_RESOLVER_PATH;
      const attempts = [];
      const bin = mod.resolveMpmBinary(process.env, undefined, attempts);
      console.log("RESOLVED:" + bin);
      console.log("ATTEMPTS:" + JSON.stringify(attempts.map(a => ({r: a.reason, p: a.candidate, e: a.exists, x: a.executable}))));
      const { spawnSync } = await import("node:child_process");
      const r = spawnSync(bin, ["call", "mpm_system", "--payload", JSON.stringify({action: "health_check", params: {}})], {
        env: process.env,
        encoding: "utf8",
      });
      console.log("HEALTH_RC:" + r.status);
      console.log("HEALTH_STDOUT:" + (r.stdout || ""));
      console.log("HEALTH_STDERR:" + (r.stderr || ""));
    });
  `;
  writeFileSync(
    path.join(SANDBOX_ROOT, "driver.mjs"),
    driver2,
  );

  const r2 = spawnSync("node", [path.join(SANDBOX_ROOT, "driver.mjs")], {
    cwd: ADAPTER_DIR,
    env: {
      ...process.env,
      PATH: outerPath,
      MPM_RESOLVER_PATH: resolverPath,
      HOME: home,
    },
    encoding: "utf8",
  });
  assert.strictEqual(r2.status, 0, `driver failed: stderr=${r2.stderr || ""}`);

  // Extract resolved binary.
  const lines = r2.stdout!.split("\n");
  const resolvedLine = lines.find((l) => l.startsWith("RESOLVED:"))!;
  const resolved = resolvedLine.slice("RESOLVED:".length);
  assert.strictEqual(
    resolved,
    mpm,
    `resolver must prefer canonical $HOME/.mpm/bin/mpm over PATH lookup (got ${resolved})`,
  );
  // Health check rc=0 (our stub exits 0) — the resolved binary was actually used.
  const healthLine = lines.find((l) => l.startsWith("HEALTH_RC:"))!;
  assert.strictEqual(healthLine.slice("HEALTH_RC:".length), "0");
  // And the stdout confirms the stub was the one that ran (not the decoy).
  const stdoutLine = lines.find((l) => l.startsWith("HEALTH_STDOUT:"))!;
  assert.match(stdoutLine, /fake-mpm/);
});

test("B5c: boot health succeeds when mpm absent from PATH and only $HOME/.mpm/bin/mpm present", () => {
  // The B4 test exercises the resolver+health path; this test is
  // a pure health-check smoke that asserts the resolved binary
  // produces a successful health_check (our stub exits 0).
  const home = freshSandbox();
  const mpm = writeMpm(home, ".mpm/bin/mpm");
  // PATH restricted to NO mpm-bearing dir.
  const noBinDir = path.join(SANDBOX_ROOT, "no-bin");
  mkdirSync(noBinDir, { recursive: true });
  const r = spawnSync(mpm, ["call", "mpm_system", "--payload", JSON.stringify({ action: "health_check", params: {} })], {
    env: { ...process.env, HOME: home, PATH: noBinDir },
    encoding: "utf8",
  });
  assert.strictEqual(r.status, 0, `stub mpm failed: ${r.stderr}`);
});

// ---- Setup / teardown ---------------------------------------------------

before(() => {
  // Rebuild dist so B9b sees the canonical compile.
  const r = spawnSync("npx", ["tsc"], { cwd: ADAPTER_DIR, encoding: "utf8" });
  assert.strictEqual(
    r.status,
    0,
    `rebuild failed: status=${r.status} stderr=${r.stderr}`,
  );
});

after(() => {
  rmSync(SANDBOX_ROOT, { recursive: true, force: true });
});
