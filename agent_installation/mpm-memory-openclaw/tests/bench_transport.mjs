// tests/bench_transport.mjs — §21/§22 benchmark harness.
//
// Compares the canonical `mpm call` subprocess transport against the
// persistent mpm-mcp transport on the same hermetic workspace, the same
// query, N=60, three runs.
//
// Reports per run: cold first call, warm median, warm p95, and — the
// structural result the tranche actually cares about — how many `mpm`
// and `mpm-mcp` processes each transport launched. Wall-clock numbers on
// a shared host are noisy; the process counts are the invariant.
//
// Usage:
//   MPM_TEST_MPM_BIN=... MPM_TEST_MCP_BIN=... \
//   MPM_BENCH_WS=/tmp/... node tests/bench_transport.mjs

import path from "node:path";
import fs from "node:fs";
import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

import { createMemoryTransport, resolveMcpBin } from "../lib/memory-transport.js";
import { withWorkspace } from "../lib/workspace.js";

const MPM_BIN = process.env.MPM_TEST_MPM_BIN;
const MCP_BIN = process.env.MPM_TEST_MCP_BIN;
const WS = process.env.MPM_BENCH_WS;
const N = Number(process.env.MPM_BENCH_N || 60);
const RUNS = Number(process.env.MPM_BENCH_RUNS || 3);
const QUERY = { action: "query", params: { query: "scheduler", limit: 5, scope: "all" } };

if (!MPM_BIN || !MCP_BIN || !WS) {
  console.error("set MPM_TEST_MPM_BIN, MPM_TEST_MCP_BIN and MPM_BENCH_WS");
  process.exit(2);
}
const env = withWorkspace({ ...process.env, MPM_WORKSPACE: WS, MPM_LOG_FORMAT: "json" });

const percentile = (sorted, p) =>
  sorted[Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1)];
const median = (sorted) =>
  sorted.length % 2
    ? sorted[(sorted.length - 1) / 2]
    : (sorted[sorted.length / 2 - 1] + sorted[sorted.length / 2]) / 2;

/**
 * Observe process EXECUTION, not process existence.
 *
 * Counting processes before and after the benchmark measures nothing:
 * a subprocess child has already exited by the time you look, so the
 * difference is always zero. Instead, sample /proc during the run and
 * record every distinct PID seen with the given process name. For the
 * subprocess transport each child lives ~20 ms, which a 2 ms sampler
 * cannot miss; for the persistent transport the single child lives for
 * the whole run and shows up as exactly one PID.
 */
function makeProcSampler(names, intervalMs = 2) {
  const seen = new Map(names.map((n) => [n, new Set()]));
  let timer = null;
  const sample = () => {
    let entries;
    try {
      entries = fs.readdirSync("/proc");
    } catch {
      return;
    }
    for (const p of entries) {
      if (!/^\d+$/.test(p)) continue;
      let comm;
      try {
        comm = fs.readFileSync(`/proc/${p}/comm`, "utf8").trim();
      } catch {
        continue; // vanished mid-scan
      }
      const bucket = seen.get(comm);
      if (bucket) bucket.add(Number(p));
    }
  };
  return {
    start() {
      sample();
      timer = setInterval(sample, intervalMs);
      // Sampling must not hold the event loop open.
      if (typeof timer.unref === "function") timer.unref();
    },
    stop() {
      if (timer) clearInterval(timer);
      timer = null;
      sample();
      return Object.fromEntries([...seen].map(([k, v]) => [k, v.size]));
    },
    counts() {
      return Object.fromEntries([...seen].map(([k, v]) => [k, v.size]));
    },
  };
}

/**
 * One canonical `mpm call` subprocess, exactly as the plugin's
 * callMpmTool issues it.
 *
 * This is deliberately a REAL spawn rather than a stub. A stub would
 * return without launching anything, so the structural process counter
 * could never detect a regression that quietly reintroduced a per-call
 * subprocess (probe A). In the healthy path the transport never falls
 * back, so this function does not run and cannot affect the timing
 * baseline it is compared against.
 */
function subprocessCall(tool, payload) {
  return new Promise((resolve) => {
    const envelope =
      payload?.params && typeof payload.params === "object" ? payload : { ...payload, params: {} };
    const c = spawn(MPM_BIN, ["call", tool, "--payload", JSON.stringify(envelope)], {
      stdio: ["ignore", "pipe", "pipe"], env, shell: false,
    });
    let out = "";
    c.stdout.on("data", (d) => (out += d));
    c.on("error", () => resolve({ success: false }));
    c.on("close", () => {
      const lines = out.split("\n");
      for (let i = lines.length - 1; i >= 0; i--) {
        const t = lines[i].trim();
        if (t.startsWith("{") && t.endsWith("}")) {
          try { return resolve(JSON.parse(t)); } catch { /* keep scanning */ }
        }
      }
      resolve({ success: false });
    });
  });
}

/** The canonical subprocess transport, timed per call. */
async function benchSubprocess() {
  const call = subprocessCall;

  const sampler = makeProcSampler(["mpm", "mpm-mcp"]);
  sampler.start();
  const samples = [];
  try {
    for (let i = 0; i < N; i++) {
      const t = process.hrtime.bigint();
      const r = await call("mpm_memory", QUERY);
      const ms = Number(process.hrtime.bigint() - t) / 1e6;
      if (r.success !== true) throw new Error("subprocess call failed");
      samples.push(ms);
    }
  } finally {
    var procCounts = sampler.stop();
  }
  return { samples, launches: procCounts.mpm, mcpLaunches: procCounts["mpm-mcp"] };
}

/** The persistent transport, timed per call. */
async function benchMcp() {
  const transport = createMemoryTransport({
    mpmBin: MPM_BIN,
    mcpBin: resolveMcpBin(MPM_BIN) || MCP_BIN,
    timeoutMs: 15000,
    log: { warn: () => {} },
    callMpmTool: subprocessCall,
    env,
  });
  const sampler = makeProcSampler(["mpm", "mpm-mcp"]);
  sampler.start();
  const samples = [];
  try {
    for (let i = 0; i < N; i++) {
      const t = process.hrtime.bigint();
      const r = await transport.call("mpm_memory", QUERY);
      const ms = Number(process.hrtime.bigint() - t) / 1e6;
      if (r.success !== true) throw new Error(`mcp call failed: ${JSON.stringify(r).slice(0, 200)}`);
      samples.push(ms);
    }
  } finally {
    await transport.close();
    var procCounts = sampler.stop();
  }
  return {
    samples,
    launches: procCounts.mpm,
    mcpLaunches: procCounts["mpm-mcp"],
    stats: { ...transport.stats },
  };
}

function summarize(samples) {
  const s = [...samples].sort((a, b) => a - b);
  return {
    cold: Number(samples[0].toFixed(2)),
    median: Number(median(s).toFixed(2)),
    p95: Number(percentile(s, 95).toFixed(2)),
    min: Number(s[0].toFixed(2)),
  };
}

const fmt = (s) => `cold ${s.cold}ms | warm median ${s.median}ms | warm p95 ${s.p95}ms | min ${s.min}ms`;

console.log(`benchmark: N=${N} runs=${RUNS} workspace=${WS}`);
console.log(`mpm    = ${MPM_BIN}`);
console.log(`mpm-mcp= ${resolveMcpBin(MPM_BIN) || MCP_BIN}\n`);

const subMedians = [];
const mcpMedians = [];
let subTotalMpm = 0;
let mcpTotalMcp = 0;
let mcpTotalChild = 0;

for (let run = 1; run <= RUNS; run++) {
  const sub = await benchSubprocess();
  const ss = summarize(sub.samples);
  subMedians.push(ss.median);
  if (run === 1) subTotalMpm = sub.launches;

  const mcp = await benchMcp();
  const ms = summarize(mcp.samples);
  mcpMedians.push(ms.median);
  if (run === 1) { mcpTotalMcp = mcp.launches; mcpTotalChild = mcp.mcpLaunches; }

  console.log(`run ${run}`);
  console.log(`  subprocess  ${fmt(ss)}  mpm pids observed=${sub.launches} for ${N} reads`);
  console.log(`  mcp         ${fmt(ms)}  mpm pids observed=${mcp.launches}  mpm-mcp pids observed=${mcp.mcpLaunches}  (mcpCalls=${mcp.stats.mcpCalls}, subprocessCalls=${mcp.stats.subprocessCalls})`);
}

const med = (a) => median([...a].sort((x, y) => x - y));
console.log(`\nacross ${RUNS} runs`);
console.log(`  subprocess warm median: ${med(subMedians).toFixed(2)} ms`);
console.log(`  mcp        warm median: ${med(mcpMedians).toFixed(2)} ms`);
console.log(`  speedup: ${(med(subMedians) / med(mcpMedians)).toFixed(2)}x`);
console.log(`\nstructural result (PIDs observed under /proc during the run, run 1)`);
console.log(`  subprocess: ${subTotalMpm} mpm processes for ${N} reads — one per call`);
console.log(`  mcp       : ${mcpTotalMcp} mpm processes for ${N} reads, ${mcpTotalChild} mpm-mcp process`);
if (mcpTotalMcp !== 0) {
  console.error(`  FAIL: the fast path still launched mpm subprocesses`);
  process.exitCode = 1;
}
if (subTotalMpm < N) {
  console.error(`  FAIL: expected ~${N} mpm launches on the subprocess path, observed ${subTotalMpm}`);
  process.exitCode = 1;
}