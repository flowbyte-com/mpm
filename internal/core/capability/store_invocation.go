package capability

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/google/uuid"
)

// =============================================================================
// store_invocation.go — capability execution telemetry (spec §4 / EX-2)
//
// RecordInvocation is the load-bearing telemetry write. Every
// successful capability invocation ends here, and the write is
// transactional with the capability counter update so:
//
//   * success_count + failure_count == COUNT(*) FROM capability_invocations
//     WHERE capability_id = ?  (the invariant EX-7 fracture
//     detection depends on)
//   * A partial write is impossible — both rows commit together
//     or neither does.
//
// The retry policy rides on top of DatabaseManager.busy_timeout=5000
// (see db.go). Per-attempt, the SQLite driver itself blocks up
// to 5s waiting for a lock holder to finish. The outer retry
// handles the rare case where one busy_timeout cycle isn't enough:
//
//   Attempt 1: immediate
//   Attempt 2: 5ms backoff
//   Attempt 3: 25ms backoff
//   Attempt 4: 125ms backoff
//   Exhausted: ErrTelemetryFailed surfaces to the Executor
//
// The retry wraps the ENTIRE WithTx closure (not individual
// statements inside it). On retry, a fresh transaction begins;
// the prior transaction's writes rolled back automatically when
// its closure returned the error. This is the only safe shape:
// SQLITE_BUSY can fire at BEGIN, at any statement, AND at COMMIT,
// so retrying inside a single transaction is unsafe.
//
// The retry is context-aware: ctx.Done() is checked between every
// attempt AND inside every backoff sleep. A caller-supplied
// deadline kills the retry promptly instead of letting it
// consume the caller's budget.
//
// Spec §4.2 / §2.2 / EX-2 of the capability lifecycle plan.
// =============================================================================

// invocationRetryMaxAttempts is the outer-retry budget. With
// busy_timeout=5000 already providing per-attempt lock-wait
// tolerance, this caps the worst-case latency at roughly 20s.
// Most retries succeed on attempt 1 or 2; the budget exists for
// the genuinely-contended case (concurrent cascade walkers,
// fracture ticks, sibling capability promotions).
const invocationRetryMaxAttempts = 4

// invocationRetryBaseBackoff is the first inter-attempt sleep.
// Doubles each retry (5ms → 25ms → 125ms) up to invocationRetryMaxBackoff.
const invocationRetryBaseBackoff = 5 * time.Millisecond

// invocationRetryMaxBackoff caps the exponential growth.
const invocationRetryMaxBackoff = 1 * time.Second

// invocationContext is the JSON payload stored in
// capability_invocations.invocation_context. Schema-versioned so
// future fields can be added without breaking old readers.
type invocationContext struct {
	SchemaVersion int      `json:"schema_version"`
	Language      string   `json:"language"`
	Truncated     bool     `json:"truncated"`
	ArgsHash      string   `json:"args_hash"`
	EnvKeys       []string `json:"env_keys"`
	DriverName    string   `json:"driver_name"`
	Kind          string   `json:"kind"` // "execute" (future: "dryrun", "fracture_check")
}

// invocationContextSchemaVersion is bumped when the JSON shape
// changes incompatibly. Readers compare against their known
// version; unknown versions fall back to the legacy fields.
const invocationContextSchemaVersion = 1

// invocationWriter is the EX-2 test seam. Production wires
// Store.recordInvocationTx (the real SQLite write); tests inject
// a mock to simulate SQLITE_BUSY contention deterministically
// without holding real locks.
//
// The interface is single-method because every retry attempt
// is a fresh transaction. There's no "did the INSERT commit but
// the UPDATE fail" state to model — the writer either succeeds
// (transaction committed) or fails (transaction rolled back).
type invocationWriter interface {
	Write(ctx context.Context, n internal.DBNode, payload *TelemetryPayload) error
}

// realInvocationWriter is the production invocationWriter.
// Writes the capability_invocations row + the capability
// counter update inside one transaction. Counter increment is
// computed in Go (success_count + 1 or failure_count + 1) rather
// than as a SQL CASE so the column list stays simple and the
// "did this row's success/failure counter increment" assertion
// is a straightforward equality check.
type realInvocationWriter struct {
	store *Store
}

func (w *realInvocationWriter) Write(ctx context.Context, n internal.DBNode, payload *TelemetryPayload) error {
	// Build the invocation_context JSON once. Bounded
	// allocation: EnvKeys sorted in the Executor (small slice),
	// JSON encoding is a few hundred bytes max.
	ictx := invocationContext{
		SchemaVersion: invocationContextSchemaVersion,
		Language:      string(payload.Language),
		Truncated:     payload.Truncated,
		ArgsHash:      payload.ArgsHash,
		EnvKeys:       payload.EnvKeys,
		DriverName:    payload.DriverName,
		Kind:          "execute",
	}
	ictxJSON, err := json.Marshal(ictx)
	if err != nil {
		return fmt.Errorf("capability: marshal invocation_context: %w", err)
	}

	// stderr is nullable (a successful invocation with empty
	// stderr stores NULL, not ""). Mirrors the column's NULL
	// allowance in schema.go.
	var stderrArg interface{}
	if len(payload.Stderr) > 0 {
		s := string(payload.Stderr)
		stderrArg = &s
	}

	now := payload.FinishedAt
	if now == 0 {
		now = w.store.Now()
	}

	// 1. INSERT the telemetry row. The id is a fresh UUID —
	// never reused, never derived. capability_invocations.id
	// is a PRIMARY KEY so any collision is caught by the
	// unique constraint (UUID4 collision probability is
	// negligible but the constraint is the safety net).
	invID := uuid.NewString()
	if _, err := n.ExecTracked(
		`INSERT INTO capability_invocations
		 (id, capability_id, invoked_at, exit_code, duration_ms,
		  stderr, invocation_context, cascade_invalidated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		2, // retries — internal retry on top of outer retry
		invID, payload.CapabilityID, payload.StartedAt,
		payload.ExitCode, payload.DurationMs,
		stderrArg, string(ictxJSON),
	); err != nil {
		return fmt.Errorf("capability: insert invocation: %w", err)
	}

	// 2. UPDATE the capability counter. The increment is
	// based on ExitCode: zero is success, anything else is
	// failure. Matches the CheckProbationCriteria math
	// (failure_count / (success_count + failure_count)).
	isSuccess := payload.ExitCode == 0
	successDelta := 0
	failureDelta := 0
	if isSuccess {
		successDelta = 1
	} else {
		failureDelta = 1
	}

	if _, err := n.ExecTracked(
		`UPDATE capabilities
		 SET success_count      = success_count + ?,
		     failure_count      = failure_count + ?,
		     last_invoked_at    = ?,
		     updated_at         = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		2,
		successDelta, failureDelta,
		now, now, payload.CapabilityID,
	); err != nil {
		return fmt.Errorf("capability: update counters: %w", err)
	}
	return nil
}

// withRetry runs fn with bounded exponential backoff on
// transient errors. Honors ctx.Done() between every attempt and
// inside every backoff sleep. Returns the final error.
//
// The classification (transient vs permanent) is the caller's
// responsibility — withRetry retries on every error, up to
// invocationRetryMaxAttempts. RecordInvocation filters first:
// non-busy errors fail-fast on the first attempt (only one
// pass through the loop), busy errors consume the full budget.
//
// Why a retry primitive on the Store rather than on the
// Executor: the policy is specifically about SQLite write
// contention, not about Executor policy. Future Store methods
// (FractureCapability in EX-7, etc.) reuse the same primitive
// without re-implementing the backoff math or the
// context-cancellation discipline.
func (s *Store) withRetry(ctx context.Context, fn func() error) error {
	var lastErr error
	backoff := invocationRetryBaseBackoff

	for attempt := 1; attempt <= invocationRetryMaxAttempts; attempt++ {
		// Pre-attempt check: bail before opening a new
		// transaction if the caller's deadline has passed.
		if err := ctx.Err(); err != nil {
			if lastErr == nil {
				return err
			}
			return lastErr
		}

		err := fn()
		if err == nil {
			return nil
		}
		lastErr = err

		// Non-busy errors fail-fast. Only SQLITE_BUSY
		// gets the full retry budget; everything else
		// (constraint violations, syntax errors, I/O
		// failures) is structural and won't fix itself
		// with another attempt.
		if !internal.IsBusyError(err) {
			return err
		}

		// Don't sleep after the last attempt — there's
		// no next attempt to wait for.
		if attempt == invocationRetryMaxAttempts {
			break
		}

		// Cancellable backoff. If the caller's ctx fires
		// during the sleep, we abort immediately rather
		// than burning the rest of the budget.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr == nil {
				return ctx.Err()
			}
			return lastErr
		case <-timer.C:
		}

		// Exponential growth with cap.
		backoff *= 5
		if backoff > invocationRetryMaxBackoff {
			backoff = invocationRetryMaxBackoff
		}
	}

	return lastErr
}

// RecordInvocation persists a successful invocation's telemetry
// and updates the capability's counters in one transaction,
// retried with bounded backoff on transient lock contention.
//
// Returns:
//
//   nil                 — transaction committed; counters updated
//   ErrTelemetryFailed  — outer retry budget exhausted OR
//                         ctx cancelled OR non-transient write
//                         error. Callers (Executor.Invoke) map
//                         this to (nil, ErrTelemetryFailed) so
//                         the agent sees the invocation as failed.
//   other error         — wrapped non-transient write failure;
//                         RecordInvocation still returns nil on
//                         success and a non-nil error on failure,
//                         but the type may be useful for tests
//                         asserting the failure shape.
//
// The contract:
//   * Telemetry write failure → ErrTelemetryFailed (wrapped)
//   * Caller-supplied ctx cancellation → ctx.Err() (wrapped as ErrTelemetryFailed)
//   * Transient SQLITE_BUSY (within budget) → silent retry, eventual success
func (s *Store) RecordInvocation(ctx context.Context, payload *TelemetryPayload) error {
	if payload == nil {
		return fmt.Errorf("capability: RecordInvocation: payload is nil")
	}
	if payload.CapabilityID == "" {
		return fmt.Errorf("capability: RecordInvocation: CapabilityID is empty")
	}

	// The retry wraps the ENTIRE transaction. Each attempt
	// opens a fresh transaction via withTx; on failure,
	// withTx rolls back automatically (see WithTx in
	// internal/core/db.go). The next retry starts from a
	// clean state.
	err := s.withRetry(ctx, func() error {
		return s.withTx(func(n internal.DBNode) error {
			return s.writer.Write(ctx, n, payload)
		})
	})
	if err != nil {
		// Map everything to ErrTelemetryFailed so callers
		// have one error to check. The wrapped error chain
		// preserves the underlying cause for diagnostics —
		// errors.Join lets callers errors.Is(err,
		// context.Canceled) AND errors.Is(err,
		// ErrTelemetryFailed) simultaneously.
		//
		// ctx.Err() is special-cased only in the message,
		// not in the chain: a caller cancellation means the
		// caller's deadline was too tight for the retry
		// budget. We still surface ErrTelemetryFailed (not
		// ctx.Err() directly) because the Invoker contract
		// is "telemetry failure == invocation failure"; the
		// caller can errors.Is(err, context.Canceled) if it
		// needs to distinguish.
		return fmt.Errorf("capability: telemetry write failed: %w",
			errors.Join(ErrTelemetryFailed, err))
	}
	return nil
}

// RecentInvocations returns the invocations for a capability
// whose started_at >= since, ordered oldest-first. Powers the
// EX-7 fracture detection sliding window (3 failures in 60s).
//
// Reads use the (capability_id, invoked_at DESC) index defined
// in schema.go so the query is index-covered even at high row
// counts.
//
// Limit defaults to 0 meaning "no limit." EX-7's caller will
// pass a small bound (e.g., 10) since the fracture window only
// needs the most recent few invocations.
func (s *Store) RecentInvocations(ctx context.Context, capabilityID string, since int64, limit int) ([]Invocation, error) {
	if capabilityID == "" {
		return nil, fmt.Errorf("capability: RecentInvocations: capabilityID is empty")
	}

	// Build the query with an optional LIMIT. The two cases
	// are split (rather than LIMIT -1) because SQLite's
	// behavior with negative LIMIT is historically quirky
	// across versions and we don't need to test it.
	q := `SELECT id, capability_id, invoked_at, exit_code, duration_ms,
	             stderr, invocation_context, cascade_invalidated
	      FROM capability_invocations
	      WHERE capability_id = ? AND invoked_at >= ?
	      ORDER BY invoked_at ASC`
	args := []interface{}{capabilityID, since}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.dm.QueryTracked(q, args...)
	if err != nil {
		return nil, fmt.Errorf("capability: RecentInvocations: %w", err)
	}
	defer rows.Close()

	var invocations []Invocation
	for rows.Next() {
		var inv Invocation
		var stderr sql.NullString
		var invCtx sql.NullString
		if err := rows.Scan(
			&inv.ID, &inv.CapabilityID, &inv.InvokedAt, &inv.ExitCode,
			&inv.DurationMs, &stderr, &invCtx, &inv.CascadeInvalidated,
		); err != nil {
			return nil, fmt.Errorf("capability: RecentInvocations: scan: %w", err)
		}
		if stderr.Valid {
			s := stderr.String
			inv.Stderr = &s
		}
		if invCtx.Valid {
			s := invCtx.String
			inv.InvocationContext = &s
		}
		invocations = append(invocations, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("capability: RecentInvocations: rows: %w", err)
	}
	return invocations, nil
}

// HashArgs computes a deterministic SHA-256 hex digest of argv
// for the ArgsHash field on TelemetryPayload. argv is hashed IN
// CALLER ORDER with NUL separators — order is semantically
// significant for shell-style invocations ("git checkout -b x"
// vs "git -b checkout x" are distinct intents and must produce
// distinct hashes so the router can cluster them separately).
//
// Empty argv returns the SHA-256 of the empty string (a
// constant). Repeated invocations of the same argv produce
// identical hashes (verified by TestExecutor_ArgsHashDeterminism).
func HashArgs(args []string) string {
	h := sha256.New()
	for _, a := range args {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SortedEnvKeys returns a sorted copy of env's keys. The
// Executor passes the result to TelemetryPayload.EnvKeys — env
// VALUES are never recorded (secrets stay out of the ledger).
//
// nil env returns nil. An env with no keys returns an empty
// (non-nil) slice so JSON encoding produces [] rather than null.
func SortedEnvKeys(env map[string]string) []string {
	if env == nil {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// errBusySentinel is a test-only error string that classifies
// as SQLITE_BUSY under IsBusyError. Lets tests inject
// deterministic contention without depending on mattn/go-sqlite3's
// actual error wrapping.
var errBusySentinel = errors.New("SQLITE_BUSY: database is locked")

// classifyForRetry is a small helper for tests + the
// RecordInvocation outer-loop decision. Returns true if the
// error is a transient busy error that should consume the
// retry budget; false for permanent errors that fail-fast.
func classifyForRetry(err error) bool {
	if err == nil {
		return false
	}
	if internal.IsBusyError(err) {
		return true
	}
	// Test injection path: errors that look like busy
	// sentinels but were constructed via errors.New (not via
	// SQLite). The string match covers both paths.
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY")
}