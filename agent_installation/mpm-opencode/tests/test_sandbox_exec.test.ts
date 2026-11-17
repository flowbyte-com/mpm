/**
 * mpm-opencode — the sandboxed execution tool's contract.
 *
 * The reviewer's isolation is CAPABILITY REMOVAL: the definition is
 * default-deny, and the ONE action it re-opens is `sandbox_exec`, a
 * host-controlled tool that runs every command inside a bubblewrap
 * mount namespace.
 *
 * These tests drive the tool's REAL implementation against DISPOSABLE
 * sentinels. They are the TypeScript half of the same contract the
 * Python suite pins: the namespace's shape
 * (`tests/test_reviewer_bwrap_sandbox.py`), the route closure and the
 * install's fail-closed behaviour (`tests/test_reviewer_isolation.py`),
 * and the reviewer's actual execution path
 * (`tests/test_reviewer_sandbox_exec_path.py`).
 *
 * The properties that matter:
 *
 *   1. the command is confined — a sentinel outside the sandbox is
 *      unreachable, the operator's home is unreachable, the trust store
 *      is unreachable, and there is no network;
 *   2. the command is DATA — an option-injecting vector is refused, and
 *      a command that begins with an option is refused;
 *   3. there is NO unsandboxed fallback — when the sandbox cannot be
 *      established, the command does not run, and the model is told so;
 *   4. the tool has no privileged mode.
 *
 * Run with: node --test --test-reporter=tap tests-build/test_sandbox_exec.test.js
 *
 * @ts-nocheck — the canonical Makefile gate compiles this file via
 * `tsc -p tsconfig.tests.json` (strict:false); the production source
 * under src/ is type-checked under the strict tsconfig.json.
 */

import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync, mkdirSync } from "node:fs";
import { spawnSync } from "node:child_process";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const ADAPTER = path.join(__dirname, "..");

function loadSandboxModule() {
  const file = path.join(ADAPTER, "dist", "sandbox-exec.js");
  if (!existsSync(file)) {
    throw new Error(
      `missing compiled sandbox module: ${file} — the adapter payload (dist/) is built ` +
        `by the gate before the tests run`,
    );
  }
  return import(file);
}

function bwrapUsable(): boolean {
  const probe = spawnSync(
    "/usr/bin/bwrap",
    ["--ro-bind", "/", "/", "--unshare-all", "--unshare-user", "--die-with-parent", "/bin/true"],
    { timeout: 30_000 },
  );
  return probe.status === 0;
}

const SANDBOX_AVAILABLE = bwrapUsable();

test("the tool's name is the one the reviewer definition opens", async () => {
  const mod = await loadSandboxModule();
  const definition = path.join(
    ADAPTER,
    "templates",
    "agents",
    "mpm-reviewer.md",
  );
  const text = readFileSync(definition, "utf8");
  assert.ok(
    text.includes(`action: ${mod.SANDBOX_EXEC_TOOL}`),
    "the reviewer definition does not open the tool the plugin registers",
  );
  assert.ok(
    text.includes('action: "*"'),
    "the reviewer definition has no default deny",
  );
});

test("a command that is not an array is refused", async () => {
  const mod = await loadSandboxModule();
  const manager = new mod.SandboxManager({
    base: mkdtempSync(path.join(os.tmpdir(), "mpm-sbx-ts-")),
    protect: [],
  });
  const envelope = await manager.run([] as string[]);
  assert.equal(envelope.ok, false);
  assert.match(envelope.error ?? "", /array/i);
  assert.match(mod.renderEnvelope(envelope), /REFUSED/);
});

test("a working directory outside the sandbox is refused", async () => {
  const mod = await loadSandboxModule();
  const manager = new mod.SandboxManager({
    base: mkdtempSync(path.join(os.tmpdir(), "mpm-sbx-ts-")),
    protect: [],
  });
  const envelope = await manager.run(["/bin/true"], { cwd: "/etc" });
  assert.equal(envelope.ok, false);
});

test("a missing sandbox script fails closed", async () => {
  const mod = await loadSandboxModule();
  const base = mkdtempSync(path.join(os.tmpdir(), "mpm-sbx-ts-"));
  const marker = path.join(base, "ran-on-the-host");
  const manager = new mod.SandboxManager({
    base,
    protect: [],
    script: path.join(base, "no-such-script.py"),
  });
  const envelope = await manager.run(["/bin/sh", "-c", `touch ${marker}`]);
  assert.equal(envelope.ok, false);
  assert.equal(existsSync(marker), false, "the command ran on the host");
  rmSync(base, { recursive: true, force: true });
});

test(
  "a command inside the sandbox cannot reach a disposable sentinel",
  { skip: SANDBOX_AVAILABLE ? false : "bubblewrap cannot build a namespace here" },
  async () => {
    const mod = await loadSandboxModule();
    const root = mkdtempSync(path.join(os.tmpdir(), "mpm-sbx-ts-"));
    const source = path.join(root, "checkout");
    mkdirSync(source, { recursive: true });
    writeFileSync(path.join(source, "README.md"), "# the repository\n");
    const protectedDir = path.join(root, "protected");
    mkdirSync(protectedDir, { recursive: true });
    writeFileSync(
      path.join(protectedDir, "secret.txt"),
      "DISPOSABLE SENTINEL - not real data\n",
    );

    const manager = new mod.SandboxManager({
      base: path.join(root, "sandbox"),
      source,
      protect: [source, protectedDir],
    });

    // The sentinel is unreachable.
    const escape = await manager.run(["/bin/cat", path.join(protectedDir, "secret.txt")]);
    assert.notEqual(escape.exit_code, 0);
    assert.ok(
      !(escape.stdout ?? "").includes("DISPOSABLE SENTINEL"),
      "the sentinel was READ from inside the namespace",
    );

    // The operator's home is unreachable.
    const home = spawnSync("python3", ["-c", "import pwd,os;print(pwd.getpwuid(os.getuid()).pw_dir)"], {
      encoding: "utf8",
    }).stdout.trim();
    if (home) {
      const homeProbe = await manager.run(["/bin/ls", home]);
      assert.notEqual(homeProbe.exit_code, 0);
    }

    // The trust store is unreachable.
    const ssl = await manager.run(["/bin/ls", "/etc/ssl"]);
    assert.notEqual(ssl.exit_code, 0);

    // There is no network.
    const net = await manager.run(
      ["/bin/sh", "-c", "getent hosts example.com >/dev/null 2>&1"],
    );
    assert.notEqual(net.exit_code, 0);

    // The disposable copy of the repository IS present, and writable.
    const present = await manager.run(["/bin/cat", "/sandbox/repo/README.md"]);
    assert.equal(present.exit_code, 0, present.stderr);
    assert.match(present.stdout ?? "", /the repository/);
    const writable = await manager.run(
      ["/bin/sh", "-c", "touch /sandbox/findings/f && echo WROTE"],
    );
    assert.match(writable.stdout ?? "", /WROTE/);

    // The option-injection vector is refused.
    const injected = await manager.run(["--bind", "/", "/", "/bin/ls", "/"]);
    assert.equal(injected.ok, false);

    rmSync(root, { recursive: true, force: true });
  },
);

test("the tool has no privileged mode", async () => {
  const mod = await loadSandboxModule();
  const source = readFileSync(
    path.join(ADAPTER, "src", "sandbox-exec.ts"),
    "utf8",
  );
  for (const forbidden of ["UNSANDBOXED", "noSandbox", "skipSandbox", "bypass"]) {
    assert.ok(
      !source.includes(forbidden),
      `the tool has an unsandboxed mode named ${forbidden}`,
    );
  }
  assert.ok(
    !source.includes("execSync("),
    "the tool runs a command without the sandbox",
  );
});
