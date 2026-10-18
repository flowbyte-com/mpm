// tests/test_resolve_mpm_binary.test.ts — Node test runner for
// agent_installation/mpm-pi/src/resolve-mpm-binary.ts.
//
// The defect these pin: every Pi call site resolved
// `opts.mpmBin ?? "mpm"`. Pi is frequently launched from a desktop entry
// or wrapper script that never sourced a login shell, so `mpm` could be
// correctly installed under ~/.mpm/bin and still not on PATH — and the
// resulting spawn error said "Is the mpm binary on PATH?", sending the
// operator to inspect a PATH that was never the cause. The OpenCode
// adapter had already been fixed for exactly this failure; Pi had not
// inherited the fix.
//
// Invariants pinned here:
//
//   D1  $HOME/.mpm/bin/mpm resolves when mpm is absent from PATH
//   D2  $MPM_WORKSPACE/bin/mpm resolves ahead of $HOME/.mpm
//   D3  $HOME/.local/bin/mpm resolves (symlink followed to real path)
//   D4  an explicit MPM_BINARY that resolves wins over everything
//   D5  an explicit MPM_BINARY that does NOT resolve fails closed
//       (returned verbatim, never silently replaced)
//   D6  `command -v mpm` is the last-resort PATH fallback
//   D7  genuine absence yields the literal "mpm" and unresolved=true
//   D8  the attempt log names every location checked
//   D9  provenance (MPM_PROVENANCE_FRAMEWORK=pi) is untouched
//   D10 index.ts no longer hardcodes "mpm" as a call-site fallback
//
// The sandbox is hermetic: HOME points at a tempdir, MPM_WORKSPACE is
// unset unless a test sets it, and the `command` lookup is stubbed so no
// real PATH entry can influence a result.
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import {
  mkdirSync,
  writeFileSync,
  chmodSync,
  rmSync,
  symlinkSync,
  realpathSync,
  readFileSync,
} from "node:fs";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const ADAPTER_DIR = path.resolve(__dirname, "..");

const SANDBOX = path.join(
  os.tmpdir(),
  `mpm-pi-binary-${process.pid}-${Date.now()}`,
);
const HOME = path.join(SANDBOX, "home");
const WORKSPACE = path.join(SANDBOX, "relocated");
const PATH_BIN = path.join(SANDBOX, "pathbin");
const REAL_TARGET = path.join(SANDBOX, "real-mpm");

/** Write an executable stand-in for the mpm binary. */
function fakeMpm(at: string): string {
  mkdirSync(path.dirname(at), { recursive: true });
  writeFileSync(at, "#!/bin/sh\necho mpm\n");
  chmodSync(at, 0o755);
  return at;
}

/** A `command -v` stub that always fails (mpm genuinely not on PATH). */
const noPath = (() => {
  throw new Error("not on PATH");
}) as unknown as typeof import("node:child_process").execFileSync;

/** A `command -v` stub that reports a fixed PATH location. */
function stubPathLookup(hit: string) {
  return ((_cmd: string, _args: string[]) => hit) as unknown as typeof import("node:child_process").execFileSync;
}

let resolveMpmBinary: typeof import("../src/resolve-mpm-binary.ts").resolveMpmBinary;
let describeDiscovery: typeof import("../src/resolve-mpm-binary.ts").describeDiscovery;

before(async () => {
  // Import the TypeScript source directly. Pi loads extensions through
  // jiti, which compiles TS on the fly; this mirrors that rather than
  // requiring a build step the Pi adapter deliberately does not have.
  const mod = await import("../src/resolve-mpm-binary.ts");
  resolveMpmBinary = mod.resolveMpmBinary;
  describeDiscovery = mod.describeDiscovery;
});

after(() => {
  rmSync(SANDBOX, { recursive: true, force: true });
});

// D1
test("D1: canonical $HOME/.mpm/bin/mpm resolves when mpm is absent from PATH", () => {
  const expected = fakeMpm(path.join(HOME, ".mpm", "bin", "mpm"));
  const r = resolveMpmBinary({ HOME }, noPath);
  assert.equal(r.binary, realpathSync(expected));
  assert.equal(r.unresolved, false);
});

// D2
test("D2: $MPM_WORKSPACE/bin/mpm resolves ahead of $HOME/.mpm", () => {
  fakeMpm(path.join(HOME, ".mpm", "bin", "mpm"));
  const wsMpm = fakeMpm(path.join(WORKSPACE, "bin", "mpm"));
  const r = resolveMpmBinary({ HOME, MPM_WORKSPACE: WORKSPACE }, noPath);
  assert.equal(r.binary, realpathSync(wsMpm));
});

// D2b
test("D2b: an empty MPM_WORKSPACE falls back to $HOME/.mpm", () => {
  const expected = fakeMpm(path.join(HOME, ".mpm", "bin", "mpm"));
  const r = resolveMpmBinary({ HOME, MPM_WORKSPACE: "   " }, noPath);
  assert.equal(r.binary, realpathSync(expected));
});

// D3
test("D3: $HOME/.local/bin/mpm symlink resolves through to its real target", () => {
  // Isolated HOME: D1 created a $HOME/.mpm/bin/mpm that would otherwise
  // win discovery before the symlink is ever reached.
  const symlinkHome = path.join(SANDBOX, "symlink-home");
  mkdirSync(symlinkHome, { recursive: true });
  const target = fakeMpm(REAL_TARGET);
  mkdirSync(path.join(symlinkHome, ".local", "bin"), { recursive: true });
  symlinkSync(target, path.join(symlinkHome, ".local", "bin", "mpm"));
  const r = resolveMpmBinary({ HOME: symlinkHome }, noPath);
  assert.equal(r.binary, realpathSync(target));
});

// D4
test("D4: explicit MPM_BINARY wins over canonical paths", () => {
  fakeMpm(path.join(HOME, ".mpm", "bin", "mpm"));
  const override = fakeMpm(path.join(PATH_BIN, "mpm"));
  const r = resolveMpmBinary(
    { HOME, MPM_BINARY: override },
    noPath,
  );
  assert.equal(r.binary, realpathSync(override));
});

// D5
test("D5: an unresolvable MPM_BINARY fails closed instead of falling back", () => {
  // The operator named a specific binary. Silently using a different one
  // would be worse than failing — it would attribute Pi's writes to a
  // workspace the operator did not choose.
  fakeMpm(path.join(HOME, ".mpm", "bin", "mpm"));
  const missing = "/nonexistent/path/to/mpm";
  const r = resolveMpmBinary({ HOME, MPM_BINARY: missing }, noPath);
  assert.equal(r.binary, missing);
  assert.equal(r.unresolved, true);
});

// D5b
test("D5b: a non-executable MPM_BINARY is not accepted as a valid override", () => {
  const notExec = path.join(PATH_BIN, "not-exec-mpm");
  mkdirSync(PATH_BIN, { recursive: true });
  writeFileSync(notExec, "#!/bin/sh\necho mpm\n");
  chmodSync(notExec, 0o644); // not executable
  const r = resolveMpmBinary({ HOME, MPM_BINARY: notExec }, noPath);
  assert.equal(r.unresolved, true);
});

// D6
test("D6: PATH lookup is the last-resort fallback", () => {
  const emptyHome = path.join(SANDBOX, "empty-home");
  mkdirSync(emptyHome, { recursive: true });
  const onPath = path.join(PATH_BIN, "mpm");
  fakeMpm(onPath);
  const r = resolveMpmBinary(
    { HOME: emptyHome },
    stubPathLookup(onPath),
  );
  assert.equal(r.binary, onPath);
  assert.equal(r.unresolved, false);
});

// D7
test("D7: genuine absence yields the literal 'mpm' and unresolved=true", () => {
  const emptyHome = path.join(SANDBOX, "no-mpm-home");
  mkdirSync(emptyHome, { recursive: true });
  const r = resolveMpmBinary({ HOME: emptyHome }, noPath);
  assert.equal(r.binary, "mpm");
  assert.equal(r.unresolved, true);
});

// D8
test("D8: the discovery log names every location checked", () => {
  // The old message said only "Is the mpm binary on PATH?", which is the
  // wrong diagnosis for the failure this resolver exists to fix.
  const emptyHome = path.join(SANDBOX, "logged-home");
  mkdirSync(emptyHome, { recursive: true });
  const r = resolveMpmBinary({ HOME: emptyHome }, noPath);
  const described = describeDiscovery(r);
  assert.match(described, /\.mpm/, "log must mention the canonical install root");
  assert.match(described, /\.local\/bin/, "log must mention the user symlink");
  assert.match(described, /PATH/, "log must mention the PATH lookup");
  assert.match(described, /missing/, "log must distinguish missing from found");
});

// D9
test("D9: provenance contract is untouched by binary resolution", () => {
  // Binary discovery must not interfere with the provenance env that
  // stamps MPM_PROVENANCE_FRAMEWORK=pi on every spawned call.
  const workspaceSrc = readFileSync(
    path.join(ADAPTER_DIR, "src", "workspace.ts"),
    "utf8",
  );
  assert.match(
    workspaceSrc,
    /MPM_PROVENANCE_FRAMEWORK: MPM_FRAMEWORK_ID/,
    "provenance stamping must remain in workspace.ts",
  );
  assert.match(workspaceSrc, /MPM_FRAMEWORK_ID = "pi"/);
  // resolve-mpm-binary.ts must not set provenance of its own.
  const binarySrc = readFileSync(
    path.join(ADAPTER_DIR, "src", "resolve-mpm-binary.ts"),
    "utf8",
  );
  assert.doesNotMatch(
    binarySrc,
    /MPM_PROVENANCE_FRAMEWORK\s*[:=]/,
    "provenance belongs to workspace.ts, not the binary resolver",
  );
});

// D10
test("D10: index.ts uses the resolved binary, not a hardcoded 'mpm'", () => {
  const src = readFileSync(
    path.join(ADAPTER_DIR, "index.ts"),
    "utf8",
  );
  assert.doesNotMatch(
    src,
    /opts\.mpmBin \?\? "mpm"/,
    "call sites must fall back to RESOLVED_MPM_BINARY, not the literal 'mpm'",
  );
  assert.match(src, /resolveMpmBinary/);
  assert.match(src, /RESOLVED_MPM_BINARY\.binary/);
});