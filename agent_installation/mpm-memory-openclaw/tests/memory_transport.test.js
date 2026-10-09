// tests/memory_transport.test.js — read/write routing, fallback policy,
// semantic equivalence against the canonical subprocess transport,
// freshness across out-of-band writes, and the no-orphan contract.
//
// Run with:
//   MPM_TEST_MPM_BIN=/path/to/mpm MPM_TEST_MCP_BIN=/path/to/mpm-mcp \
//   node --test tests/memory_transport.test.js
//
// The routing/fallback tests use tests/fake-mcp-server.mjs so failure
// injection is deterministic. The equivalence and freshness tests use the
// REAL mpm/mpm-mcp binaries against a throwaway MPM_WORKSPACE, and skip
// when those are not supplied — no test in this file ever touches the
// live ~/.mpm database.

import { test, describe, before, after } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { fileURLToPath } from "node:url";
import { spawn, spawnSync } from "node:child_process";

import {
  createMemoryTransport,
  resolveMcpBin,
  READ_ROUTED_TOOLS,
} from "../lib/memory-transport.js";
import { withWorkspace } from "../lib/workspace.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const MPM_BIN = process.env.MPM_TEST_MPM_BIN || null;
const MCP_BIN = process.env.MPM_TEST_MCP_BIN || null;
const HAVE_REAL = Boolean(MPM_BIN && MCP_BIN && fs.existsSync(MPM_BIN) && fs.existsSync(MCP_BIN));

let tmpDir;
let workspace;

before(() => {
  tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "mpm-transport-"));
  if (HAVE_REAL) {
    workspace = path.join(tmpDir, "ws");
    fs.mkdirSync(workspace, { recursive: true });
    const env = withWorkspace({ ...process.env, MPM_WORKSPACE: workspace });
    for (const text of [
      "scheduler drift detection: mpm-scheduler claims due wakes atomically",
      "openclaw plugin registration declares mpm_memory_search and mpm_memory_get",
      "unicode fixture: café naïve 日本語 🚀 emoji survives transport",
    ]) {
      spawnSync(MPM_BIN, ["add", text, "--tags", "probe"], { env, stdio: "ignore" });
    }
  }
});

after(() => {
  fs.rmSync(tmpDir, { recursive: true, force: true });
});

/**
 * The canonical subprocess transport, as shipped before the fast path.
 *
 * `env` is REQUIRED and must already pin MPM_WORKSPACE. Without this the
 * caller's ambient environment wins, `withWorkspace` falls back to
 * ~/.mpm, and a hermetic test silently reads — and, because recall
 * updates `retrieval_metadata.reuse_count` and writes a
 * `tool_invocations` row, silently MUTATES — the live database.
 */
function subprocessCaller(mpmBin, env, timeoutMs = 15000) {
  if (!env || !env.MPM_WORKSPACE) {
    throw new Error("subprocessCaller requires an env with MPM_WORKSPACE pinned");
  }
  return (tool, payload, opts) =>
    new Promise((resolve) => {
      const envelope =
        payload && typeof payload === "object" && payload.params && typeof payload.params === "object"
          ? payload
          : { ...(payload || {}), params: {} };
      const child = spawn(mpmBin, ["call", tool, "--payload", JSON.stringify(envelope)], {
        stdio: ["ignore", "pipe", "pipe"],
        env,
        shell: false,
      });
      let out = "";
      let err = "";
      const timer = setTimeout(() => {
        try { child.kill("SIGKILL"); } catch { /* dead */ }
      }, timeoutMs);
      child.stdout.on("data", (d) => (out += d));
      child.stderr.on("data", (d) => (err += d));
      child.on("error", (e) => {
        clearTimeout(timer);
        resolve({ success: false, error: `mpm ${tool} failed: ${e.message}` });
      });
      child.on("close", (code) => {
        clearTimeout(timer);
        const lines = out.split("\n");
        for (let i = lines.length - 1; i >= 0; i--) {
          const t = lines[i].trim();
          if (t.startsWith("{") && t.endsWith("}")) {
            try { return resolve(JSON.parse(t)); } catch { /* keep scanning */ }
          }
        }
        const tail = (err || out || "").trim().split("\n").slice(-3).join(" | ").slice(0, 500);
        resolve({ success: false, error: `mpm ${tool} (exit ${code}) — ${tail || "no output"}` });
      });
    });
}

describe("tool routing", () => {
  test("the read-routed tool set is exactly the plugin's read surface", () => {
    // Both memory tools issue mpm_memory action=query; the session hook
    // issues mpm_context; the boot check issues mpm_system. All reads.
    assert.deepEqual([...READ_ROUTED_TOOLS].sort(), ["mpm_context", "mpm_memory", "mpm_system"]);
  });

  test("an unrouted tool never touches the persistent child", async () => {
    let subprocessCalls = 0;
    const transport = createMemoryTransport({
      mpmBin: "/nonexistent/bin/mpm",
      // Deliberately broken: if an unrouted tool ever reached for the
      // persistent child it would fail AND fall back to the subprocess,
      // making subprocessCalls 2 rather than 1.
      mcpBin: "/bin/false",
      timeoutMs: 5000,
      log: { warn: () => {} },
      callMpmTool: () => {
        subprocessCalls += 1;
        return Promise.resolve({ success: true });
      },
    });
    const r = await transport.call("mpm_decision", { action: "add", params: { text: "x" } });
    assert.strictEqual(r.success, true);
    assert.strictEqual(subprocessCalls, 1, "writes must stay on the subprocess path");
    assert.strictEqual(transport.client, null, "no child may be started for an unrouted tool");
    await transport.close();
  });

  test("no mcpBin means every call uses the subprocess path", async () => {
    let subprocessCalls = 0;
    const transport = createMemoryTransport({
      mpmBin: "/nonexistent/bin/mpm",
      mcpBin: null,
      timeoutMs: 5000,
      log: { warn: () => {} },
      callMpmTool: () => {
        subprocessCalls += 1;
        return Promise.resolve({ success: true, count: 0 });
      },
    });
    await transport.call("mpm_memory", { action: "query", params: { query: "x" } });
    await transport.call("mpm_memory", { action: "query", params: { query: "y" } });
    assert.strictEqual(subprocessCalls, 2);
    assert.strictEqual(transport.stats.mcpCalls, 0);
    await transport.close();
  });
});

describe("fallback policy", () => {
  test("a fatal MCP failure falls back to the subprocess for that call", async () => {
    let subprocessCalls = 0;
    const transport = createMemoryTransport({
      mpmBin: "/nonexistent/bin/mpm",
      mcpBin: "/definitely/not/a/real/binary/mpm-mcp",
      timeoutMs: 2000,
      log: { warn: () => {} },
      callMpmTool: () => {
        subprocessCalls += 1;
        return Promise.resolve({ success: true, count: 1, via: "subprocess" });
      },
    });
    const r = await transport.call("mpm_memory", { action: "query", params: { query: "x" } });
    assert.strictEqual(r.via, "subprocess");
    assert.strictEqual(subprocessCalls, 1);
    assert.strictEqual(transport.stats.mcpFailures, 1);
    await transport.close();
  });

  test("the restart budget is bounded and then disables the fast path", async () => {
    let subprocessCalls = 0;
    const transport = createMemoryTransport({
      mpmBin: "/nonexistent/bin/mpm",
      mcpBin: "/definitely/not/a/real/binary/mpm-mcp",
      timeoutMs: 2000,
      log: { warn: () => {} },
      callMpmTool: () => {
        subprocessCalls += 1;
        return Promise.resolve({ success: true });
      },
    });
    // 1st failure -> restart 1 granted. 2nd failure -> budget exhausted.
    await transport.call("mpm_memory", { action: "query", params: {} });
    assert.strictEqual(transport.stats.restarts, 1);
    assert.strictEqual(transport.stats.permanentDisable, false);
    await transport.call("mpm_memory", { action: "query", params: {} });
    assert.strictEqual(transport.stats.permanentDisable, true);
    // Third and later calls must not keep retrying the dead transport.
    const failuresBefore = transport.stats.mcpFailures;
    await transport.call("mpm_memory", { action: "query", params: {} });
    await transport.call("mpm_memory", { action: "query", params: {} });
    assert.strictEqual(transport.stats.mcpFailures, failuresBefore,
      "a permanently-disabled transport must not keep spawning failing children");
    assert.strictEqual(subprocessCalls, 4);
    await transport.close();
  });

  test("close() is safe on a transport that never started a child", async () => {
    const transport = createMemoryTransport({
      mpmBin: "/nonexistent/bin/mpm",
      mcpBin: null,
      timeoutMs: 1000,
      log: { warn: () => {} },
      callMpmTool: () => Promise.resolve({ success: true }),
    });
    await transport.close();
    assert.strictEqual(transport.client, null);
  });
});

describe("no orphan process contract", () => {
  /** Direct children of this test process, from /proc. */
  function liveChildren() {
    const out = [];
    for (const entry of fs.readdirSync("/proc")) {
      if (!/^\d+$/.test(entry)) continue;
      try {
        const status = fs.readFileSync(`/proc/${entry}/status`, "utf8");
        const ppid = /PPid:\s*(\d+)/.exec(status)?.[1];
        if (ppid === String(process.pid)) out.push(Number(entry));
      } catch { /* process vanished mid-scan */ }
    }
    return out;
  }

  test("repeated open/close cycles leave no accumulating children (probe D)", { skip: !HAVE_REAL }, async () => {
    const env = withWorkspace({ ...process.env, MPM_WORKSPACE: workspace });
    const baseline = liveChildren().length;

    for (let i = 0; i < 3; i++) {
      const transport = createMemoryTransport({
        mpmBin: MPM_BIN,
        mcpBin: MCP_BIN,
        timeoutMs: 15000,
        log: { warn: () => {} },
        callMpmTool: subprocessCaller(MPM_BIN, env),
        env,
      });
      const r = await transport.call("mpm_memory", {
        action: "query",
        params: { query: "scheduler", limit: 3, scope: "all" },
      });
      assert.strictEqual(r.success, true, "memory query must succeed through the fast path");
      assert.strictEqual(transport.stats.mcpCalls, 1);
      await transport.close();
      // close() resolves only after `exit`, so nothing may linger.
      const after = liveChildren();
      assert.strictEqual(after.length, baseline,
        `cycle ${i} leaked a child (before=${baseline}, after=${after.length})`);
    }
  });

  test("a failed startup leaves no child behind", { skip: !HAVE_REAL }, async () => {
    const env = withWorkspace({ ...process.env, MPM_WORKSPACE: workspace });
    const baseline = liveChildren().length;
    const transport = createMemoryTransport({
      mpmBin: MPM_BIN,
      // A binary that exists but is not an MCP server: it exits at once.
      mcpBin: "/bin/true",
      timeoutMs: 15000,
      log: { warn: () => {} },
      callMpmTool: subprocessCaller(MPM_BIN, env),
      env,
    });
    const r = await transport.call("mpm_memory", {
      action: "query",
      params: { query: "scheduler", limit: 3, scope: "all" },
    });
    assert.strictEqual(r.success, true, "the subprocess fallback must serve the call");
    await transport.close();
    assert.strictEqual(liveChildren().length, baseline);
  });
});

describe("semantic equivalence (real binaries)", { skip: !HAVE_REAL }, () => {
  const env = () => withWorkspace({ ...process.env, MPM_WORKSPACE: workspace });

  /**
   * Fields that legitimately differ between two retrievals regardless of
   * transport: the substrate records a reuse on every recall. Nothing else
   * may differ.
   */
  const retrievalInherent = new Set(["reuse_count", "last_retrieved_at", "last_used_at"]);

  function normalize(value) {
    return JSON.stringify(value, (key, v) => (retrievalInherent.has(key) ? `<${key}>` : v));
  }

  async function withTransport(fn) {
    const transport = createMemoryTransport({
      mpmBin: MPM_BIN,
      mcpBin: resolveMcpBin(MPM_BIN) || MCP_BIN,
      timeoutMs: 15000,
      log: { warn: () => {} },
      callMpmTool: subprocessCaller(MPM_BIN, env()),
      env: env(),
    });
    try {
      return await fn(transport);
    } finally {
      await transport.close();
    }
  }

  test("resolveMcpBin finds the real sibling of the configured mpm", () => {
    assert.strictEqual(resolveMcpBin(MPM_BIN), MCP_BIN);
  });

  test("search envelope matches the subprocess transport field for field", async () => {
    const payload = { action: "query", params: { query: "scheduler", limit: 3, scope: "all" } };
    const viaMcp = await withTransport((t) => t.call("mpm_memory", payload));
    const viaSub = await subprocessCaller(MPM_BIN, env())("mpm_memory", payload, {});
    assert.strictEqual(viaMcp.success, viaSub.success);
    assert.strictEqual(viaMcp.count, viaSub.count);
    assert.strictEqual(viaMcp.scope, viaSub.scope);
    assert.strictEqual(viaMcp.mode, viaSub.mode);
    assert.strictEqual(normalize(viaMcp), normalize(viaSub),
      "envelopes must agree once retrieval-inherent counters are normalized");
  });

  test("repeated reads are served by one child with no new mpm launches", async () => {
    await withTransport(async (transport) => {
      const payload = { action: "query", params: { query: "scheduler", limit: 3, scope: "all" } };
      await transport.call("mpm_memory", payload); // cold: starts the child
      for (let i = 0; i < 25; i++) {
        await transport.call("mpm_memory", payload);
      }
      assert.strictEqual(transport.stats.mcpCalls, 26);
      assert.strictEqual(transport.stats.subprocessCalls, 0,
        "26 memory reads must not launch a single `mpm` process");
    });
  });

  test("freshness: an out-of-band write is visible to the SAME child (probe E)", async () => {
    await withTransport(async (transport) => {
      const before = await transport.call("mpm_memory", {
        action: "query",
        params: { query: "kryptonite-marker", limit: 5, scope: "all" },
      });
      const idsBefore = new Set((before.memories || []).map((m) => m.id));

      // Write through a completely separate process, as another host or
      // `mpm remember` on the CLI would.
      const writeEnv = env();
      const added = spawnSync(MPM_BIN, [
        "add",
        "kryptonite-marker: out-of-band write proving fresh reads",
        "--tags", "freshness",
      ], { env: writeEnv, encoding: "utf8" });
      assert.ok(added.status === 0, `out-of-band write failed: ${added.stderr}`);

      // Same persistent child, no restart, no cache.
      const after = await transport.call("mpm_memory", {
        action: "query",
        params: { query: "kryptonite-marker", limit: 5, scope: "all" },
      });
      assert.strictEqual(transport.stats.restarts, 0, "the child must not have been recycled");
      const idsAfter = (after.memories || []).map((m) => m.id);
      assert.ok(
        idsAfter.some((id) => !idsBefore.has(id)),
        "the memory written by another process must be visible immediately"
      );
    });
  });

  test("unicode and special characters survive both transports identically", async () => {
    const payload = {
      action: "query",
      params: { query: "café 日本語 🚀", limit: 5, scope: "all" },
    };
    const viaMcp = await withTransport((t) => t.call("mpm_memory", payload));
    const viaSub = await subprocessCaller(MPM_BIN, env())("mpm_memory", payload, {});
    assert.strictEqual(normalize(viaMcp), normalize(viaSub));
    assert.ok(viaMcp.success, "a unicode query must still succeed");
  });

  test("an empty result set matches the subprocess transport", async () => {
    const payload = {
      action: "query",
      params: { query: "zzzz-no-such-token-zzzz", limit: 5, scope: "all" },
    };
    const viaMcp = await withTransport((t) => t.call("mpm_memory", payload));
    const viaSub = await subprocessCaller(MPM_BIN, env())("mpm_memory", payload, {});
    assert.deepEqual(viaMcp.memories, viaSub.memories);
    assert.strictEqual(viaMcp.count, viaSub.count);
  });

  test("a handler-level failure produces the same envelope on both transports", async () => {
    // action=query with no query is an application-level validation
    // failure. Both transports surface it as success:false rather than a
    // transport error, which is what the tool's disabled:true branch reads.
    const payload = { action: "query", params: {} };
    const viaMcp = await withTransport((t) => t.call("mpm_memory", payload));
    const viaSub = await subprocessCaller(MPM_BIN, env())("mpm_memory", payload, {});
    assert.strictEqual(viaMcp.success, viaSub.success);
    assert.ok(viaMcp.error || viaSub.error, "both transports must report the validation failure");
  });

  test("maxResults handling: a larger limit returns more rows, same as the subprocess", async () => {
    const small = { action: "query", params: { query: "probe", limit: 1, scope: "all" } };
    const large = { action: "query", params: { query: "probe", limit: 10, scope: "all" } };
    const r1 = await withTransport((t) => t.call("mpm_memory", small));
    const r10 = await withTransport((t) => t.call("mpm_memory", large));
    const s1 = await subprocessCaller(MPM_BIN, env())("mpm_memory", small, {});
    const s10 = await subprocessCaller(MPM_BIN, env())("mpm_memory", large, {});
    assert.ok(r1.memories.length <= 1);
    assert.ok(r10.memories.length <= 10);
    assert.ok(r10.count >= r1.count, "a larger limit must not return fewer rows");
    assert.strictEqual(normalize(r10), normalize(s10));
  });

  test("concurrent reads through one child all return correct envelopes", async () => {
    await withTransport(async (transport) => {
      const results = await Promise.all([
        transport.call("mpm_memory", { action: "query", params: { query: "scheduler", limit: 3, scope: "all" } }),
        transport.call("mpm_memory", { action: "query", params: { query: "unicode", limit: 3, scope: "all" } }),
        transport.call("mpm_memory", { action: "query", params: { query: "registration", limit: 3, scope: "all" } }),
        transport.call("mpm_memory", { action: "query", params: { query: "probe", limit: 3, scope: "all" } }),
      ]);
      assert.strictEqual(results.length, 4);
      for (const r of results) assert.strictEqual(r.success, true);
      // Distinct queries must come back distinct, correctly-correlated.
      assert.strictEqual(transport.stats.mcpCalls, 4);
      assert.strictEqual(transport.stats.subprocessCalls, 0);
      assert.strictEqual(transport.stats.mcpFailures, 0);
    });
  });
});
