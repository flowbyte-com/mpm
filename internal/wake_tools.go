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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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
func (dm *DatabaseManager) CheckPendingWakes(now time.Time) ([]map[string]interface{}, error) {
	nowUnix := now.Unix()
	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT id, target_time, reason, theory_id, recurring_rule, created_by, metadata, created_at
		 FROM scheduled_wakes
		 WHERE fired = 0 AND target_time <= ?
		 ORDER BY target_time ASC`,
		nowUnix,
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
	out := make([]map[string]interface{}, 0, len(batch))
	for _, p := range batch {
		_, err := tx.Exec(
			`UPDATE scheduled_wakes SET fired = 1, fired_at = ? WHERE id = ? AND fired = 0`,
			nowUnix, p.id,
		)
		if err != nil {
			return nil, fmt.Errorf("mark wake fired: %w", err)
		}
		row := map[string]interface{}{
			"id":           p.id,
			"target_time":  p.targetTime,
			"reason":       p.reason,
			"theory_id":    nullableString(p.theoryID),
			"recurring_rule": nullableString(p.recurringRule),
			"created_by":   p.createdBy,
			"created_at":   p.createdAt,
			"fired_at":     nowUnix,
			"overdue_secs": nowUnix - p.targetTime,
		}
		if p.metadata != nil && *p.metadata != "" {
			var meta map[string]interface{}
			if json.Unmarshal([]byte(*p.metadata), &meta) == nil {
				row["metadata"] = meta
			}
		}
		out = append(out, row)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit wake tx: %w", err)
	}
	return out, nil
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
	d, err := parseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("target_time %q: %w (expected unix epoch or relative like '24h', '30m', '7d')", s, err)
	}
	return now.Add(d).Unix(), nil
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