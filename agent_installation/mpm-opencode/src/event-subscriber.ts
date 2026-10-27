// src/event-subscriber.ts — subscribe, iterate, bound, map, buffer.
//
// Contract §3.8 / A5, binding on this implementation:
//
//   > No subprocess call may run inside the event iterator. The adapter must
//   > enqueue and handle envelopes out of band. (Measured: one `mpm call`
//   > spawn costs ~39 ms; a synchronous call inside the loop is a slow
//   > consumer, and a slow consumer fails the stream.)
//
// So the iteration loop does exactly three things per event: map it, enqueue
// the result, and return. Delivery runs in a separate drain loop.
//
// The host stream is volatile by contract (a slow consumer overflows and the
// stream fails; there is no replay). Two consequences are encoded here:
//
//   * the buffer is BOUNDED and drop-oldest, with drops counted rather than
//     hidden — an unbounded queue would trade a visible gap for unbounded
//     memory, which is worse for a long-lived host process;
//   * a missing event is never reconstructed. Nothing in this module infers
//     an event that the host did not deliver.
//
// CORE INGESTION IS P5 (Session A). This module implements the CLIENT half:
// the transport interface, strict receipt validation, bounded retry, and
// honest degradation. It never fabricates a receipt and never claims a
// durable write. With no ingest endpoint configured (today's default) it
// counts `dropped_unconfigured` and delivers nothing.

import { appendFileSync } from "node:fs";
import {
  type EventEnvelope,
  SCHEMA_VERSION,
  validateEnvelope,
} from "./event-envelope.js";
import {
  buildSessionObserved,
  type HostEvent,
  type MappedEvent,
  type MappingContext,
  mapHostEvent,
} from "./event-mapper.js";

// --------------------------------------------------------------------------
// Transport
// --------------------------------------------------------------------------

/** The receipt the CORE must return (§3.10 step 7). Never minted here. */
export interface IngestReceipt {
  event_id: string;
  ingested_at_ms: number;
  duplicate: boolean;
}

export type DeliverResult =
  | { ok: true; receipt: IngestReceipt }
  | { ok: false; reason: "unconfigured" | "unavailable" | "rejected" | "malformed_receipt" };

export interface EventTransport {
  readonly mode: "unconfigured" | "mpm_system";
  deliver(envelope: EventEnvelope): Promise<DeliverResult>;
}

/** Today's default: there is no ingest endpoint, and that is not an error. */
export function createUnconfiguredTransport(): EventTransport {
  return {
    mode: "unconfigured",
    async deliver() {
      return { ok: false, reason: "unconfigured" };
    },
  };
}

export interface MpmSystemCall {
  (toolName: string, payload: unknown): Promise<{
    success: boolean;
    payload: Record<string, unknown> | null;
  }>;
}

/**
 * PROVISIONAL transport toward P5's ingest endpoint.
 *
 * The endpoint does not exist yet (A9 places it on `mpm_system`; the action
 * name and payload shape are settled in P5). This client is written against
 * the contract's ingest semantics so that P5 only has to implement the
 * server side:
 *
 *   request : mpm_system { action: "event_ingest", params: { schema_version,
 *             event } }
 *   receipt : { event_id, ingested_at_ms, duplicate }  (§3.10 step 7)
 *
 * A response that is not a well-formed receipt is NOT success. In
 * particular a `success:true` with no receipt is treated as
 * `malformed_receipt`, because a silent "ok" is how a caller comes to
 * believe an observation is durable when nothing stored it.
 */
export function createMpmSystemTransport(opts: {
  call: MpmSystemCall;
  retryDelayMs?: number;
}): EventTransport {
  return {
    mode: "mpm_system",
    async deliver(envelope: EventEnvelope): Promise<DeliverResult> {
      let res: { success: boolean; payload: Record<string, unknown> | null };
      try {
        res = await opts.call("mpm_system", {
          action: "event_ingest",
          params: { schema_version: SCHEMA_VERSION, event: envelope },
        });
      } catch {
        return { ok: false, reason: "unavailable" };
      }

      const payload = res.payload;
      if (!res.success || payload === null || payload.success === false) {
        const error = typeof payload?.error === "string" ? payload.error : "";
        // No parseable envelope at all (timeout, spawn failure, non-JSON) is
        // a transport-availability fact, not a rejection of the event.
        if (error === "") {
          return { ok: false, reason: "unavailable" };
        }
        // The endpoint is absent in this build (unknown action/tool). That is
        // also not a rejection of the event.
        if (/unknown action|unknown tool|unrecognized action/i.test(error)) {
          return { ok: false, reason: "unavailable" };
        }
        return { ok: false, reason: "rejected" };
      }

      const receipt = payload.receipt;
      if (
        typeof receipt !== "object" ||
        receipt === null ||
        Array.isArray(receipt)
      ) {
        return { ok: false, reason: "malformed_receipt" };
      }
      const r = receipt as Record<string, unknown>;
      if (r.event_id !== envelope.event_id) {
        return { ok: false, reason: "malformed_receipt" };
      }
      if (typeof r.ingested_at_ms !== "number" || !Number.isFinite(r.ingested_at_ms)) {
        return { ok: false, reason: "malformed_receipt" };
      }
      if (typeof r.duplicate !== "boolean") {
        return { ok: false, reason: "malformed_receipt" };
      }
      return {
        ok: true,
        receipt: {
          event_id: r.event_id,
          ingested_at_ms: r.ingested_at_ms,
          duplicate: r.duplicate,
        },
      };
    },
  };
}

// --------------------------------------------------------------------------
// Counters
// --------------------------------------------------------------------------

export interface ObserverCounters {
  /** host events the iterator delivered */
  host_events_seen: number;
  /** envelopes that passed validation */
  envelopes_built: number;
  /** per-observation-type counts */
  session_started: number;
  session_observed: number;
  tool_outcomes: number;
  compaction_started: number;
  compaction_completed: number;
  /** host events deliberately not mapped */
  unsupported: number;
  ignored: number;
  /** envelope rejected by the contract validator (never emitted) */
  invalid_envelope: number;
  /** enqueue outcomes */
  enqueued: number;
  dropped_overflow: number;
  dropped_unconfigured: number;
  /** delivery outcomes */
  delivered: number;
  duplicates: number;
  unavailable: number;
  rejected: number;
  malformed_receipt: number;
  delivery_failed: number;
  /** stream health */
  stream_errors: number;
  /** diagnostic: per-event-type unsupported/ignored tally */
  unmapped_by_type: Record<string, number>;
}

function freshCounters(): ObserverCounters {
  return {
    host_events_seen: 0,
    envelopes_built: 0,
    session_started: 0,
    session_observed: 0,
    tool_outcomes: 0,
    compaction_started: 0,
    compaction_completed: 0,
    unsupported: 0,
    ignored: 0,
    invalid_envelope: 0,
    enqueued: 0,
    dropped_overflow: 0,
    dropped_unconfigured: 0,
    delivered: 0,
    duplicates: 0,
    unavailable: 0,
    rejected: 0,
    malformed_receipt: 0,
    delivery_failed: 0,
    stream_errors: 0,
    unmapped_by_type: {},
  };
}

// --------------------------------------------------------------------------
// Bounded buffer
// --------------------------------------------------------------------------

/**
 * A fixed-capacity FIFO. On overflow the OLDEST envelope is dropped and
 * counted: the most recent observations are the ones a retrospective hunt
 * can still act on, and a bounded drop is preferable to unbounded growth in
 * a long-lived host process.
 */
export class BoundedEventBuffer {
  private readonly items: EventEnvelope[] = [];
  constructor(private readonly capacity: number) {
    if (!Number.isInteger(capacity) || capacity <= 0) {
      throw new Error("buffer capacity must be a positive integer");
    }
  }
  get size(): number {
    return this.items.length;
  }
  /** returns "evicted" when the oldest envelope was dropped to make room */
  push(envelope: EventEnvelope): "retained" | "evicted" {
    let outcome: "retained" | "evicted" = "retained";
    if (this.items.length >= this.capacity) {
      this.items.shift();
      outcome = "evicted";
    }
    this.items.push(envelope);
    return outcome;
  }
  shift(): EventEnvelope | undefined {
    return this.items.shift();
  }
}

// --------------------------------------------------------------------------
// Observer
// --------------------------------------------------------------------------

export interface ObserverOptions {
  subscribe: (opts: { signal: AbortSignal }) => AsyncIterable<HostEvent>;
  transport: EventTransport;
  mapping: MappingContext;
  bufferCapacity?: number;
  /** attempts per envelope against a *temporarily* unavailable endpoint */
  maxDeliveryAttempts?: number;
  retryDelayMs?: number;
  /** diagnostic: append each validated envelope to this file (off by default) */
  debugDumpPath?: string;
  /** bounded per-session set used to emit session.observed at most once */
  sessionMemory?: number;
}

export interface LifecycleObserver {
  start(): void;
  stop(opts?: { timeoutMs?: number }): Promise<ObserverCounters>;
  counters(): ObserverCounters;
  /** deterministic test seam: process everything currently buffered */
  drainOnce(): Promise<void>;
  /** deterministic test seam: map+enqueue one event without a stream */
  observe(ev: HostEvent): void;
}

const DEFAULT_BUFFER_CAPACITY = 256;
const DEFAULT_MAX_ATTEMPTS = 2;
const DEFAULT_SESSION_MEMORY = 512;

export function createLifecycleObserver(opts: ObserverOptions): LifecycleObserver {
  const counters = freshCounters();
  const buffer = new BoundedEventBuffer(opts.bufferCapacity ?? DEFAULT_BUFFER_CAPACITY);
  const maxAttempts = Math.max(1, opts.maxDeliveryAttempts ?? DEFAULT_MAX_ATTEMPTS);
  const retryDelayMs = opts.retryDelayMs ?? 0;
  const sessionMemory = Math.max(1, opts.sessionMemory ?? DEFAULT_SESSION_MEMORY);

  const seenSessions = new Set<string>();
  let controller: AbortController | null = null;
  let loopDone: Promise<void> | null = null;
  let drainDone: Promise<void> | null = null;
  let started = false;

  /**
   * Wake signal for the drain loop. Polling with a timer is wrong here: the
   * host process must be free to exit, so a timer would have to be unref'd —
   * and an unref'd timer can leave `stop()` waiting on a promise the event
   * loop will never resolve. A promise resolved by enqueue/abort has neither
   * problem.
   */
  let wakeDrain: (() => void) | null = null;
  function notifyDrain(): void {
    const wake = wakeDrain;
    wakeDrain = null;
    wake?.();
  }
  function waitForWork(signal: AbortSignal): Promise<void> {
    if (signal.aborted) return Promise.resolve();
    return new Promise<void>((resolve) => {
      wakeDrain = resolve;
      signal.addEventListener(
        "abort",
        () => {
          if (wakeDrain === resolve) wakeDrain = null;
          resolve();
        },
        { once: true },
      );
    });
  }

  /** A diagnostic sink only: absent unless the operator opts in. */
  function diagnostic(record: Record<string, unknown>): void {
    if (!opts.debugDumpPath) return;
    try {
      appendFileSync(opts.debugDumpPath, JSON.stringify(record) + "\n");
    } catch {
      /* a diagnostic must never break the host */
    }
  }

  function noteSession(sessionID: string | null): void {
    if (!sessionID || seenSessions.has(sessionID)) return;
    if (seenSessions.size >= sessionMemory) {
      const oldest = seenSessions.values().next().value;
      if (oldest !== undefined) seenSessions.delete(oldest);
    }
    seenSessions.add(sessionID);
  }

  function dump(envelope: EventEnvelope): void {
    if (!opts.debugDumpPath) return;
    try {
      appendFileSync(opts.debugDumpPath, JSON.stringify(envelope) + "\n");
    } catch {
      /* a diagnostic must never break the host */
    }
  }

  function emit(envelope: EventEnvelope): void {
    const valid = validateEnvelope(envelope);
    if (!valid.ok) {
      counters.invalid_envelope++;
      // Diagnose by field, never by content: the detail names the offending
      // field only.
      diagnostic({
        kind: "observer.invalid_envelope",
        event_type: envelope.event_type,
        reason: valid.reason,
        detail: valid.detail,
      });
      return;
    }
    counters.envelopes_built++;
    switch (envelope.event_type) {
      case "session.started":
        counters.session_started++;
        break;
      case "session.observed":
        counters.session_observed++;
        break;
      case "tool.outcome":
        counters.tool_outcomes++;
        break;
      case "compaction.started":
        counters.compaction_started++;
        break;
      case "compaction.completed":
        counters.compaction_completed++;
        break;
      default:
        break;
    }
    dump(envelope);

    if (opts.transport.mode === "unconfigured") {
      // Nothing will ever consume the buffer. Counting is the honest
      // representation: the observation was made, and it was not delivered.
      counters.dropped_unconfigured++;
      return;
    }
    counters.enqueued++;
    if (buffer.push(envelope) === "evicted") {
      counters.dropped_overflow++;
    }
    notifyDrain();
  }

  function recordUnmapped(type: string): void {
    counters.unmapped_by_type[type] = (counters.unmapped_by_type[type] ?? 0) + 1;
  }

  function observe(ev: HostEvent): void {
    counters.host_events_seen++;
    const mapped: MappedEvent = mapHostEvent(ev, opts.mapping);

    if (mapped.kind === "envelope") {
      emit(mapped.envelope);
      noteSession(mapped.sessionID);
      return;
    }
    if (mapped.kind === "session.activity") {
      if (!seenSessions.has(mapped.sessionID)) {
        noteSession(mapped.sessionID);
        emit(buildSessionObserved(ev, opts.mapping, mapped.sessionID));
      }
      return;
    }
    if (mapped.kind === "unsupported") {
      counters.unsupported++;
      recordUnmapped(mapped.hostEventType);
      return;
    }
    counters.ignored++;
    recordUnmapped(mapped.hostEventType);
  }

  async function deliverOne(envelope: EventEnvelope): Promise<void> {
    let attempt = 0;
    let result: DeliverResult = { ok: false, reason: "unavailable" };
    while (attempt < maxAttempts) {
      attempt++;
      result = await opts.transport.deliver(envelope);
      if (result.ok || result.reason !== "unavailable") break;
      if (attempt < maxAttempts && retryDelayMs > 0) {
        await new Promise<void>((resolve) => setTimeout(resolve, retryDelayMs));
      }
    }
    if (result.ok) {
      // Defence in depth: a receipt that names a different event is not a
      // receipt for THIS event, whatever the transport claimed.
      if (result.receipt.event_id !== envelope.event_id) {
        counters.malformed_receipt++;
        return;
      }
      counters.delivered++;
      if (result.receipt.duplicate) counters.duplicates++;
      return;
    }
    switch (result.reason) {
      case "unavailable":
        counters.unavailable++;
        break;
      case "rejected":
        counters.rejected++;
        break;
      case "malformed_receipt":
        counters.malformed_receipt++;
        break;
      default:
        counters.delivery_failed++;
        break;
    }
  }

  async function drainOnce(): Promise<void> {
    for (;;) {
      const next = buffer.shift();
      if (!next) return;
      await deliverOne(next);
    }
  }

  function start(): void {
    if (started) return;
    started = true;
    controller = new AbortController();
    const signal = controller.signal;

    loopDone = (async () => {
      diagnostic({ kind: "observer.loop_started" });
      let first = true;
      try {
        for await (const ev of opts.subscribe({ signal })) {
          if (signal.aborted) break;
          if (first) {
            first = false;
            diagnostic({ kind: "observer.first_event", type: String((ev as HostEvent)?.type ?? "") });
          }
          // Synchronous by design (A5): map + enqueue only. No I/O here.
          observe(ev);
        }
        diagnostic({ kind: "observer.loop_ended" });
      } catch (e) {
        if (!signal.aborted) {
          counters.stream_errors++;
          diagnostic({ kind: "observer.stream_error", error: String(e) });
        }
      }
    })();

    drainDone = (async () => {
      while (!signal.aborted) {
        if (buffer.size === 0) {
          await waitForWork(signal);
          continue;
        }
        await drainOnce();
      }
    })();
  }

  async function stop(stopOpts: { timeoutMs?: number } = {}): Promise<ObserverCounters> {
    const timeoutMs = stopOpts.timeoutMs ?? 500;
    const deadline = Date.now() + timeoutMs;
    // Snapshot BEFORE any await: teardown may be interrupted by process exit,
    // and a diagnostic that only lands after a flush is not reliable evidence.
    diagnostic({ kind: "observer.counters", phase: "teardown", counters: { ...counters } });
    controller?.abort();

    const bounded = async (p: Promise<void> | null): Promise<void> => {
      if (!p) return;
      const remaining = Math.max(0, deadline - Date.now());
      // A ref'd timer, cleared as soon as the promise wins: the bounded wait
      // must be *observable* to the event loop, or a stalled promise turns
      // into "the event loop resolved while a promise was still pending".
      let timer: ReturnType<typeof setTimeout> | undefined;
      const timeout = new Promise<void>((resolve) => {
        timer = setTimeout(resolve, remaining);
      });
      try {
        await Promise.race([p.catch(() => undefined), timeout]);
      } finally {
        if (timer !== undefined) clearTimeout(timer);
      }
    };

    await bounded(loopDone);
    await bounded(drainDone);
    // Final bounded flush; never waits past the deadline.
    if (buffer.size > 0 && Date.now() < deadline) {
      let timer: ReturnType<typeof setTimeout> | undefined;
      const timeout = new Promise<void>((resolve) => {
        timer = setTimeout(resolve, Math.max(0, deadline - Date.now()));
      });
      try {
        await Promise.race([drainOnce().catch(() => undefined), timeout]);
      } finally {
        if (timer !== undefined) clearTimeout(timer);
      }
    }
    diagnostic({ kind: "observer.counters", phase: "final", counters });
    return counters;
  }

  return {
    start,
    stop,
    counters: () => counters,
    drainOnce,
    observe,
  };
}

/** One-line, operator-facing summary; contains counts only. */
export function summariseCounters(c: ObserverCounters): string {
  return [
    "mpm-opencode lifecycle observer:",
    `host_events=${c.host_events_seen}`,
    `envelopes=${c.envelopes_built}`,
    `started=${c.session_started}`,
    `observed=${c.session_observed}`,
    `tools=${c.tool_outcomes}`,
    `compaction=${c.compaction_started}/${c.compaction_completed}`,
    `unsupported=${c.unsupported}`,
    `ignored=${c.ignored}`,
    `invalid=${c.invalid_envelope}`,
    `enqueued=${c.enqueued}`,
    `dropped_overflow=${c.dropped_overflow}`,
    `dropped_unconfigured=${c.dropped_unconfigured}`,
    `delivered=${c.delivered}`,
    `duplicates=${c.duplicates}`,
    `unavailable=${c.unavailable}`,
    `rejected=${c.rejected}`,
    `malformed_receipt=${c.malformed_receipt}`,
    `stream_errors=${c.stream_errors}`,
  ].join(" ");
}
