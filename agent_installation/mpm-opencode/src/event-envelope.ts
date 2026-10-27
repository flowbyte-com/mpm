// src/event-envelope.ts — mpm-event/0.1 envelope construction and validation.
//
// This module is the adapter-side half of the agreed lifecycle-event
// contract (docs/dev/event-contract.md, amendments A1–A13). It owns:
//
//   * the frozen schema version and the closed event-type vocabulary;
//   * strict-mode identity (`ev1_…`) derived from the host's per-event id,
//     and best-effort identity (`ev1b_…`) when the host supplies none;
//   * envelope validation, including the contract's hard limits;
//   * project identity (§3.3 / A13) — adopted from the host ONLY when a git
//     remote independently confirms it.
//
// It deliberately contains NO transport and NO storage. The adapter emits
// observations; ingestion is P5 (Session A) and is not simulated here.
//
// A1: a host `callID` is a CORRELATION id, never an event identity. It is
// carried as `meta.host_call_id` and does not enter the derivation. Nested
// tool calls share one `callID`, so deriving identity from it would silently
// deduplicate a real event away.
//
// A3: the measured host identity is `evt_<hex>`, unique per published event
// (119/119 distinct in the committed captures) and present on every event.

import { createHash, randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

export const SCHEMA_VERSION = "mpm-event/0.1";
export const HOST_NAME = "opencode";
export const ADAPTER_SOURCE = "opencode-plugin";
export const TRUST_OBSERVATION = "observation";

/** The closed vocabulary of §4. Adding a type is a minor version bump. */
export const EVENT_TYPES = [
  "session.started",
  "session.observed",
  "context.changed",
  "tool.outcome",
  "task.boundary",
  "compaction.started",
  "compaction.completed",
  "handoff.written",
  "critic.proposal",
] as const;
export type EventType = (typeof EVENT_TYPES)[number];

/** `internal.ToolOutcomeClass`, reused verbatim. No new vocabulary. */
export const TOOL_OUTCOME_CLASSES = [
  "ok",
  "validation",
  "not_found",
  "conflict",
  "degraded",
  "substrate",
  "integration",
  "timeout",
  "internal",
] as const;
export type ToolOutcomeClass = (typeof TOOL_OUTCOME_CLASSES)[number];

export type Operation = "present" | "absent";
export type ResultStatus = "success" | "error" | "unknown";
export type ProjectIdSource =
  | "explicit-env"
  | "explicit-file"
  | "git-remote"
  | "host-git-remote"
  | "unresolved";

/**
 * `meta` — the ONLY field bag in mpm-event/0.1 (A7 adds `host_event_type`,
 * `host_call_id`, `host_message_id`). Per-key string bounds are the contract's
 * tighter bounds; a key absent from this table is not allowlisted at all.
 */
export const META_STRING_BOUNDS: Readonly<Record<string, number>> = {
  host_event_type: 64,
  host_tool_name: 128,
  host_action: 64,
  host_result: 64,
  host_agent: 64,
  host_model_provider: 64,
  host_model_id: 128,
  host_message_kind: 64,
  host_error_code: 64,
  compaction_kind: 32,
  diff_stat_summary: 256,
  payload_hash: 80,
  host_call_id: 128,
  host_message_id: 128,
};

export const META_BOOLEAN_KEYS = ["overflow"] as const;
export const META_INTEGER_KEYS = ["changed_paths_count", "duration_ms"] as const;

export const LIMITS = {
  metaFields: 16,
  metaBytes: 2048,
  payloadBytes: 4096,
  envelopeBytes: 8192,
  nestingDepth: 3,
  arrayLength: 64,
} as const;

export type MetaValue = string | boolean | number;

export interface EventEnvelope {
  schema_version: typeof SCHEMA_VERSION;
  event_id: string;
  idempotency: "strict" | "best_effort";
  event_type: EventType;
  observation: "observed";
  observed_at_ms: number;
  host: string;
  host_version: string;
  source: string;
  adapter_version: string;
  project_id: string | null;
  project_id_source: ProjectIdSource;
  framework_name: string;
  framework_session_id: string | null;
  operation: Operation;
  result_status: ResultStatus;
  outcome_class?: ToolOutcomeClass;
  outcome_code?: string;
  trust: typeof TRUST_OBSERVATION;
  meta: Record<string, MetaValue>;
  payload: null;
}

export type ValidationResult =
  | { ok: true }
  | { ok: false; reason: "invalid_envelope" | "limit_exceeded"; detail: string };

// --------------------------------------------------------------------------
// Identity (§3.5)
// --------------------------------------------------------------------------

/** Unsigned LEB128, the `uvarint` of the contract's length prefix. */
function uvarint(n: number): Buffer {
  const out: number[] = [];
  let v = n;
  while (v >= 0x80) {
    out.push((v & 0x7f) | 0x80);
    v = Math.floor(v / 128);
  }
  out.push(v);
  return Buffer.from(out);
}

/** `encode(s) := uvarint(len(s)) ‖ s_bytes` — unambiguous field framing. */
export function encodeField(s: string): Buffer {
  const body = Buffer.from(s, "utf8");
  return Buffer.concat([uvarint(body.length), body]);
}

/**
 * Strict-mode identity: `"ev1_" ‖ hex(sha256(encode(schema) ‖ encode(host) ‖
 * encode(event_type) ‖ encode(host_event_key)))`.
 *
 * `event_type` is part of the input (A2) so the guarantee does not depend on
 * which host key an adapter happens to select. The input is length-prefixed,
 * so characters cannot be moved across a field boundary to force a collision.
 */
export function deriveStrictEventId(input: {
  schemaVersion?: string;
  host?: string;
  eventType: string;
  hostEventKey: string;
}): string {
  const digest = createHash("sha256");
  digest.update(encodeField(input.schemaVersion ?? SCHEMA_VERSION));
  digest.update(encodeField(input.host ?? HOST_NAME));
  digest.update(encodeField(input.eventType));
  digest.update(encodeField(input.hostEventKey));
  return "ev1_" + digest.digest("hex");
}

/**
 * Best-effort identity for a host event with no stable key: every emission is
 * a distinct observation, and the contract says so rather than implying
 * otherwise.
 */
export function deriveBestEffortEventId(): string {
  return "ev1b_" + randomUUID().replace(/-/g, "");
}

export const STRICT_EVENT_ID_RE = /^ev1_[0-9a-f]{64}$/;
export const BEST_EFFORT_EVENT_ID_RE = /^ev1b_[0-9a-f]{32}$/;
export const PROJECT_ID_RE = /^pj_[0-9a-f]{32}$/;

// --------------------------------------------------------------------------
// Project identity (§3.3, A13)
// --------------------------------------------------------------------------

export const PROJECT_ID_SOURCES: readonly ProjectIdSource[] = [
  "explicit-env",
  "explicit-file",
  "git-remote",
  "host-git-remote",
  "unresolved",
];

const EXPLICIT_ID_RE = /^[A-Za-z0-9._:-]{1,128}$/;

/**
 * Normalise a git remote to `host[:port]/path`, collapsing the spellings that
 * name the same repository (§3.3):
 *
 *   git@github.com:org/repo.git          -> github.com/org/repo
 *   https://user:pw@GitHub.com/org/repo/ -> github.com/org/repo
 *   ssh://git@github.com:22/org/repo     -> github.com/org/repo
 *
 * Returns "" when the value cannot be normalised to a host/path pair.
 */
export function normaliseRemoteUrl(raw: string): string {
  let v = raw.trim();
  if (v === "") return "";
  v = v.replace(/\/+$/, "");
  v = v.replace(/\.git$/, "");
  v = v.replace(/\/+$/, "");

  const scpLike = /^([^@/]+@)?([^:/]+):(.+)$/.exec(v);
  if (scpLike && !/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(v)) {
    // scp-like: user@host:path — drop the user entirely.
    return collapse(`${scpLike[2]}/${scpLike[3]}`);
  }

  const withScheme = /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\/(.*)$/.exec(v);
  if (withScheme) {
    const scheme = withScheme[1].toLowerCase();
    let rest = withScheme[2];
    rest = rest.replace(/^[^@/]*@/, ""); // credentials
    const slash = rest.indexOf("/");
    if (slash < 0) return "";
    let hostPort = rest.slice(0, slash);
    const pathPart = rest.slice(slash + 1);
    hostPort = hostPort.toLowerCase();
    const m = /^(.*?):(\d+)$/.exec(hostPort);
    if (m) {
      const defaultPort =
        (scheme === "ssh" && m[2] === "22") ||
        (scheme === "http" && m[2] === "80") ||
        (scheme === "https" && m[2] === "443");
      hostPort = defaultPort ? m[1] : `${m[1]}:${m[2]}`;
    }
    return collapse(`${hostPort}/${pathPart}`);
  }

  return "";
}

function collapse(hostPath: string): string {
  const slash = hostPath.indexOf("/");
  if (slash <= 0) return "";
  const host = hostPath.slice(0, slash).toLowerCase();
  const rest = hostPath.slice(slash + 1).replace(/\/{2,}/g, "/").replace(/^\/+/, "");
  if (host === "" || rest === "") return "";
  return `${host}/${rest}`;
}

/** §3.3: `"pj_" + hex(sha256("mpm-project/1\n" + normalised_remote))[:32]`. */
export function projectIdFromRemote(normalisedRemote: string): string {
  const hex = createHash("sha256")
    .update("mpm-project/1\n" + normalisedRemote, "utf8")
    .digest("hex");
  return "pj_" + hex.slice(0, 32);
}

export interface ProjectIdentity {
  project_id: string | null;
  project_id_source: ProjectIdSource;
  /** true when a host-supplied id was present and matched the derived one. */
  host_confirmed: boolean;
  /** recorded diagnostic; never fabricates an id */
  note?: string;
}

export interface ProjectIdentityDeps {
  /** host's project directory (ctx.location.project.directory / directory) */
  projectDir?: string;
  /** the host's own id, when it supplies one */
  hostProjectId?: string | null;
  env?: NodeJS.ProcessEnv;
  /** injectable for tests; returns the raw `remote.origin.url` or "" */
  readGitRemote?: (dir: string) => string;
  /** injectable for tests */
  readIdFile?: (file: string) => string | null;
}

function defaultGitRemote(dir: string): string {
  try {
    return execFileSync("git", ["-C", dir, "config", "--get", "remote.origin.url"], {
      encoding: "utf8",
      timeout: 2000,
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
  } catch {
    return "";
  }
}

function defaultReadFile(file: string): string | null {
  try {
    return readFileSync(file, "utf8");
  } catch {
    return null;
  }
}

/**
 * The §3.3 chain: explicit env → explicit file → git remote → unresolved.
 *
 * A13: a host-supplied project id is adopted ONLY when a git remote
 * independently confirms it, and `project_id_source` then reads
 * `host-git-remote`. A host id on its own is never a project identity — it is
 * another party's label, and when there is no remote the host derives one
 * from the filesystem path, which is exactly what §3.3 forbids.
 */
export function resolveProjectIdentity(deps: ProjectIdentityDeps = {}): ProjectIdentity {
  const env = deps.env ?? process.env;

  const explicitEnv = (env.MPM_PROJECT_ID ?? "").trim();
  if (explicitEnv !== "") {
    if (EXPLICIT_ID_RE.test(explicitEnv)) {
      return {
        project_id: explicitEnv,
        project_id_source: "explicit-env",
        host_confirmed: false,
      };
    }
    return continueFromFile(deps, "MPM_PROJECT_ID rejected: does not match the explicit-id pattern");
  }

  return continueFromFile(deps);
}

function continueFromFile(deps: ProjectIdentityDeps, priorNote?: string): ProjectIdentity {
  const dir = deps.projectDir;
  if (dir) {
    const file = path.join(dir, ".mpm-project-id");
    const body = (deps.readIdFile ?? defaultReadFile)(file);
    if (typeof body === "string") {
      const first = (body.split("\n").map((l) => l.trim()).find((l) => l !== "") ?? "");
      if (first !== "") {
        if (EXPLICIT_ID_RE.test(first)) {
          return {
            project_id: first,
            project_id_source: "explicit-file",
            host_confirmed: false,
          };
        }
        priorNote = `${priorNote ? priorNote + "; " : ""}.mpm-project-id rejected: does not match the explicit-id pattern`;
      }
    }
  }

  if (dir) {
    const raw = (deps.readGitRemote ?? defaultGitRemote)(dir);
    const normalised = raw ? normaliseRemoteUrl(raw) : "";
    if (normalised !== "") {
      const derived = projectIdFromRemote(normalised);
      const hostId = (deps.hostProjectId ?? "").trim();
      const confirmed = hostId !== "" && hostId === derived;
      return {
        project_id: derived,
        project_id_source: confirmed ? "host-git-remote" : "git-remote",
        host_confirmed: confirmed,
        ...(priorNote ? { note: priorNote } : {}),
      };
    }
  }

  return {
    project_id: null,
    project_id_source: "unresolved",
    host_confirmed: false,
    ...(priorNote ? { note: priorNote } : {}),
  };
}

// --------------------------------------------------------------------------
// adapter_version
// --------------------------------------------------------------------------

let cachedAdapterVersion: string | null = null;

/**
 * The adapter build identity. Prefers the build-provenance record emitted
 * beside the compiled payload, falls back to the package version, and never
 * invents a commit sha.
 */
export function resolveAdapterVersion(): string {
  if (cachedAdapterVersion !== null) return cachedAdapterVersion;
  let here = "";
  try {
    here = path.dirname(fileURLToPath(import.meta.url));
  } catch {
    here = "";
  }
  if (here) {
    try {
      const prov = JSON.parse(readFileSync(path.join(here, "mpm-build-provenance.json"), "utf8"));
      if (typeof prov?.source_commit === "string" && prov.source_commit !== "") {
        cachedAdapterVersion = String(prov.source_commit);
        return cachedAdapterVersion;
      }
    } catch {
      /* fall through */
    }
    try {
      const pkg = JSON.parse(readFileSync(path.join(here, "..", "package.json"), "utf8"));
      if (typeof pkg?.version === "string" && pkg.version !== "") {
        cachedAdapterVersion = `mpm-opencode@${pkg.version}`;
        return cachedAdapterVersion;
      }
    } catch {
      /* fall through */
    }
  }
  cachedAdapterVersion = "mpm-opencode@unknown";
  return cachedAdapterVersion;
}

// --------------------------------------------------------------------------
// Validation (§3.2, §3.7, §3.8)
// --------------------------------------------------------------------------

const OUTCOME_CODE_RE = /^[A-Za-z0-9_.:-]{1,64}$/;

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function byteLength(s: string): number {
  return Buffer.byteLength(s, "utf8");
}

function metaValidation(meta: unknown): ValidationResult {
  if (!isPlainObject(meta)) {
    return { ok: false, reason: "invalid_envelope", detail: "meta must be an object" };
  }
  const keys = Object.keys(meta);
  if (keys.length > LIMITS.metaFields) {
    return {
      ok: false,
      reason: "limit_exceeded",
      detail: `meta has ${keys.length} fields, limit ${LIMITS.metaFields}`,
    };
  }
  for (const key of keys) {
    const value = meta[key];
    const bound = META_STRING_BOUNDS[key];
    if (bound !== undefined) {
      if (typeof value !== "string") {
        return { ok: false, reason: "invalid_envelope", detail: `meta.${key} must be a string` };
      }
      if (byteLength(value) > bound) {
        return {
          ok: false,
          reason: "limit_exceeded",
          detail: `meta.${key} is ${byteLength(value)} B, limit ${bound} B`,
        };
      }
      continue;
    }
    if ((META_BOOLEAN_KEYS as readonly string[]).includes(key)) {
      if (typeof value !== "boolean") {
        return { ok: false, reason: "invalid_envelope", detail: `meta.${key} must be a boolean` };
      }
      continue;
    }
    if ((META_INTEGER_KEYS as readonly string[]).includes(key)) {
      if (typeof value !== "number" || !Number.isInteger(value) || value < 0) {
        return {
          ok: false,
          reason: "invalid_envelope",
          detail: `meta.${key} must be a non-negative integer`,
        };
      }
      continue;
    }
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: `meta.${key} is not allowlisted`,
    };
  }
  const encoded = byteLength(JSON.stringify(meta));
  if (encoded > LIMITS.metaBytes) {
    return {
      ok: false,
      reason: "limit_exceeded",
      detail: `meta encodes to ${encoded} B, limit ${LIMITS.metaBytes} B`,
    };
  }
  return { ok: true };
}

/**
 * Validate an envelope against the frozen 0.1 rules. An envelope that does
 * not validate is never emitted and never delivered — the caller counts the
 * rejection instead (§3.8: reject, never truncate into a valid-looking event).
 */
export function validateEnvelope(envelope: unknown): ValidationResult {
  if (!isPlainObject(envelope)) {
    return { ok: false, reason: "invalid_envelope", detail: "envelope must be an object" };
  }
  const e = envelope;

  if (e.schema_version !== SCHEMA_VERSION) {
    // §3.1: an unrecognised version is ignored and counted, never partially
    // parsed. The adapter only ever emits this version.
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: `schema_version must be ${SCHEMA_VERSION}`,
    };
  }
  if ("ingested_at_ms" in e) {
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: "an adapter-emitted envelope must not carry ingested_at_ms",
    };
  }

  const idempotency = e.idempotency;
  if (idempotency === "strict") {
    if (typeof e.event_id !== "string" || !STRICT_EVENT_ID_RE.test(e.event_id)) {
      return { ok: false, reason: "invalid_envelope", detail: "strict event_id must be ev1_<64 hex>" };
    }
  } else if (idempotency === "best_effort") {
    if (typeof e.event_id !== "string" || !BEST_EFFORT_EVENT_ID_RE.test(e.event_id)) {
      return {
        ok: false,
        reason: "invalid_envelope",
        detail: "best-effort event_id must be ev1b_<32 hex>",
      };
    }
  } else {
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: "idempotency must be strict or best_effort",
    };
  }

  if (typeof e.event_type !== "string" || !(EVENT_TYPES as readonly string[]).includes(e.event_type)) {
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: `event_type is not in the closed vocabulary: ${String(e.event_type)}`,
    };
  }
  if (e.observation !== "observed") {
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: "the adapter emits observed events only",
    };
  }
  if (typeof e.observed_at_ms !== "number" || !Number.isInteger(e.observed_at_ms) || e.observed_at_ms <= 0) {
    return { ok: false, reason: "invalid_envelope", detail: "observed_at_ms must be a positive integer" };
  }
  for (const key of ["host", "host_version", "source", "adapter_version"] as const) {
    if (typeof e[key] !== "string" || (e[key] as string) === "") {
      return { ok: false, reason: "invalid_envelope", detail: `${key} must be a non-empty string` };
    }
    if (byteLength(e[key] as string) > LIMITS.envelopeBytes) {
      return { ok: false, reason: "limit_exceeded", detail: `${key} is too large` };
    }
  }
  // §3.8 hard limit: an individual string field is ≤ 512 B. The per-key meta
  // bounds are tighter; this catches the unbounded top-level fields.
  for (const key of [
    "event_id",
    "host",
    "host_version",
    "source",
    "adapter_version",
    "project_id",
    "project_id_source",
    "framework_name",
    "framework_session_id",
    "outcome_code",
  ] as const) {
    const value = e[key];
    if (typeof value === "string" && byteLength(value) > 512) {
      return {
        ok: false,
        reason: "limit_exceeded",
        detail: `${key} is ${byteLength(value)} B, per-field limit 512 B`,
      };
    }
  }
  if (e.source !== ADAPTER_SOURCE) {
    return { ok: false, reason: "invalid_envelope", detail: `source must be ${ADAPTER_SOURCE}` };
  }
  if (e.trust !== TRUST_OBSERVATION) {
    return { ok: false, reason: "invalid_envelope", detail: "trust must be observation" };
  }
  if (e.payload !== null) {
    return { ok: false, reason: "invalid_envelope", detail: "payload must be null in mpm-event/0.1" };
  }
  if (e.framework_name !== HOST_NAME) {
    return { ok: false, reason: "invalid_envelope", detail: `framework_name must be ${HOST_NAME}` };
  }
  if (e.framework_session_id !== null && typeof e.framework_session_id !== "string") {
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: "framework_session_id must be a string or null",
    };
  }
  if (typeof e.framework_session_id === "string" && e.framework_session_id.trim() === "") {
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: "framework_session_id must be absent/null rather than empty",
    };
  }
  if (e.project_id !== null) {
    // A derived id is `pj_<32 hex>`; an explicitly configured one is any
    // normalised token (`^[A-Za-z0-9._:-]{1,128}$`, §3.3). Both are legal.
    if (typeof e.project_id !== "string" ||
        (!PROJECT_ID_RE.test(e.project_id) && !EXPLICIT_ID_RE.test(e.project_id))) {
      return {
        ok: false,
        reason: "invalid_envelope",
        detail: "project_id must be null, pj_<32 hex>, or an explicit normalised id",
      };
    }
  }
  if (typeof e.project_id_source !== "string" ||
      !(PROJECT_ID_SOURCES as readonly string[]).includes(e.project_id_source)) {
    return { ok: false, reason: "invalid_envelope", detail: "project_id_source is not a known source" };
  }
  if ((e.project_id === null) !== (e.project_id_source === "unresolved")) {
    return {
      ok: false,
      reason: "invalid_envelope",
      detail: "project_id and project_id_source disagree (null iff unresolved)",
    };
  }

  // §3.7 outcomes
  if (e.operation !== "present" && e.operation !== "absent") {
    return { ok: false, reason: "invalid_envelope", detail: "operation must be present or absent" };
  }
  if (!["success", "error", "unknown"].includes(String(e.result_status))) {
    return { ok: false, reason: "invalid_envelope", detail: "result_status is not a known value" };
  }
  if (e.operation === "absent") {
    if (e.result_status !== "unknown") {
      return {
        ok: false,
        reason: "invalid_envelope",
        detail: "an observation with no operation must not assert an outcome",
      };
    }
    if ("outcome_class" in e || "outcome_code" in e) {
      return {
        ok: false,
        reason: "invalid_envelope",
        detail: "outcome_class/outcome_code must be absent when operation is absent",
      };
    }
  } else {
    if (e.result_status === "unknown") {
      return {
        ok: false,
        reason: "invalid_envelope",
        detail: "unknown result_status is legal only when operation is absent",
      };
    }
    if (typeof e.outcome_class !== "string" ||
        !(TOOL_OUTCOME_CLASSES as readonly string[]).includes(e.outcome_class)) {
      return {
        ok: false,
        reason: "invalid_envelope",
        detail: "outcome_class must be a ToolOutcomeClass member when operation is present",
      };
    }
    if ("outcome_code" in e) {
      if (e.outcome_class === "ok") {
        return {
          ok: false,
          reason: "invalid_envelope",
          detail: "outcome_code must be absent when outcome_class is ok",
        };
      }
      if (typeof e.outcome_code !== "string" || !OUTCOME_CODE_RE.test(e.outcome_code)) {
        return { ok: false, reason: "invalid_envelope", detail: "outcome_code must be a bounded identifier" };
      }
    }
  }

  const metaResult = metaValidation(e.meta);
  if (!metaResult.ok) return metaResult;

  const encoded = byteLength(JSON.stringify(e));
  if (encoded > LIMITS.envelopeBytes) {
    return {
      ok: false,
      reason: "limit_exceeded",
      detail: `envelope encodes to ${encoded} B, limit ${LIMITS.envelopeBytes} B`,
    };
  }
  return { ok: true };
}
