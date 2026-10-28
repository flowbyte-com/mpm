// tests/test_lifecycle_events.test.ts — P4 adapter tests.
//
// Covers the OpenCode lifecycle observer:
//   * envelope construction, strict identity and contract validation;
//   * the STRUCTURAL ALLOWLIST, driven by the REAL captured host events in
//     measure/evidence/event_stream_probe_run*.jsonl (A's ruling: a
//     hand-written shape is not sufficient evidence);
//   * session / tool / compaction mapping;
//   * subscription, bounded cancellation, buffering and honest degradation
//     when the P5 ingest endpoint is absent.
//
// @ts-nocheck — see test_resolve_mpm_binary.test.ts for the rationale.

import { test, before } from "node:test";
import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const ADAPTER_DIR = path.resolve(__dirname, "..");
const EVIDENCE_DIR = path.join(ADAPTER_DIR, "measure", "evidence");

let env: any; // event-envelope
let mapper: any; // event-mapper
let sub: any; // event-subscriber

before(async () => {
  // The adapter payload (dist/) is built ONCE, by `make
  // test-agent-adapters`, BEFORE any test file runs.
  //
  // It used to be built HERE — and again in test_wake_context.test.ts —
  // by each test file's own `before` hook, both writing into the SAME
  // dist/ directory, while `node --test` runs test files in parallel
  // processes. Two compilers writing one output directory is a data
  // race: a reader that catches a file mid-rewrite (tsc truncates
  // before it writes) sees a VALID module with no exports, and the
  // failure names a TypeScript symbol instead of the race:
  //
  //   "The requested module './event-envelope.js' does not provide an
  //    export named 'ADAPTER_SOURCE'"
  //
  // The build belongs to the gate, which owns the order. This hook
  // only READS dist/.
  const required = ["event-envelope.js", "event-mapper.js", "event-subscriber.js"];
  for (const name of required) {
    const p = path.join(ADAPTER_DIR, "dist", name);
    assert.ok(
      existsSync(p),
      `missing compiled adapter module: ${p} — the adapter payload (dist/) is built ` +
        `once by 'make test-agent-adapters' (or by 'npm run build' in ${ADAPTER_DIR}). ` +
        `The test files must NOT build it themselves: two test files building into ` +
        `the same directory race, and the failure names a symbol rather than the race.`,
    );
  }
  env = await import(pathToFileURL(path.join(ADAPTER_DIR, "dist", "event-envelope.js")).href);
  mapper = await import(pathToFileURL(path.join(ADAPTER_DIR, "dist", "event-mapper.js")).href);
  sub = await import(pathToFileURL(path.join(ADAPTER_DIR, "dist", "event-subscriber.js")).href);
});

const FIXED_NOW = 1793047200000;

function mappingCtx(over: any = {}) {
  return {
    hostVersion: "2.1.4",
    adapterVersion: "test-adapter",
    projectId: "pj_0123456789abcdef0123456789abcdef",
    projectIdSource: "git-remote",
    now: () => FIXED_NOW,
    ...over,
  };
}

function hostEvent(type: string, data: any, id = "evt_179f10f85001LYlIygAAyW5UCz") {
  return { id, created: 1793047203717, type, location: { directory: "/tmp/x" }, data };
}

// ---------------------------------------------------------------------------
// Envelope identity
// ---------------------------------------------------------------------------

test("1: strict event_id is deterministic and well-formed", () => {
  const a = env.deriveStrictEventId({ eventType: "tool.outcome", hostEventKey: "evt_abc" });
  const b = env.deriveStrictEventId({ eventType: "tool.outcome", hostEventKey: "evt_abc" });
  assert.equal(a, b, "same inputs must derive the same id");
  assert.match(a, /^ev1_[0-9a-f]{64}$/);
});

test("2: event_type participates in the derivation (A2)", () => {
  const a = env.deriveStrictEventId({ eventType: "tool.outcome", hostEventKey: "evt_x" });
  const b = env.deriveStrictEventId({ eventType: "session.started", hostEventKey: "evt_x" });
  assert.notEqual(a, b, "the same host key under different types must not collide");
});

test("3: length-prefixed framing prevents field-boundary collisions", () => {
  // Without length prefixes, ("ab","c") and ("a","bc") would both encode to
  // "abc"; with them the ids differ.
  const a = env.deriveStrictEventId({ eventType: "ab", hostEventKey: "c" });
  const b = env.deriveStrictEventId({ eventType: "a", hostEventKey: "bc" });
  assert.notEqual(a, b);
});

test("4: best-effort ids are distinct and self-describing", () => {
  const a = env.deriveBestEffortEventId();
  const b = env.deriveBestEffortEventId();
  assert.match(a, /^ev1b_[0-9a-f]{32}$/);
  assert.notEqual(a, b, "each best-effort emission is a distinct observation");
});

test("5: a mapped envelope validates", () => {
  const ev = mapper.mapHostEvent(
    hostEvent("session.tool.success", { sessionID: "ses_a", id: "call_1", assistantMessageID: "msg_1" }),
    mappingCtx(),
  );
  assert.equal(ev.kind, "envelope");
  const v = env.validateEnvelope(ev.envelope);
  assert.deepEqual(v, { ok: true });
});

test("6: identity is the host evt id, and the callID stays correlation only (A1)", () => {
  // Two DIFFERENT events that share one callID (nested tool calls, measured)
  // must produce two different event_ids and keep the callID in meta.
  const outer = mapper.mapHostEvent(
    hostEvent("session.tool.success", { sessionID: "s", id: "call_shared", assistantMessageID: "m" }, "evt_outer"),
    mappingCtx(),
  );
  const inner = mapper.mapHostEvent(
    hostEvent("session.tool.success", { sessionID: "s", id: "call_shared", assistantMessageID: "m" }, "evt_inner"),
    mappingCtx(),
  );
  assert.notEqual(outer.envelope.event_id, inner.envelope.event_id, "callID must not collapse two events");
  assert.equal(outer.envelope.meta.host_call_id, "call_shared");
  assert.equal(inner.envelope.meta.host_call_id, "call_shared");
  assert.equal(outer.envelope.idempotency, "strict");
});

// ---------------------------------------------------------------------------
// Envelope validation
// ---------------------------------------------------------------------------

function validEnvelope() {
  return mapper.mapHostEvent(
    hostEvent("session.tool.success", { sessionID: "ses_a", id: "call_1" }),
    mappingCtx(),
  ).envelope;
}

test("7: validation rejects a non-frozen schema version", () => {
  const e = { ...validEnvelope(), schema_version: "mpm-event/0.2" };
  assert.equal(env.validateEnvelope(e).ok, false);
});

test("8: validation rejects adapter-set ingested_at_ms", () => {
  const e = { ...validEnvelope(), ingested_at_ms: 1 };
  assert.equal(env.validateEnvelope(e).ok, false);
});

test("9: validation rejects a non-null payload", () => {
  const e = { ...validEnvelope(), payload: { anything: 1 } };
  assert.equal(env.validateEnvelope(e).ok, false);
});

test("10: validation rejects an unknown event type and an inferred observation", () => {
  assert.equal(env.validateEnvelope({ ...validEnvelope(), event_type: "session.resumed" }).ok, false);
  assert.equal(env.validateEnvelope({ ...validEnvelope(), observation: "inferred" }).ok, false);
});

test("11: validation enforces the outcome rules", () => {
  const absent = { ...validEnvelope(), operation: "absent", result_status: "unknown" };
  delete absent.outcome_class;
  assert.deepEqual(env.validateEnvelope(absent), { ok: true });

  // an observation with no operation must not assert an outcome…
  assert.equal(env.validateEnvelope({ ...absent, result_status: "success" }).ok, false);
  assert.equal(env.validateEnvelope({ ...absent, outcome_class: "ok" }).ok, false);
  // …and `unknown` is legal only when the operation is absent.
  assert.equal(
    env.validateEnvelope({ ...validEnvelope(), result_status: "unknown" }).ok,
    false,
  );
  // outcome_code is absent when the class is ok (A12).
  assert.equal(env.validateEnvelope({ ...validEnvelope(), outcome_code: "x" }).ok, false);
});

test("12: validation rejects a non-allowlisted meta key and oversized values", () => {
  assert.equal(
    env.validateEnvelope({ ...validEnvelope(), meta: { ...validEnvelope().meta, prompt: "secret" } }).ok,
    false,
  );
  const e = validEnvelope();
  e.meta.host_tool_name = "x".repeat(200); // bound is 128
  assert.equal(env.validateEnvelope(e).ok, false);
});

test("13: validation enforces the per-field and count limits", () => {
  // 17 allowlisted meta keys is possible; the contract allows at most 16.
  const many = validEnvelope();
  many.meta = {
    host_event_type: "a",
    host_tool_name: "b",
    host_action: "c",
    host_result: "d",
    host_agent: "e",
    host_model_provider: "f",
    host_model_id: "g",
    host_message_kind: "h",
    host_error_code: "i",
    compaction_kind: "j",
    diff_stat_summary: "k",
    payload_hash: "l",
    host_call_id: "m",
    host_message_id: "n",
    overflow: false,
    changed_paths_count: 1,
    duration_ms: 2,
  };
  assert.equal(Object.keys(many.meta).length, 17);
  const manyResult = env.validateEnvelope(many);
  assert.equal(manyResult.ok, false);
  assert.equal(manyResult.reason, "limit_exceeded");

  // An individual string field above 512 B is a hard limit.
  const long = validEnvelope();
  long.framework_session_id = "x".repeat(600);
  const longResult = env.validateEnvelope(long);
  assert.equal(longResult.ok, false);
  assert.equal(longResult.reason, "limit_exceeded");
});

test("14: project_id and project_id_source must agree", () => {
  assert.equal(env.validateEnvelope({ ...validEnvelope(), project_id: null }).ok, false);
  assert.equal(
    env.validateEnvelope({ ...validEnvelope(), project_id_source: "unresolved" }).ok,
    false,
  );
});

// ---------------------------------------------------------------------------
// Project identity (A13)
// ---------------------------------------------------------------------------

test("15: remote spellings collapse to one normalised identity", () => {
  const remote = "github.com/flowbyte-com/mpm";
  for (const raw of [
    "git@github.com:flowbyte-com/mpm.git",
    "git@github.com:flowbyte-com/mpm",
    "https://github.com/flowbyte-com/mpm",
    "https://github.com/flowbyte-com/mpm.git",
    "https://user:secret@github.com/flowbyte-com/mpm.git",
    "ssh://git@github.com:22/flowbyte-com/mpm",
  ]) {
    assert.equal(env.normaliseRemoteUrl(raw), remote, `failed for ${raw}`);
  }
  assert.equal(
    env.projectIdFromRemote("github.com/flowbyte-com/mpm"),
    "pj_5a6c0d9dbf77282fe197b644291fd9e9",
    "matches the id verified in the contract",
  );
});

test("16: no remote means no project id — never fabricated", () => {
  const id = env.resolveProjectIdentity({
    projectDir: "/tmp/not-a-repo",
    hostProjectId: "d155668d00000000000000000000000000000000",
    env: {},
    readGitRemote: () => "",
    readIdFile: () => null,
  });
  assert.equal(id.project_id, null);
  assert.equal(id.project_id_source, "unresolved");
  assert.equal(id.host_confirmed, false);
});

test("17: the host id is adopted only when a git remote confirms it", () => {
  const derived = env.projectIdFromRemote("github.com/flowbyte-com/mpm");
  const confirmed = env.resolveProjectIdentity({
    projectDir: "/tmp/repo",
    hostProjectId: derived,
    env: {},
    readGitRemote: () => "git@github.com:flowbyte-com/mpm.git",
    readIdFile: () => null,
  });
  assert.equal(confirmed.project_id, derived);
  assert.equal(confirmed.project_id_source, "host-git-remote");

  const unconfirmed = env.resolveProjectIdentity({
    projectDir: "/tmp/repo",
    hostProjectId: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
    env: {},
    readGitRemote: () => "git@github.com:flowbyte-com/mpm.git",
    readIdFile: () => null,
  });
  assert.equal(unconfirmed.project_id, derived, "a bare host id is not an identity");
  assert.equal(unconfirmed.project_id_source, "git-remote");
});

test("18: explicit configuration wins over the derived value", () => {
  const viaEnv = env.resolveProjectIdentity({
    projectDir: "/tmp/repo",
    env: { MPM_PROJECT_ID: "my-project" },
    readGitRemote: () => "git@github.com:flowbyte-com/mpm.git",
    readIdFile: () => null,
  });
  assert.equal(viaEnv.project_id, "my-project");
  assert.equal(viaEnv.project_id_source, "explicit-env");

  const viaFile = env.resolveProjectIdentity({
    projectDir: "/tmp/repo",
    env: {},
    readGitRemote: () => "git@github.com:flowbyte-com/mpm.git",
    readIdFile: () => "  file-project  \n",
  });
  assert.equal(viaFile.project_id, "file-project");
  assert.equal(viaFile.project_id_source, "explicit-file");
});

// ---------------------------------------------------------------------------
// Mapping: session / tool / compaction
// ---------------------------------------------------------------------------

test("19: session.created maps to session.started with the allowlisted metadata", () => {
  const mapped = mapper.mapHostEvent(
    hostEvent("session.created", {
      sessionID: "ses_a",
      projectID: "abc",
      title: "SECRET TITLE",
      slug: "secret-slug",
      agent: "build",
      model: { id: "MiniMax-M3.1-Flash", providerID: "minimax-coding-plan" },
    }),
    mappingCtx(),
  );
  assert.equal(mapped.kind, "envelope");
  assert.equal(mapped.envelope.event_type, "session.started");
  assert.equal(mapped.envelope.framework_session_id, "ses_a");
  assert.deepEqual(Object.keys(mapped.envelope.meta).sort(), [
    "host_agent",
    "host_event_type",
    "host_model_id",
    "host_model_provider",
  ]);
  const json = JSON.stringify(mapped.envelope);
  assert.ok(!json.includes("SECRET TITLE"));
  assert.ok(!json.includes("secret-slug"));
});

test("20: a session-level outcome is not an MPM operation (A12)", () => {
  for (const [type, word] of [
    ["session.execution.succeeded", "succeeded"],
    ["session.execution.failed", "failed"],
    ["session.execution.interrupted", "interrupted"],
  ]) {
    const m = mapper.mapHostEvent(hostEvent(type, { sessionID: "ses_a" }), mappingCtx());
    assert.equal(m.envelope.event_type, "session.observed");
    assert.equal(m.envelope.operation, "absent");
    assert.equal(m.envelope.result_status, "unknown");
    assert.equal("outcome_class" in m.envelope, false);
    assert.equal(m.envelope.meta.host_result, word);
  }
});

test("21: tool outcomes carry MPM-scoped outcomes, per A12", () => {
  const ok = mapper.mapHostEvent(
    hostEvent("session.tool.success", { sessionID: "s", id: "call_1", assistantMessageID: "msg_9", content: [{ type: "text", text: "SECRET OUTPUT" }] }),
    mappingCtx(),
  );
  assert.equal(ok.envelope.event_type, "tool.outcome");
  assert.equal(ok.envelope.operation, "present");
  assert.equal(ok.envelope.result_status, "success");
  assert.equal(ok.envelope.outcome_class, "ok");
  assert.equal(ok.envelope.meta.host_result, "success");
  assert.equal(ok.envelope.meta.host_call_id, "call_1");
  assert.equal(ok.envelope.meta.host_message_id, "msg_9");
  assert.ok(!JSON.stringify(ok.envelope).includes("SECRET OUTPUT"));

  const failed = mapper.mapHostEvent(
    hostEvent("session.tool.failed", { sessionID: "s", id: "call_2", content: "SECRET ERROR" }),
    mappingCtx(),
  );
  assert.equal(failed.envelope.operation, "present");
  assert.equal(failed.envelope.result_status, "error");
  assert.equal(failed.envelope.outcome_class, "internal");
  assert.ok(!JSON.stringify(failed.envelope).includes("SECRET ERROR"));
});

test("22: compaction maps to the agreed pair, and a failed compaction is unsupported", () => {
  const started = mapper.mapHostEvent(
    hostEvent("session.compaction.started", { sessionID: "s", reason: "manual", inputID: "msg_1" }),
    mappingCtx(),
  );
  assert.equal(started.envelope.event_type, "compaction.started");
  assert.equal(started.envelope.meta.host_result, "started");
  assert.equal(started.envelope.meta.compaction_kind, "manual");

  const ended = mapper.mapHostEvent(
    hostEvent("session.compaction.ended", { sessionID: "s", reason: "manual", text: "SECRET SUMMARY" }),
    mappingCtx(),
  );
  assert.equal(ended.envelope.event_type, "compaction.completed");
  assert.equal(ended.envelope.meta.host_result, "ended");
  assert.ok(!JSON.stringify(ended.envelope).includes("SECRET SUMMARY"));

  const failed = mapper.mapHostEvent(
    hostEvent("session.compaction.failed", { sessionID: "s" }),
    mappingCtx(),
  );
  assert.equal(failed.kind, "unsupported", "no vocabulary type exists; never neighbour-map");
});

test("23: observed_at_ms is the adapter clock, not the host's created field", () => {
  const m = mapper.mapHostEvent(
    hostEvent("session.created", { sessionID: "s" }, "evt_1"),
    mappingCtx({ now: () => 42 }),
  );
  assert.equal(m.envelope.observed_at_ms, 42);
  assert.notEqual(m.envelope.observed_at_ms, 1793047203717);
});

// ---------------------------------------------------------------------------
// THE STRUCTURAL ALLOWLIST, against the REAL captured shapes
// ---------------------------------------------------------------------------

const FORBIDDEN_KEYS = new Set([
  "text",
  "delta",
  "content",
  "input",
  "title",
  "slug",
  "reasoning",
  "instructions",
]);

/** Collect every string in a subtree, however deeply the host nests it. */
function collectStrings(v: unknown, out: string[] = []): string[] {
  if (typeof v === "string") {
    if (v.length >= 8) out.push(v);
    return out;
  }
  if (Array.isArray(v)) {
    for (const item of v) collectStrings(item, out);
    return out;
  }
  if (typeof v === "object" && v !== null) {
    for (const val of Object.values(v)) collectStrings(val, out);
  }
  return out;
}

/** Collect the content-bearing values a host event carries. */
function forbiddenValues(v: unknown, out: string[] = [], key = ""): string[] {
  if (key !== "" && FORBIDDEN_KEYS.has(key)) {
    return collectStrings(v, out);
  }
  if (Array.isArray(v)) {
    for (const item of v) forbiddenValues(item, out, key);
    return out;
  }
  if (typeof v === "object" && v !== null) {
    for (const [k, val] of Object.entries(v)) forbiddenValues(val, out, k);
  }
  return out;
}

test("24: NO captured host event leaks content into an envelope", () => {
  const files = readdirSync(EVIDENCE_DIR).filter(
    (f) => f.startsWith("event_stream_probe_run") && f.endsWith(".jsonl"),
  );
  assert.ok(files.length >= 3, `expected the committed captures, found ${files.join(", ")}`);

  const ALLOWED_META = new Set([
    ...Object.keys(env.META_STRING_BOUNDS),
    ...env.META_BOOLEAN_KEYS,
    ...env.META_INTEGER_KEYS,
  ]);

  let records = 0;
  let envelopes = 0;
  let checkedValues = 0;

  for (const file of files) {
    const lines = readFileSync(path.join(EVIDENCE_DIR, file), "utf8").split("\n");
    for (const line of lines) {
      if (line.trim() === "") continue;
      const rec = JSON.parse(line);
      if (rec.kind !== "formB.event") continue; // only real delivered host events
      records++;
      const ev = { id: rec.id, created: rec.created, type: rec.type, data: rec.data };

      // The mapper must never throw on a real shape.
      const mapped = mapper.mapHostEvent(ev, mappingCtx());
      const forbidden = [...new Set(forbiddenValues(rec.data))];
      checkedValues += forbidden.length;

      if (mapped.kind !== "envelope") continue;
      envelopes++;

      const json = JSON.stringify(mapped.envelope);
      for (const value of forbidden) {
        assert.ok(
          !json.includes(value),
          `${file}: content escaped the allowlist for ${rec.type}: ${JSON.stringify(value.slice(0, 60))}`,
        );
      }
      for (const key of Object.keys(mapped.envelope.meta)) {
        assert.ok(ALLOWED_META.has(key), `${file}: non-allowlisted meta key ${key}`);
      }
      const v = env.validateEnvelope(mapped.envelope);
      assert.deepEqual(v, { ok: true }, `${file}: mapped envelope failed validation for ${rec.type}`);
    }
  }

  assert.ok(records >= 100, `expected the full captures (>100 events), got ${records}`);
  assert.ok(envelopes > 0, "the captures must produce mapped envelopes");
  assert.ok(checkedValues > 0, "the captures must actually contain content-bearing values");
});

test("25: the allowlist drops content even when the host nests it", () => {
  const ev = hostEvent("session.tool.called", {
    sessionID: "s",
    id: "call_1",
    input: { path: "/etc/shadow", body: "SECRET PATCH" },
    content: [{ type: "text", text: "SECRET CONTENT" }],
  });
  const mapped = mapper.mapHostEvent(ev, mappingCtx());
  // session.tool.called has no vocabulary type; it becomes session activity.
  assert.equal(mapped.kind, "session.activity");
  const observed = mapper.buildSessionObserved(ev, mappingCtx(), "s");
  const json = JSON.stringify(observed);
  assert.ok(!json.includes("/etc/shadow"));
  assert.ok(!json.includes("SECRET PATCH"));
  assert.ok(!json.includes("SECRET CONTENT"));
});

// ---------------------------------------------------------------------------
// Subscriber: out-of-band delivery, buffering, degradation
// ---------------------------------------------------------------------------

function countingTransport(result: any) {
  const calls: any[] = [];
  return {
    mode: "mpm_system",
    calls,
    async deliver(envelope: any) {
      calls.push(envelope);
      return typeof result === "function" ? result(envelope) : result;
    },
  };
}

test("26: mapping is synchronous and delivery is out-of-band (A5)", async () => {
  const transport = countingTransport((e: any) => ({
    ok: true,
    receipt: { event_id: e.event_id, ingested_at_ms: 1, duplicate: false },
  }));
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    mapping: mappingCtx(),
  });

  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }));
  // observe() is synchronous by contract: nothing has been delivered yet.
  assert.equal(transport.calls.length, 0, "no delivery may happen inside the iterator");
  assert.equal(observer.counters().enqueued, 1);

  await observer.drainOnce();
  assert.equal(transport.calls.length, 1);
  assert.equal(observer.counters().delivered, 1);
});

test("27: unconfigured transport counts drops and never fabricates a receipt", () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }));
  const c = observer.counters();
  assert.equal(c.envelopes_built, 1);
  assert.equal(c.dropped_unconfigured, 1);
  assert.equal(c.delivered, 0);
  assert.equal(c.enqueued, 0, "nothing is buffered when no sink exists");
});

test("28: an unavailable endpoint degrades without claiming durability", async () => {
  const transport = countingTransport({ ok: false, reason: "unavailable" });
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    mapping: mappingCtx(),
    maxDeliveryAttempts: 2,
  });
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }));
  await observer.drainOnce();
  const c = observer.counters();
  assert.equal(transport.calls.length, 2, "bounded retry, then give up");
  assert.equal(c.unavailable, 1);
  assert.equal(c.delivered, 0);
  assert.equal(c.duplicates, 0);
});

test("29: a success response without a valid receipt is NOT delivery", async () => {
  const transport = countingTransport({ ok: true, receipt: { event_id: "mismatch", ingested_at_ms: 1, duplicate: false } });
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    mapping: mappingCtx(),
  });
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }));
  await observer.drainOnce();
  assert.equal(observer.counters().delivered, 0);
  assert.equal(observer.counters().malformed_receipt, 1);
});

test("30: a valid receipt counts delivery, and a duplicate is reported as such", async () => {
  let n = 0;
  const transport = countingTransport((e: any) => {
    n++;
    return { ok: true, receipt: { event_id: e.event_id, ingested_at_ms: 7, duplicate: n > 1 } };
  });
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    mapping: mappingCtx(),
  });
  const ev = hostEvent("session.tool.success", { sessionID: "s", id: "call_1" });
  observer.observe(ev);
  await observer.drainOnce();
  assert.equal(observer.counters().delivered, 1);
  assert.equal(observer.counters().duplicates, 0);
});

test("31: the buffer is bounded and drops the oldest, counted", async () => {
  let resolveDelivery: any = null;
  const transport = {
    mode: "mpm_system",
    async deliver() {
      await new Promise((r) => (resolveDelivery = r));
      return { ok: false, reason: "unavailable" };
    },
  };
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    mapping: mappingCtx(),
    bufferCapacity: 2,
    maxDeliveryAttempts: 1,
  });
  for (let i = 0; i < 5; i++) {
    observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: `call_${i}` }, `evt_${i}`));
  }
  assert.equal(observer.counters().dropped_overflow, 3, "capacity 2 of 5 → 3 oldest dropped");
  assert.equal(observer.counters().enqueued, 5);
});

test("32: session.observed is emitted once per session, not per event", () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  const ev = hostEvent("session.text.delta", { sessionID: "ses_once", delta: "SECRET" });
  for (let i = 0; i < 10; i++) observer.observe(ev);
  assert.equal(observer.counters().session_observed, 1);
  assert.equal(observer.counters().host_events_seen, 10);

  const other = hostEvent("session.text.delta", { sessionID: "ses_other", delta: "SECRET" });
  observer.observe(other);
  assert.equal(observer.counters().session_observed, 2, "one per distinct session");
});

test("33: session.created also suppresses the later activity observation", () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.observe(hostEvent("session.created", { sessionID: "ses_x" }));
  observer.observe(hostEvent("session.text.delta", { sessionID: "ses_x", delta: "SECRET" }));
  assert.equal(observer.counters().session_started, 1);
  assert.equal(observer.counters().session_observed, 0, "already known from creation");
});

test("34: unsupported and ignored events are counted, never neighbour-mapped", () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.observe(hostEvent("session.compaction.failed", { sessionID: "s" }));
  observer.observe(hostEvent("provider.updated", {}));
  const c = observer.counters();
  assert.equal(c.unsupported, 1);
  assert.equal(c.ignored, 1);
  assert.equal(c.envelopes_built, 0);
  assert.equal(c.unmapped_by_type["session.compaction.failed"], 1);
});

test("35: an invalid envelope is rejected and never delivered", async () => {
  const transport = countingTransport({ ok: true, receipt: { event_id: "x", ingested_at_ms: 1, duplicate: false } });
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    // an empty adapter_version makes every envelope invalid
    mapping: mappingCtx({ adapterVersion: "" }),
  });
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }));
  await observer.drainOnce();
  assert.equal(observer.counters().invalid_envelope, 1);
  assert.equal(transport.calls.length, 0, "an invalid envelope must not reach the transport");
});

// ---------------------------------------------------------------------------
// Subscriber: iteration and bounded cancellation
// ---------------------------------------------------------------------------

test("36: the iterator is consumed and bounded cancellation stops it", async () => {
  let sawAbort = false;
  const events = [hostEvent("session.tool.success", { sessionID: "s", id: "c1" }, "evt_1")];

  const subscribe = ({ signal }: any) => ({
    async *[Symbol.asyncIterator]() {
      yield events[0];
      // hang until aborted: a slow/hung consumer must be cancellable
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        signal.addEventListener("abort", () => resolve(), { once: true });
      });
      sawAbort = signal.aborted;
    },
  });

  const observer = sub.createLifecycleObserver({
    subscribe,
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.start();
  // wait until the first event has been observed
  for (let i = 0; i < 100 && observer.counters().host_events_seen === 0; i++) {
    await new Promise((r) => setTimeout(r, 5));
  }
  assert.equal(observer.counters().host_events_seen, 1);

  const t0 = Date.now();
  const snapshot = await observer.stop({ timeoutMs: 300 });
  const elapsed = Date.now() - t0;
  assert.ok(elapsed < 2000, `stop must be bounded, took ${elapsed}ms`);
  assert.equal(sawAbort, true, "the signal the stream received was aborted");
  assert.equal(snapshot.counters.stream_errors, 0, "an aborted stream is not an error");
});

test("37: a throwing stream is counted, not propagated", async () => {
  const subscribe = () => ({
    async *[Symbol.asyncIterator]() {
      throw new Error("host stream failed");
    },
  });
  const observer = sub.createLifecycleObserver({
    subscribe,
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.start();
  await new Promise((r) => setTimeout(r, 20));
  await observer.stop({ timeoutMs: 100 });
  assert.equal(observer.counters().stream_errors, 1);
});

test("38: stop() is idempotent and returns stable counters", async () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.start();
  const a = await observer.stop({ timeoutMs: 100 });
  const b = await observer.stop({ timeoutMs: 100 });
  assert.equal(a.counters.host_events_seen, b.counters.host_events_seen);
});

// ---------------------------------------------------------------------------
// Transport: strict receipt handling
// ---------------------------------------------------------------------------

test("39: the P5 transport sends the agreed request and accepts only a valid receipt", async () => {
  const seen: any[] = [];
  const transport = sub.createMpmSystemTransport({
    call: async (tool: string, payload: any) => {
      seen.push({ tool, payload });
      return { success: true, payload: { success: true, receipt: { event_id: "ev1_x", ingested_at_ms: 5, duplicate: false } } };
    },
  });
  const envelope = validEnvelope();
  const res = await transport.deliver(envelope);
  assert.equal(seen.length, 1);
  assert.equal(seen[0].tool, "mpm_system");
  assert.equal(seen[0].payload.action, "event_ingest");
  assert.equal(seen[0].payload.params.schema_version, "mpm-event/0.1");
  assert.equal(seen[0].payload.params.event.event_id, envelope.event_id);
  // receipt event_id must match the envelope's
  assert.equal(res.ok, false);
  assert.equal(res.reason, "malformed_receipt");
});

test("40: an absent endpoint degrades to `unavailable`, a rejection is not retried forever", async () => {
  const absent = sub.createMpmSystemTransport({
    call: async () => ({ success: false, payload: { success: false, error: "unknown action: event_ingest" } }),
  });
  assert.deepEqual(await absent.deliver(validEnvelope()), { ok: false, reason: "unavailable" });

  const rejected = sub.createMpmSystemTransport({
    call: async () => ({ success: false, payload: { success: false, error: "event_type is not in the closed vocabulary" } }),
  });
  assert.deepEqual(await rejected.deliver(validEnvelope()), { ok: false, reason: "rejected" });

  const threw = sub.createMpmSystemTransport({
    call: async () => {
      throw new Error("spawn failed");
    },
  });
  assert.deepEqual(await threw.deliver(validEnvelope()), { ok: false, reason: "unavailable" });

  // A response with no parseable envelope at all (timeout / non-JSON) is an
  // availability fact, not a rejection: retrying is meaningful.
  const silent = sub.createMpmSystemTransport({
    call: async () => ({ success: false, payload: null }),
  });
  assert.deepEqual(await silent.deliver(validEnvelope()), { ok: false, reason: "unavailable" });
});

test("41: a success:true envelope with no receipt is malformed, not delivered", async () => {
  const transport = sub.createMpmSystemTransport({
    call: async () => ({ success: true, payload: { success: true } }),
  });
  assert.deepEqual(await transport.deliver(validEnvelope()), { ok: false, reason: "malformed_receipt" });
});

test("43: stop() settles even when the stream never reacts to abort", async () => {
  // Regression for the CI failure mode: an unref'd bounded-wait timer let the
  // event loop drain while a promise was still pending, so node:test reported
  // "Promise resolution is still pending but the event loop has already
  // resolved" and cancelled every later test in the file.
  const subscribe = () => ({
    async *[Symbol.asyncIterator]() {
      yield hostEvent("session.tool.success", { sessionID: "s", id: "c1" }, "evt_hang");
      await new Promise<void>(() => {
        /* deliberately ignores the abort signal */
      });
    },
  });
  const observer = sub.createLifecycleObserver({
    subscribe,
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.start();
  for (let i = 0; i < 100 && observer.counters().host_events_seen === 0; i++) {
    await new Promise((r) => setTimeout(r, 5));
  }
  const t0 = Date.now();
  const snapshot = await observer.stop({ timeoutMs: 150 });
  const elapsed = Date.now() - t0;
  assert.ok(elapsed < 1500, `stop must resolve despite a stuck stream, took ${elapsed}ms`);
  assert.equal(snapshot.counters.host_events_seen, 1);
});

test("42: drainOnce processes everything currently buffered, in order", async () => {
  const transport = countingTransport((e: any) => ({
    ok: true,
    receipt: { event_id: e.event_id, ingested_at_ms: 1, duplicate: false },
  }));
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    mapping: mappingCtx(),
    bufferCapacity: 8,
  });
  for (let i = 0; i < 4; i++) {
    observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: `call_${i}` }, `evt_${i}`));
  }
  await observer.drainOnce();
  assert.equal(transport.calls.length, 4);
  assert.deepEqual(
    transport.calls.map((e: any) => e.event_type),
    ["tool.outcome", "tool.outcome", "tool.outcome", "tool.outcome"],
  );
  assert.equal(observer.counters().delivered, 4);
});

// ---------------------------------------------------------------------------
// Delivery accounting (the 2026-10-27 correction)
// ---------------------------------------------------------------------------
//
// THE REPORTED DEFECT.
//
// Session A's isolated P4/P5 end-to-end run recorded:
//
//   core:   10 lifecycle events durably stored
//   audit:  10 successful event_ingest rows
//   client: enqueued=10, delivered=9
//
// The server stored TEN. The client reported NINE. The question these
// tests answer is the only question that matters: was the tenth event
// LOST, or was it IN FLIGHT and uncounted?
//
// It was IN FLIGHT. The observer delivers SERIALLY — one envelope at a
// time — so exactly one delivery can be outstanding, and the old
// counters had no bucket for it: a snapshot taken in that window showed
// `enqueued=10 delivered=9`, and the outstanding delivery was
// indistinguishable from a dropped event.
//
// WHAT IS PINNED HERE.
//
//   * every enqueued envelope is in exactly ONE bucket — delivered,
//     failed, evicted by the bound, abandoned at teardown, still queued,
//     or in flight — and the buckets sum to `enqueued`;
//   * a delivery requires a VALID RECEIPT for THIS envelope; a transport
//     attempt is never a delivery;
//   * `duplicates` is a SUBSET of `delivered`, not an extra bucket;
//   * the reading written SYNCHRONOUSLY at teardown — the only one that
//     survives a host exit — is truthful;
//   * an acknowledged delivery is on record even when the final reading
//     is lost to an interrupted teardown.
//
// WHAT IS NOT CLAIMED.
//
// The host event stream is volatile and delivery is best-effort. Nothing
// here claims exactly-once delivery: MPM's ingest endpoint performs the
// durable deduplication, not the adapter.

/** A temporary directory for a diagnostic dump. */
function dumpDir(): string {
  return mkdtempSync(path.join(tmpdir(), "mpm-lifecycle-"));
}

/** The rows of a diagnostic dump, in order. */
function readDump(p: string): any[] {
  if (!existsSync(p)) return [];
  return readFileSync(p, "utf8")
    .split("\n")
    .filter((l) => l.trim() !== "")
    .map((l) => JSON.parse(l));
}

/** Wait until a predicate holds, or fail loudly. */
async function waitFor(predicate: () => boolean, label: string, timeoutMs = 3000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${label}`);
    await new Promise((r) => setTimeout(r, 1));
  }
}

/**
 * A transport whose every delivery is HELD until the test releases it.
 *
 * The gate is what makes the in-flight window deterministic: a timer
 * would make the test a race, and a race cannot prove anything about a
 * window that lasts microseconds.
 */
function gatedTransport() {
  const gates: Array<(receipt: any) => void> = [];
  const attempts: string[] = [];
  const transport = {
    mode: "mpm_system" as const,
    attempts,
    async deliver(envelope: any) {
      attempts.push(envelope.event_id);
      return new Promise((resolve) => {
        gates.push((receipt: any) =>
          resolve({
            ok: true,
            receipt: {
              event_id: envelope.event_id,
              ingested_at_ms: 1,
              duplicate: false,
              ...receipt,
            },
          }),
        );
      });
    },
  };
  /** release every held delivery */
  const releaseAll = (receipt: any = {}) => {
    while (gates.length > 0) gates.shift()!(receipt);
  };
  return { transport, gates, releaseAll };
}

/** An observer over a given transport, with optional overrides. */
function makeObserver(transport: any, over: any = {}) {
  return sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport,
    mapping: mappingCtx(),
    ...over,
  });
}

/** The balance of a snapshot read from a diagnostic dump row. */
function dumpBalance(row: any): any {
  return sub.accountingBalance({ counters: row.counters, state: row.state });
}

test("44: a successful delivery is counted only with a valid receipt, and the accounting closes", async () => {
  const transport = countingTransport((e: any) => ({
    ok: true,
    receipt: { event_id: e.event_id, ingested_at_ms: 1, duplicate: false },
  }));
  const observer = makeObserver(transport);
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_ok"));
  await observer.drainOnce();

  const snap = observer.snapshot();
  assert.equal(snap.counters.delivered, 1, "a valid receipt is a delivery");
  assert.equal(snap.counters.duplicates, 0);
  assert.equal(snap.counters.delivery_attempts, 1, "one attempt, one delivery");
  assert.equal(snap.state.queued, 0);
  assert.equal(snap.state.in_flight, 0);
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
  assert.equal(balance.accounted, 1);
});

test("45: a duplicate acknowledgement IS a delivery, and duplicates are a subset of delivered", async () => {
  const transport = countingTransport((e: any) => ({
    ok: true,
    receipt: { event_id: e.event_id, ingested_at_ms: 1, duplicate: true },
  }));
  const observer = makeObserver(transport);
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_dup"));
  await observer.drainOnce();

  const snap = observer.snapshot();
  assert.equal(snap.counters.delivered, 1, "a duplicate receipt is still a delivery");
  assert.equal(snap.counters.duplicates, 1);
  assert.ok(snap.counters.duplicates <= snap.counters.delivered, "duplicates ⊆ delivered");
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
  assert.equal(balance.accounted, 1, "a duplicate must not be counted a second time");
});

test("46: an explicit server rejection is counted as rejected, never as a delivery", async () => {
  const transport = countingTransport(() => ({ ok: false, reason: "rejected" }));
  const observer = makeObserver(transport);
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_rej"));
  await observer.drainOnce();

  const snap = observer.snapshot();
  assert.equal(snap.counters.rejected, 1);
  assert.equal(snap.counters.delivered, 0, "a rejection is not a delivery");
  assert.equal(snap.counters.unavailable, 0, "a rejection is not an outage");
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
});

test("47: an unavailable endpoint is counted as unavailable, never as a delivery", async () => {
  const transport = countingTransport(() => ({ ok: false, reason: "unavailable" }));
  const observer = makeObserver(transport, { maxDeliveryAttempts: 1 });
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_unav"));
  await observer.drainOnce();

  const snap = observer.snapshot();
  assert.equal(snap.counters.unavailable, 1);
  assert.equal(snap.counters.delivered, 0, "an unreachable endpoint delivers nothing");
  assert.equal(snap.counters.delivery_attempts, 1);
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
});

test("48: a retried delivery is ONE delivery and TWO attempts", async () => {
  let n = 0;
  const transport = countingTransport((e: any) => {
    n++;
    if (n === 1) return { ok: false, reason: "unavailable" };
    return { ok: true, receipt: { event_id: e.event_id, ingested_at_ms: 1, duplicate: false } };
  });
  const observer = makeObserver(transport, { maxDeliveryAttempts: 2, retryDelayMs: 1 });
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_retry"));
  await observer.drainOnce();

  const snap = observer.snapshot();
  assert.equal(snap.counters.delivery_attempts, 2, "two transport attempts were made");
  assert.equal(snap.counters.delivered, 1, "one envelope, one delivery");
  assert.equal(snap.counters.unavailable, 0, "a retry that succeeds is not a terminal failure");
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
  assert.equal(balance.accounted, 1, "the retry must not double-count the envelope");
});

test("48b: a retry that never succeeds ends in ONE terminal failure", async () => {
  const transport = countingTransport(() => ({ ok: false, reason: "unavailable" }));
  const observer = makeObserver(transport, { maxDeliveryAttempts: 3, retryDelayMs: 1 });
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_exh"));
  await observer.drainOnce();

  const snap = observer.snapshot();
  assert.equal(snap.counters.delivery_attempts, 3, "three transport attempts were made");
  assert.equal(snap.counters.unavailable, 1, "one envelope, one terminal failure");
  assert.equal(snap.counters.delivered, 0);
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
});

test("49: shutdown with a nonempty queue abandons what was never attempted", async () => {
  const { transport, releaseAll } = gatedTransport();
  const observer = makeObserver(transport);
  observer.start();
  for (let i = 0; i < 5; i++) {
    observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: `call_${i}` }, `evt_q_${i}`));
  }
  await waitFor(() => observer.state().in_flight === 1, "the first delivery to start");

  const snap = await observer.stop({ timeoutMs: 120 });
  assert.equal(snap.counters.delivered, 0, "nothing was acknowledged");
  assert.equal(snap.state.queued, 0, "the queue was emptied at teardown");
  assert.ok(
    snap.counters.abandoned + snap.state.in_flight === 5,
    `every envelope must be accounted for: abandoned=${snap.counters.abandoned} in_flight=${snap.state.in_flight}`,
  );
  assert.ok(snap.counters.abandoned >= 1, "the queued envelopes were abandoned, not lost");
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
  assert.equal(balance.accounted, 5);

  releaseAll();
});

test("50: shutdown with one delivery in flight — the reading that survives a host exit says so", async () => {
  const dir = dumpDir();
  const dump = path.join(dir, "dump.jsonl");
  const { transport, gates } = gatedTransport();
  const observer = makeObserver(transport, { debugDumpPath: dump });
  observer.start();
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_inflight"));
  await waitFor(() => observer.state().in_flight === 1, "the delivery to start");

  const before = observer.snapshot();
  assert.equal(before.counters.enqueued, 1);
  assert.equal(before.counters.delivered, 0, "not acknowledged yet");
  assert.equal(before.state.in_flight, 1, "the delivery is IN FLIGHT, not lost");
  assert.equal(sub.accountingBalance(before).balanced, true);

  // stop() writes the teardown reading SYNCHRONOUSLY, before any await:
  // that is the reading that survives a host exit.
  const stopped = observer.stop({ timeoutMs: 120 });
  const rows = readDump(dump);
  const teardown = rows.find((r) => r.kind === "observer.counters" && r.phase === "teardown");
  assert.ok(teardown, "the teardown reading is written synchronously");
  assert.equal(teardown.counters.delivered, 0);
  assert.equal(teardown.state.in_flight, 1, "the teardown reading accounts for the in-flight delivery");
  assert.equal(dumpBalance(teardown).balanced, true, "the teardown reading balances");

  gates[0]({});
  await stopped;
  const after = observer.snapshot();
  assert.equal(after.counters.delivered, 1, "the acknowledged delivery is counted once it lands");
  assert.equal(after.state.in_flight, 0);
  assert.equal(sub.accountingBalance(after).balanced, true);
  rmSync(dir, { recursive: true, force: true });
});

test("51: shutdown immediately after the acknowledgement keeps the delivery on record", async () => {
  const dir = dumpDir();
  const dump = path.join(dir, "dump.jsonl");
  const { transport, gates } = gatedTransport();
  const observer = makeObserver(transport, { debugDumpPath: dump });
  observer.start();
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_ack"));
  await waitFor(() => observer.state().in_flight === 1, "the delivery to start");

  // The server acknowledges.
  gates[0]({});
  await waitFor(() => observer.counters().delivered === 1, "the acknowledgement to land");

  const snap = await observer.stop({ timeoutMs: 120 });
  assert.equal(snap.counters.delivered, 1, "the acknowledged delivery is not lost from the counters");
  assert.equal(snap.state.in_flight, 0);
  assert.equal(sub.accountingBalance(snap).balanced, true);

  // And it is on record per-delivery, so it survives even if the final
  // reading is lost to an interrupted teardown.
  const rows = readDump(dump);
  const delivery = rows.find((r) => r.kind === "observer.delivery");
  assert.ok(delivery, "a per-delivery record was written");
  assert.equal(delivery.outcome, "delivered");
  assert.equal(delivery.duplicate, false);
  assert.equal(delivery.attempts, 1);
  assert.ok(
    !("event_id" in delivery),
    "a delivery row must not carry `event_id` — the dump is also the envelope dump",
  );
  rmSync(dir, { recursive: true, force: true });
});

test("52: an interrupted teardown still leaves a truthful reading on disk", async () => {
  const dir = dumpDir();
  const dump = path.join(dir, "dump.jsonl");
  const { transport, releaseAll } = gatedTransport();
  const observer = makeObserver(transport, { debugDumpPath: dump });
  observer.start();
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_abort"));
  await waitFor(() => observer.state().in_flight === 1, "the delivery to start");

  // The host's cleanup is fire-and-forget: OpenCode's contract is a
  // SYNCHRONOUS `() => void`, so nothing can be awaited.
  void observer.stop({ timeoutMs: 500 });

  // No await between the call and the assertion: the teardown reading
  // must already be on disk, because it is written synchronously.
  const rows = readDump(dump);
  const teardown = rows.find((r) => r.kind === "observer.counters" && r.phase === "teardown");
  assert.ok(teardown, "the teardown reading is written before the first await");
  assert.equal(teardown.state.in_flight, 1, "it accounts for the in-flight delivery");
  assert.equal(teardown.counters.delivered, 0);
  assert.equal(dumpBalance(teardown).balanced, true, "the teardown reading balances");

  releaseAll();
  rmSync(dir, { recursive: true, force: true });
});

test("53: a queue overflow is counted as evicted, and the accounting still closes", async () => {
  const transport = countingTransport((e: any) => ({
    ok: true,
    receipt: { event_id: e.event_id, ingested_at_ms: 1, duplicate: false },
  }));
  const observer = makeObserver(transport, { bufferCapacity: 2 });
  for (let i = 0; i < 5; i++) {
    observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: `call_${i}` }, `evt_o_${i}`));
  }
  const before = observer.snapshot();
  assert.equal(before.counters.enqueued, 5);
  assert.equal(before.counters.dropped_overflow, 3, "capacity 2 of 5 → 3 oldest evicted");
  assert.equal(before.state.queued, 2, "two remain in the queue");
  assert.equal(
    sub.accountingBalance(before).balanced,
    true,
    `evictions must balance: ${JSON.stringify(sub.accountingBalance(before))}`,
  );

  await observer.drainOnce();
  const after = observer.snapshot();
  assert.equal(after.counters.delivered, 2, "the two survivors were delivered");
  assert.equal(after.counters.dropped_overflow, 3);
  const balance = sub.accountingBalance(after);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
  assert.equal(balance.accounted, 5, "2 delivered + 3 evicted = 5 enqueued");
});

test("54: THE REPORTED CASE — ten stored, nine reported — the tenth is IN FLIGHT", async () => {
  const dir = dumpDir();
  const dump = path.join(dir, "dump.jsonl");
  const { transport, gates } = gatedTransport();
  const observer = makeObserver(transport, { debugDumpPath: dump, maxDeliveryAttempts: 1 });
  observer.start();

  for (let i = 0; i < 10; i++) {
    observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: `call_${i}` }, `evt_c_${i}`));
  }
  // Nine settle; the tenth is held in the transport. The drain loop is
  // serial, so each delivery must be released before the next starts.
  while (observer.counters().delivered < 9) {
    if (gates.length > 0) gates.shift()!({});
    else await new Promise((r) => setTimeout(r, 1));
  }
  await waitFor(() => gates.length === 1, "the tenth delivery to be held in the transport");
  await waitFor(() => observer.state().in_flight === 1, "the tenth delivery to be in flight");

  const before = observer.snapshot();
  assert.equal(before.counters.enqueued, 10);
  assert.equal(before.counters.delivered, 9, "nine acknowledged so far");
  assert.equal(before.state.in_flight, 1, "the tenth is IN FLIGHT — this is what was missing");
  assert.equal(before.state.queued, 0);
  const balance = sub.accountingBalance(before);
  assert.equal(balance.balanced, true, `the accounting closes: ${JSON.stringify(balance)}`);
  assert.equal(balance.accounted, 10, "10 = 9 delivered + 1 in flight");

  // The teardown reading — the one that survives a host exit — accounts
  // for the tenth event instead of implying it was lost.
  const stopped = observer.stop({ timeoutMs: 750 });
  const teardown = readDump(dump).find((r) => r.kind === "observer.counters" && r.phase === "teardown");
  assert.ok(teardown);
  assert.equal(teardown.counters.delivered, 9);
  assert.equal(teardown.state.in_flight, 1);
  assert.equal(dumpBalance(teardown).balanced, true);

  // The tenth lands: the server stores all ten.
  gates.shift()!({});
  await stopped;
  await waitFor(() => observer.counters().delivered === 10, "the tenth delivery to land");
  const after = observer.snapshot();
  assert.equal(after.counters.delivered, 10);
  assert.equal(after.state.in_flight, 0);
  assert.equal(sub.accountingBalance(after).balanced, true);
  rmSync(dir, { recursive: true, force: true });
});

test("55: the accounting check is not vacuous", async () => {
  const { transport, gates } = gatedTransport();
  const observer = makeObserver(transport);
  observer.start();
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_nv"));
  await waitFor(() => observer.state().in_flight === 1, "the delivery to start");

  const real = observer.snapshot();

  // (a) the real reading balances.
  assert.equal(sub.accountingBalance(real).balanced, true, "the real reading balances");

  // (b) a reading with one envelope over-counted does NOT balance: the
  //     check is sensitive to the counters, not a tautology.
  const over = { counters: { ...real.counters, delivered: real.counters.delivered + 1 }, state: real.state };
  assert.equal(sub.accountingBalance(over).balanced, false, "the check must reject an over-count");

  // (c) THE decisive one: the balance closes BECAUSE of the in-flight
  //     bucket. Remove it and the reading stops balancing — which is
  //     exactly the old behaviour that made the tenth event look lost.
  const withoutInFlight = { counters: real.counters, state: { ...real.state, in_flight: 0 } };
  assert.equal(
    sub.accountingBalance(withoutInFlight).balanced,
    false,
    "without the in-flight bucket the envelope is unaccounted for",
  );
  assert.equal(real.state.in_flight, 1, "and the real reading really does have one in flight");

  // (d) an empty observer balances trivially — but (c) shows the check
  //     is not trivially true.
  const empty = makeObserver(countingTransport(() => ({ ok: false, reason: "unavailable" })));
  assert.equal(sub.accountingBalance(empty.snapshot()).balanced, true);
  assert.equal(empty.snapshot().counters.enqueued, 0);

  gates[0]({});
  await waitFor(() => observer.counters().delivered === 1, "the delivery to land");
});

test("56: the counters are cumulative and the state is instantaneous", async () => {
  const { transport, gates } = gatedTransport();
  const observer = makeObserver(transport);
  observer.start();
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_cum"));
  await waitFor(() => observer.state().in_flight === 1, "the delivery to start");

  const first = observer.snapshot();
  assert.equal(first.state.in_flight, 1);
  assert.equal(first.counters.delivered, 0);

  gates[0]({});
  await waitFor(() => observer.counters().delivered === 1, "the delivery to land");

  const second = observer.snapshot();
  assert.equal(second.counters.delivered, 1, "the cumulative counter only grows");
  assert.equal(second.state.in_flight, 0, "the instantaneous state fell back");
  assert.ok(second.counters.delivered >= first.counters.delivered, "cumulative counters never shrink");
});

test("57: a response that is not a valid receipt for THIS envelope is never a delivery", async () => {
  // (a) a receipt naming a different event
  const wrongId = countingTransport(() => ({
    ok: true,
    receipt: { event_id: "ev1_someone_else", ingested_at_ms: 1, duplicate: false },
  }));
  const o1 = makeObserver(wrongId);
  o1.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_m1"));
  await o1.drainOnce();
  assert.equal(o1.counters().delivered, 0, "a receipt for another event is not a delivery");
  assert.equal(o1.counters().malformed_receipt, 1);
  assert.equal(sub.accountingBalance(o1.snapshot()).balanced, true);

  // (b) no receipt at all
  const noReceipt = countingTransport(() => ({ ok: true } as any));
  const o2 = makeObserver(noReceipt);
  o2.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_m2"));
  await o2.drainOnce();
  assert.equal(o2.counters().delivered, 0, "a silent ok is not a delivery");
  assert.equal(o2.counters().malformed_receipt, 1);
  assert.equal(sub.accountingBalance(o2.snapshot()).balanced, true);
});

test("58: a transport that throws is a failed delivery, not a lost envelope", async () => {
  const transport = {
    mode: "mpm_system" as const,
    async deliver(): Promise<any> {
      throw new Error("spawn failed");
    },
  };
  const observer = makeObserver(transport);
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_throw"));
  await observer.drainOnce();

  const snap = observer.snapshot();
  assert.equal(snap.counters.delivered, 0);
  assert.equal(snap.counters.delivery_failed, 1, "the envelope failed; it did not vanish");
  assert.equal(snap.state.in_flight, 0, "the in-flight slot was released");
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
});

test("59: an unconfigured transport never enqueues, and the accounting still closes", async () => {
  const observer = makeObserver(sub.createUnconfiguredTransport());
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_unc"));
  const snap = observer.snapshot();
  assert.equal(snap.counters.envelopes_built, 1);
  assert.equal(snap.counters.dropped_unconfigured, 1);
  assert.equal(snap.counters.enqueued, 0, "an unconfigured envelope is never enqueued");
  assert.equal(snap.counters.delivered, 0);
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);
});

test("60: stop() stays bounded when the transport never answers", async () => {
  const { transport, releaseAll } = gatedTransport();
  const observer = makeObserver(transport);
  observer.start();
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_bound"));
  await waitFor(() => observer.state().in_flight === 1, "the delivery to start");

  const t0 = Date.now();
  const snap = await observer.stop({ timeoutMs: 150 });
  const elapsed = Date.now() - t0;
  assert.ok(elapsed < 1500, `stop must be bounded, took ${elapsed}ms`);
  assert.equal(snap.state.in_flight, 1, "the delivery is still in flight — and says so");
  const balance = sub.accountingBalance(snap);
  assert.equal(balance.balanced, true, `accounting must close: ${JSON.stringify(balance)}`);

  releaseAll();
});

test("61: the operator summary carries the state, not just the totals", async () => {
  const { transport, gates } = gatedTransport();
  const observer = makeObserver(transport);
  observer.start();
  observer.observe(hostEvent("session.tool.success", { sessionID: "s", id: "call_1" }, "evt_sum"));
  await waitFor(() => gates.length === 1, "the delivery to start");

  // The summary must name the buckets that close the accounting, so a
  // reader can tell an in-flight delivery from a lost one without
  // opening the diagnostic file.
  const line = sub.summariseSnapshot(observer.snapshot());
  for (const needle of ["delivered=", "queued=", "in_flight=", "abandoned=", "balanced="]) {
    assert.ok(line.includes(needle), `the summary must carry ${needle}: ${line}`);
  }
  assert.ok(line.includes("in_flight=1"), `the summary must show the in-flight delivery: ${line}`);
  gates[0]({});
});

// ---------------------------------------------------------------------------
// Session-lifecycle events (the 2026-10-28 session-end investigation)
// ---------------------------------------------------------------------------
//
// WHY THESE TESTS EXIST.
//
// The isolated-host investigation (measure/session_end_scenarios.py) found
// that the host DOES publish session-lifecycle events — `session.archived`,
// `session.unarchived`, `session.deleted` — and that the adapter SWALLOWED
// every one of them.
//
// The mechanism: those events carry a session id, so they used to fall into
// the activity fallback, which emits AT MOST ONE `session.observed` per
// session. By the time the host archives or deletes a session, that single
// observation has long since been emitted, so the event was seen and then
// dropped. Measured on the real host: 27 host events observed, and the
// archive recorded NOTHING.
//
// These tests pin the correction: a session-lifecycle event is a DURABLE
// observation, and it asserts nothing beyond "the adapter saw it".
//
// WHAT IS DELIBERATELY NOT ASSERTED.
//
// An archive is NOT a session end. It is reversible (`session.unarchived`
// exists), and a delete is a user action, not an agent conclusion. No
// envelope here claims a session ended, and nothing infers an end from the
// absence of later events (§3.4 forbids it, and §4 excludes
// `session.resumed` for exactly that reason).

test("62: session.archived is a durable observation, not swallowed by the once-per-session dedup", () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  // The session is seen first — which is what used to swallow the archive.
  observer.observe(hostEvent("session.text.delta", { sessionID: "ses_arch", delta: "SECRET" }, "evt_a1"));
  assert.equal(observer.counters().session_observed, 1, "the activity observation");

  observer.observe(hostEvent("session.archived", { sessionID: "ses_arch" }, "evt_a2"));
  assert.equal(
    observer.counters().session_observed,
    2,
    "the archive must be recorded as its own observation — not deduplicated away",
  );
  assert.equal(observer.counters().ignored, 0, "the archive is not an ignored event");
  assert.equal(observer.counters().unsupported, 0, "the archive is not unsupported");
});

test("63: session.deleted and session.unarchived are recorded the same way", () => {
  for (const type of ["session.deleted", "session.unarchived"]) {
    const observer = sub.createLifecycleObserver({
      subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
      transport: sub.createUnconfiguredTransport(),
      mapping: mappingCtx(),
    });
    observer.observe(hostEvent("session.text.delta", { sessionID: "ses_x", delta: "SECRET" }, "evt_d1"));
    observer.observe(hostEvent(type, { sessionID: "ses_x" }, "evt_d2"));
    assert.equal(
      observer.counters().session_observed,
      2,
      `${type} must be recorded, not swallowed`,
    );
  }
});

test("64: a session-lifecycle observation asserts NOTHING about the session ending", () => {
  const mapped = mapper.mapHostEvent(
    hostEvent("session.archived", { sessionID: "ses_life" }, "evt_l1"),
    mappingCtx(),
  );
  assert.equal(mapped.kind, "envelope");
  const e = mapped.envelope;
  assert.equal(e.event_type, "session.observed");
  // The contract's neutral base type: the adapter saw an event for this
  // session. Nothing more.
  assert.equal(e.observation, "observed", "an observation, never an inference");
  assert.equal(e.trust, "observation");
  assert.equal(e.operation, "absent", "not an MPM operation");
  assert.equal(e.result_status, "unknown", "the host states no result for an archive");
  assert.equal(e.outcome_class, undefined, "no outcome is asserted");
  assert.equal(e.framework_session_id, "ses_life");
  assert.equal(e.meta.host_event_type, "session.archived", "the host's own event type is the fact");
  // No new meta key: the allowlist is closed.
  for (const k of Object.keys(e.meta)) {
    assert.ok(
      k === "host_event_type" || k in env.META_STRING_BOUNDS,
      `meta key ${k} is not on the allowlist`,
    );
  }
  assert.deepEqual(env.validateEnvelope(e), { ok: true }, "the envelope must validate");
});

test("65: a session-lifecycle event with NO session id is not fabricated into one", () => {
  const mapped = mapper.mapHostEvent(
    { id: "evt_l2", created: 1793047203717, type: "session.archived", location: { directory: "/tmp/x" }, data: {} },
    mappingCtx(),
  );
  // No session id and no vocabulary type: ignored, never given a
  // synthesised session.
  assert.equal(mapped.kind, "ignored");
});

test("66: the once-per-session ACTIVITY dedup is unchanged by the fix", () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  const ev = hostEvent("session.text.delta", { sessionID: "ses_act", delta: "SECRET" });
  for (let i = 0; i < 10; i++) observer.observe(ev);
  assert.equal(
    observer.counters().session_observed,
    1,
    "the fix must not widen the activity fallback into a per-event stream",
  );
  assert.equal(observer.counters().host_events_seen, 10);
});

test("67: location.shutdown carries no session id, so it stays COUNTED and not recorded", () => {
  // The host publishes `location.shutdown` on a graceful stop. It has no
  // session id, and `session.observed`'s capability is "any host event
  // that carries a session id" — so there is NO conformant mpm-event/0.1
  // type for it. It is counted as ignored.
  //
  // This test pins the STATUS QUO deliberately: giving it a type would be
  // a schema change (mpm-event/0.2) and needs Session A's ruling, not a
  // quiet mapper edit.
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: sub.createUnconfiguredTransport(),
    mapping: mappingCtx(),
  });
  observer.observe({
    id: "evt_shutdown",
    created: 1793047203717,
    type: "location.shutdown",
    location: { directory: "/tmp/x" },
    data: {},
  });
  assert.equal(observer.counters().ignored, 1, "a session-less host event is counted");
  assert.equal(observer.counters().session_observed, 0, "and it is not recorded as a session observation");
  assert.equal(observer.counters().enqueued, 0);
});

test("68: the archive observation carries no content — only the host's event type", async () => {
  const observer = sub.createLifecycleObserver({
    subscribe: () => ({ async *[Symbol.asyncIterator]() {} }),
    transport: countingTransport((e: any) => ({
      ok: true,
      receipt: { event_id: e.event_id, ingested_at_ms: 1, duplicate: false },
    })),
    mapping: mappingCtx(),
  });
  observer.observe(hostEvent("session.archived", { sessionID: "ses_safe", title: "SECRET TITLE" }, "evt_s1"));
  await observer.drainOnce();
  const sent = observer.counters();
  assert.equal(sent.delivered, 1);
  // The envelope is built by the mapper, not from the host's data, so the
  // title cannot leak.
  const mapped = mapper.mapHostEvent(
    hostEvent("session.archived", { sessionID: "ses_safe", title: "SECRET TITLE" }, "evt_s1"),
    mappingCtx(),
  );
  assert.ok(!JSON.stringify(mapped.envelope).includes("SECRET TITLE"), "no host content in the envelope");
});
