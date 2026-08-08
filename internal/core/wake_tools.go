// wake_tools.go — DM methods for the scheduled_wakes table (Phase 5a).
//
// Architecture: stateless, opportunistic scheduling. There is no
// long-lived process and no time.Ticker. Any MPM call (CLI or MCP) that
// passes through CheckPendingWakes sees due wakes surfaced in its
// response payload. The "agent has initiative" effect is achieved by the
// next tool call after the wake time, regardless of which session or
// agent issues it.
//
// Lifecycle of a wake:
//
//   1. Agent calls ScheduleWake(reason, target_time, theory_id?, recurring_rule?).
//      Row written with fired=0.
//   2. Time passes. No daemon runs. The DB is the only substrate.
//   3. ANY subsequent MPM call invokes CheckPendingWakes (called by
//      handlers from within their dispatch). Wakes where fired=0 AND
//      target_time <= now() are returned in a WakesPending block on the
//      response, marked fired=1, fired_at=now.
//   4. The agent sees the wake, evaluates the reason, optionally
//      resolves a theory, optionally schedules the next wake.
//
// Why no daemon: avoids reintroducing the watcher's long-lived
// footprint (deprecated 2026-06-26 in commit e1bc707) for a primitive
// that "fire on next contact" covers cleanly. The agent is the
// scheduling loop. The DB is the queue.
package internal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxCascadeWakePerCheck bounds cascade wake delivery per CheckPendingWakes
// call. Cascade wakes (metadata.kind == "cascade") above this cap are left
// unfired (pending) for the next call. Notification and cron wakes are
// unaffected by this cap. Default value: 3.
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
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, NULL, NULL, 0, ?, ?)`,
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
		 WHERE json_extract(metadata, '$.kind') = 'cascade_summary'
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
// recurring_rule is a hint for the agent's own next-schedule logic
// (the daemon does NOT parse it).
func (dm *DatabaseManager) ScheduleWake(reason, targetTime, theoryID, recurringRule, createdBy string, metadata map[string]interface{}) (map[string]interface{}, error) {
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
	var metaJSON string
	if metadata != nil {
		b, err := json.Marshal(metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal metadata: %w", err)
		}
		metaJSON = string(b)
	}
	var theoryPtr interface{}
	if theoryID != "" {
		theoryPtr = theoryID
	}
	var recurPtr interface{}
	if recurringRule != "" {
		recurPtr = recurringRule
	}
	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		id, absolute, reason, theoryPtr, recurPtr, createdBy, metaJSON,
	)
	if err != nil {
		return nil, fmt.Errorf("insert scheduled_wakes: %w", err)
	}
	return map[string]interface{}{
		"success":      true,
		"id":           id,
		"target_time":  absolute,
		"target_iso":   time.Unix(absolute, 0).UTC().Format(time.RFC3339),
		"reason":       reason,
		"theory_id":    theoryID,
		"recurring_rule": recurringRule,
	}, nil
}

// CheckPendingWakes returns every wake where fired=0 AND target_time <= now,
// marks them fired=1 with fired_at=now, and returns them in chronological
// order. Returns an empty slice when nothing is due (NOT an error).
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
//   - kinds nil/empty: backward-compatible default. Surfaces notification
//     wakes (kind='notification' or kind absent on metadata). Cron-injected
//     and other system-kind wakes are excluded.
//   - kinds contains "*": surface every pending wake regardless of kind.
//   - kinds is a specific list: surface only wakes whose metadata.kind
//     matches one of the entries (json_extract IN (...)). Wakes with
//     no kind set are excluded by this branch.
//
// Cascade wake cap:
//   When kinds includes "cascade" (or is "*"), cascade wakes
//   (metadata.kind == "cascade") are limited to MaxCascadeWakePerCheck per
//   call. Notification and cron wakes are unaffected. Uncapped cascade
//   wakes remain pending for the next call.
func (dm *DatabaseManager) CheckPendingWakes(now time.Time, kinds []string) ([]map[string]interface{}, error) {
	nowUnix := now.Unix()
	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Build the kind-filter clause. Three branches:
	//   "*" anywhere  -> no filter (surface everything)
	//   specific list -> IN clause on json_extract(metadata, '$.kind')
	//   nil / empty   -> backward-compat default (notification only)
	whereExtra := ""
	args := []interface{}{nowUnix}

	includeAll := false
	for _, k := range kinds {
		if k == "*" {
			includeAll = true
			break
		}
	}

	switch {
	case includeAll:
		// Surface every pending wake regardless of metadata.kind.
	case len(kinds) > 0:
		placeholders := make([]string, 0, len(kinds))
		for _, k := range kinds {
			placeholders = append(placeholders, "?")
			args = append(args, k)
		}
		whereExtra = ` AND json_extract(metadata, '$.kind') IN (` + strings.Join(placeholders, ",") + `)`
	default:
		// Backward-compat default: only notification-kind wakes.
		whereExtra = ` AND (
		    metadata IS NULL OR metadata = ''
		    OR json_extract(metadata, '$.kind') IS NULL
		    OR json_extract(metadata, '$.kind') = 'notification'
		  )`
	}

	rows, err := tx.Query(
		`SELECT id, target_time, reason, theory_id, recurring_rule, created_by, metadata, created_at
		 FROM scheduled_wakes
		 WHERE fired = 0 AND target_time <= ?`+whereExtra+`
		 ORDER BY target_time ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("select pending wakes: %w", err)
	}
	defer rows.Close()
	type pending struct {
		id            string
		targetTime    int64
		reason        string
		theoryID      *string
		recurringRule *string
		createdBy     string
		metadata      *string
		createdAt     string
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.targetTime, &p.reason, &p.theoryID, &p.recurringRule, &p.createdBy, &p.metadata, &p.createdAt); err != nil {
			return nil, fmt.Errorf("scan wake row: %w", err)
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate wake rows: %w", err)
	}
	rows.Close()

	// Separate cascade from non-cascade rows so we can apply the per-check
	// cap only to cascade wakes. This preserves full delivery of notification
	// and cron wakes regardless of how many cascade wakes are queued.
	var cascadeBatch, nonCascadeBatch []pending
	for _, p := range batch {
		if p.metadata != nil && strings.Contains(*p.metadata, `"kind":"cascade"`) {
			cascadeBatch = append(cascadeBatch, p)
		} else {
			nonCascadeBatch = append(nonCascadeBatch, p)
		}
	}

	// Apply cascade cap. Rows beyond the cap are left in the DB (not marked
	// fired) so a subsequent call will deliver them.
	if len(cascadeBatch) > MaxCascadeWakePerCheck {
		cascadeBatch = cascadeBatch[:MaxCascadeWakePerCheck]
	}

	// Interleave: non-cascade first (chronological), then capped cascade.
	// ORDER BY target_time ASC over the full batch already placed them in
	// chronological order; splitting and re-combining preserves that order.
	out := make([]map[string]interface{}, 0, len(nonCascadeBatch)+len(cascadeBatch))
	for _, p := range nonCascadeBatch {
		out = append(out, dm.wakeRowToMap(p, nowUnix))
	}
	for _, p := range cascadeBatch {
		out = append(out, dm.wakeRowToMap(p, nowUnix))
	}

	// Mark all selected rows fired in a single batch.
	for _, p := range append(nonCascadeBatch, cascadeBatch...) {
		_, err := tx.Exec(
			`UPDATE scheduled_wakes SET fired = 1, fired_at = ? WHERE id = ? AND fired = 0`,
			nowUnix, p.id,
		)
		if err != nil {
			return nil, fmt.Errorf("mark wake fired: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit wake tx: %w", err)
	}
	return out, nil
}

// wakeRowToMap converts a pending wake row to the map format returned by
// CheckPendingWakes, without any database side effects.
func (dm *DatabaseManager) wakeRowToMap(p struct {
	id            string
	targetTime    int64
	reason        string
	theoryID      *string
	recurringRule *string
	createdBy     string
	metadata      *string
	createdAt     string
}, nowUnix int64) map[string]interface{} {
	row := map[string]interface{}{
		"id":             p.id,
		"target_time":     p.targetTime,
		"reason":         p.reason,
		"theory_id":      nullableString(p.theoryID),
		"recurring_rule": nullableString(p.recurringRule),
		"created_by":     p.createdBy,
		"created_at":     p.createdAt,
		"fired_at":       nowUnix,
		"overdue_secs":   nowUnix - p.targetTime,
	}
	if p.metadata != nil && *p.metadata != "" {
		var meta map[string]interface{}
		if json.Unmarshal([]byte(*p.metadata), &meta) == nil {
			row["metadata"] = meta
		}
	}
	return row
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
	q := `SELECT id, target_time, reason, theory_id, recurring_rule, fired, fired_at, created_by, metadata, created_at
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
			theoryID, recurringRule          *string
			fired                            int
			firedAt                          *int64
			metadata                         *string
		)
		if err := rows.Scan(&id, &targetTime, &reason, &theoryID, &recurringRule, &fired, &firedAt, &createdBy, &metadata, &createdAt); err != nil {
			return nil, fmt.Errorf("scan wake list row: %w", err)
		}
		row := map[string]interface{}{
			"id":              id,
			"target_time":     targetTime,
			"target_iso":      time.Unix(targetTime, 0).UTC().Format(time.RFC3339),
			"reason":          reason,
			"theory_id":       nullableString(theoryID),
			"recurring_rule":  nullableString(recurringRule),
			"fired":           fired == 1,
			"fired_at":        nullableInt64(firedAt),
			"created_by":      createdBy,
			"created_at":      createdAt,
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
				"reason":        reason,
				"target_iso":    time.Unix(targetTime, 0).UTC().Format(time.RFC3339),
				"overdue_secs":  overdueSecs,
				"theory_id":     nullableString(theoryID),
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
		"success":            true,
		"total_overdue":      total,
		"total_pending_future": pendingFuture,
		"linked_to_theory":   linkedToTheory,
		"age_buckets": map[string]int{
			"under_1h":   bucketUnder1h,
			"1h_to_1d":   bucket1hTo1d,
			"1d_to_1w":   bucket1dTo1w,
			"1w_to_1mo":  bucket1wTo1mo,
			"over_1mo":   bucketOver1mo,
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
			"type":                 "stale_foundation",
			"theory_id":            theoryID,
			"missing_artifact_id":  deletedArtifactID,
			"detected_at":          time.Now().UTC().Format(time.RFC3339Nano),
		}
		reason := fmt.Sprintf("Stale foundation: theory %s depends on missing artifact %s",
			theoryID, deletedArtifactID)
		if _, err := dm.ScheduleWake(reason, targetTime, theoryID, "", "mpm-reconcile", meta); err != nil {
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
		return n, nil
	}
	if d, err := parseDuration(s); err == nil {
		return now.Add(d).Unix(), nil
	}
	// ISO 8601 absolute timestamp — same contract every other MPM time
	// field honors (snooze_until, since). RFC3339Nano covers the "Z"
	// suffix, RFC3339 covers explicit offsets, and the space form covers
	// SQLite-style datetimes. This is the fallback that makes the wake
	// scheduler consistent with parseClusterSnoozeUntil.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
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
// Each wake entry includes: id (truncated), reason (up to 80 chars), and
// overdue_secs so the agent can autonomously triage (alert immediately,
// archive as stale, etc.).
func FormatWakeNotification(wakes []map[string]interface{}) string {
	if len(wakes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n<system_wake_notification>\n")
	b.WriteString("Scheduled tasks now due:")
	b.WriteString("\n")
	for _, w := range wakes {
		id := maxRunes(w["id"].(string), 8)
		reason := ""
		if r, ok := w["reason"].(string); ok {
			reason = maxRunes(r, 80)
		}
		overdueSecs := int64(0)
		if o, ok := w["overdue_secs"].(int64); ok {
			overdueSecs = o
		} else if o, ok := w["overdue_secs"].(float64); ok {
			overdueSecs = int64(o)
		}
		b.WriteString(fmt.Sprintf("  • id=%s | reason=%q | overdue_secs=%d\n", id, reason, overdueSecs))
	}
	b.WriteString("</system_wake_notification>\n")
	return b.String()
}

// maxBytes truncates s to max rune count, appending U+2026 if trimmed.
func maxRunes(s string, max int) string {
	// runes: count in actual characters, not bytes
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}