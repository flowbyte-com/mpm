// watchdog_v2.go — the v2 watchdog record: a human's black box, not a
// statement trace.
//
// v1 wrote one line per tracked SQL operation, success or failure. Measured
// over 88 days that is 2,423,679 lines, of which ~99.5% are fast successful
// statements (design §5.4) — 27.5k appends a day, each a file open, a
// rotation stat and a mutex acquisition, to record that an INSERT worked.
//
// v2 keeps the lines a person actually reads while diagnosing something:
//
//	{"v":2,"ts":"…","level":"error","op":"exec","duration_ms":3,
//	 "sql_shape":"INSERT INTO memories (…)",
//	 "detail":"INSERT INTO memories failed","error":"constraint failed: …"}
//
// A fast successful statement produces no line at all. That is not a
// sampling decision and it is not a data loss: the operations are ordinary
// database work, `mpm.db` holds the result, and per-shape counts and
// histograms are telemetry's job (design §13) — explicitly not this
// task's, and explicitly not something to smuggle in under a different
// name here.
package internal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// watchdogFormatVersion is the `v` field on every v2 watchdog record.
const watchdogFormatVersion = 2

// Log levels. Errors and warns dominate; `info` is reserved for lifecycle
// and migration outcomes, which are normal and worth seeing in a tail.
const (
	watchdogLevelInfo  = "info"
	watchdogLevelWarn  = "warn"
	watchdogLevelError = "error"
)

// Watchdog operation vocabulary.
//
// The first three name a SQL statement and appear on records the statement
// produced. The rest are lifecycle operations. The design's full list also
// includes migration, fts_recovery/fts_error, scheduler lifecycle,
// db_open/checkpoint/recovery, service_lifecycle, integrity/health failure
// and unusual_state_transition; the constant names are declared here so the
// vocabulary is one place, but a category with no writer today is NOT given
// an invented one — see the comment on lifecycle watchdog operations
// below.
const (
	// SQL statement operations.
	watchdogOpExec     = "exec"
	watchdogOpQuery    = "query"
	watchdogOpQueryRow = "query_row"

	// Lifecycle / subsystem operations with an existing writer.
	watchdogOpSynthesize           = "synthesize"
	watchdogOpSynthesizeSkip       = "synthesize_skip"
	watchdogOpSynthesizeFailed     = "synthesize_failed"
	watchdogOpDestructiveOperation = "destructive_operation"
	watchdogOpMemoryShredded       = "memory_shredded"

	// Lifecycle operations the design names but that have no writer yet.
	// Declared, not emitted: giving a category a writer "because the data
	// exists" is the same mistake as keeping fast-success SQL, one level
	// up. Each is a small, individually reviewable addition when the
	// subsystem that should emit it is next touched.
	watchdogOpBusy                = "busy"
	watchdogOpSlow                = "slow"
	watchdogOpMigration           = "migration"
	watchdogOpFTSRecovery         = "fts_recovery"
	watchdogOpFTSError            = "fts_error"
	watchdogOpSchedulerStartup    = "scheduler_startup"
	watchdogOpSchedulerShutdown   = "scheduler_shutdown"
	watchdogOpSchedulerDrainAnom  = "scheduler_drain_anomaly"
	watchdogOpDBOpen              = "db_open"
	watchdogOpDBCheckpoint        = "db_checkpoint"
	watchdogOpDBRecovery          = "db_recovery"
	watchdogOpServiceLifecycle    = "service_lifecycle"
	watchdogOpIntegrityFailure    = "integrity_failure"
	watchdogOpHealthFailure       = "health_failure"
	watchdogOpUnusualStateTrans   = "unusual_state_transition"
	watchdogOpBlockedSensitiveKey = "blocked_sensitive_key"
)

// watchdogSQLOps is the set of operations a SQL-derived record may carry.
var watchdogSQLOps = map[string]bool{
	watchdogOpExec:     true,
	watchdogOpQuery:    true,
	watchdogOpQueryRow: true,
}

// watchdogDetailMax bounds the free-form `detail` field. Same 280 ceiling
// as the mirror preview, for the same reason: these lines are read in a
// terminal, and a line that wraps is a line that gets filtered out of a
// `tail -f`.
const watchdogDetailMax = 280

// retention-reason values: which trigger earned a SQL record its line.
const (
	watchdogReasonError     = "sql_error"
	watchdogReasonBusyRetry = "busy_retry"
	watchdogReasonSlow      = "slow"
)

// watchdogEvent is one v2 watchdog record.
//
// The design's minimum shape is `v, ts, level, op, duration_ms, sql_shape,
// detail, error`. The two additions are deliberate:
//
//   - `reason` says WHICH trigger retained the line. Without it a reader
//     cannot tell a 200ms query that was merely slow from one that only
//     reached the log because it was retried twice under lock contention —
//     two very different diagnoses from two records that look alike.
//   - `retries` is kept on the record rather than folded into `detail`
//     because it is a number, and a number a reader might want to filter
//     on should not be a substring of a sentence.
type watchdogEvent struct {
	V          int    `json:"v"`
	Ts         string `json:"ts"`
	Level      string `json:"level"`
	Op         string `json:"op"`
	Reason     string `json:"reason,omitempty"`
	DurationMs *int64 `json:"duration_ms,omitempty"`
	Retries    int    `json:"retries,omitempty"`
	SQLShape   string `json:"sql_shape,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Error      string `json:"error,omitempty"`
	// Fields carries subsystem-specific extras (synthesis ids, safeguard
	// labels). Nested rather than merged into the top level so the v2
	// envelope's field set stays fixed and a reader can rely on it.
	Fields map[string]interface{} `json:"fields,omitempty"`
}

// newWatchdogEvent stamps the v2 envelope. Every watchdog write goes
// through here, so "every v2 record carries v, ts and level" is structural
// rather than a convention each call site has to remember.
func newWatchdogEvent(level, op, detail string) watchdogEvent {
	return watchdogEvent{
		V:      watchdogFormatVersion,
		Ts:     time.Now().UTC().Format(time.RFC3339Nano),
		Level:  level,
		Op:     op,
		Detail: truncatePreview(redactSensitiveText(detail), watchdogDetailMax),
	}
}

// logWatchdogEvent writes one v2 record through the shared path. A log
// failure never fails the operation that produced it.
func (dm *DatabaseManager) logWatchdogEvent(ev watchdogEvent) {
	if dm == nil {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := hitlStreamFor(dm.watchdogPath, watchdogPolicy).Append(data); err != nil {
		slog.Debug("watchdog append skipped", "err", err)
	}
}

// ==================== SQL outcome filtering ====================

// classifySQLOutcome decides whether a completed SQL operation earns a
// watchdog line, and at what severity.
//
// The rule is the whole of the v2 watchdog: a statement that did what it
// was supposed to do, quickly, is not news. Only three things are —
// it failed, it was retried (which means something else held the database),
// or it was slow (which means something is wrong with the schema or the
// contention). Precedence is error, then retry, then slow, so a statement
// that both failed and retried produces exactly one line at the highest
// severity rather than two lines that double-count the same event.
func classifySQLOutcome(elapsed time.Duration, err error, retries int) (reason, level string, ok bool) {
	switch {
	case err != nil:
		return watchdogReasonError, watchdogLevelError, true
	case retries > 0:
		return watchdogReasonBusyRetry, watchdogLevelWarn, true
	case elapsed > slowQueryThreshold:
		return watchdogReasonSlow, watchdogLevelWarn, true
	default:
		return "", "", false
	}
}

// logSQLOutcome is the single call site the tracked Exec/Query paths use.
// Everything about "should this be logged" and "what is safe to put in it"
// is decided here, so none of the six tracked paths can drift into
// re-implementing the policy.
//
// query is the statement text. It is never logged as text: only its
// normalized shape (normalizeSQLShape) reaches the file, which is the
// design's SQL secrecy contract. The bound values that a caller passed are
// Go-level arguments and never appear in query at all.
func (dm *DatabaseManager) logSQLOutcome(op, query string, elapsed time.Duration, err error, retries int) {
	reason, level, ok := classifySQLOutcome(elapsed, err, retries)
	if !ok {
		return
	}

	ms := elapsed.Milliseconds()
	ev := watchdogEvent{
		V:          watchdogFormatVersion,
		Ts:         time.Now().UTC().Format(time.RFC3339Nano),
		Level:      level,
		Op:         op,
		Reason:     reason,
		DurationMs: &ms,
		Retries:    retries,
		SQLShape:   normalizeSQLShape(query),
		Detail:     sqlOutcomeDetail(op, reason),
	}
	if err != nil {
		// The error string is driver text and may, in principle, echo a
		// value. Redaction is defence in depth behind the shape
		// normaliser, not a substitute for it.
		ev.Error = redactSensitiveText(truncatePreview(err.Error(), watchdogDetailMax))
	}
	dm.logWatchdogEvent(ev)
}

// sqlOutcomeDetail is the fixed human sentence for a retained SQL record.
// Fixed strings rather than interpolated ones: the shape is already in
// sql_shape, and a template that grew a slot for the statement text would
// quietly become the leak the shape normaliser exists to prevent.
func sqlOutcomeDetail(op, reason string) string {
	switch reason {
	case watchdogReasonError:
		return op + " failed"
	case watchdogReasonBusyRetry:
		return op + " completed after SQLITE_BUSY retries"
	case watchdogReasonSlow:
		return "slow " + op
	default:
		return op
	}
}

// ==================== Synthesis lifecycle ====================

// synthesisPassthroughFields are the synthesis event fields copied onto a v2
// record's `fields` object. Ids, hashes, counters and machine-stable reason
// labels qualify; the pre-v1 `content` excerpt deliberately does not — see
// logSynthesizeEvent.
var synthesisPassthroughFields = []string{
	"memory_id", "new_id", "old_ids", "content_hash",
	"prior_runs", "cooldown_s", "bounded_safeguard", "safeguard_reason",
}

// logSynthesizeEvent records a synthesis outcome. Synthesis is a lifecycle
// event rather than a fast SQL statement, so its success line is retained —
// "why did my memories not merge?" is a question the log has to be able to
// answer, and the answer includes the runs that worked.
//
// The pre-v1 writer attached up to 120 characters of the memory body being
// synthesized. That is row content in an operational log: the same class of
// copy the redesign removes everywhere else, and one the SQL secrecy
// contract forbids outright. The failure record keeps the driver's error and
// the bounded-execution safeguard's reason, which is what an operator
// actually diagnoses from; the content it was synthesizing is in mpm.db and
// retrievable by the id the record carries.
func logSynthesizeEvent(dm CoreDB, op string, fields map[string]interface{}) {
	real, ok := dm.(*DatabaseManager)
	if !ok || real == nil {
		// Not a manager we can reach the file through (a test double, or
		// a future implementation). Best-effort observability: drop it
		// rather than fail the caller, same as the pre-v2 contract.
		return
	}

	ev := newWatchdogEvent(watchdogLevelInfo, op, "")
	switch op {
	case watchdogOpSynthesizeFailed:
		ev.Level = watchdogLevelError
		if s := stringField(fields, "safeguard_reason"); s != "" {
			ev.Detail = truncatePreview(redactSensitiveText(s), watchdogDetailMax)
		}
		ev.Error = truncatePreview(redactSensitiveText(stringField(fields, "error")), watchdogDetailMax)
	case watchdogOpSynthesizeSkip:
		ev.Detail = truncatePreview(redactSensitiveText(stringField(fields, "reason")), watchdogDetailMax)
	}

	if extra := synthesisExtraFields(fields); len(extra) > 0 {
		ev.Fields = extra
	}
	real.logWatchdogEvent(ev)
}

// synthesisExtraFields projects the passthrough set out of a synthesis
// event's data map, dropping anything that is not a value type the JSON
// encoder can round-trip losslessly into an operator's log.
func synthesisExtraFields(fields map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, k := range synthesisPassthroughFields {
		v, ok := fields[k]
		if !ok || v == nil {
			continue
		}
		switch v.(type) {
		case string, bool, int, int64, float64, []string, []interface{}:
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stringField reads a string key from a synthesis event's data map,
// tolerating a non-string value rather than panicking on a caller that
// changed shape.
func stringField(fields map[string]interface{}, key string) string {
	if fields == nil {
		return ""
	}
	s, _ := fields[key].(string)
	return s
}

// LogDestructiveOperation appends a destructive_operation record naming
// what was operated on and how many files it touched.
//
// It exists for the case where a destructive command removes a log but must
// not remove the record that it did. `mpm memory wipe` clears the mirror
// journal; without this, an operator inspecting watchdog.jsonl afterwards
// would find the evidence of their own action missing from the one log that
// was supposed to be the operational black box.
//
// The payload is a scope string and a count. Never content, never an id
// list: the count is what a reader needs to check the operation against,
// and anything more is a second copy of the thing being destroyed.
func (dm *DatabaseManager) LogDestructiveOperation(scope string, count int) {
	if dm == nil {
		return
	}
	ev := newWatchdogEvent(watchdogLevelWarn, watchdogOpDestructiveOperation,
		fmt.Sprintf("%s: %d file(s)", scope, count))
	ev.Fields = map[string]interface{}{"scope": scope, "count": count}
	dm.logWatchdogEvent(ev)
}
