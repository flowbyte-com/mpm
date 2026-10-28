// src/event-mapper.ts — the structural allowlist (contract §3.8, A7).
//
// A's ruling before P4 could start:
//
//   > No lifecycle event may reach MPM storage until the adapter maps it
//   > through a structural allowlist … The adapter maps explicitly — for each
//   > event type it enumerates the exact allowlisted fields it copies. No
//   > spread, no `data` forwarding.
//
// This module is that allowlist. It reads a small, named set of scalar fields
// from a host event and NEVER copies a container. The fields the host uses to
// carry content are not merely dropped — they are never read:
//
//   session.text.*.text / .delta          assistant output
//   session.tool.input.ended.text          raw tool input (commands, patches)
//   session.tool.called.input              parsed tool input
//   session.tool.success.content           raw tool output (file contents)
//   session.compaction.delta / ended.text  the full compaction summary
//   session.created / renamed.title        user-authored text
//   session.instructions.updated.delta     instruction content
//
// Everything the mapper emits comes from `HostEvent.data` scalar reads whose
// exact key names are below. `payload` stays null (contract §3.8).

import {
  deriveBestEffortEventId,
  deriveStrictEventId,
  type EventEnvelope,
  type EventType,
  type MetaValue,
  type Operation,
  type ProjectIdSource,
  type ResultStatus,
  SCHEMA_VERSION,
  ADAPTER_SOURCE,
  HOST_NAME,
  TRUST_OBSERVATION,
  type ToolOutcomeClass,
} from "./event-envelope.js";

/** The subset of the host's event envelope the mapper is allowed to look at. */
export interface HostEvent {
  id?: unknown;
  created?: unknown;
  type?: unknown;
  data?: unknown;
}

export interface MappingContext {
  hostVersion: string;
  adapterVersion: string;
  projectId: string | null;
  projectIdSource: ProjectIdSource;
  /** injectable clock (unix ms); the adapter observes at receipt time (§3.6) */
  now?: () => number;
}

export type MappedEvent =
  | {
      kind: "envelope";
      /** what was observed, for counters and tests */
      observation:
        | "started"
        | "session.outcome"
        | "session.lifecycle"
        | "tool"
        | "compaction.started"
        | "compaction.completed";
      envelope: EventEnvelope;
      sessionID: string | null;
    }
  /** an event for a session that produced no envelope of its own */
  | { kind: "session.activity"; sessionID: string }
  | { kind: "unsupported"; hostEventType: string; reason: string }
  | { kind: "ignored"; hostEventType: string; reason: string };

// --------------------------------------------------------------------------
// bounded scalar reads — the only way data crosses this boundary
// --------------------------------------------------------------------------

function asRecord(v: unknown): Record<string, unknown> | null {
  return typeof v === "object" && v !== null && !Array.isArray(v)
    ? (v as Record<string, unknown>)
    : null;
}

/** A bounded, non-empty string, or undefined. Containers are never stringified. */
function scalar(v: unknown, maxBytes: number): string | undefined {
  if (typeof v !== "string") return undefined;
  const s = v.trim();
  if (s === "") return undefined;
  return Buffer.byteLength(s, "utf8") <= maxBytes ? s : undefined;
}

function idScalar(v: unknown): string | undefined {
  // Identifiers are opaque tokens; 128 B is the contract's meta bound and is
  // generous for ses_/msg_/call_/evt_ values.
  return scalar(v, 128);
}

function hostEventType(ev: HostEvent): string {
  return scalar(ev.type, 64) ?? "";
}

function dataOf(ev: HostEvent): Record<string, unknown> | null {
  return asRecord(ev.data);
}

function sessionIdOf(ev: HostEvent): string | undefined {
  const data = dataOf(ev);
  return data ? idScalar(data.sessionID) : undefined;
}

// --------------------------------------------------------------------------
// envelope assembly
// --------------------------------------------------------------------------

interface EnvelopeArgs {
  eventType: EventType;
  hostType: string;
  sessionID: string | null;
  operation: Operation;
  resultStatus: ResultStatus;
  outcomeClass?: ToolOutcomeClass;
  meta: Record<string, MetaValue>;
}

function buildEnvelope(ctx: MappingContext, ev: HostEvent, args: EnvelopeArgs): EventEnvelope {
  // A3: the measured host identity is `evt_<hex>`, unique per published
  // event. A1: it is the ONLY thing that may serve as an identity — the
  // host's callID is correlation, and nested calls share it.
  const hostEventKey = idScalar(ev.id);
  const strict = hostEventKey !== undefined;
  const eventId = strict
    ? deriveStrictEventId({ eventType: args.eventType, hostEventKey: hostEventKey as string })
    : deriveBestEffortEventId();

  const meta: Record<string, MetaValue> = {};
  if (args.hostType !== "") meta.host_event_type = args.hostType;
  for (const [k, v] of Object.entries(args.meta)) {
    if (v !== undefined && v !== "") meta[k] = v;
  }

  const envelope: EventEnvelope = {
    schema_version: SCHEMA_VERSION,
    event_id: eventId,
    idempotency: strict ? "strict" : "best_effort",
    event_type: args.eventType,
    observation: "observed",
    // §3.6: taken at the moment of observation, never from a payload field.
    observed_at_ms: (ctx.now ?? Date.now)(),
    host: HOST_NAME,
    host_version: ctx.hostVersion,
    source: ADAPTER_SOURCE,
    adapter_version: ctx.adapterVersion,
    project_id: ctx.projectId,
    project_id_source: ctx.projectIdSource,
    framework_name: HOST_NAME,
    framework_session_id: args.sessionID,
    operation: args.operation,
    result_status: args.resultStatus,
    ...(args.operation === "present"
      ? { outcome_class: args.outcomeClass ?? "internal" }
      : {}),
    trust: TRUST_OBSERVATION,
    meta,
    payload: null,
  };
  return envelope;
}

// --------------------------------------------------------------------------
// the mapping table
// --------------------------------------------------------------------------

/**
 * Map one host event. The switch is exhaustive over the types the contract
 * has vocabulary for; every other type is reported as `ignored` or
 * `unsupported` and counted by the caller — never mapped onto a neighbouring
 * type (§3.4).
 */
export function mapHostEvent(ev: HostEvent, ctx: MappingContext): MappedEvent {
  const type = hostEventType(ev);
  const data = dataOf(ev);
  const sessionID = sessionIdOf(ev) ?? null;

  switch (type) {
    case "session.created": {
      // session.started. `title`, `slug` and `location` are never read.
      const model = data ? asRecord(data.model) : null;
      const envelope = buildEnvelope(ctx, ev, {
        eventType: "session.started",
        hostType: type,
        sessionID,
        operation: "absent",
        resultStatus: "unknown",
        meta: {
          host_agent: scalar(data?.agent, 64) ?? "",
          host_model_provider: scalar(model?.providerID, 64) ?? "",
          host_model_id: scalar(model?.id, 128) ?? "",
        },
      });
      return { kind: "envelope", observation: "started", envelope, sessionID };
    }

    case "session.execution.succeeded":
    case "session.execution.failed":
    case "session.execution.interrupted": {
      // A12: a session-level outcome is NOT an MPM operation. The host's word
      // is opaque and lives in meta; result_status stays `unknown` and no
      // outcome_class is asserted.
      const hostResult = type.slice("session.execution.".length);
      const envelope = buildEnvelope(ctx, ev, {
        eventType: "session.observed",
        hostType: type,
        sessionID,
        operation: "absent",
        resultStatus: "unknown",
        meta: { host_result: hostResult },
      });
      return { kind: "envelope", observation: "session.outcome", envelope, sessionID };
    }

    case "session.archived":
    case "session.unarchived":
    case "session.deleted": {
      // The host's SESSION-LIFECYCLE events (measured on opencode
      // v2.1.4 — see measure/session_end_scenarios.py).
      //
      // WHY THEY NEED AN EXPLICIT CASE.
      //
      // They carry a session id, so without one they fall into the
      // activity fallback below — and the fallback emits AT MOST ONE
      // `session.observed` per session. By the time the host archives or
      // deletes a session, that one observation has long since been
      // emitted, so the event was SWALLOWED: the adapter saw it and
      // recorded nothing at all. Measured on the isolated host: 27 host
      // events seen, and ZERO of them recorded the archive.
      //
      // WHY `session.observed` IS THE RIGHT TYPE.
      //
      // §4's table: "`session.observed` | any host event that carries a
      // session id | the adapter saw an event for this session". That is
      // precisely what an archive is, and it is the contract's own
      // NEUTRAL base type — not a neighbour-mapping (§3.4 forbids
      // mapping an event onto a type that ASSERTS something else).
      //
      // WHAT IT DOES NOT ASSERT.
      //
      // It does NOT assert that the session ENDED. An archive is
      // reversible (`session.unarchived` exists, and the host's own
      // `Session.Info.time.archived` is a timestamp, not a tombstone); a
      // delete is a user action, not an agent conclusion. The envelope
      // says "the adapter saw this event for this session" and carries
      // the host's own event type in `meta.host_event_type` — nothing
      // more. No new meta key is introduced: the allowlist (§3.8) is
      // closed and `host_event_type` is already on it.
      //
      // WITHOUT a session id there is no session to have observed, and
      // `session.observed`'s whole meaning depends on it — so the event
      // is COUNTED, exactly as the fallback branch below would count it.
      // A session id is never fabricated.
      if (!sessionID) {
        return {
          kind: "ignored",
          hostEventType: type,
          reason: "no session id and no vocabulary type",
        };
      }
      const envelope = buildEnvelope(ctx, ev, {
        eventType: "session.observed",
        hostType: type,
        sessionID,
        operation: "absent",
        resultStatus: "unknown",
        meta: {},
      });
      return { kind: "envelope", observation: "session.lifecycle", envelope, sessionID };
    }

    case "session.tool.success":
    case "session.tool.failed": {
      // A12: this IS an MPM-scoped operation outcome, so operation=present.
      // The host supplies no finer failure code on the event, so a failure
      // maps to `internal` — the contract's stated fallback. The host's
      // error text and the tool's `content` are never read.
      const ok = type === "session.tool.success";
      const meta: Record<string, MetaValue> = {
        host_result: ok ? "success" : "failed",
        host_call_id: idScalar(data?.id) ?? "",
        host_message_id: idScalar(data?.assistantMessageID) ?? "",
      };
      const envelope = buildEnvelope(ctx, ev, {
        eventType: "tool.outcome",
        hostType: type,
        sessionID,
        operation: "present",
        resultStatus: ok ? "success" : "error",
        outcomeClass: ok ? "ok" : "internal",
        meta,
      });
      return { kind: "envelope", observation: "tool", envelope, sessionID };
    }

    case "session.compaction.started":
    case "session.compaction.ended": {
      const started = type === "session.compaction.started";
      const envelope = buildEnvelope(ctx, ev, {
        eventType: started ? "compaction.started" : "compaction.completed",
        hostType: type,
        sessionID,
        operation: "absent",
        resultStatus: "unknown",
        meta: {
          host_result: started ? "started" : "ended",
          compaction_kind: scalar(data?.reason, 32) ?? "",
        },
      });
      return {
        kind: "envelope",
        observation: started ? "compaction.started" : "compaction.completed",
        envelope,
        sessionID,
      };
    }

    case "session.compaction.failed":
      // The vocabulary has started/completed and no failure type. Mapping a
      // failure onto `compaction.completed` would be exactly the
      // neighbour-mapping §3.4 forbids, so it is counted, not invented.
      return {
        kind: "unsupported",
        hostEventType: type,
        reason: "no mpm-event/0.1 vocabulary type for a failed compaction",
      };

    default: {
      if (sessionID) {
        // Any other event for a session still proves the session exists; the
        // subscriber emits at most one `session.observed` per session for it.
        return { kind: "session.activity", sessionID };
      }
      return { kind: "ignored", hostEventType: type, reason: "no session id and no vocabulary type" };
    }
  }
}

/**
 * Build the once-per-session `session.observed` envelope. The host event key
 * is the triggering event's `evt_…`, so the envelope keeps a stable identity
 * of its own even though the observation is "this session was seen".
 */
export function buildSessionObserved(
  ev: HostEvent,
  ctx: MappingContext,
  sessionID: string,
): EventEnvelope {
  return buildEnvelope(ctx, ev, {
    eventType: "session.observed",
    hostType: hostEventType(ev),
    sessionID,
    operation: "absent",
    resultStatus: "unknown",
    meta: {},
  });
}
