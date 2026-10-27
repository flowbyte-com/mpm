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
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync } from "node:fs";
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
  const bin = path.join(ADAPTER_DIR, "node_modules", ".bin", "tsc");
  const r = existsSync(bin)
    ? spawnSync(bin, [], { cwd: ADAPTER_DIR, encoding: "utf8" })
    : spawnSync("npx", ["tsc"], { cwd: ADAPTER_DIR, encoding: "utf8" });
  assert.equal(r.status, 0, `tsc failed: ${r.stderr || ""}`);
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
  const counters = await observer.stop({ timeoutMs: 300 });
  const elapsed = Date.now() - t0;
  assert.ok(elapsed < 2000, `stop must be bounded, took ${elapsed}ms`);
  assert.equal(sawAbort, true, "the signal the stream received was aborted");
  assert.equal(counters.stream_errors, 0, "an aborted stream is not an error");
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
  assert.equal(a.host_events_seen, b.host_events_seen);
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
  const counters = await observer.stop({ timeoutMs: 150 });
  const elapsed = Date.now() - t0;
  assert.ok(elapsed < 1500, `stop must resolve despite a stuck stream, took ${elapsed}ms`);
  assert.equal(counters.host_events_seen, 1);
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
