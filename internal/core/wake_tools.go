// wake_tools.go — DM methods for the scheduled_wakes table (Phase 5a).
//
// Architecture: opportunistic delivery with scheduler audit. The DB
// is the durable queue. Three actors cooperate:
//
//	a. The scheduler (cmd/mpm-scheduler + internal/scheduler) runs a
//	   deadline-driven loop that stamps `dispatched_at` on each
//	   notification-kind wake whose target_time has elapsed. This is
//	   the AUDIT TRAIL — it records "the scheduler saw this wake and
//	   when", but it does NOT flip fired=1. The wake stays pending.
//	   See internal/scheduler/dispatch.go dispatchClaimNextAdHocWake
//	   and the 2026-09-23 release-blocker repair.
//
//	b. The opportunistic fold (cmd/mpm/call.go, cmd/mpm-mcp/tools.go,
//	   internal/core/tools/handlers.go checkWakesAndFold) runs
//	   CheckPendingWakes on every MPM call. Notification-kind wakes
//	   where fired=0 AND target_time <= now are surfaced in a
//	   WakesPending block (and rendered as a <system_wake_notification>
//	   XML prefix on MCP responses), then flipped to fired=1 with
//	   fired_at=now. This is the user/agent acknowledgement path for
//	   notification-kind wakes.
//
//	c. Explicit operator resolution (handleResolveWake / ResolveWake)
//	   flips fired=1 with fired_at=now and writes an audit row tagged
//	   fired_by='wake-resolver'. Same terminal state as the fold, with
//	   an explicit audit trail.
//
// Lifecycle of a wake:
//
//  1. Agent calls ScheduleWake(reason, target_time, theory_id?).
//     Row written with fired=0, dispatched_at=NULL.
//  2. Time passes. The scheduler's deadline-driven drain stamps
//     dispatched_at when target_time is reached. fired stays 0.
//  3. ANY subsequent MPM call invokes CheckPendingWakes via the
//     fold (called by handlers from within their dispatch). Wakes
//     where fired=0 AND target_time <= now() are returned in a
//     WakesPending block on the response, marked fired=1,
//     fired_at=now.
//  4. The agent sees the wake, evaluates the reason, optionally
//     resolves a theory, optionally schedules the next wake. Or the
//     operator calls ResolveWake explicitly.
//
// Canonical invariant for scheduled_wakes.fired:
//
//	fired=1 means the wake is TERMINAL — no longer pending for
//	normal delivery. The terminal transition depends on the wake's
//	kind:
//	  - notification-kind:  fold (CheckPendingWakes), explicit
//	                        ResolveWake, OR bounded retirement via
//	                        internal/scheduler/wake_expiration.go
//	                        (the 7-day retention sweep)
//	  - system-kind:        MarkFired after the scheduler-owned
//	                        HandlerFunc completes (recorded in
//	                        scheduler.go:tick dispatch)
//	fired=0 means the wake is still pending for delivery (one of
//	the above terminal transitions has not occurred).
//
// State machine:
//
//	fired=0  dispatched_at=NULL      scheduled; target_time not yet reached
//	fired=0  dispatched_at=<epoch>   scheduler dispatched the wake;
//	                                 still pending (the dispatched_at
//	                                 audit stamp does NOT imply fired=1)
//	fired=1                           terminal; not pending for normal
//	                                 delivery (any of the transitions
//	                                 above)
//
// The scheduler is the daemon (introduced post-Phase-5a as the
// deadline-driven executor). The earlier "no daemon" rationale
// (deprecated 2026-06-26 in commit e1bc707) predates this daemon; the
// current design keeps the daemon but constrains its role to the
// audit trail so it cannot preempt the fold.
package internal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ── THE PER-CALL DELIVERY BUDGET ─────────────────────────────────────
//
// The opportunistic fold is the agent's delivery path and it runs on
// EVERY agent request. It must therefore deliver a BOUNDED amount
// per call.
//
// Three limits, all per call:
//
//   - A ROW cap, per kind. It bounds the scan, the settlement, and
//     the JSON envelope the tools-module handler puts the delivered
//     rows into (WakesPending).
//   - A BYTE cap on the rendered <system_wake_notification> block,
//     per kind.
//   - A BYTE bound on every RENDERED FIELD, and on every
//     caller-supplied string in a delivered row. This is the
//     foundation the byte cap rests on.
//
// WHY THE FIELD BOUNDS ARE THE FOUNDATION.
//
// A byte cap measured on a BATCH is only a bound on the RESPONSE if
// every member of the batch is itself bounded — and the members were
// not. The directive_id is caller-supplied and was rendered verbatim,
// so ONE wake could carry a 200 KB directive_id and the block would
// be 200 KB: fifty times the per-kind cap. Measured at the head that
// carried the byte cap:
//
//   - one wake with a 204,800-byte directive_id rendered a 204,957-byte
//     block against a 4,096-byte per-kind cap;
//   - the same wake's STRUCTURED row — the WakesPending payload the
//     core tool handlers and `mpm call` return — was 205,020 bytes,
//     because the delivered row carried the caller's metadata
//     verbatim. Bounding only the rendered XML block would NOT have
//     closed the hole: `mpm call` returns the structured row, not the
//     rendered one;
//   - the agent-facing arming path accepted a 102,400-byte
//     directive_id and stored it, so one ordinary `mpm_wakes
//     schedule` call was enough to arm the oversized wake.
//
// So the budget is now built from the field bounds up:
//
//   - every field the renderer emits is bounded in BYTES and cut at a
//     CHARACTER boundary. A byte cut can split a multi-byte
//     character; the renderer then escaped the orphan byte as \xNN,
//     which mangles the text the agent reads and inflates the byte
//     cost unpredictably;
//   - every caller-supplied string in a delivered row is bounded, so
//     the STRUCTURED payload is bounded too;
//   - the arming path REFUSES a directive_id too long to render,
//     because a truncated identifier is unusable and a silently
//     mangled one is worse than an explicit refusal.
//
// WHAT IS ACCOUNTED FOR SEPARATELY.
//
// Four quantities, four bounds. They do NOT share one budget, and the
// envelope is the SUM of two of them:
//
//	| quantity                          | bound                          |
//	|-----------------------------------|--------------------------------|
//	| rendered block, per kind          | MaxWakeNotificationBytesPerKind |
//	| rendered block, all kinds         | 3 x the above                  |
//	| structured WakesPending payload   | row caps x per-row delivered bound |
//	| the tool's own payload            | the output policy's own threshold |
//	| the MCP envelope                  | block + payload: MAY exceed the |
//	|                                   | output policy's threshold       |
//
// The MCP adapter prepends the wake block to the tool's response AFTER
// the output-policy spill decision, so the policy's threshold applies to
// the tool's payload ALONE and never bounded the wake block. The two
// are bounded independently and the envelope is their sum.
//
// WHY THE BUDGET IS PER KIND, NOT GLOBAL.
//
// A global budget applied in selection order lets the oldest kind
// starve the others permanently. With a 7-day notification backlog
// and a 1-hour cron backlog, a global cap spent on the oldest rows
// would give the newer kind a zero share on every call, forever —
// which is a silent loss window, the exact thing a budget must not
// create. Per-kind caps give every kind an independent share, so
// every kind makes progress on every call and no kind is starved by
// another's backlog.
//
// WHY THESE NUMBERS.
//
//   - MaxNotificationWakePerCheck / MaxCronWakePerCheck = 16. The
//     budget must keep up with PRODUCTION. One one-minute cron task
//     produces 60 wakes/hour, so 16 wakes per call keeps up with an
//     agent that issues one request every four minutes, and has 16x
//     headroom for an agent that issues one per minute. It must also
//     not swamp the agent's triage: the seeded directive
//     mpm-seed-wake-triage-policy tells the agent to alert the user
//     for every entry with overdue_secs <= 86400, so a burst of 60
//     identical cron entries produces 60 identical alerts. 16 is a
//     page, not a wall.
//
//   - MaxCascadeWakePerCheck = 3 is UNCHANGED. It was already the
//     per-call bound for cascade delivery, and the cascade kind keeps
//     its own cap.
//
//   - MaxWakeNotificationBytesPerKind = 4096. The rendered line of a
//     normal wake is ~105 bytes, so 4096 bytes is ~39 lines: under
//     the 16-row cap for ordinary rows, which means the row cap binds
//     in normal operation and the byte cap is the BACKSTOP for rows
//     whose rendered length is larger. With every rendered field now
//     bounded (see the field bounds below), a single rendered row is at
//     most ~450 bytes, so the cap can NEVER be exceeded by one row —
//     the "one row may exceed the cap" exception this budget used to
//     carry is GONE, and the cap is a true bound.
//
// WHAT THE BUDGET DOES NOT DO.
//
// It does not drop anything. A wake that does not fit the budget
// stays PENDING (fired = 0) and remains eligible for the next call,
// exactly as a cascade wake above MaxCascadeWakePerCheck already
// behaves. The budget is a THROTTLE, not a loss window: an eligible
// wake stays eligible until its own retention window closes.
//
// It also does not make a wake undeliverable. Because every rendered
// field is bounded, an oversized wake costs at most its bounded size
// against the budget, so the healthy wakes behind it are delivered in
// the SAME call. An oversized row cannot starve the rows behind it.
const (
	// MaxNotificationWakePerCheck bounds notification-kind (and
	// untagged) wake delivery per CheckPendingWakes call. Rows above
	// the cap are left pending for the next call.
	MaxNotificationWakePerCheck = 16

	// MaxCronWakePerCheck bounds cron-kind wake delivery per
	// CheckPendingWakes call. Rows above the cap are left pending for
	// the next call.
	MaxCronWakePerCheck = 16

	// MaxWakeNotificationBytesPerKind bounds the rendered
	// <system_wake_notification> block contributed by ONE kind in one
	// call. It is a TRUE bound: every rendered field is itself bounded
	// (MaxWakeRenderedIDBytes, MaxWakeRenderedReasonBytes,
	// MaxWakeDirectiveIDBytes), so a single rendered row can never
	// exceed it and the cap is never exceeded at all.
	MaxWakeNotificationBytesPerKind = 4096
)

// ── THE FIELD BOUNDS ─────────────────────────────────────────────────
//
// The per-kind byte cap is only as strong as the rows it measures.
// These bounds are what make it a bound on the agent-visible RESPONSE
// rather than a bound on the batch.
//
// Every bound is in BYTES, because the budget is measured in bytes and
// a budget quoted in characters is not the budget the agent receives.
// Every cut is made at a CHARACTER boundary, because a byte cut that
// splits a multi-byte character both mangles what the agent reads and
// makes the byte cost unpredictable (the orphan byte is escaped as
// \xNN, which is four bytes of output for one byte of input).
//
// The bounds fall into two families:
//
//   - RENDERED bounds (MaxWakeRenderedIDBytes,
//     MaxWakeRenderedReasonBytes, MaxWakeDirectiveIDBytes) size what
//     the agent reads in the <system_wake_notification> block.
//
//   - DELIVERED bounds (MaxWakeDeliveredStringBytes,
//     MaxWakeDeliveredMetadataBytes, MaxWakeDeliveredMetadataKeys,
//     MaxWakeDeliveredIDBytes) size what the agent reads in the
//     STRUCTURED WakesPending payload. That payload is a DIFFERENT
//     surface with a DIFFERENT bound, and it is accounted for
//     separately.
const (
	// MaxWakeRenderedIDBytes bounds the rendered wake id. The id is
	// substrate-generated (ScheduleWake mints it), so this is a display
	// bound; the full id is in the structured payload.
	MaxWakeRenderedIDBytes = 8

	// MaxWakeRenderedReasonBytes bounds the rendered reason, in bytes.
	// The reason is prose: it is cut with an ellipsis, and the ellipsis
	// is the signal that it was cut.
	MaxWakeRenderedReasonBytes = 80

	// MaxWakeDirectiveIDBytes bounds a rendered directive_id, in bytes.
	// A directive id is an IDENTIFIER the agent passes back to
	// read_directives, so a truncated one is unusable: the arming path
	// refuses a longer value outright (see handleScheduleWake) and the
	// renderer marks a value that was already persisted.
	MaxWakeDirectiveIDBytes = 256

	// MaxWakeDeliveredStringBytes bounds each caller-supplied string in
	// a delivered row: the reason, the theory_id, the created_by, and
	// every string value in the metadata.
	MaxWakeDeliveredStringBytes = 256

	// MaxWakeDeliveredMetadataBytes bounds the marshalled metadata
	// document of a delivered row.
	MaxWakeDeliveredMetadataBytes = 512

	// MaxWakeDeliveredMetadataKeys bounds the number of keys in a
	// delivered row's metadata.
	MaxWakeDeliveredMetadataKeys = 16

	// MaxWakeDeliveredIDBytes bounds the wake id in a delivered row.
	// The id is substrate-generated, so this bound is DEFENSIVE: it
	// exists so that a row written by an older binary, by direct SQL,
	// or by a future producer cannot make the structured payload
	// unbounded.
	MaxWakeDeliveredIDBytes = 64
)

// MaxCascadeWakePerCheck bounds cascade wake delivery per CheckPendingWakes
// call. Cascade wakes (metadata.kind == "cascade") above this cap are left
// unfired (pending) for the next call. Default value: 3.
//
// The cap bounds the BATCH, not the OBLIGATION. It does not
// deduplicate: two cascade wakes carrying the same theory_id and the
// same invalidation_event_id are one obligation that the reconciler
// duplicated, and both are delivered if both are pending (2 <= 3).
// See internal/core/cascade_materializer.go for why that duplication
// arises and what the durable fix is.
const MaxCascadeWakePerCheck = 3

// ScheduleCascadeSummaryWake persists a typed `cascade_summary` wake that
// the scheduler will route through its registered CascadeSummaryHandler.
// Mirrors the CLI's `materialized=... failed=... pending_after=... elapsed=...`
// summary line so the scheduler log (and any operator grepping the wake
// table) sees the same four fields the CLI prints.
//
// The wake row is consumed by the registered handler in the next tick —
// it does NOT surface to the agent via `mpm wake` / `check_wakes` because
// CascadeSummaryHandler is registered (handlers bypass CheckPendingWakes'
// 3-per-call cap). See CascadeDrainHandler for the dedupe policy that
// decides whether to call this at all.
func (dm *DatabaseManager) ScheduleCascadeSummaryWake(ctx context.Context, materialized, failed, pendingAfter int, elapsed time.Duration) error {
	id := fmt.Sprintf("cascade_summary_%d", time.Now().UnixNano())
	meta := map[string]interface{}{
		"kind":          "cascade_summary",
		"materialized":  materialized,
		"failed":        failed,
		"pending_after": pendingAfter,
		"elapsed_ms":    elapsed.Milliseconds(),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal cascade summary metadata: %w", err)
	}
	_, err = dm.db.ExecContext(ctx,
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, fired, created_by, metadata)
		 VALUES (?, ?, ?, NULL, 0, ?, ?)`,
		id, time.Now().Unix(), "cascade tick summary", "cascade_drain", metaJSON,
	)
	if err != nil {
		return fmt.Errorf("insert cascade summary wake: %w", err)
	}
	return nil
}

// LastCascadeSummaryMetrics returns the materialized/failed/pending_after/
// elapsed_ms from the most recent cascade_summary wake row, or
// (zero, zero, zero, zero, false) when no prior summary exists. Used by
// CascadeDrainHandler to dedupe identical idle ticks before inserting a
// new wake row.
//
// The ok return is false when no cascade_summary row has ever been
// written, in which case the caller should always insert (no dedupe
// possible against an empty baseline).
func (dm *DatabaseManager) LastCascadeSummaryMetrics(ctx context.Context) (materialized, failed, pendingAfter int, elapsedMs int64, ok bool, err error) {
	var metaJSON string
	row := dm.db.QueryRowContext(ctx,
		`SELECT metadata FROM scheduled_wakes
		 WHERE `+WakeCascadeSummaryKindFilter+`
		 ORDER BY created_at DESC, target_time DESC LIMIT 1`)
	if err := row.Scan(&metaJSON); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0, 0, 0, false, nil
		}
		return 0, 0, 0, 0, false, fmt.Errorf("query last cascade summary: %w", err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return 0, 0, 0, 0, false, fmt.Errorf("unmarshal last cascade summary: %w", err)
	}
	if v, ok := meta["materialized"].(float64); ok {
		materialized = int(v)
	}
	if v, ok := meta["failed"].(float64); ok {
		failed = int(v)
	}
	if v, ok := meta["pending_after"].(float64); ok {
		pendingAfter = int(v)
	}
	if v, ok := meta["elapsed_ms"].(float64); ok {
		elapsedMs = int64(v)
	}
	return materialized, failed, pendingAfter, elapsedMs, true, nil
}

// ScheduleWake persists a new pending wake. Mirrors handleScheduleWake.
//
// target_time may be an absolute unix epoch (seconds) or a relative
// duration string ("24h", "90m", "7d"). Relative strings are resolved
// against the local clock at insert time; absolute is treated as UTC.
// Returns the new id and the resolved absolute target_time so the
// caller can log it.
//
// theory_id is optional; when set, the wake is intended to evaluate
// that theory (e.g. "check WC2026 R32 result for theory 7383f157...").
//
// recurring workflows are NOT scheduled through ScheduleWake. The
// field that historically carried a cron expression was DEPRECATED
// 2026-07-23 and retired 2026-10-04. Use UpsertScheduledTask (or
// `mpm tasks upsert`) for recurring workflows; the daemon's 60s
// tick polls scheduled_tasks and injects a standard scheduled_wakes
// row for each due task.
// BlockedWakeKindCriticAudit is a wake kind that must not be ARMED and must
// not be DISPATCHED, for as long as the critic's automatic trust behaviour
// is unsafe.
//
// WHY
// ---
// The critic's hunts USED TO emit `mpm_lessons save` findings: they wrote
// TRUSTED lessons directly, with no evidence citation, no review, and no
// pending state, and they tagged each finding with `cycle_<n>` so a
// repeated finding was never recognised as the same one. Meanwhile duplicate
// evidence inflated confidence. That combination made an
// automatically-scheduled critic unsound: it would manufacture
// corroborated-looking knowledge from uncorroborated observations.
//
// The hunts now emit `mpm_theories propose` — a PENDING proposal that
// carries its evidence — and the agent-reachable lesson write is refused
// outright. The arming rule below stands regardless.
//
// The critic's automatic path is a HANDLER, not a scheduler: the scheduler
// fires any due wake whose kind has a registered handler. The handler for
// `critic_audit` exists (internal/scheduler/scheduler.go), the wake kind is
// caller-supplied and was not validated at the producer (ScheduleWake only
// DEFAULTS an absent kind to "notification"), and the public `mpm` scheduling
// tool reaches that producer. So arming the critic was one API call away.
//
// THE TWO GATES
// -------------
//  1. ARMING (producer side, here): ScheduleWake refuses the kind. Every
//     producer is covered - the public handler, the cascade materializer,
//     the wake reconcile loop, and any future internal producer - because
//     the fix lives in the DB-layer producer rather than in one caller.
//  2. DISPATCH (consumer side, internal/scheduler): a wake that is ALREADY
//     PERSISTED - created before this gate existed, or by an older binary,
//     or by direct SQL - is refused at dispatch time rather than executed.
//
// The two gates are deliberately both present. The producer gate cannot
// speak for rows that already exist, and the consumer gate cannot speak for
// producers that have not been taught.
//
// A refused wake is recorded, not swallowed: the dispatch gate returns an
// error, and the scheduler marks the wake fired with the refusal reason in
// `last_error`. A blocked wake therefore does NOT retry forever, and the
// refusal is observable rather than silent.
//
// REMOVING THIS GATE
// ------------------
// It is removed when the trust boundary is actually enforced - the critic
// emits a proposal on an existing untrusted surface with typed, validated,
// resolvable citations, and no longer writes trusted lessons directly. That
// is Session B's work (docs/dev/session-a-adjudication.md §4.2, P2 and P3);
// this gate is what makes the interim state safe rather than merely
// documented.
const BlockedWakeKindCriticAudit = "critic_audit"

// WakeKindBlocked reports whether a wake kind is currently barred from being
// armed and from being dispatched.
//
// The predicate lives in the core module because BOTH sides of the gate need
// it: the producer (ScheduleWake, in this package) and the dispatcher
// (internal/scheduler, in the root module, which imports this one). Two
// copies of the constant would be two chances to drift.
func WakeKindBlocked(kind string) bool {
	return kind == BlockedWakeKindCriticAudit
}

// BlockedWakeKindError is the message a refused wake carries. It names the
// kind and says why, so the refusal is self-explanatory in a log or in a
// wake's `last_error` column - an operator reading it must not have to go
// and find this file to learn what happened.
func BlockedWakeKindError(kind string) error {
	return fmt.Errorf(
		"wake kind %q is blocked: the critic's automatic trust behaviour is not yet safe "+
			"(it writes trusted lessons with no evidence, no review, and no pending state). "+
			"This wake was NOT executed. See internal/core/wake_tools.go and "+
			"docs/dev/session-a-adjudication.md (P2b).",
		kind,
	)
}

// AgentOwnedWakeKinds are the wake kinds the AGENT's notification
// surface owns.
//
// A wake kind belongs to exactly one owner. The agent owns the kinds
// below: the opportunistic fold (CheckPendingWakes) delivers them to
// the agent and retires them (fired=1) when the agent acknowledges
// them. Every OTHER kind belongs to the scheduler, which dispatches it
// through a registered handler and retires it after the handler
// returns.
//
// The list is an ALLOWLIST, so the default is DENY: a kind this tree
// does not know is not the agent's. That direction is the one that
// matters, because the failure it prevents is silent. A fold that
// retires a scheduler-owned wake loses the work — the handler never
// runs — and nothing in the tree records the loss.
//
// The membership is the CODE's, not a document's:
//
//   - "notification" — the default kind. An untagged wake is one
//     (scheduler.Wake.Kind() defaults to "notification" for an absent
//     kind), and the scheduler's hasHandler() already refuses
//     "notification" so the two surfaces cannot both act on one wake.
//   - "cascade" — the cascade materializer's booking. The fold
//     delivers it under the per-call cap (MaxCascadeWakePerCheck).
//   - "cron" — the recurring-task bookkeeping rows. The scheduler's
//     ad-hoc claim excludes them (dispatchClaimNextAdHocWake) and the
//     fold is their delivery path: the row carries the directive_id
//     the agent has to consult.
//
// Every one of these three is delivered by the DEFAULT fold — the
// no-kinds branch that every CLI and MCP request runs. A kind that is
// agent-owned but delivered only by an explicit kinds=["..."] request
// is delivered to nobody in practice: the agent does not know to ask,
// and the retention sweep eventually retires the row. The producer
// writes the directive_id into the cron wake's metadata precisely so
// the agent can read it "at wake-time without an extra lookup"
// (internal/core/scheduled_tasks.go), and FormatWakeNotification
// renders it into the <system_wake_notification> block — an affordance
// that is dead code unless the default fold delivers the row.
//
// Everything else — snapshot, critic_audit, gc, broadcast,
// cascade_summary, drill, and any kind a future binary introduces — is
// scheduler-owned, because the scheduler is what runs it.
var AgentOwnedWakeKinds = []string{"notification", "cascade", "cron"}

// WakeKindAgentOwned reports whether a wake kind belongs to the agent's
// notification surface rather than to the scheduler's.
//
// The predicate lives in the core module because BOTH sides of the
// boundary need it: the fold (CheckPendingWakes, in this package) and
// the scheduler's handler registry (internal/scheduler, in the root
// module, which imports this one). Two copies of the list would be
// two chances to drift.
//
// An EMPTY kind is agent-owned: an absent or empty kind is an untagged
// wake, which is a notification wake (the same reading
// scheduler.Wake.Kind() takes).
//
// The comparison is exact: no prefix, no case-folding, no trimming.
func WakeKindAgentOwned(kind string) bool {
	if kind == "" {
		return true
	}
	for _, owned := range AgentOwnedWakeKinds {
		if kind == owned {
			return true
		}
	}
	return false
}

// WakeMetadataJSON is the always-valid JSON document expression for
// scheduled_wakes.metadata.
//
// It is the CANONICAL SPELLING of "read this column's JSON safely".
// Every surface that reads or writes scheduled_wakes.metadata through
// a JSON function must go through this expression (or one derived from
// it), so the surfaces that must agree cannot drift apart. See
// DirectiveExclusionClause for the same pattern applied to a
// predicate.
//
// WHY THIS FORM EXISTS. json_extract() and json_set() RAISE
// "malformed JSON" when their argument is not valid JSON. An
// unguarded call on scheduled_wakes.metadata therefore turns ONE
// corrupt row into a statement-wide failure: the statement aborts, so
// no row in the table is reachable — not even the healthy ones.
//
// The usual workaround is an OR-chain:
//
//	metadata IS NULL OR metadata = '' OR
//	json_extract(metadata, '$.kind') IS NULL OR ...
//
// That form only works if SQLite evaluates the OR terms left-to-right
// and short-circuits before it ever reaches json_extract. That is NOT
// guaranteed. The order of the operands of an OR is unspecified: the
// query planner is free to reorder the branches (it does exactly that
// when it can hoist a constant or an indexed predicate into one of
// them), and SQLite evaluates the branches in whatever order the
// generated bytecode happens to use. A guard that depends on that
// ordering is not a guard.
//
// This expression has no such dependency:
//
//	CASE WHEN json_valid(metadata) THEN metadata ELSE '{}' END
//
// json_valid() never raises — it returns 0 for malformed input, 1 for
// valid input, and NULL for NULL input — so the CASE always selects
// either the original document or the literal '{}', and BOTH are valid
// JSON. The JSON functions are then called on a document they can
// always parse. The CASE's WHEN condition is a single scalar predicate
// and both of its branches are plain values, so nothing about this
// expression depends on evaluation order either.
//
// The result is that a malformed metadata document is read and written
// as the EMPTY OBJECT — which is exactly how a NULL or empty
// metadata is already treated.
//
// BUT "PARSEABLE" IS NOT ENOUGH — THE DOCUMENT MUST BE WRITABLE.
//
// The first version of this guard was:
//
//	CASE WHEN json_valid(metadata) THEN metadata ELSE '{}' END
//
// json_valid() returns 1 for every parseable JSON document, including
// the ones that are not OBJECTS: 'null', '42', '"str"', 'true', '[]'.
// For those the THEN branch kept the original document — and
// SQLite's json_set() on a non-object document returns the document
// UNCHANGED, silently dropping the key:
//
//	json_set('null',  '$.k','v') -> null
//	json_set('42',    '$.k','v') -> 42
//	json_set('"str"', '$.k','v') -> "str"
//	json_set('true',  '$.k','v') -> true
//	json_set('[]',    '$.k','v') -> []
//
// The write "landed" (one row affected) and carried NO audit note. The
// 7-day sweep's own read-back assertion then failed, and because the
// sweep is one transaction for the whole batch, every healthy overdue
// row behind that one was left un-retired — the very "one bad row
// stops the sweep" failure this guard exists to remove.
//
// So the guard yields a WRITABLE OBJECT: the original document when it
// IS an object, and the empty object otherwise. json_type() does raise
// on malformed input, but it is only ever reached inside the THEN
// branch of json_valid(metadata) — and CASE evaluates only the branch
// it selects. That is a property of CASE itself, not of the evaluation
// order of an expression list, which is the dependence this guard
// exists to eliminate.
//
// The result: a malformed document, a JSON scalar and a JSON array are
// all read and written as the EMPTY OBJECT; a real object is preserved
// verbatim.
const WakeMetadataJSON = `CASE WHEN json_valid(metadata) THEN CASE WHEN json_type(metadata) = 'object' THEN metadata ELSE '{}' END ELSE '{}' END`

// WakeMetadataIsCorrupt is the always-valid predicate for "this row's
// metadata is CORRUPT".
//
// A row is corrupt when its metadata is PRESENT, NON-EMPTY, and NOT a
// parseable JSON document. json_valid() never raises — it returns 0
// for malformed input, 1 for valid input and NULL for NULL input — so
// this predicate is safe in a WHERE clause with no dependence on
// evaluation order.
//
// It is deliberately NOT "not a JSON object": a JSON scalar or array
// is parseable, and the guard above makes it writable. A corrupt
// document is one that no JSON function can read at all.
//
// WHY CORRUPT ROWS ARE A DISTINCT CLASS.
//
// A corrupt document is the one shape whose ORIGINAL BYTES may name an
// owner that cannot be read back: the truncated prefix
// '{"kind": "cron"' is not parseable, so the kind is unreadable — but
// the bytes are exactly the evidence that the row was a CRON wake.
// Reading it as untagged would silently reclassify scheduler-owned
// work as an ordinary notification, and writing to it would destroy
// the only copy of that evidence. See WakeMetadataCorruption.
//
// A VALID JSON SCALAR OR ARRAY IS NOT CORRUPT. It parses, so no JSON
// function can raise on it, and the guard above makes it writable. Its
// original bytes are replaced by the audit-note object when a writer
// touches it — that is normalisation, not corruption. The distinction
// matters because only a MALFORMED document can hide an owner: a
// well-formed document either names its kind in full or names none at
// all.
//
// THE ADOPTED POLICY (issue #178, defect W4)
// ------------------------------------------
// A corrupt row is owned by NO partition. It is never delivered, never
// claimed, never retired by a retention sweep, never resolved by an
// agent and never executed by the scheduler. Its original bytes are
// preserved verbatim, because they are the only evidence of what it
// was. Every surface that meets one rejects it INDIVIDUALLY, with a
// diagnostic that carries those bytes, and carries on with the healthy
// rows. One corrupt row never blocks, delays or rolls back work on any
// other row.
const WakeMetadataIsCorrupt = `(metadata IS NOT NULL AND metadata <> '' AND json_valid(metadata) = 0)`

// The surface names a writer that refused to touch a corrupt row.
//
// The vocabulary is shared so that a rejection record says WHICH
// writer rejected the row, and so that no surface invents its own
// spelling.
const (
	// WakeSurfaceNotificationExpiration is the 7-day notification retention sweep.
	WakeSurfaceNotificationExpiration = "notification_expiration"
	// WakeSurfaceCronRetention is the 1-hour cron retention sweep.
	WakeSurfaceCronRetention = "cron_retention"
	// WakeSurfaceResolve is the agent-facing wake resolver.
	WakeSurfaceResolve = "resolve_wake"
	// WakeSurfaceClaim is the scheduler's notification claim.
	WakeSurfaceClaim = "dispatch_claim"
	// WakeSurfaceSettlement is the scheduler's wake settlement.
	WakeSurfaceSettlement = "wake_settlement"
)

// WakeMetadataCorruption is the diagnostic record for a row that a
// writer refused to touch because its metadata is corrupt.
//
// It carries the ORIGINAL METADATA BYTES on purpose: the rejection
// PRESERVES them (the row is never written), so the record is the only
// place the operator can read what the row's bytes actually were —
// including, in the truncated case, the name of the owner the row was
// meant to be.
type WakeMetadataCorruption struct {
	// WakeID is the row's id.
	WakeID string
	// Metadata is the row's original metadata, verbatim and never
	// overwritten.
	Metadata string
	// Surface names the writer that rejected the row.
	Surface string
}

// String renders the corruption for a log line.
func (c WakeMetadataCorruption) String() string {
	return c.WakeID + " (" + c.Surface + "): metadata is not a parseable JSON document: " + c.Metadata
}

// WakeMetadataIsCorruptValue is the GO twin of the
// WakeMetadataIsCorrupt SQL predicate.
//
// The writers that already hold a candidate row's metadata classify it
// in Go rather than issuing another query, so the Go rule and the SQL
// rule must agree exactly. A test pins them to each other by
// evaluating BOTH against the same fixture table — behaviour, not
// string equality.
//
// The rules are the same: present, non-empty, and not a parseable JSON
// document. encoding/json's Valid and SQLite's json_valid agree on
// every shape the column can hold; the agreement is what the pinning
// test asserts.
func WakeMetadataIsCorruptValue(metadata sql.NullString) bool {
	if !metadata.Valid || metadata.String == "" {
		return false
	}
	return !json.Valid([]byte(metadata.String))
}

// WakeKindExpr is the always-valid expression for a wake's `kind`.
//
// It evaluates to the kind string, or ” when the document carries no
// usable kind. A malformed document reads as UNTAGGED (”), exactly
// like a NULL or empty one — which is the same reading Wake.Kind()
// takes, and the same reading the fold's untagged branch already used.
//
// It reads the kind through WakeMetadataJSON, so it never raises on
// malformed metadata and never depends on SQL evaluation order. See
// WakeMetadataJSON for the full argument.
//
// THE EXPRESSION IS NOT THE POLICY. It reads a corrupt document as
// untagged — safely, with no raise. Whether an untagged row
// is then DELIVERED, CLAIMED or RETIRED is a separate decision, and
// the W4 policy is that a corrupt row is delivered, claimed and
// retired by NOBODY. Every surface that acts on a row therefore
// excludes the corrupt ones with WakeMetadataIsCorrupt BEFORE it
// consults the kind. The expression above is only what a surface sees
// once that exclusion has been applied.
const WakeKindExpr = `COALESCE(json_extract(` + WakeMetadataJSON + `, '$.kind'), '')`

// The canonical kind filters. Each is TRUE for the rows whose kind is
// in the set — and, where the name says so, for the UNTAGGED rows
// (a NULL, empty or malformed metadata document).
//
// "Untagged" is the notification reading: an untagged wake IS a
// notification wake. A filter that says "untagged counts" therefore
// covers the notification partition; a filter that does not say so
// covers only the rows that name their kind exactly — which is how
// the scheduler's cron sweep keeps its kind boundary from widening.
//
// The kinds are inlined rather than bound, because every value here
// comes from a fixed list in this package — never from a caller.
const (
	// WakeNotificationKindFilter matches notification-kind wakes AND
	// untagged wakes. It is the scheduler's ad-hoc claim predicate
	// and the 7-day retention sweep's candidate predicate.
	WakeNotificationKindFilter = `(` + WakeKindExpr + ` IN ('', 'notification'))`

	// WakeCronKindFilter matches cron-kind wakes ONLY. An untagged
	// wake is NOT a cron wake: it belongs to the notification
	// partition, and widening this filter would let the cron sweep
	// retire rows the notification sweep owns.
	WakeCronKindFilter = `(` + WakeKindExpr + ` IN ('cron'))`

	// WakeCascadeSummaryKindFilter matches cascade-summary wakes
	// ONLY.
	WakeCascadeSummaryKindFilter = `(` + WakeKindExpr + ` IN ('cascade_summary'))`

	// WakeNotificationAndCascadeKindFilter matches notification-kind
	// wakes, cascade-kind wakes AND untagged wakes. It is the
	// `wakes_overdue` health counter's predicate.
	WakeNotificationAndCascadeKindFilter = `(` + WakeKindExpr + ` IN ('', 'notification', 'cascade'))`
)

// wakeKindSQL builds the kind filter for a fold query.
//
// kinds is the set of kinds the branch matches. untaggedMatches says
// whether a row with no kind — absent, empty, or malformed metadata —
// counts as a match.
//
// It composes the same WakeKindExpr the exported filters above are
// built from, so the fold and the scheduler share ONE idiom for
// reading a wake's kind out of possibly-malformed metadata. That idiom
// is deliberately free of any dependence on SQL evaluation order: see
// WakeMetadataJSON for why the OR-chain form is not acceptable here.
//
// The kinds are inlined rather than bound, because every value that
// reaches here comes from AgentOwnedWakeKinds — a fixed list in this
// package, never from a caller.
func wakeKindSQL(kinds []string, untaggedMatches bool) string {
	quoted := make([]string, 0, len(kinds)+1)
	if untaggedMatches {
		quoted = append(quoted, "''")
	}
	for _, k := range kinds {
		quoted = append(quoted, "'"+k+"'")
	}
	if len(quoted) == 0 {
		// No kind and no untagged tolerance: nothing matches.
		return "0"
	}
	return "(" + WakeKindExpr + " IN (" + strings.Join(quoted, ", ") + "))"
}

// agentOwnedSubset returns the requested kinds that are the agent's.
//
// A request that names only scheduler-owned kinds matches nothing: the
// agent's surface cannot see the scheduler's work.
func agentOwnedSubset(requested []string) []string {
	out := make([]string, 0, len(requested))
	for _, k := range requested {
		if WakeKindAgentOwned(k) {
			out = append(out, k)
		}
	}
	return out
}

func (dm *DatabaseManager) ScheduleWake(reason, targetTime, theoryID, createdBy string, metadata map[string]interface{}) (map[string]interface{}, error) {
	if reason == "" {
		return nil, fmt.Errorf("reason is required")
	}
	if createdBy == "" {
		createdBy = "mpm_call"
	}
	absolute, err := resolveTargetTime(targetTime, time.Now())
	if err != nil {
		return nil, err
	}
	id, err := newWakeID()
	if err != nil {
		return nil, err
	}
	// 2026-09-22 release-blocker D-1: canonical wake default. When
	// the caller does not supply a `kind` discriminator in the
	// metadata map, author `kind=notification` here so the persisted
	// row is consumable by both `CheckPendingWakes` (whose backward-
	// compat default treats `kind` absent or `notification` as the
	// notification surface) and `ResolveWake` (whose discriminator
	// requires `kind` to be present). This is the producer-side
	// half of the D-1 fix; the consumer-side half lives in
	// `ResolveWake` (json_extract-based matcher that also accepts
	// legacy NULL/empty metadata rows). Fix lives here, not in
	// handleScheduleWake, so every caller of ScheduleWake — the
	// public handler, the cascade materializer, the wake reconcile
	// loop, and any future internal producer — gets the same default.
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	if _, hasKind := metadata["kind"]; !hasKind {
		metadata["kind"] = "notification"
	}
	// ARMING GATE. Refuse a blocked kind here, at the DB-layer producer,
	// so that EVERY producer is covered by one check rather than by each
	// caller's own diligence. The kind is read defensively: metadata is
	// caller-supplied and its "kind" value need not be a string.
	if kind, _ := metadata["kind"].(string); WakeKindBlocked(kind) {
		return nil, BlockedWakeKindError(kind)
	}
	var metaJSON string
	b, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	metaJSON = string(b)
	var theoryPtr interface{}
	if theoryID != "" {
		theoryPtr = theoryID
	}
	_, err = dm.ExecTracked(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, fired, created_by, metadata)
		 VALUES (?, ?, ?, ?, 0, ?, ?)`,
		5,
		id, absolute, reason, theoryPtr, createdBy, metaJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("insert scheduled_wakes: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"id":          id,
		"target_time": absolute,
		"target_iso":  time.Unix(absolute, 0).UTC().Format(time.RFC3339),
		"reason":      reason,
		"theory_id":   theoryID,
	}, nil
}

// ResolveWake marks a wake fired=1 with fired_at=now and returns the
// resolution outcome. The lifecycle contract:
//   - This is the explicit, audited mechanism for an agent (or operator
//     tool) to mark a wake resolved without having to consume it through
//     the CheckPendingWakes delivery path.
//   - The action is IDEMPOTENT: re-running on an already-fired wake
//     returns fired=true with no audit row written (avoids audit noise
//     from retries / double-clicks). It is NOT destructive: it does
//     not delete the wake row, only flips fired=1. The wake remains
//     visible via `mpm_wakes list --include_fired` for the audit trail.
//
// 2026-09-19 release-pass: surface for lesson 9b9f286c (mpm_wakes
// lacks explicit resolution). Pre-fix the only way to retire a
// non-scheduled-task wake was to delete the row directly via SQL
// (which an operator correctly refused to do) or to wait for the
// cascade materializer's downstream liveness guard (added in the
// same pass) to skip materialization. ResolveWake is the
// operator-friendly path for retiring historical ghost wakes that
// the new guard cannot retroactively erase.
//
// Refusal semantics:
//   - wake_id not found → (false, "wake_not_found")
//   - the row's metadata.kind is a SCHEDULER-owned kind →
//     (false, "not_a_wake"). Those rows belong to the scheduler: it
//     runs them through a registered handler, and the agent may not
//     retire one, because retiring it would suppress that handler's
//     run. A row whose kind is absent is an untagged notification
//     wake and IS resolvable.
//   - already fired → (true, "already_resolved") — no-op success.
//   - agent-owned wake resolved with the supplied reason →
//     (true, "resolved") and an audit row is emitted.
//
// The kind gate is the SHARED wake-ownership policy
// (WakeKindAgentOwned), the same one the opportunistic fold and the
// agent-facing arming gate use. One policy, one list, no drift: a
// kind the agent may not SEE (because the fold would retire it) is a
// kind it may not ARM, and a kind the agent owns is a kind it may
// retire.
//
// Safe-credential contract: reason is bounded to the enum below; no
// arbitrary string is accepted. result_reference, if supplied, is
// recorded verbatim in the audit row's context (not on the wake
// row) — secrets should never be passed as the reference; if the
// caller needs to record sensitive evidence, the canonical evidence
// tool is the right surface.
//
// TRANSACTION-LEVEL CONTENTION (2026-11-04, issue #178)
// -------------------------------------------------------
// The resolver READS the wake row and then WRITES it. That read-then-
// write pair is what makes a DEFERRED transaction vulnerable: the read
// takes the transaction's read snapshot, and the write has to upgrade it
// to the write lock. In WAL mode ANY commit by another connection between
// those two points invalidates the snapshot, and the upgrade fails with
// SQLITE_BUSY_SNAPSHOT.
//
// SQLITE_BUSY_SNAPSHOT is NOT ordinary contention. It shares its primary
// code (SQLITE_BUSY) and its message ("database is locked") with it, so
// the two are indistinguishable by text — only the EXTENDED code tells
// them apart. It is also never serviced by the busy handler, so
// busy_timeout is never consulted (the error returns in microseconds),
// and a statement-level retry can NEVER clear it: the snapshot belongs to
// the transaction, and the transaction's snapshot does not change between
// attempts.
//
// The remedy is therefore a TRANSACTION RESTART against a fresh snapshot,
// which is what dm.WithTx does (bounded by TxSnapshotMaxAttempts). The
// pre-fix code bypassed BOTH retry layers — it opened its own deferred
// transaction with dm.db.Begin() and wrote with a raw tx.Exec — so two
// concurrent resolutions of DIFFERENT agent-owned wakes could lose one
// resolution with "resolve wake: update: database is locked".
//
// The transactional body below is replay-safe: every side effect it
// produces lives inside the transaction (so an abandoned attempt's write
// rolls back with it), the UPDATE is guarded by `fired = 0` (so a re-run
// cannot double-resolve), and the audit row is written OUTSIDE the
// transaction (so a re-run cannot duplicate it).
func (dm *DatabaseManager) ResolveWake(wakeID, reason string, resultReference string) (resolved bool, status string, err error) {
	if wakeID == "" {
		return false, "missing_wake_id", fmt.Errorf("resolve wake: wake_id is required")
	}
	if !isValidWakeResolveReason(reason) {
		return false, "invalid_reason", fmt.Errorf("resolve wake: reason %q is invalid — must be one of reconciled|obsolete|superseded|already_satisfied", reason)
	}

	// Single tx: look up + atomic claim. The UPDATE with WHERE
	// fired=0 prevents two concurrent resolve calls from racing
	// the same wake to a "double-resolved" audit row.
	//
	// The transaction is opened by dm.WithTx, not by a bare
	// dm.db.Begin(), because the resolver's read-then-write pair needs
	// the TRANSACTION-level restart on a stale snapshot (see the method
	// doc). WithTx owns that budget; the body below must not open its
	// own transaction, and must not retry its own statements around it.
	var (
		outcome         string
		auditCreatedBy  string
		auditResolvedAt int64
	)
	txErr := dm.WithTx(func(node DBNode) error {
		// Per-attempt state. WithTx re-runs this callback against a
		// FRESH transaction when the previous attempt's snapshot went
		// stale, so the captured state must be RESET, not accumulated.
		outcome = ""
		auditCreatedBy = ""
		auditResolvedAt = 0

		var (
			fired     int
			createdBy string
			metadata  sql.NullString
			corrupt   int
			rowKind   sql.NullString
		)
		row := node.QueryRowTracked(`
		SELECT fired, created_by, metadata,
		       CASE WHEN `+WakeMetadataIsCorrupt+` THEN 1 ELSE 0 END,
		       json_extract(`+WakeMetadataJSON+`, '$.kind')
		FROM scheduled_wakes WHERE id = ?
	`, wakeID)
		if scanErr := row.Scan(&fired, &createdBy, &metadata, &corrupt, &rowKind); scanErr != nil {
			if scanErr == sql.ErrNoRows {
				return &wakeResolveStatusError{status: "wake_not_found", err: fmt.Errorf("resolve wake: wake %q not found", wakeID)}
			}
			return &wakeResolveStatusError{status: "lookup_failed", err: fmt.Errorf("resolve wake: lookup: %w", scanErr)}
		}

		// CORRUPT METADATA — THE OWNERSHIP QUESTION, ANSWERED ONCE.
		//
		// A row whose metadata is present, non-empty and NOT a
		// parseable JSON document has an owner that cannot be
		// determined. The truncated prefix '{"kind": "cron"' is the
		// evidence that the row was a CRON wake, and it cannot be read
		// back as one.
		//
		// Reading it as UNTAGGED would make it an agent-owned
		// notification wake — and the agent would then be able to
		// retire scheduler-owned work whose handler never ran. That is
		// the same decision the retention sweeps and the claim make, so
		// it is made HERE, ONCE, for the whole substrate: a corrupt row
		// is owned by NO partition.
		//
		// The refusal is a refusal, not a repair: the row is NOT
		// written, so the original bytes survive for the operator.
		if corrupt != 0 {
			return &wakeResolveStatusError{status: "corrupt_metadata", err: fmt.Errorf(
				"resolve wake: row %q has a corrupt metadata document (%q): its owner cannot be determined, "+
					"so the agent may not retire it — an untagged reading would let the agent retire "+
					"scheduler-owned work whose handler never ran", wakeID, metadata.String)}
		}

		// The KIND PROBE. The row's metadata.kind selects the wake's
		// OWNER, and the owner decides whether the agent may retire it.
		//
		// 2026-09-22 release-blocker D-1: the prior substring check
		// (`strings.Contains(metadata, `"kind":`)`) was brittle — it
		// missed rows whose `metadata` JSON was NULL or empty (e.g.
		// rows authored before ScheduleWake authored `kind=notification`
		// by default), and it conflated "kind absent" with "scheduled
		// task". The canonical matcher now uses json_extract (the same
		// surface CheckPendingWakes uses).
		//
		// 2026-09-22 verification pass: hardened the probe against
		// pre-fix data with arbitrarily-corrupted metadata. SQLite's
		// json_extract raises an error on malformed JSON rather than
		// returning NULL — which would surface as `lookup_failed` and
		// fail every write through the wake lifecycle for legacy rows.
		//
		// 2026-11-05 (issue #178, W4): the probe no longer carries its
		// OWN spelling of the rule. It reads the kind through the
		// canonical WakeMetadataJSON expression, exactly like every
		// other kind-reading surface, and the corruption question is
		// answered by the canonical WakeMetadataIsCorrupt predicate
		// above. There is no second dialect of the same rule left in
		// this package to drift.
		//
		// The inputs the legacy compat branch covered are all still
		// covered, by the canonical expression:
		//   1. metadata IS NULL            — the guard yields '{}',
		//                                    json_extract finds no
		//                                    kind -> untagged.
		//   2. metadata == ''              — same.
		//   3. json_valid(metadata) == 0   — the guard yields '{}'
		//                                    -> untagged; and a
		//                                    MALFORMED document is
		//                                    refused above rather
		//                                    than read as untagged.
		//   4. otherwise                  — the document's kind.
		kind := ""
		if rowKind.Valid {
			kind = rowKind.String
		}

		// OWNERSHIP GATE. The agent may retire only the kinds it OWNS:
		// notification (explicitly or untagged), cascade and cron. Every
		// other kind — snapshot, critic_audit, gc, broadcast,
		// cascade_summary, drill, and any kind a future binary
		// introduces — belongs to the scheduler, which runs it through a
		// registered handler. The agent may not retire one: the resolver
		// sets fired=1, the scheduler's QueryDueWakes selects only
		// fired=0 rows, and the handler would never run.
		//
		// This is the same predicate the arming gate uses
		// (internal/core/tools/handlers.go) and the same one the
		// opportunistic fold uses, so the three surfaces cannot disagree
		// about which kinds are the agent's.
		if !WakeKindAgentOwned(kind) {
			return &wakeResolveStatusError{status: "not_a_wake",
				err: fmt.Errorf("resolve wake: row %q has metadata.kind=%q which is a scheduler-owned kind; "+
					"the scheduler runs it through a registered handler, so the agent cannot retire it", wakeID, kind)}
		}

		if fired != 0 {
			// Already resolved — idempotent no-op. No audit row.
			outcome = "already_resolved"
			return nil
		}

		now := time.Now().Unix()
		res, execErr := node.ExecTracked(`
		UPDATE scheduled_wakes
		SET fired = 1, fired_at = ?, fired_by = ?
		WHERE id = ? AND fired = 0
	`, wakeResolveWriteRetries, now, "wake-resolver", wakeID)
		if execErr != nil {
			return &wakeResolveStatusError{status: "update_failed", err: fmt.Errorf("resolve wake: update: %w", execErr)}
		}
		affected, raErr := res.RowsAffected()
		if raErr != nil {
			return &wakeResolveStatusError{status: "rows_affected_failed", err: fmt.Errorf("resolve wake: rows-affected: %w", raErr)}
		}
		if affected == 0 {
			// Lost the race (another resolver fired this wake first).
			// Treat as already_resolved — the audit trail will reflect
			// whichever resolver won.
			outcome = "already_resolved"
			return nil
		}

		outcome = "resolved"
		auditCreatedBy = createdBy
		auditResolvedAt = now
		return nil
	})
	if txErr != nil {
		// Map the failure onto the resolver's status taxonomy. WithTx
		// produces exactly three kinds of failure: a CALLBACK error
		// (which carries its own status), a failed COMMIT (ambiguous —
		// the write may or may not be durable), and a failed BEGIN.
		var statusErr *wakeResolveStatusError
		switch {
		case errors.As(txErr, &statusErr):
			return false, statusErr.status, statusErr.err
		case errors.Is(txErr, ErrTxCommitAmbiguous):
			return false, "tx_commit_failed", fmt.Errorf("resolve wake: commit: %w", txErr)
		default:
			return false, "tx_begin_failed", fmt.Errorf("resolve wake: begin: %w", txErr)
		}
	}

	if outcome == "already_resolved" {
		return true, "already_resolved", nil
	}

	// Forensic log — every wake resolution is a state transition.
	// The audit row carries the wake id, the canonical reason, the
	// actor ("wake-resolver" sentinel — distinguishes operator-
	// initiated resolutions from automatic materializer/reconciler
	// firings), the wake's original creator (so an investigator can
	// tell which subsystem enqueued the wake), and the optional
	// result reference (a pointer, NOT the raw content of the
	// resolution — e.g. the theory id that was disproven, the new
	// artifact id that superseded the downstream).
	//
	// The audit row is written OUTSIDE the transaction, deliberately.
	// Inside it, a transaction restart would replay the write and emit
	// a SECOND audit row for one resolution; outside it, exactly one
	// row is emitted per successful resolution.
	dm.LogAudit(AuditInfo, "wake-resolver",
		fmt.Sprintf("resolve_wake %s reason=%s", wakeID, reason),
		"",
		AuditContext{
			"wake_id":          wakeID,
			"reason":           reason,
			"created_by":       auditCreatedBy,
			"resolved_at":      auditResolvedAt,
			"result_reference": resultReference,
		})
	return true, "resolved", nil
}

// wakeResolveWriteRetries is the statement-level retry budget for the
// resolver's UPDATE.
//
// It covers ORDINARY contention: another connection may hold the write
// lock, and SQLite's busy handler already waits out busy_timeout before
// the driver ever sees an error, so the budget only has to cover what is
// left over. Five is the substrate's documented write budget (issue #164).
//
// It does NOT cover SQLITE_BUSY_SNAPSHOT, and it is not meant to.
// txNode.ExecTracked returns a snapshot conflict to the caller
// IMMEDIATELY (without burning the backoff), because a statement-level
// retry can never clear it — the snapshot belongs to the transaction.
// The ENCLOSING transaction (dm.WithTx) is what restarts it.
const wakeResolveWriteRetries = 5

// wakeResolveStatusError carries the resolver's public status alongside the
// underlying error.
//
// The resolver's transactional body runs inside dm.WithTx, whose callback
// can only return an error. The status taxonomy the public envelope
// exposes (wake_not_found, lookup_failed, not_a_wake, update_failed, …)
// therefore rides along with the error and is unpacked by the caller.
//
// The wrapper UNWRAPS to the underlying error, which matters: WithTx
// classifies the callback's error with errors.As, so a wrapped
// SQLITE_BUSY_SNAPSHOT is still recognised as a snapshot conflict and
// still triggers the transaction restart — and a wrapped context
// cancellation is still recognised as a cancellation, so an abandoned
// attempt is not replayed.
type wakeResolveStatusError struct {
	status string
	err    error
}

func (e *wakeResolveStatusError) Error() string { return e.err.Error() }

func (e *wakeResolveStatusError) Unwrap() error { return e.err }

// isValidWakeResolveReason reports whether reason is one of the canonical
// resolution reasons. The set is intentionally bounded so the audit
// taxonomy is stable across operators and agent scripts.
func isValidWakeResolveReason(reason string) bool {
	switch reason {
	case "reconciled", "obsolete", "superseded", "already_satisfied":
		return true
	}
	return false
}

// CheckPendingWakes returns the DUE, AGENT-OWNED wakes that fit this
// call's DELIVERY BUDGET, marks them fired=1 with fired_at=now, and
// returns them in chronological order. Returns an empty slice when
// nothing is due (NOT an error).
//
// The caller (the handler) is expected to fold the result into a
// "WakesPending" block on the outgoing response so the agent sees the
// wake the next time it issues any MPM call.
//
// Idempotency: marking fired=1 happens in the same transaction as the
// SELECT, so concurrent CheckPendingWakes calls will not return the
// same wake twice. Two callers racing for the same wake will see one
// winner and the other an empty list.
//
// Kinds filter:
//
//   - kinds nil/empty: the DEFAULT delivery path — every AGENT-OWNED
//     pending wake, whatever its kind. This is the same predicate as
//     the "*" branch below, and it must stay the same predicate: the
//     fold is the delivery path for every kind the agent owns, so a
//     default that is narrower than the allowlist silently starves the
//     kinds it omits. See AgentOwnedWakeKinds for the membership and
//     the per-kind reason.
//   - kinds contains "*": surface every AGENT-OWNED pending wake
//     regardless of kind. The scheduler's kinds are NOT crossed into:
//     a wake the scheduler owns must reach its handler, and a fold
//     that retired it would lose the work silently.
//   - kinds is a specific list: surface only wakes whose metadata.kind
//     matches one of the entries (json_extract IN (...)). Wakes with
//     no kind set are excluded by this branch. A requested kind the
//     scheduler owns matches nothing.
//
// The per-call delivery budget:
//
//	Every agent-owned kind has its own row cap and its own byte cap
//	on the rendered <system_wake_notification> block. Rows that do
//	not fit stay PENDING (fired = 0) and are delivered by a later
//	call, exactly as a cascade wake above MaxCascadeWakePerCheck
//	already behaves. The budget throttles the batch; it never drops
//	a wake and it never settles one it does not deliver.
//
//	The budget is PER KIND so that a backlog of one kind cannot
//	starve another. See the constants above for the numbers and the
//	reasoning.
func (dm *DatabaseManager) CheckPendingWakes(now time.Time, kinds []string) ([]map[string]interface{}, error) {
	nowUnix := now.Unix()
	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Resolve the delivery buckets — one per agent-owned kind this
	// call may deliver.
	//
	// Three branches:
	//   "*" anywhere  -> every AGENT-OWNED wake, whatever its kind
	//   specific list -> only the requested agent-owned kinds
	//   nil / empty   -> every AGENT-OWNED wake (the default path)
	//
	// The default and the wildcard are the SAME predicate. They were
	// not, and the difference was a defect: the default branch
	// matched only notification wakes, so the two kinds the fold owns
	// but the default omitted — cron and cascade — were never
	// delivered. A cron wake was injected, became due, was invisible
	// to every agent, and was retired by the cron retention sweep
	// with its directive_id undelivered. A cascade wake was never
	// delivered either, although its producer documents "immediate
	// delivery on next check_wakes" and check_wakes without a kinds
	// argument uses this branch.
	//
	// One predicate for "what the fold delivers" is the whole point
	// of AgentOwnedWakeKinds: a kind the agent owns is delivered by
	// the fold, and a kind the scheduler owns never is.
	includeAll := false
	for _, k := range kinds {
		if k == "*" {
			includeAll = true
			break
		}
	}

	var buckets []wakeDeliveryBucket
	switch {
	case includeAll, len(kinds) == 0:
		// The default and the wildcard treat an UNTAGGED wake as a
		// notification wake (WakeNotificationKindFilter).
		buckets = agentOwnedWakeBuckets(true)
	case len(kinds) > 0:
		// Only the requested kinds, and only the ones that are the
		// agent's. A request that names only scheduler-owned kinds
		// matches nothing at all.
		allowed := agentOwnedSubset(kinds)
		if len(allowed) == 0 {
			return []map[string]interface{}{}, nil
		}
		// An EXPLICIT request matches the named kinds exactly: an
		// untagged wake is not a "notification" wake to
		// json_extract(...)= 'notification'. This has always been the
		// explicit branch's semantics and it is unchanged.
		buckets = agentOwnedWakeBuckets(false)
		buckets = filterWakeBuckets(buckets, allowed)
	}

	// Select and budget each bucket.
	//
	// A CORRUPT ROW IS NEVER FOLDED. The fold RETIRES what it
	// delivers, so folding a row whose owner cannot be determined
	// would deliver scheduler-owned work to the agent as an ordinary
	// notification AND retire it — the partition shift the W4 policy
	// forbids. The exclusion is the canonical predicate, so it cannot
	// drift from the one the claim and the sweeps use.
	//
	// The SELECT carries the bucket's ROW CAP as its LIMIT, so the
	// scan is bounded too: a backlog of 10,080 cron wakes costs the
	// same read as a backlog of 16.
	selected := make(map[string][]selectedWake, len(buckets))
	for _, b := range buckets {
		rows, err := tx.Query(
			`SELECT id, target_time, reason, theory_id, created_by, metadata, created_at
			 FROM scheduled_wakes
			 WHERE fired = 0 AND target_time <= ? AND (`+b.predicate+`)
			   AND NOT `+WakeMetadataIsCorrupt+`
			 ORDER BY target_time ASC, id ASC
			 LIMIT ?`,
			nowUnix, b.rowCap,
		)
		if err != nil {
			return nil, fmt.Errorf("select pending %s wakes: %w", b.label, err)
		}
		candidates, scanErr := scanPendingWakes(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan pending %s wakes: %w", b.label, scanErr)
		}
		selected[b.label] = budgetWakeBucket(dm, b, candidates, nowUnix)
	}

	// Order the delivered set.
	//
	// The historical order is preserved: the non-cascade kinds merged
	// chronologically, then the capped cascade batch. The cascade
	// kind is appended rather than merged because it has always been
	// delivered after the non-cascade batch, and the cap truncates
	// the OLDEST end of its own bucket (the SELECT's ORDER BY).
	nonCascade := mergeSelectedWakes(selected["notification"], selected["cron"])
	delivered := make([]selectedWake, 0, len(nonCascade)+len(selected["cascade"]))
	delivered = append(delivered, nonCascade...)
	delivered = append(delivered, selected["cascade"]...)

	// Mark EXACTLY the delivered rows fired, in one batch.
	//
	// A row that was read but excluded by the budget is NOT marked:
	// it stays pending and a later call delivers it. The fold never
	// retires a wake it did not deliver.
	for _, w := range delivered {
		if _, err := tx.Exec(
			`UPDATE scheduled_wakes SET fired = 1, fired_at = ? WHERE id = ? AND fired = 0`,
			nowUnix, w.id,
		); err != nil {
			return nil, fmt.Errorf("mark wake fired: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit wake tx: %w", err)
	}

	out := make([]map[string]interface{}, 0, len(delivered))
	for _, w := range delivered {
		out = append(out, w.out)
	}
	return out, nil
}

// wakeDeliveryBucket is one agent-owned kind's share of a call's
// delivery budget.
//
// The kind's rows are selected with the canonical kind predicate —
// the same JSON-safe expression the claim and the sweeps use — so the
// classification cannot drift from the substrate's, and a row whose
// metadata is written with different whitespace is classified the same
// way as one written compactly.
type wakeDeliveryBucket struct {
	// label identifies the bucket in error messages and in the
	// selection map.
	label string
	// predicate is the canonical SQL predicate for this bucket's rows.
	predicate string
	// rowCap is the maximum number of rows of this kind delivered in
	// one call. Rows above it stay pending.
	rowCap int
	// byteCap is the maximum number of bytes this kind may contribute
	// to the rendered <system_wake_notification> block in one call.
	byteCap int
}

// agentOwnedWakeBuckets returns the delivery buckets for the
// agent-owned kinds.
//
// untaggedMatches selects the semantics of the notification bucket:
// the default and wildcard folds treat an untagged wake as a
// notification wake, while an explicit kinds=["notification"] request
// matches the named kinds exactly.
func agentOwnedWakeBuckets(untaggedMatches bool) []wakeDeliveryBucket {
	return []wakeDeliveryBucket{
		{
			label:     "notification",
			predicate: wakeKindSQL([]string{"notification"}, untaggedMatches),
			rowCap:    MaxNotificationWakePerCheck,
			byteCap:   MaxWakeNotificationBytesPerKind,
		},
		{
			label:     "cron",
			predicate: wakeKindSQL([]string{"cron"}, false),
			rowCap:    MaxCronWakePerCheck,
			byteCap:   MaxWakeNotificationBytesPerKind,
		},
		{
			label:     "cascade",
			predicate: wakeKindSQL([]string{"cascade"}, false),
			rowCap:    MaxCascadeWakePerCheck,
			byteCap:   MaxWakeNotificationBytesPerKind,
		},
	}
}

// filterWakeBuckets keeps only the buckets whose label is in allowed.
// A requested kind the agent does not own has no bucket, and the
// scheduler's kinds are never in the bucket list at all.
func filterWakeBuckets(buckets []wakeDeliveryBucket, allowed []string) []wakeDeliveryBucket {
	permitted := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		permitted[k] = true
	}
	out := make([]wakeDeliveryBucket, 0, len(buckets))
	for _, b := range buckets {
		if permitted[b.label] {
			out = append(out, b)
		}
	}
	return out
}

// pendingWake is a candidate row read from the wake table by the fold.
type pendingWake struct {
	id         string
	targetTime int64
	reason     string
	theoryID   *string
	createdBy  string
	metadata   *string
	createdAt  string
}

// selectedWake is a candidate the fold selected for delivery: the
// rendered row the agent will see, plus the identity the settlement
// needs.
type selectedWake struct {
	id         string
	targetTime int64
	out        map[string]interface{}
}

// scanPendingWakes reads the candidate rows out of a bucket's result
// set. The rows are closed before it returns.
func scanPendingWakes(rows *sql.Rows) ([]pendingWake, error) {
	defer rows.Close()
	var out []pendingWake
	for rows.Next() {
		var p pendingWake
		if err := rows.Scan(&p.id, &p.targetTime, &p.reason, &p.theoryID, &p.createdBy, &p.metadata, &p.createdAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// budgetWakeBucket applies the bucket's BYTE cap to its candidates and
// returns the ones this call delivers.
//
// The byte cap is measured on the bytes the agent actually receives:
// the rendered <system_wake_notification> line for the row, produced
// by the same function the renderer uses. A row cap alone cannot bound
// the block, because the directive_id is caller-supplied and can be
// arbitrarily long — which is why every rendered field is bounded
// before it is measured.
//
// A candidate that does not fit is SKIPPED, not truncated and not
// dropped: it stays pending, and the candidates behind it are still
// considered.
//
// THERE IS NO "FIRST ROW IS ALWAYS DELIVERED" EXEMPTION.
//
// The exemption existed because a row's rendered length was not
// bounded: an oversized first row consumed the whole kind budget, and
// the healthy wakes behind it were not delivered in that call. Now
// that every rendered field is bounded, a single rendered row is at
// most ~450 bytes — well inside the 4,096-byte cap — so the cap can
// never be exceeded and the exemption could only ever have been
// exercised by the unbounded rows the field bounds removed.
//
// The consequence is the property that matters: an oversized wake
// costs at most its BOUNDED size against the budget, so the healthy
// wakes behind it are delivered in the SAME call. An oversized row
// cannot starve the rows behind it.
func budgetWakeBucket(dm *DatabaseManager, b wakeDeliveryBucket, candidates []pendingWake, nowUnix int64) []selectedWake {
	out := make([]selectedWake, 0, len(candidates))
	used := 0
	for _, p := range candidates {
		row := dm.wakeRowToMap(p, nowUnix)
		size := len(formatWakeNotificationRow(row))
		if used+size > b.byteCap {
			// Over budget: this row stays pending for a later call.
			continue
		}
		used += size
		out = append(out, selectedWake{id: p.id, targetTime: p.targetTime, out: row})
	}
	return out
}

// mergeSelectedWaves merges two chronologically ordered selections into
// one chronological selection. The tie-break is the wake id, so the
// order is deterministic.
func mergeSelectedWakes(a, b []selectedWake) []selectedWake {
	out := make([]selectedWake, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].targetTime < b[j].targetTime ||
			(a[i].targetTime == b[j].targetTime && a[i].id <= b[j].id) {
			out = append(out, a[i])
			i++
			continue
		}
		out = append(out, b[j])
		j++
	}
	out = append(out, a[i:]...)
	out = append(out, b[j:]...)
	return out
}

// wakeRowToMap converts a pending wake row to the map format returned by
// CheckPendingWakes, without any database side effects.
//
// THE ROW IS A BOUNDED PROJECTION, not the row as stored.
//
// This map is the STRUCTURED payload the agent receives: the core tool
// handlers put it in the response as `WakesPending`, and `mpm call`
// puts it in the tool's own result. The stored row's caller-supplied
// fields — the reason, the theory_id, the created_by, and every string
// in the metadata — are unbounded in the database, and an unbounded
// string in this map makes the agent-visible response unbounded: one
// wake with a 204,800-byte directive_id produced a 205,020-byte
// structured row at the head that carried the delivery budget.
//
// So every caller-supplied string in the row is bounded here, and the
// metadata is bounded as a whole. The stored row is untouched — the
// persisted value stays the source of truth and remains readable
// through `mpm_wakes list`; this is the DELIVERED view.
//
// The rendered block is bounded separately, by the renderer's own
// field bounds. The two surfaces are accounted for separately.
func (dm *DatabaseManager) wakeRowToMap(p pendingWake, nowUnix int64) map[string]interface{} {
	reason, _ := truncateBytes(p.reason, MaxWakeDeliveredStringBytes)
	createdBy, _ := truncateBytes(p.createdBy, MaxWakeDeliveredStringBytes)
	createdAt, _ := truncateBytes(p.createdAt, MaxWakeDeliveredStringBytes)
	var theoryID interface{}
	if p.theoryID != nil {
		truncated, _ := truncateBytes(*p.theoryID, MaxWakeDeliveredStringBytes)
		theoryID = truncated
	}
	row := map[string]interface{}{
		"id":           truncateString(p.id, MaxWakeDeliveredIDBytes),
		"target_time":  p.targetTime,
		"reason":       reason,
		"theory_id":    theoryID,
		"created_by":   createdBy,
		"created_at":   createdAt,
		"fired_at":     nowUnix,
		"overdue_secs": nowUnix - p.targetTime,
	}
	if p.metadata != nil && *p.metadata != "" {
		var meta map[string]interface{}
		if json.Unmarshal([]byte(*p.metadata), &meta) == nil {
			row["metadata"] = boundWakeMetadata(meta)
		}
	}
	return row
}

// truncateString bounds s to max bytes, marking the cut. It is the
// string form of truncateBytes for the call sites that do not need the
// "was it cut" answer.
func truncateString(s string, max int) string {
	out, _ := truncateBytes(s, max)
	return out
}

// wakeMetadataMarkerAllowance is the room reserved inside
// MaxWakeDeliveredMetadataBytes for the truncation marker, so the
// marker can never push the marshalled document over the bound.
//
// The reservation is generous on purpose: the marker names how many
// keys were omitted, so its length grows with the count, and the
// bound must hold for every count.
const wakeMetadataMarkerAllowance = 96

// boundWakeMetadata returns a bounded projection of a wake's metadata.
//
// THE METADATA IS CALLER-SUPPLIED, so this is what keeps the
// STRUCTURED WakesPending payload bounded.
//
// The projection keeps the two fields the substrate's own delivery
// contract depends on:
//
//   - `kind`, which decides OWNERSHIP — a wake whose kind cannot be
//     read is a wake nobody may deliver; and
//   - `directive_id`, the agent's lookup seam.
//
// The remaining keys are kept in lexicographic order, so the projection
// is deterministic, and only as many of them as fit. Every string
// value is bounded, and a value that was cut says so.
//
// A key that did not fit is not silently lost: the projection carries
// a marker naming how many keys were omitted.
func boundWakeMetadata(meta map[string]interface{}) map[string]interface{} {
	if len(meta) == 0 {
		return meta
	}

	// Deterministic key order: the protected keys first, then the rest
	// in lexicographic order.
	keys := make([]string, 0, len(meta))
	seen := make(map[string]bool, 2)
	for _, k := range []string{"kind", "directive_id"} {
		if _, ok := meta[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(meta))
	for k := range meta {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	out := make(map[string]interface{}, len(keys))
	used := 0
	dropped := 0
	for i, k := range keys {
		if i >= MaxWakeDeliveredMetadataKeys {
			dropped++
			continue
		}
		val := meta[k]
		if s, ok := val.(string); ok {
			s, _ = truncateBytes(s, MaxWakeDeliveredStringBytes)
			val = s
		}
		encoded, err := json.Marshal(val)
		if err != nil {
			dropped++
			continue
		}
		// +1 for the comma between entries.
		if used+len(encoded)+1 > MaxWakeDeliveredMetadataBytes-wakeMetadataMarkerAllowance {
			dropped++
			continue
		}
		out[k] = val
		used += len(encoded) + 1
	}
	if dropped > 0 {
		out["_mpm_meta_truncated"] = fmt.Sprintf(
			"%d key(s) omitted (metadata > %d bytes)",
			dropped, MaxWakeDeliveredMetadataBytes)
	}
	return out
}

// ListScheduledWakes returns wakes filtered by fired state. Defaults to
// pending only (fired=0) so the agent sees its own queue. Pass
// includeFired=true to also surface already-fired wakes (audit trail).
// overdueOnly=true narrows to fired=0 AND target_time < now — useful for
// the "what did I forget?" inspection case.
func (dm *DatabaseManager) ListScheduledWakes(includeFired, overdueOnly bool, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	nowUnix := time.Now().Unix()
	args := []interface{}{}
	where := []string{}
	if !includeFired {
		where = append(where, "fired = 0")
	}
	if overdueOnly {
		where = append(where, "target_time < ?")
		args = append(args, nowUnix)
	}
	q := `SELECT id, target_time, reason, theory_id, fired, fired_at, created_by, metadata, created_at
	      FROM scheduled_wakes`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY target_time ASC LIMIT ?"
	args = append(args, limit)
	rows, err := dm.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list scheduled_wakes: %w", err)
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var (
			id, reason, createdBy, createdAt string
			targetTime                       int64
			theoryID                         *string
			fired                            int
			firedAt                          *int64
			metadata                         *string
		)
		if err := rows.Scan(&id, &targetTime, &reason, &theoryID, &fired, &firedAt, &createdBy, &metadata, &createdAt); err != nil {
			return nil, fmt.Errorf("scan wake list row: %w", err)
		}
		row := map[string]interface{}{
			"id":          id,
			"target_time": targetTime,
			"target_iso":  time.Unix(targetTime, 0).UTC().Format(time.RFC3339),
			"reason":      reason,
			"theory_id":   nullableString(theoryID),
			"fired":       fired == 1,
			"fired_at":    nullableInt64(firedAt),
			"created_by":  createdBy,
			"created_at":  createdAt,
		}
		if metadata != nil && *metadata != "" {
			var meta map[string]interface{}
			if json.Unmarshal([]byte(*metadata), &meta) == nil {
				row["metadata"] = meta
			}
		}
		if firedAt == nil && targetTime < nowUnix {
			row["overdue_secs"] = nowUnix - targetTime
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate wake list rows: %w", err)
	}
	return out, nil
}

// DigestScheduledWakes returns a compact summary of overdue and pending
// wakes — designed for the "agent wakes up after a long idle" case where
// listing every overdue wake individually would blow out context. Returns
// age-bucket counts, total overdue/pending, oldest overdue timestamp, and
// the top-N most overdue (by target_time ASC) with their reasons.
//
// topN defaults to 5 if <=0. The pending_future count is bounded at 10000
// (it's a count, not a row dump — high count just means "lots queued").
//
// Called by handleDigestWakes in internal/tools/handlers.go.
func (dm *DatabaseManager) DigestScheduledWakes(topN int) (map[string]interface{}, error) {
	if topN <= 0 {
		topN = 5
	}
	now := time.Now()
	nowUnix := now.Unix()

	// Pull all overdue wakes (fired=0, target_time < now). Cap at 1000
	// for the aggregation; the digest is a SUMMARY, not an enumeration.
	// If a session ever has >1000 overdue wakes, the system has bigger
	// problems than this digest can solve — the agent should fire the
	// oldest first via CheckPendingWakes.
	rows, err := dm.db.Query(`
		SELECT id, target_time, reason, theory_id
		FROM scheduled_wakes
		WHERE fired = 0 AND target_time < ?
		ORDER BY target_time ASC
		LIMIT 1000
	`, nowUnix)
	if err != nil {
		return nil, fmt.Errorf("query overdue wakes: %w", err)
	}
	defer rows.Close()

	var (
		bucketUnder1h, bucket1hTo1d, bucket1dTo1w, bucket1wTo1mo, bucketOver1mo int
		linkedToTheory                                                          int
		oldestTarget                                                            int64
		oldestReason                                                            string
		topOverdue                                                              = []map[string]interface{}{}
		total                                                                   int
	)
	for rows.Next() {
		var id, reason string
		var targetTime int64
		var theoryID *string
		if err := rows.Scan(&id, &targetTime, &reason, &theoryID); err != nil {
			return nil, fmt.Errorf("scan overdue wake row: %w", err)
		}
		overdueSecs := nowUnix - targetTime
		if overdueSecs < 0 {
			overdueSecs = 0
		}

		switch {
		case overdueSecs < 3600: // < 1 hour
			bucketUnder1h++
		case overdueSecs < 86400: // < 1 day
			bucket1hTo1d++
		case overdueSecs < 604800: // < 1 week
			bucket1dTo1w++
		case overdueSecs < 2592000: // < 30 days
			bucket1wTo1mo++
		default:
			bucketOver1mo++
		}

		if theoryID != nil && *theoryID != "" {
			linkedToTheory++
		}

		if oldestTarget == 0 || targetTime < oldestTarget {
			oldestTarget = targetTime
			oldestReason = reason
		}

		if len(topOverdue) < topN {
			topOverdue = append(topOverdue, map[string]interface{}{
				"reason":       reason,
				"target_iso":   time.Unix(targetTime, 0).UTC().Format(time.RFC3339),
				"overdue_secs": overdueSecs,
				"theory_id":    nullableString(theoryID),
			})
		}
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate overdue wake rows: %w", err)
	}

	// Pending future count (fired=0, target_time >= now). Just a count —
	// the digest doesn't enumerate them.
	var pendingFuture int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 0 AND target_time >= ?`,
		nowUnix,
	).Scan(&pendingFuture); err != nil {
		return nil, fmt.Errorf("count pending future: %w", err)
	}

	out := map[string]interface{}{
		"success":              true,
		"total_overdue":        total,
		"total_pending_future": pendingFuture,
		"linked_to_theory":     linkedToTheory,
		"age_buckets": map[string]int{
			"under_1h":  bucketUnder1h,
			"1h_to_1d":  bucket1hTo1d,
			"1d_to_1w":  bucket1dTo1w,
			"1w_to_1mo": bucket1wTo1mo,
			"over_1mo":  bucketOver1mo,
		},
		"top_overdue": topOverdue,
	}
	if oldestTarget > 0 {
		out["oldest_overdue_iso"] = time.Unix(oldestTarget, 0).UTC().Format(time.RFC3339)
		out["oldest_overdue_secs"] = nowUnix - oldestTarget
		out["oldest_overdue_reason"] = oldestReason
	}
	return out, nil
}

// FireStaleFoundationWakes scans theories whose `dependencies` column
// contains deletedArtifactID and fires one wake per match. Called from
// MemoryStore.DeleteMemory (soft) and DatabaseManager.ShredMemory
// (hard) — both paths funnel through here so the reconciliation surface
// is symmetric regardless of how the foundation rotted.
//
// The wake is fired with a small delay (default 60 seconds) so the
// agent that deleted the artifact can finish its current operation
// before being interrupted. The reason is human-readable; the
// metadata carries structured fields (theory_id, missing_artifact_id)
// for programmatic consumers.
//
// Returns the count of wakes fired. Zero is normal — most memories
// aren't dependencies of any theory. Errors are non-fatal: a failure
// to fire one wake should not abort the upstream delete.
//
// Cost: O(theories) row scan with json_each per row. Fine at current
// scale (theories < 1K). The edge table that would replace this
// (with a memory_id -> theories reverse index) is a future optimization
// — we are not at the scale where it matters yet.
func (dm *DatabaseManager) FireStaleFoundationWakes(deletedArtifactID string) (int, error) {
	if deletedArtifactID == "" {
		return 0, nil
	}

	rows, err := dm.db.Query(`
		SELECT id, dependencies
		FROM memories
		WHERE collection = 'theories'
		  AND deleted_at IS NULL
		  AND dependencies IS NOT NULL
		  AND EXISTS (
		    SELECT 1 FROM json_each(dependencies)
		    WHERE value = ?
		  )
	`, deletedArtifactID)
	if err != nil {
		return 0, fmt.Errorf("scan dependent theories: %w", err)
	}
	defer rows.Close()

	fired := 0
	var failures []string
	for rows.Next() {
		var theoryID string
		var depsJSON sql.NullString
		if err := rows.Scan(&theoryID, &depsJSON); err != nil {
			return fired, fmt.Errorf("scan dependent theory row: %w", err)
		}
		// Target ~60s in the future so the deleting operation can
		// complete before the wake surfaces. Convert to absolute
		// unix seconds via time.Time.
		targetTime := strconv.FormatInt(time.Now().Add(60*time.Second).Unix(), 10)
		meta := map[string]interface{}{
			"type":                "stale_foundation",
			"theory_id":           theoryID,
			"missing_artifact_id": deletedArtifactID,
			"detected_at":         time.Now().UTC().Format(time.RFC3339Nano),
		}
		reason := fmt.Sprintf("Stale foundation: theory %s depends on missing artifact %s",
			theoryID, deletedArtifactID)
		if _, err := dm.ScheduleWake(reason, targetTime, theoryID, "mpm-reconcile", meta); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", theoryID, err))
			continue
		}
		fired++
	}
	if err := rows.Err(); err != nil {
		return fired, fmt.Errorf("iterate dependent theories: %w", err)
	}
	if len(failures) > 0 {
		return fired, fmt.Errorf("partial failure firing stale-foundation wakes: %v", failures)
	}
	return fired, nil
}

// ── helpers ────────────────────────────────────────────────────────────

// newWakeID returns a 32-char hex ID with a "wk-" prefix for fast
// visual differentiation from memories and lessons. Uses crypto/rand;
// uniqueness is also guaranteed by PRIMARY KEY collisions if entropy
// ever fails (extremely unlikely).
func newWakeID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate wake id: %w", err)
	}
	return "wk-" + hex.EncodeToString(b[:]), nil
}

// resolveTargetTime parses an absolute unix epoch OR a relative duration
// string ("30s", "5m", "2h", "1d", "24h"). Returns the resolved absolute
// unix epoch in seconds.
//
// Pure numbers are treated as absolute unix epoch (seconds since 1970).
// Anything ending with a unit letter (s/m/h/d) is relative to `now`.
// Empty input is rejected (would produce a wake due immediately,
// which is rarely what the caller wants; use CheckPendingWakes directly
// if you want "fire right now").
func resolveTargetTime(s string, now time.Time) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("target_time is required (use absolute unix epoch or relative like '24h')")
	}
	if n, err := parseInt64(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("absolute target_time must be non-negative (got %d)", n)
		}
		// Cap absolute target_time at 10 years in the future so a
		// typo (extra digit, wrong unit) can't schedule a wake for
		// year 33658 that the scheduler would never fire. The cap
		// matches the snooze --days cap.
		maxFuture := now.Add(10 * 365 * 24 * time.Hour).Unix()
		if n > maxFuture {
			return 0, fmt.Errorf("absolute target_time too far in the future (max 10 years, got epoch %d = ~%s)", n, time.Unix(n, 0).UTC().Format("2006-01-02"))
		}
		return n, nil
	}
	if d, err := parseDuration(s); err == nil {
		target := now.Add(d).Unix()
		maxFuture := now.Add(10 * 365 * 24 * time.Hour).Unix()
		if target > maxFuture {
			return 0, fmt.Errorf("relative target_time too far in the future (max 10 years, got duration %s)", d)
		}
		return target, nil
	}
	// ISO 8601 absolute timestamp — same contract every other MPM time
	// field honors (snooze_until, since). RFC3339Nano covers the "Z"
	// suffix, RFC3339 covers explicit offsets, and the space form covers
	// SQLite-style datetimes. This is the fallback that makes the wake
	// scheduler consistent with parseClusterSnoozeUntil.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			maxFuture := now.Add(10 * 365 * 24 * time.Hour)
			if t.After(maxFuture) {
				return 0, fmt.Errorf("ISO-8601 target_time too far in the future (max 10 years, got %s)", t.UTC().Format(time.RFC3339))
			}
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("target_time %q: unrecognized (expected unix epoch, relative like '24h'/'30m'/'7d', or ISO-8601 timestamp)", s)
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

// parseDuration supports "30s", "5m", "2h", "1d". Week/month not
// supported because they are ambiguous for autonomous scheduling.
func parseDuration(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("too short")
	}
	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	var n int64
	for _, r := range numStr {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric prefix")
		}
		n = n*10 + int64(r-'0')
	}
	if n == 0 {
		return 0, fmt.Errorf("zero duration")
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unknown unit %q (use s/m/h/d)", string(unit))
}

func nullableString(p *string) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

func nullableInt64(p *int64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// FormatWakeNotification renders due/overdue wakes as an XML system notification
// block. LLMs parse bounded XML-style markers reliably, keeping the wake signal
// visually distinct from the tool's actual output without requiring custom
// per-client annotation parsing.
//
// Each wake entry includes: id (truncated to MaxWakeRenderedIDBytes),
// reason (up to MaxWakeRenderedReasonBytes BYTES, cut at a character
// boundary), overdue_secs, and (when present) directive_id — the latter
// extracted from the wake's metadata so the agent can identify which
// prime directive to consult without a follow-up lookup. The
// directive_id field is the canonical seam between the substrate
// (which delivers the wake) and the agent (which reads the directive
// and acts). For cron-injected wakes from seed.SeedScheduledTasks,
// this is the stable id the agent should pass to read_directives.
//
// Every field is bounded in bytes, so the block is bounded: see
// formatWakeNotificationRow.
//
// Fields omitted when not applicable:
//
//   - directive_id is only rendered when the wake's metadata has a
//     non-empty `directive_id` string. Notification-kind wakes and
//     cascade wakes without a directive reference render without the
//     field — preserves the legacy contract for non-cron wakes.
func FormatWakeNotification(wakes []map[string]interface{}) string {
	if len(wakes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n<system_wake_notification>\n")
	b.WriteString("Scheduled tasks now due:")
	b.WriteString("\n")
	for _, w := range wakes {
		b.WriteString(formatWakeNotificationRow(w))
	}
	b.WriteString("</system_wake_notification>\n")
	return b.String()
}

// formatWakeNotificationRow renders ONE wake as the notification
// block's line.
//
// It is the single source of the block's per-wake cost: the fold's
// byte budget (MaxWakeNotificationBytesPerKind) measures the bytes
// this function produces, so the budget is expressed in the units the
// agent actually receives rather than in a proxy (a row count) that
// does not bound the output.
//
// EVERY FIELD IS BOUNDED, IN BYTES.
//
//   - the id is cut to MaxWakeRenderedIDBytes;
//   - the reason is cut to MaxWakeRenderedReasonBytes;
//   - the directive_id is bounded to MaxWakeDirectiveIDBytes.
//
// The directive_id was the one field that was NOT bounded, and it was
// the whole hole: it is caller-supplied and was rendered verbatim, so
// one wake could carry a 200 KB directive_id and make the block fifty
// times its own budget. It is now bounded, and a value that was cut
// says so — the marker names the original byte length, because a
// directive id is an identifier the agent passes back to
// read_directives and a silently shortened one would be worse than an
// obviously shortened one.
//
// The function bounds defensively as well as faithfully: it is
// exported and can be called with a row map this package did not
// build, so the bounds cannot be assumed to have been applied already.
func formatWakeNotificationRow(w map[string]interface{}) string {
	id := ""
	if s, ok := w["id"].(string); ok {
		id = truncateRunes(s, MaxWakeRenderedIDBytes)
	}
	reason := ""
	if r, ok := w["reason"].(string); ok {
		reason = truncateRunes(r, MaxWakeRenderedReasonBytes)
	}
	overdueSecs := int64(0)
	if o, ok := w["overdue_secs"].(int64); ok {
		overdueSecs = o
	} else if o, ok := w["overdue_secs"].(float64); ok {
		overdueSecs = int64(o)
	}
	// Extract directive_id from the parsed metadata map, if
	// present. Defensive against both nil metadata and
	// non-string directive_id (e.g. null) — the field is omitted
	// rather than rendered as "directive_id=null" which would
	// confuse the LLM triage path.
	directiveID := extractDirectiveID(w)
	if directiveID != "" {
		directiveID, _ = truncateBytes(directiveID, MaxWakeDirectiveIDBytes)
		return fmt.Sprintf("  • id=%s | reason=%q | overdue_secs=%d | directive_id=%q\n",
			id, reason, overdueSecs, directiveID)
	}
	return fmt.Sprintf("  • id=%s | reason=%q | overdue_secs=%d\n",
		id, reason, overdueSecs)
}

// extractDirectiveID pulls the directive_id string out of a wake row
// map, defensively. The map's metadata key (when present) is a
// parsed JSON object (from wakeRowToMap), but external callers
// passing a raw row map may also have a string-typed metadata —
// handle both shapes for forward compatibility.
func extractDirectiveID(w map[string]interface{}) string {
	meta, ok := w["metadata"]
	if !ok || meta == nil {
		return ""
	}
	switch v := meta.(type) {
	case map[string]interface{}:
		if id, ok := v["directive_id"].(string); ok {
			return id
		}
	case string:
		// Best-effort: parse if it looks like JSON. Keep the call
		// cheap (single attempt, swallow errors).
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			if id, ok := parsed["directive_id"].(string); ok {
				return id
			}
		}
	}
	return ""
}

// truncateRunes cuts s to at most max BYTES of content at a character
// boundary, and appends the ellipsis when it had to cut.
//
// The cut is at a CHARACTER boundary, not a byte boundary. A byte cut
// can land in the middle of a multi-byte character, and the orphan
// byte is then escaped as \xNN by the renderer: the agent reads a
// mangled character, and the line's byte cost becomes unpredictable
// (four bytes of output for one byte of input).
//
// The result is at most max+3 bytes — the ellipsis is three bytes.
//
// This is the DISPLAY cut: the ellipsis is the signal that the value
// was cut. It is used for the id and the reason, both of which are
// prose the agent reads rather than values it passes back.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut, _ := cutAtCharBoundary(s, max)
	return cut + "…"
}

// truncateBytes bounds s to at most max bytes TOTAL, cut at a
// character boundary, and reports whether it had to cut.
//
// The marker names the ORIGINAL byte length, so the agent can tell
// exactly how much was withheld instead of guessing from an ellipsis.
// The marker is charged to the budget: the returned string is at most
// max bytes, marker included.
//
// This is the EXACT cut: it is used for values the agent passes back
// to the substrate, where a silent shortening would be a correctness
// problem rather than a cosmetic one.
func truncateBytes(s string, max int) (string, bool) {
	if max <= 0 {
		return "", s != ""
	}
	if len(s) <= max {
		return s, false
	}
	marker := "…[truncated " + strconv.Itoa(len(s)) + " bytes]"
	room := max - len(marker)
	if room < 0 {
		// The marker alone does not fit in the budget. Cut as far as
		// the budget allows and say only that the value was cut.
		if max >= 3 {
			cut, _ := cutAtCharBoundary(s, max-3)
			return cut + "…", true
		}
		cut, _ := cutAtCharBoundary(s, max)
		return cut, true
	}
	cut, _ := cutAtCharBoundary(s, room)
	return cut + marker, true
}

// cutAtCharBoundary cuts s to at most max bytes without splitting a
// multi-byte character. It reports whether anything was cut.
func cutAtCharBoundary(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	if max <= 0 {
		return "", true
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
