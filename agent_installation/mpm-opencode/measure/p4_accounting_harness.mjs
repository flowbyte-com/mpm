// measure/p4_accounting_harness.mjs — drive the REAL transport against a
// REAL disposable MPM event store.
//
// The unit tests use a fake transport. This harness does not: it spawns
// the actual `mpm call mpm_system --payload '{"action":"event_ingest"}'`
// against a disposable workspace, so the client's counters are compared
// with what the core actually STORED.
//
// Usage:
//   MPM_BINARY=… MPM_WORKSPACE=… MPM_DB_PATH=… node p4_accounting_harness.mjs [N]
//
// Prints one JSON object on stdout:
//   { "snapshot": {…}, "balance": {…}, "summary": "…" }

import path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ADAPTER = path.resolve(HERE, "..");

const BIN = process.env.MPM_BINARY;
if (!BIN) {
  console.error("MPM_BINARY is required");
  process.exit(2);
}
const WORKSPACE = process.env.MPM_WORKSPACE ?? "";
const DB_PATH = process.env.MPM_DB_PATH ?? "";
const N = Number(process.argv[2] ?? 10);
const DUMP = process.env.MPM_EVENT_DEBUG_DUMP ?? "";

const sub = await import(pathToFileURL(path.join(ADAPTER, "dist", "event-subscriber.js")).href);

/** The same spawn-and-parse shape the plugin uses (src/index.v2.ts). */
function callMpm(toolName, payload, timeoutMs = 15_000) {
  const json = JSON.stringify(payload);
  return new Promise((resolve) => {
    let stdout = "";
    let stderr = "";
    let settled = false;
    const finish = (r) => {
      if (settled) return;
      settled = true;
      resolve(r);
    };
    let child;
    try {
      child = spawn(BIN, ["call", toolName, "--payload", json], {
        stdio: ["ignore", "pipe", "pipe"],
        env: {
          ...process.env,
          MPM_WORKSPACE: WORKSPACE,
          MPM_DB_PATH: DB_PATH,
        },
      });
    } catch (err) {
      finish({ success: false, payload: null, raw: "", stderr: String(err) });
      return;
    }
    child.on("error", (err) => finish({ success: false, payload: null, raw: stdout, stderr: String(err) }));
    child.stdout?.on("data", (c) => (stdout += c.toString()));
    child.stderr?.on("data", (c) => (stderr += c.toString()));
    const timer = setTimeout(() => {
      try {
        child.kill("SIGKILL");
      } catch {
        /* ignore */
      }
    }, timeoutMs);
    timer.unref();
    child.on("close", () => {
      clearTimeout(timer);
      let parsed = null;
      for (let i = stdout.split("\n").length - 1; i >= 0; i--) {
        const t = stdout.split("\n")[i].trim();
        if (t.startsWith("{") && t.endsWith("}")) {
          try {
            parsed = JSON.parse(t);
            break;
          } catch {
            /* keep scanning */
          }
        }
      }
      finish({
        success: parsed !== null && parsed.success !== false,
        payload: parsed,
        raw: stdout.trim(),
        stderr: stderr.trim(),
      });
    });
  });
}

const transport = sub.createMpmSystemTransport({
  call: async (toolName, payload) => {
    const r = await callMpm(toolName, payload);
    return { success: r.success, payload: r.payload };
  },
  retryDelayMs: 50,
});

const observer = sub.createLifecycleObserver({
  subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
  transport,
  mapping: {
    hostVersion: "2.1.4",
    adapterVersion: "measure-harness",
    projectId: "pj_0123456789abcdef0123456789abcdef",
    projectIdSource: "git-remote",
    now: () => 1793047200000,
  },
  debugDumpPath: DUMP || undefined,
});
observer.start();

for (let i = 0; i < N; i++) {
  observer.observe({
    id: `evt_measure_${String(i).padStart(4, "0")}`,
    created: 1793047203717,
    type: "session.tool.success",
    location: { directory: WORKSPACE },
    data: { sessionID: "measure-session", id: `call_${i}` },
  });
}

// MODE 1 (default): let the drain loop finish the queue.
if (process.env.MPM_ACCOUNTING_STOP_IN_FLIGHT !== "1") {
  const deadline = Date.now() + 120_000;
  while (observer.state().queued > 0 || observer.state().in_flight > 0) {
    if (Date.now() > deadline) break;
    await new Promise((r) => setTimeout(r, 5));
  }
} else {
  // MODE 2: reproduce the REPORTED CASE against the real store.
  //
  // Stop the observer while the last delivery is IN FLIGHT — the window
  // the defect report captured. The teardown reading is written
  // synchronously inside stop(), so it records the state at that
  // instant; the delivery then completes and the core stores it.
  const deadline = Date.now() + 120_000;
  while (observer.counters().delivered < N - 1) {
    if (Date.now() > deadline) break;
    await new Promise((r) => setTimeout(r, 2));
  }
  while (observer.state().in_flight !== 1) {
    if (Date.now() > deadline) break;
    await new Promise((r) => setTimeout(r, 2));
  }
}

const snapshot = await observer.stop({ timeoutMs: 5000 });

// In MODE 2 the in-flight delivery is still running after stop() returns;
// give it the time to land, so the core's stored count is the truth.
if (process.env.MPM_ACCOUNTING_STOP_IN_FLIGHT === "1") {
  const deadline = Date.now() + 60_000;
  while (observer.state().in_flight > 0) {
    if (Date.now() > deadline) break;
    await new Promise((r) => setTimeout(r, 5));
  }
}

console.log(
  JSON.stringify({
    snapshot,
    balance: sub.accountingBalance(snapshot),
    summary: sub.summariseSnapshot(snapshot),
    mode: process.env.MPM_ACCOUNTING_STOP_IN_FLIGHT === "1" ? "stop-in-flight" : "drain-all",
  }),
);
