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
  | {
      ok: false;
      reason: "unconfigured" | "unavailable" | "rejected" | "malformed_receipt" | "delivery_failed";
    };

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
//
// TWO KINDS OF NUMBER, AND THE DISTINCTION IS THE POINT.
//
// CUMULATIVE counters (ObserverCounters) are monotonic totals: they only
// ever go up, and they describe what has happened so far.
//
// INSTANTANEOUS state (ObserverState) describes what is true RIGHT NOW:
// how many envelopes are still queued, and how many deliveries are in
// flight.
//
// Mixing the two was the defect. `delivered` is cumulative, so an
// envelope whose delivery is still in flight is in NEITHER `delivered`
// nor any failure bucket — and a snapshot taken at that moment made the
// envelope look lost, when the only truth was that it had not finished
// yet. The 2026-10-27 isolated run recorded 10 events stored by the core
// against a client summary of `enqueued=10, delivered=9`: the tenth
// delivery was IN FLIGHT, and the client had no way to say so.
//
// So the accounting is now closed. At any snapshot:
//
//   enqueued = delivered + failed + dropped_overflow + abandoned
//               + queued + in_flight
//
// where `failed` is unavailable + rejected + malformed_receipt +
// delivery_failed, and `duplicates` is a SUBSET of `delivered` (a
// duplicate is a successful delivery the server recognised as one) and
// is therefore not added separately.
//
// `delivered` counts a delivery ONLY when the server returned a receipt
// that validated against THIS envelope's event id. A transport attempt
// is never a delivery. The host stream is volatile and delivery is
// best-effort; MPM's ingest endpoint performs the durable
// deduplication. Nothing here claims exactly-once delivery.

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
  /**
   * deliveries that ended in a RECEIPT that validated for this envelope.
   * Includes `duplicates`. A transport attempt alone is never a delivery.
   */
  delivered: number;
  /** subset of `delivered`: the server reported the event id as already stored */
  duplicates: number;
  /** the endpoint was not reachable (absent action, timeout, spawn failure) */
  unavailable: number;
  /** the server explicitly refused the envelope */
  rejected: number;
  /** a response that was not a valid receipt for THIS envelope */
  malformed_receipt: number;
  /** any other delivery failure */
  delivery_failed: number;
  /**
   * total transport ATTEMPTS. A retried delivery counts once per
   * attempt, so this exceeds the number of envelopes attempted.
   */
  delivery_attempts: number;
  /**
   * envelopes discarded at teardown WITHOUT a delivery attempt. They
   * were queued, and the queue was abandoned because the observer stopped.
   */
  abandoned: number;
  /** stream health */
  stream_errors: number;
  /** diagnostic: per-event-type unsupported/ignored tally */
  unmapped_by_type: Record<string, number>;
}

/**
 * INSTANTANEOUS state — what is true at the instant of the snapshot.
 *
 * These are NOT cumulative. `queued` shrinks as the queue drains;
 * `in_flight` rises and falls with the transport.
 */
export interface ObserverState {
  /** envelopes in the bounded queue, not yet attempted */
  queued: number;
  /** deliveries currently awaiting the transport */
  in_flight: number;
}

/** A cumulative reading and an instantaneous reading, taken together. */
export interface ObserverSnapshot {
  counters: ObserverCounters;
  state: ObserverState;
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
    delivery_attempts: 0,
    abandoned: 0,
    stream_errors: 0,
    unmapped_by_type: {},
  };
}

/** The delivery-failure total: every terminal failure bucket. */
function failedTotal(c: ObserverCounters): number {
  return c.unavailable + c.rejected + c.malformed_receipt + c.delivery_failed;
}

/**
 * The DELIVERY ACCOUNTING invariant, evaluated against a snapshot.
 *
 * Every envelope that was enqueued must be in exactly one bucket:
 * delivered, failed, evicted by the bound, abandoned at teardown, still
 * queued, or in flight. A snapshot that does not balance is a
 * diagnostic bug, not a transport condition — and the difference between
 * the two is exactly what this function exists to expose.
 */
export function accountingBalance(s: ObserverSnapshot): {
  enqueued: number;
  accounted: number;
  balanced: boolean;
} {
  const accounted =
    s.counters.delivered +
    failedTotal(s.counters) +
    s.counters.dropped_overflow +
    s.counters.abandoned +
    s.state.queued +
    s.state.in_flight;
  return {
    enqueued: s.counters.enqueued,
    accounted,
    balanced: accounted === s.counters.enqueued,
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
  /**
   * Drop everything still queued and return how many envelopes were
   * discarded.
   *
   * This is the teardown path: the queue is abandoned because the
   * observer stopped, and the count says so. Without it the abandoned
   * envelopes would sit in a bucket nobody counts — indistinguishable
   * from envelopes that were never observed at all.
   */
  discard(): number {
    const n = this.items.length;
    this.items.length = 0;
    return n;
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

/**
 * The outcome of ONE envelope's delivery attempt sequence.
 *
 * The vocabulary is deliberately closed and one-to-one with the
 * cumulative counters, so a diagnostic record can be aggregated into the
 * same totals the counters carry. `delivered` means a receipt validated
 * for THIS envelope's event id — a transport attempt is never a
 * delivery. `duplicate` is a property of a delivery, not an outcome of
 * its own (a duplicate is still a delivery).
 */
export type DeliveryOutcome =
  | "delivered"
  | "malformed_receipt"
  | "unavailable"
  | "rejected"
  | "delivery_failed";

export interface LifecycleObserver {
  start(): void;
  /** bounded teardown; returns the FINAL reading (cumulative + instantaneous) */
  stop(opts?: { timeoutMs?: number }): Promise<ObserverSnapshot>;
  /** the cumulative counters alone (a reading, not the live object) */
  counters(): ObserverCounters;
  /** the cumulative counters AND the instantaneous queue/in-flight state */
  snapshot(): ObserverSnapshot;
  /** the instantaneous queue/in-flight state alone */
  state(): ObserverState;
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
   * Deliveries currently awaiting the transport.
   *
   * This is INSTANTANEOUS state, not a cumulative counter: it rises when
   * an envelope leaves the queue for the transport and falls when the
   * transport answers. Before this existed, an envelope in this window
   * was in no bucket at all, and a reading taken in that window made it
   * look lost.
   */
  let inFlight = 0;

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
    // The envelope leaves the queue and enters the transport. Registering
    // that transition SYNCHRONOUSLY — before the first await — is what
    // closes the accounting gap: there is no instant at which an
    // envelope is in neither `queued` nor `in_flight`.
    inFlight++;
    try {
      let attempt = 0;
      let result: DeliverResult = { ok: false, reason: "unavailable" };
      while (attempt < maxAttempts) {
        attempt++;
        // An ATTEMPT is not a delivery. The two are counted separately:
        // `delivery_attempts` is what the transport was asked, `delivered`
        // is what the server acknowledged with a valid receipt.
        counters.delivery_attempts++;
        try {
          result = await opts.transport.deliver(envelope);
        } catch {
          // A transport that throws is a delivery failure, not a reason
          // to lose the envelope from the accounting.
          result = { ok: false, reason: "delivery_failed" };
        }
        if (result.ok || result.reason !== "unavailable") break;
        if (attempt < maxAttempts && retryDelayMs > 0) {
          await new Promise<void>((resolve) => setTimeout(resolve, retryDelayMs));
        }
      }

      // ONE synchronous block: the outcome counters and the diagnostic
      // record all move together. Splitting them across an await would
      // recreate the very gap this file exists to close.
      let outcome: DeliveryOutcome;
      let duplicate = false;
      if (result.ok) {
        // Defence in depth: a receipt that names a different event is not a
        // receipt for THIS event, whatever the transport claimed.
        if (!result.receipt || result.receipt.event_id !== envelope.event_id) {
          counters.malformed_receipt++;
          outcome = "malformed_receipt";
        } else {
          counters.delivered++;
          duplicate = result.receipt.duplicate === true;
          if (duplicate) counters.duplicates++;
          outcome = "delivered";
        }
      } else {
        switch (result.reason) {
          case "unavailable":
            counters.unavailable++;
            outcome = "unavailable";
            break;
          case "rejected":
            counters.rejected++;
            outcome = "rejected";
            break;
          case "malformed_receipt":
            counters.malformed_receipt++;
            outcome = "malformed_receipt";
            break;
          case "delivery_failed":
            counters.delivery_failed++;
            outcome = "delivery_failed";
            break;
          default:
            // "unconfigured" can only reach here if the transport
            // contradicted its own mode. The envelope WAS enqueued, so it
            // is a failed delivery — not an unconfigured drop, which would
            // break the accounting by counting an envelope that was never
            // enqueued.
            counters.delivery_failed++;
            outcome = "delivery_failed";
            break;
        }
      }
      // A per-delivery record, so an ACKNOWLEDGED delivery is on record
      // even if the final snapshot is lost to an interrupted teardown. The
      // key is `envelope_event_id`, not `event_id`: the diagnostic file is
      // also the envelope dump, and a delivery row must not be mistaken
      // for an envelope.
      diagnostic({
        kind: "observer.delivery",
        envelope_event_id: envelope.event_id,
        outcome,
        duplicate,
        attempts: attempt,
      });
    } finally {
      // The in-flight slot is ALWAYS released. A leaked slot would make
      // every later reading unbalanced, which is worse than the defect
      // this file exists to correct.
      inFlight--;
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

  /** the instantaneous state — what is true at this instant */
  function readState(): ObserverState {
    return { queued: buffer.size, in_flight: inFlight };
  }

  /** a reading: the cumulative counters plus the instantaneous state */
  function snapshot(): ObserverSnapshot {
    return { counters: { ...counters }, state: readState() };
  }

  async function stop(stopOpts: { timeoutMs?: number } = {}): Promise<ObserverSnapshot> {
    const timeoutMs = stopOpts.timeoutMs ?? 500;
    const deadline = Date.now() + timeoutMs;

    // Stop accepting new work FIRST. Aborting is synchronous and its
    // effect on the drain loop lands in a microtask, so the reading
    // below still describes the instant of teardown.
    controller?.abort();

    // The reading is taken SYNCHRONOUSLY and written SYNCHRONOUSLY:
    // teardown may be interrupted by process exit, and a diagnostic that
    // only lands after a flush is not reliable evidence. This is the
    // snapshot that survives a host exit — so it must be TRUTHFUL, which
    // is why it now carries the instantaneous `queued` and `in_flight`
    // state and not only cumulative counters.
    //
    // Before the correction this reading said `enqueued=10 delivered=9`
    // for a run in which the core had stored ten events: the tenth
    // delivery was in flight, and the reading had no way to say so.
    diagnostic({
      kind: "observer.counters",
      phase: "teardown",
      counters: { ...counters },
      state: readState(),
    });

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

    // Whatever is STILL queued was never attempted: the observer stopped
    // before the transport was asked. Counting it as `abandoned` is the
    // difference between "not delivered" and "not even tried", and it
    // keeps the accounting closed.
    counters.abandoned += buffer.discard();

    const finalSnapshot = snapshot();
    diagnostic({
      kind: "observer.counters",
      phase: "final",
      counters: { ...finalSnapshot.counters },
      state: finalSnapshot.state,
    });
    return finalSnapshot;
  }

  return {
    start,
    stop,
    counters: () => ({ ...counters }),
    snapshot,
    state: readState,
    drainOnce,
    observe,
  };
}

/**
 * One-line, operator-facing summary; contains counts only.
 *
 * The instantaneous state (`queued`, `in_flight`) is on the same line as
 * the cumulative counters, because a reader who sees `enqueued=10
 * delivered=9` must be able to tell, from the line itself, whether the
 * tenth delivery was still in flight, still queued, or genuinely gone.
 */
export function summariseSnapshot(s: ObserverSnapshot): string {
  const c = s.counters;
  const balance = accountingBalance(s);
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
    `delivery_failed=${c.delivery_failed}`,
    `delivery_attempts=${c.delivery_attempts}`,
    `abandoned=${c.abandoned}`,
    `queued=${s.state.queued}`,
    `in_flight=${s.state.in_flight}`,
    `balanced=${balance.balanced}`,
    `stream_errors=${c.stream_errors}`,
  ].join(" ");
}
