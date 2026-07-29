// Package core — Agentic Cron (scheduled_tasks).
//
// Recurring agentic workflows are first-class citizens in MPM. The
// mpm-scheduler daemon's 60-second tick loop polls the
// scheduled_tasks table, injects a standard scheduled_wakes row for
// each due task, and rolls over next_run_at to the next cron
// occurrence. The agent remains stateless: it wakes, reads the
// directive named in metadata.directive_id, executes, and returns.
//
// All three operations (read due, inject wake, rollover timestamp)
// wrap in a single SQLite transaction so a daemon crash between
// wake-injection and rollover cannot double-fire.
//
// Three subtle invariants the schema enforces:
//
//   - next_run_at is pre-computed at upsert time, never on the daemon
//     hot path. The daemon's polling query reduces to a single
//     indexed lookup against (status, next_run_at).
//
//   - last_run_at captures the injection timestamp, not the wake-
//     processing time. "Did the agent actually finish?" is the
//     agent's own audit concern.
//
//   - Re-upserting an existing task recalculates next_run_at from
//     "now". A task paused for six months then unpaused does NOT
//     backfill — it waits for the next cron occurrence from the
//     unpause moment.
package internal

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// ScheduledTask represents a recurring agentic workflow managed by the daemon.
//
// Status field is constrained to ScheduledTaskActive | ScheduledTaskPaused
// (CHECK constraint at the schema level). LastRunAt is a pointer because
// it is NULL before the task's first execution.
type ScheduledTask struct {
	ID          string     `json:"id" db:"id"`
	Name        string     `json:"name" db:"name"`
	CronExpr    string     `json:"cron_expr" db:"cron_expr"`
	DirectiveID string     `json:"directive_id" db:"directive_id"`
	Status      string     `json:"status" db:"status"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty" db:"last_run_at"`
	NextRunAt   time.Time  `json:"next_run_at" db:"next_run_at"`
	CreatedAt   time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at" db:"updated_at"`
}

// Status enums for scheduled_tasks. Mirrored in the CHECK constraint
// at the schema level so the substrate rejects garbage at the boundary.
const (
	ScheduledTaskActive = "active"
	ScheduledTaskPaused = "paused"
)

// CronWakePrefix is the reason prefix used when the daemon injects a
// scheduled_wakes row from a scheduled_task. The agent's wake handler
// can detect cron-injected wakes by reason startswith this prefix.
const CronWakePrefix = "cron:"

// CronSource is the metadata.source value set on cron-injected wakes.
// Lets the agent distinguish cron fires from user/agent-scheduled wakes.
const CronSource = "cron"

// CronWakeKind is the metadata.kind value set on cron-injected wakes.
// Aligns with the check_wakes kinds taxonomy so `kinds: ["cron"]` matches
// as expected. Distinct from CronSource by intent (kind = taxonomy tag
// for the MCP fold; source = daemon provenance marker) even though both
// carry the literal value "cron".
const CronWakeKind = "cron"

// CronCreatedBy is the created_by value set on cron-injected wakes.
// The daemon has no agent identity, so it claims itself.
const CronCreatedBy = "mpm-scheduler"

// CalculateNextRun parses a 5-field cron expression and returns the next
// occurrence after `from`, in UTC.
//
// The parser is configured for standard 5-field syntax (minute hour dom
// month dow). Operators in non-UTC timezones write cron in their
// wall-clock understanding; the daemon compares UTC instants
// regardless. Two equivalent cron expressions evaluated from the same
// instant produce the same next_run_at, modulo the standard timezone
// subtlety — this is documented as a known limitation, not a bug.
func CalculateNextRun(expr string, from time.Time) (time.Time, error) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", expr, err)
	}
	return schedule.Next(from).UTC(), nil
}

// UpsertScheduledTask creates or updates a recurring task.
//
// On update (id collision), the existing row's last_run_at and created_at
// are preserved — only name, cron_expr, directive_id, status,
// next_run_at, updated_at change. This is the documented "re-upsert
// recalculates the schedule from now" semantic: a task paused for
// six months then re-upserted with status=active will fire at the
// next cron occurrence from this call's instant, not backfill
// missed fires.
func (dm *DatabaseManager) UpsertScheduledTask(task ScheduledTask) error {
	if task.ID == "" {
		return fmt.Errorf("id is required")
	}
	if task.Name == "" {
		return fmt.Errorf("name is required")
	}
	if task.CronExpr == "" {
		return fmt.Errorf("cron_expr is required")
	}
	if task.DirectiveID == "" {
		return fmt.Errorf("directive_id is required")
	}
	if task.Status != ScheduledTaskActive && task.Status != ScheduledTaskPaused {
		return fmt.Errorf("invalid status %q: must be %q or %q",
			task.Status, ScheduledTaskActive, ScheduledTaskPaused)
	}

	now := time.Now().UTC()
	nextRun, err := CalculateNextRun(task.CronExpr, now)
	if err != nil {
		return err
	}

	query := `
		INSERT INTO scheduled_tasks
		(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name         = excluded.name,
			cron_expr    = excluded.cron_expr,
			directive_id = excluded.directive_id,
			status       = excluded.status,
			next_run_at  = excluded.next_run_at,
			updated_at   = excluded.updated_at
	`
	_, err = dm.db.Exec(query,
		task.ID, task.Name, task.CronExpr, task.DirectiveID, task.Status,
		nextRun, now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert scheduled_task %q: %w", task.ID, err)
	}
	return nil
}

// ProcessDueTasks runs in the daemon's ticker loop. It finds active tasks
// whose next_run_at has passed, injects a scheduled_wakes row for each,
// and rolls over the next_run_at timestamp. All three operations happen
// in a single SQLite transaction — a daemon crash between wake injection
// and next_run_at rollover cannot double-fire.
//
// Returns the count of tasks processed. Cron expressions that fail to
// parse at rollover time cause the offending task to be paused (not
// deleted) — better to halt than to spin a poison-pill task.
//
// This is the *DatabaseManager convenience wrapper. The scheduler daemon
// (which holds its own *sql.DB) calls ProcessScheduledTasks directly
// to avoid importing the entire DatabaseManager struct.
func (dm *DatabaseManager) ProcessDueTasks() (int, error) {
	return ProcessScheduledTasks(dm.db)
}

// ProcessScheduledTasks is the underlying polling engine, callable on any
// *sql.DB connection. The scheduler uses this to keep the daemon package
// free of DatabaseManager imports (which would create a cycle).
func ProcessScheduledTasks(db *sql.DB) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	// Safe to defer; no-op if Commit() succeeds.
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	rows, err := tx.Query(`
		SELECT id, cron_expr, directive_id
		FROM scheduled_tasks
		WHERE status = ? AND next_run_at <= ?
	`, ScheduledTaskActive, now)
	if err != nil {
		return 0, fmt.Errorf("fetch due tasks: %w", err)
	}

	type dueTask struct {
		id          string
		cronExpr    string
		directiveID string
	}
	var due []dueTask
	for rows.Next() {
		var t dueTask
		if err := rows.Scan(&t.id, &t.cronExpr, &t.directiveID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan due task: %w", err)
		}
		due = append(due, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate due tasks: %w", err)
	}
	rows.Close()

	processed := 0
	for _, t := range due {
		// Inject the standard wake. Use the substrate's newWakeID
		// (32-char hex with "wk-" prefix) so wake IDs are visually
		// distinguishable from memory / lesson IDs.
		wakeID, err := newWakeID()
		if err != nil {
			return 0, fmt.Errorf("generate wake id for task %s: %w", t.id, err)
		}
		reason := CronWakePrefix + t.id
		// JSON metadata so the agent can discover the directive_id at
		// wake-time without an extra lookup. Both "kind" and "source"
		// are set: "kind" aligns with the check_wakes kinds taxonomy
		// (so `kinds: ["cron"]` matches as expected), "source" is the
		// daemon's internal provenance marker. Using fmt.Sprintf with
		// hex-escaped strings is sufficient — directive IDs and task
		// IDs are constrained to safe-character sets.
		metadata := fmt.Sprintf(`{"kind":%q,"source":%q,"task_id":%q,"directive_id":%q}`,
			CronWakeKind, CronSource, t.id, t.directiveID)

		_, err = tx.Exec(`
			INSERT INTO scheduled_wakes
			(id, target_time, reason, fired, created_by, metadata)
			VALUES (?, ?, ?, 0, ?, ?)
		`, wakeID, now.Unix(), reason, CronCreatedBy, metadata)
		if err != nil {
			return 0, fmt.Errorf("inject wake for task %s: %w", t.id, err)
		}

		// Calculate the next occurrence AFTER now. If cron became
		// invalid mid-flight (e.g., operator typo corrected then
		// re-typo'd), pause the task to prevent a poison-pill loop.
		nextRun, err := CalculateNextRun(t.cronExpr, now)
		if err != nil {
			_, _ = tx.Exec(
				`UPDATE scheduled_tasks SET status = ?, updated_at = ? WHERE id = ?`,
				ScheduledTaskPaused, now, t.id,
			)
			continue
		}

		// Rollover. last_run_at is the injection timestamp (now);
		// next_run_at is the next cron occurrence.
		_, err = tx.Exec(`
			UPDATE scheduled_tasks
			SET last_run_at = ?, next_run_at = ?, updated_at = ?
			WHERE id = ?
		`, now, nextRun, now, t.id)
		if err != nil {
			return 0, fmt.Errorf("rollover task %s: %w", t.id, err)
		}

		processed++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return processed, nil
}

// ListScheduledTasks returns all scheduled tasks ordered by next_run_at.
// Used by the CLI `mpm tasks list` surface and by the eventual
// manage_scheduled_task tool's read path.
func (dm *DatabaseManager) ListScheduledTasks() ([]ScheduledTask, error) {
	rows, err := dm.db.Query(`
		SELECT id, name, cron_expr, directive_id, status,
		       last_run_at, next_run_at, created_at, updated_at
		FROM scheduled_tasks
		ORDER BY next_run_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list scheduled_tasks: %w", err)
	}
	defer rows.Close()

	var tasks []ScheduledTask
	for rows.Next() {
		var t ScheduledTask
		var lastRun *string // scan into nullable string, then parse
		if err := rows.Scan(
			&t.ID, &t.Name, &t.CronExpr, &t.DirectiveID, &t.Status,
			&lastRun, &t.NextRunAt, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan scheduled_task: %w", err)
		}
		if lastRun != nil && *lastRun != "" {
			parsed, err := time.Parse(time.RFC3339, *lastRun)
			if err == nil {
				t.LastRunAt = &parsed
			}
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scheduled_tasks: %w", err)
	}
	return tasks, nil
}

// DeleteScheduledTask hard-deletes a task by id. Use sparingly — most
// operators should set status='paused' instead so the schedule is
// preserved for forensics.
func (dm *DatabaseManager) DeleteScheduledTask(id string) error {
	if id == "" {
		return fmt.Errorf("id is required")
	}
	_, err := dm.db.Exec(`DELETE FROM scheduled_tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete scheduled_task %q: %w", id, err)
	}
	return nil
}