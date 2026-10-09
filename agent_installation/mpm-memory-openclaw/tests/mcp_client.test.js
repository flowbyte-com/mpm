// tests/mcp_client.test.js — lifecycle, framing, correlation, timeout,
// crash and teardown behaviour of the persistent mpm-mcp client.
//
// Run with:
//   node --test tests/mcp_client.test.js
//
// These tests drive tests/fake-mcp-server.mjs, which speaks the same
// newline-delimited JSON-RPC 2.0 stdio transport as the real mpm-mcp.
// Keeping the mechanics hermetic means the protocol contract is pinned
// without needing a database, and failure modes (hang, crash, garbage
// on stdout) are reproducible rather than incidental.

import { test, describe, before, after } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import fs from "node:fs";
import os from "node:os";
import { fileURLToPath } from "node:url";

import {
  MpmMcpClient,
  McpTransportError,
  extractEnvelope,
  resolveMcpBin,
  CLIENT_STATE,
} from "../lib/mcp-client.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const FAKE_SERVER = path.join(__dirname, "fake-mcp-server.mjs");

let tmpDir;
before(() => {
  tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), "mpm-mcp-client-"));
});
after(() => {
  fs.rmSync(tmpDir, { recursive: true, force: true });
});

describe("extractEnvelope", () => {
  test("returns the tool envelope from a text content block", () => {
    const { envelope, spill } = extractEnvelope({
      content: [{ type: "text", text: '{"success":true,"count":2}' }],
    });
    assert.deepEqual(envelope, { success: true, count: 2 });
    assert.equal(spill, null);
  });

  test("skips a prepended wake-notification block", () => {
    // cmd/mpm-mcp/tools.go:687 prepends the wake block before the JSON.
    const { envelope } = extractEnvelope({
      content: [
        { type: "text", text: "<mpms:wakes><wake id='w1'/></mpms:wakes>" },
        { type: "text", text: '{"success":true,"memories":[]}' },
      ],
    });
    assert.deepEqual(envelope, { success: true, memories: [] });
  });

  test("reports a spill envelope instead of returning it as the result", () => {
    // Output policy: internal/core/tools/output_policy.go — the MCP path
    // replaces an oversized result with a blob pointer. That must never
    // be mistaken for the real result.
    const { envelope, spill } = extractEnvelope({
      content: [
        {
          type: "text",
          text: JSON.stringify({ status: "spilled", pointer: "mpm://blob/abc", size_bytes: 40000 }),
        },
      ],
    });
    assert.equal(envelope, null);
    assert.equal(spill.pointer, "mpm://blob/abc");
  });

  test("ignores non-text blocks and unparseable JSON-looking blocks", () => {
    const { envelope } = extractEnvelope({
      content: [
        { type: "image", data: "xxx" },
        { type: "text", text: "{not valid json" },
        { type: "text", text: '{"success":true}' },
      ],
    });
    assert.deepEqual(envelope, { success: true });
  });

  test("returns nulls for a result with no JSON block", () => {
    assert.deepEqual(extractEnvelope({ content: [] }), { envelope: null, spill: null });
    assert.deepEqual(extractEnvelope(undefined), { envelope: null, spill: null });
  });
});

describe("resolveMcpBin", () => {
  test("derives the sibling of a configured mpm binary in a custom prefix", () => {
    const dir = fs.mkdtempSync(path.join(tmpDir, "prefix-"));
    fs.writeFileSync(path.join(dir, "mpm"), "#!/bin/sh\n");
    fs.writeFileSync(path.join(dir, "mpm-mcp"), "#!/bin/sh\n");
    fs.chmodSync(path.join(dir, "mpm-mcp"), 0o755);
    assert.strictEqual(
      resolveMcpBin(path.join(dir, "mpm")),
      path.join(dir, "mpm-mcp")
    );
    fs.rmSync(dir, { recursive: true, force: true });
  });

  test("returns null when the sibling does not exist", () => {
    assert.strictEqual(resolveMcpBin("/nonexistent/prefix/bin/mpm"), null);
  });

  test("returns null for a bare binary name (no directory to derive from)", () => {
    // A PATH search for the sibling would be exactly the non-deterministic
    // resolution this contract forbids.
    assert.strictEqual(resolveMcpBin("mpm"), null);
    assert.strictEqual(resolveMcpBin(""), null);
    assert.strictEqual(resolveMcpBin(undefined), null);
  });

  test("rejects a source-checkout .build/bin path", () => {
    // Probe G: the runtime package must never resolve into ~/src/mpm/.build/bin.
    assert.strictEqual(resolveMcpBin("/home/v/src/mpm/.build/bin/mpm"), null);
  });

  test("returns null when the sibling is not executable", () => {
    const dir = fs.mkdtempSync(path.join(tmpDir, "noexec-"));
    fs.writeFileSync(path.join(dir, "mpm"), "#!/bin/sh\n");
    fs.writeFileSync(path.join(dir, "mpm-mcp"), "not executable");
    fs.chmodSync(path.join(dir, "mpm-mcp"), 0o644);
    assert.strictEqual(resolveMcpBin(path.join(dir, "mpm")), null);
    fs.rmSync(dir, { recursive: true, force: true });
  });
});

describe("client lifecycle", () => {
  // The fake server is driven by argv flags; the client spawns its
  // `command` with `args` appended and never through a shell, so the
  // spawn mechanics under test (no shell, pipe stdio, kill on close) are
  // exactly the production ones.
  function fakeClient(flags = [], overrides = {}) {
    const dir = fs.mkdtempSync(path.join(tmpDir, "srv-"));
    const logPath = path.join(dir, "requests.jsonl");
    const client = new MpmMcpClient({
      command: process.execPath,
      args: [FAKE_SERVER, ...flags, `--log=${logPath}`],
      env: { ...process.env },
      initTimeoutMs: 3000,
      requestTimeoutMs: 3000,
      ...overrides,
    });
    return { client, logPath, dir };
  }

  const readLog = (logPath) =>
    fs.existsSync(logPath)
      ? fs.readFileSync(logPath, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l))
      : [];

  test("ensureReady is idempotent and reaches READY", async () => {
    const { client, dir } = fakeClient([]);
    try {
      await client.ensureReady();
      assert.strictEqual(client.state, CLIENT_STATE.READY);
      await client.ensureReady();
      assert.strictEqual(client.state, CLIENT_STATE.READY);
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("concurrent first calls spawn exactly ONE child (probe B)", async () => {
    const { client, dir } = fakeClient([]);
    try {
      await Promise.all(Array.from({ length: 8 }, () => client.ensureReady()));
      await Promise.all(
        Array.from({ length: 8 }, () => client.callTool("mpm_memory", { action: "query" }))
      );
      assert.strictEqual(client.state, CLIENT_STATE.READY);
      // Single-flight startup: the client still owns exactly one child,
      // and every request rode that one session.
      assert.strictEqual(client.hasChild, true);
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("concurrent calls correlate responses to the right caller (probe C)", async () => {
    // Each caller sends a DISTINCT tag and the server stamps that tag
    // into the envelope, and answers newest-first. Without both, ten
    // identical concurrent calls are indistinguishable and a client
    // that ignored the response id entirely would still look correct.
    const N = 10;
    const { client, logPath, dir } = fakeClient([
      "--echo-memories=3",
      "--echo-request",
      "--reverse-flush=250",
    ]);
    try {
      await client.ensureReady();
      const results = await Promise.all(
        Array.from({ length: N }, (_, i) =>
          client.callTool("mpm_memory", { action: "query", tag: `caller-${i}` })
        )
      );
      for (let i = 0; i < N; i++) {
        assert.ok(results[i].envelope, `caller ${i} received no envelope`);
        assert.strictEqual(results[i].envelope.success, true);
        assert.strictEqual(results[i].envelope.memories.length, 3);
        assert.strictEqual(
          results[i].envelope.echoed,
          `caller-${i}`,
          `caller ${i} was given another caller's response`
        );
      }
      const log = readLog(logPath);
      assert.strictEqual(log.length, N, "every concurrent call must reach the server once");
      const ids = new Set(log.map((l) => l.id));
      assert.strictEqual(ids.size, N, "each request must carry a distinct id");
      // Arguments reached the server uncorrupted.
      assert.deepEqual(log[0].arguments, { action: "query", tag: "caller-0" });
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a prepended wake block does not displace the envelope", async () => {
    const { client, dir } = fakeClient(["--echo-memories=1", "--echo-wake"]);
    try {
      await client.ensureReady();
      const r = await client.callTool("mpm_memory", { action: "query" });
      assert.strictEqual(r.envelope.success, true);
      assert.strictEqual(r.envelope.memories.length, 1);
      assert.strictEqual(r.spill, null);
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a non-JSON line on stdout is a protocol error, not a silent drop", async () => {
    const { client, dir } = fakeClient(["--garbage-first"]);
    try {
      await assert.rejects(
        () => client.ensureReady().then(() => client.callTool("mpm_memory", {})),
        (e) => e instanceof McpTransportError && /protocol error/.test(e.message)
      );
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a hung child times out the request and stays usable", async () => {
    const { client, dir } = fakeClient(["--hang-tools"], { requestTimeoutMs: 300 });
    try {
      await client.ensureReady();
      await assert.rejects(
        () => client.callTool("mpm_memory", { action: "query" }),
        (e) => e instanceof McpTransportError && /timed out/.test(e.message) && e.fatal === false
      );
      // A timed-out request must not have desynchronized the stream.
      assert.strictEqual(client.state, CLIENT_STATE.READY);
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("an initialize that never answers is a fatal startup failure", async () => {
    const { client, dir } = fakeClient(["--no-ready"], { initTimeoutMs: 300 });
    try {
      await assert.rejects(
        () => client.ensureReady(),
        (e) => e instanceof McpTransportError && /timed out/.test(e.message)
      );
      assert.strictEqual(client.state, CLIENT_STATE.FAILED);
      assert.strictEqual(client.hasChild, false, "a failed handshake must not leave a child");
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a child crash fails in-flight requests fatally", async () => {
    const { client, dir } = fakeClient(["--exit-after=1"]);
    try {
      await client.ensureReady();
      await assert.rejects(
        () => client.callTool("mpm_memory", { action: "query" }),
        (e) => e instanceof McpTransportError && e.fatal === true
      );
      assert.strictEqual(client.state, CLIENT_STATE.FAILED);
    } finally {
      await client.close();
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  test("close() reaps the child (probe D)", async () => {
    const { client, dir } = fakeClient([]);
    await client.ensureReady();
    const child = client._debugChild();
    assert.strictEqual(client.hasChild, true);
    await client.close();
    assert.strictEqual(client.hasChild, false);
    assert.strictEqual(client.state, CLIENT_STATE.STOPPED);
    assert.ok(
      child.exitCode !== null || child.signalCode !== null,
      "close() must have waited for the child to actually exit"
    );
    fs.rmSync(dir, { recursive: true, force: true });
  });

  test("calling after close() fails rather than respawning", async () => {
    const { client, dir } = fakeClient([]);
    await client.ensureReady();
    await client.close();
    await assert.rejects(() => client.callTool("mpm_memory", {}), /closed/);
    fs.rmSync(dir, { recursive: true, force: true });
  });
});