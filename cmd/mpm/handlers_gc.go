package main

import (
	"encoding/json"
	"fmt"
	"github.com/flowbyte-com/mpm-core/usererror"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ============================================================================
// Handler: gc (garbage collection / memory decay)
// ============================================================================

// gcOptions declares every option `mpm gc` accepts.
//
// The command read its arguments by SCANNING for known spellings, so an
// unrecognised option was silently ignored and the sweep RAN: `mpm gc
// --anything` performed the decay sweep over the operator's memories.
// The declaration below lets the shared helper reject it.
//
// The set is the scanner's set, exactly: --dry-run, --aggressive,
// --review, --purge, --shred-negative and --max-age=N, plus the --json
// output flag the command already honoured.
func gcOptions() []OptionSpec {
	return []OptionSpec{
		HelpOption,
		{Name: "json", Aliases: []string{"j"}, Usage: "Output JSON for tool integration", Kind: OptBool},
		{Name: "dry-run", Usage: "Report what would change, without changing it", Kind: OptBool},
		{Name: "aggressive", Usage: "Also decay long-term memories", Kind: OptBool},
		{Name: "review", Usage: "Soft-delete the dead and zombie memories", Kind: OptBool},
		{Name: "purge", Usage: "Hard-delete the reviewed memories", Kind: OptBool},
		{Name: "shred-negative", Usage: "Hard-delete negative-weight memories", Kind: OptBool},
		{Name: "max-age", Usage: "The age, in hours, above which a memory is dead", Kind: OptInt, ValueName: "N", ValueForm: "=N", DefaultText: "default 24"},
	}
}

func handleGC(args []string) int {
	// An unrecognised option is rejected BEFORE the sweep runs. The
	// command used to ignore it and decay the operator's memories.
	if rc := RejectUnknownOptionInArgs("gc", "mpm gc [options]", gcOptions(), args); rc != 0 {
		return rc
	}

	jsonOutput, _ := ExtractJSONFlag(args)
	dryRun := false
	aggressive := false
	review := false
	purge := false
	maxAgeHours := 24

	for _, arg := range args {
		if arg == "--dry-run" {
			dryRun = true
		}
		if arg == "--aggressive" {
			aggressive = true
		}
		if arg == "--review" {
			review = true
		}
		if arg == "--purge" {
			purge = true
		}
		if arg == "--shred-negative" {
			dryRun = false // explicit override to allow actual shredding
		}
		if strings.HasPrefix(arg, "--max-age=") {
			fmt.Sscanf(arg, "--max-age=%d", &maxAgeHours)
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// --shred-negative: hard-delete negative-weight memories that have a proven theory
	if shredNegative := func() bool {
		for _, arg := range args {
			if arg == "--shred-negative" {
				return true
			}
		}
		return false
	}(); shredNegative {
		negMemories, err := dm.GetNegativeWeightMemories()
		if err != nil {
			// Fail closed. The previous shape printed the error and then
			// fell through to "No negative-weight memories found." with
			// exit 0 — a failed READ reported as a successful empty scan.
			usererror.Error("gc --shred-negative: could not read negative-weight memories: %v", err)
			return 1
		}
		if len(negMemories) == 0 {
			fmt.Println("No negative-weight memories found.")
			return 0
		}
		fmt.Printf("Found %d negative-weight memories:\n\n", len(negMemories))
		var shredded, shredFailures int
		for _, m := range negMemories {
			id, _ := m["id"].(string)
			weight, _ := m["weight"].(int)
			theory, err := dm.GetProvenTheoryForMemory(id)
			if err != nil {
				usererror.Warn("error checking theory: %s: %v", id, err)
				shredFailures++
				continue
			}
			if theory != nil {
				if dryRun {
					fmt.Printf("  [%s] weight=%d → WOULD SHRED (proven theory %s)\n", id, weight, theory["id"])
				} else {
					if err := dm.ShredMemory(id); err != nil {
						usererror.Warn("shred error: %s: %v", id, err)
						shredFailures++
						continue
					}
					fmt.Printf("  [%s] weight=%d → SHREDDED (proven theory %s)\n", id, weight, theory["id"])
					shredded++
				}
			} else {
				fmt.Printf("  [%s] weight=%d → SKIP (no proven theory)\n", id, weight)
			}
		}
		if !dryRun && shredded > 0 {
			fmt.Printf("\nShredded %d memories with proven theories.\n", shredded)
		}
		if shredFailures > 0 {
			// A partially-failed destructive sweep must not read as a
			// clean one.
			usererror.Error("gc --shred-negative: %d memory/memories could NOT be shredded; the store is NOT fully swept", shredFailures)
			return 1
		}
		return 0
	}

	now := time.Now()
	// Key name MUST be "updated_at" — the cooldown SQL reads $.updated_at
	// (handlers.go:960) and the "Skipped: last gc was X" branch (line 968)
	// reads lastGC["updated_at"]. A previous version used "timestamp" here,
	// which made the cooldown check permanently see NULL (json_extract on a
	// missing key returns NULL → the < comparison short-circuits → every
	// GC run reported rowsAffected=0 and printed "Skipped"). Fixed 2026-06-26.
	gcTimestampJSON, _ := json.Marshal(map[string]string{"updated_at": now.Format(time.RFC3339)})

	// Dry runs are read-only — they don't claim the cooldown. Otherwise a
	// user inspecting the dead-memory preview would block their next real
	// GC for 24h. The cooldown is only meaningful for runs that mutate
	// state (decay writes, purge, review soft-deletes).
	if !dryRun {
		// Atomic frequency cap: claim the GC slot by lazy-initialising the
		// last_gc_at row, then doing a compare-and-swap against its timestamp.
		// The previous plain UPDATE returned 0 rowsAffected on a fresh DB (no
		// row existed) which the engine then misread as "cooldown active",
		// silently aborting the maintenance loop. The upsert pattern fixes this:
		//   - row missing                  → INSERT happens           → rowsAffected=1 (claim)
		//   - row exists, no updated_at    → UPDATE fires (no timestamp to gate on) → rowsAffected=1 (claim)
		//   - row exists, old timestamp    → ON CONFLICT UPDATE fires  → rowsAffected=1 (claim)
		//   - row exists, hot timestamp    → ON CONFLICT UPDATE no-ops → rowsAffected=0 (skip)
		//   - operator deleted             → next run self-heals      → rowsAffected=1 (claim)
		// The empty-JSON case ('{}') used to silently no-op because json_extract
		// on a missing key returns NULL, and `NULL < <anything>` is NULL (falsy),
		// short-circuiting the WHERE clause. Adding the explicit IS NULL clause
		// treats "no timestamp" as "claim it" — same semantic as a fresh row.
		// The cooldown check is inside the DO UPDATE WHERE clause so the
		// atomicity of the compare-and-swap is preserved across concurrent
		// GC invocations.
		result, err := dm.SQLDB().Exec(`
			INSERT INTO system_config (key, raw_json, content_hash)
			VALUES ('last_gc_at', ?, '')
			ON CONFLICT(key) DO UPDATE SET
			  raw_json = excluded.raw_json,
			  updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE (
			  system_config.raw_json IS NULL
			  OR json_extract(system_config.raw_json, '$.updated_at') IS NULL
			  OR datetime(json_extract(system_config.raw_json, '$.updated_at')) < datetime('now', '-' || ? || ' hours')
			)
		`, string(gcTimestampJSON), strconv.Itoa(maxAgeHours))
		if err != nil {
			usererror.Error("gc: cooldown claim: %v", err)
			return 1
		}
		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			// last_gc_at was updated by another process while we were working — skip this run.
			// Re-read and report the actual last GC time.
			lastGC, _ := dm.GetSystemConfig("last_gc_at")
			if updatedAt, ok := lastGC["updated_at"].(string); ok {
				if last, parseErr := time.Parse(time.RFC3339, updatedAt); parseErr == nil {
					fmt.Printf("Skipped: last gc was %s\n", last.Format("2006-01-02 15:04"))
					return 0
				}
			}
			fmt.Println("Skipped: recent GC detected")
			return 0
		}
		// Atomic update succeeded — we have the lock. Proceed with GC.
		// Note: all subsequent work happens AFTER this atomic check-and-set.
	}

	// Purge mode: hard delete old reviewed memories and exit.
	//
	// deleted_at is stored as INTEGER Unix epoch (matches expires_at).
	// Comparison is integer-vs-integer via strftime('%s','now', ...).
	// ROLLBACK WARNING: SQLite compares INTEGER < TEXT as TRUE regardless
	// of value. If you revert the Go binary that writes deleted_at as
	// integer, FIRST run the reverse migration or every soft-deleted row
	// in the database will be shredded by this sweep:
	//
	//   UPDATE memories
	//   SET deleted_at = datetime(deleted_at, 'unixepoch')
	//   WHERE typeof(deleted_at) = 'integer';
	if purge && !dryRun {
		result, err := dm.SQLDB().Exec(`
			DELETE FROM memories
			WHERE deleted_at IS NOT NULL
			AND deleted_at < strftime('%s','now', '-30 days')
			AND ` + mpminternal.DirectiveExclusionClause + `
		`)
		if err != nil {
			// Fail closed. The previous shape logged the error, then
			// called RowsAffected on the nil Result (panic) and, if that
			// had not panicked, printed "Purged 0" and exited 0 — a
			// failed destructive sweep reported as a successful no-op.
			usererror.Error("gc --purge: delete failed, nothing was purged: %v", err)
			return 1
		}
		purged, err := result.RowsAffected()
		if err != nil {
			usererror.Error("gc --purge: cannot confirm the purge row count: %v", err)
			return 1
		}
		fmt.Printf("Purged %d old deleted memories\n", purged)
		return 0
	}
	if purge && dryRun {
		// Surface the purge intent without writing.
		fmt.Println("Dry run: --purge would hard-delete soft-deleted memories older than 30 days.")
	}

	// A failed maintenance sweep must be visible in the RUN, not only in a
	// structured log line: `mpm ops gc` exiting 0 while a requested sweep
	// did nothing reads as "the store is clean" when it is not. The sweep
	// is not fatal to the rest of the pass (decay and review still run),
	// but it is counted and the command exits non-zero.
	maintenanceFailures := 0
	pruneFailed := func(what string, err error) {
		maintenanceFailures++
		slog.Warn(what+" prune failed", "error", err.Error())
		usererror.Error("gc: %s prune failed: %v", what, err)
	}

	// Audit log retention sweep — drop entries older than 30 days. Wired
	// into the GC cycle rather than a separate cron because GC is the
	// canonical cleanup pass and we want one place to tune retention.
	if !dryRun {
		if pruned, err := dm.PruneAuditLog(30); err != nil {
			pruneFailed("audit log", err)
		} else if pruned > 0 {
			fmt.Printf("Pruned %d audit log entries older than 30 days\n", pruned)
		}

		// Session handoffs retention sweep — 90 days. Handoffs are
		// higher-signal, lower-volume than audit log, so they get a longer
		// retention window. The next session may need to look back more than
		// 30 days to understand a long-running project.
		if pruned, err := dm.PruneHandoffs(90); err != nil {
			pruneFailed("session handoffs", err)
		} else if pruned > 0 {
			fmt.Printf("Pruned %d session handoffs older than 90 days\n", pruned)
		}

		// Cascade outbox retention sweep — 30 days. Terminal intents
		// (materialized/failed) accumulate monotonically with every
		// shred event; without a sweep the table grows forever.
		if pruned, err := dm.PruneCascadeOutbox(30); err != nil {
			pruneFailed("cascade outbox", err)
		} else if pruned > 0 {
			fmt.Printf("Pruned %d terminal cascade intents older than 30 days\n", pruned)
		}
	}

	// Capture monotonic clock offset once at start of GC run to prevent clock-rollback exploits.
	// Using a captured "now" ensures all time calculations within this GC pass use the same
	// reference point, even if the system clock goes backward mid-run.
	monotonicNow := time.Now()
	var deadMemories []map[string]interface{}
	var updated, scanned int

	// F-D1: accrue runtime to every live memory BEFORE reading rows for
	// decay calculation. The accrual is bounded by min(wall_delta, process
	// uptime) — wall-clock downtime cannot inflate runtime counters.
	// Rows with NULL runtime_last_accrued_at are seeded with the current
	// wall-clock and zero runtime; they don't get a free decay hit on the
	// first gc tick.
	if !dryRun {
		if _, err := dm.SQLDB().Exec(`
			UPDATE memories
			SET runtime_seconds_since_access = CASE
				WHEN runtime_last_accrued_at IS NULL OR runtime_last_accrued_at <= 0 THEN 0
				ELSE runtime_seconds_since_access + MIN(
					CAST(strftime('%s','now') AS INTEGER) - runtime_last_accrued_at,
					CAST(? AS INTEGER)
				)
			END,
			runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE deleted_at IS NULL
		`, int64(monotonicNow.Sub(mpminternal.GCProcessStart()).Seconds())); err != nil {
			// The runtime accrual is the INPUT to the whole decay
			// computation. If it fails, the weights computed below are
			// derived from stale accumulators — a maintenance sweep that
			// did not do what it was asked. Count it like any other
			// failed sweep rather than continuing silently.
			pruneFailed("runtime accrual", err)
		}
	}

	// Get all non-deleted memories
	rows, err := dm.SQLDB().Query(`
		SELECT id, collection, weight, runtime_seconds_since_access, created_at, is_long_term
		FROM memories WHERE deleted_at IS NULL
	`)
	if err != nil {
		// Fail closed. The previous shape printed the error and fell
		// through to `defer rows.Close()` on a nil *sql.Rows — a nil
		// pointer dereference, i.e. a panic in the middle of the
		// maintenance pass.
		usererror.Error("gc: could not read the memory set: %v", err)
		return 1
	}
	defer rows.Close()

	// Collect all computed weight changes for batch application (avoids N+1 SQL pattern).
	// Structure: []struct{ id string, oldWeight int, newWeight float64, isLTM bool }
	type weightDelta struct {
		id        string
		oldWeight int
		newWeight float64
		isLTM     bool
	}
	var deltas []weightDelta

	for rows.Next() {
		scanned++
		var id, collection string
		var weight, runtimeSec float64
		var runtimeSecNull *float64
		var createdAt *int64
		var isLongTerm bool

		if err := rows.Scan(&id, &collection, &weight, &runtimeSecNull, &createdAt, &isLongTerm); err != nil {
			usererror.Warn("handleGC: scan failed for memory row, skipping: %v", err)
			continue
		}
		if runtimeSecNull != nil {
			runtimeSec = *runtimeSecNull
		}

		// Zero-decay collections (append-only audit trails like
		// decisions) are exempt from the forgetting curve — mirrors the
		// RunGC exemption so the CLI and the tool agree.
		if policy, ok := mpminternal.DefaultDecayPolicies[collection]; ok && policy.DecayPercent <= 0 {
			continue
		}

		// F-D1: decay is now based on accumulated runtime-since-access,
		// not wall-clock days. Runtime accrual already happened above; we
		// just consume the resulting counter here.
		var createdAtTime *time.Time
		if createdAt != nil {
			t := time.Unix(*createdAt, 0)
			createdAtTime = &t
		}
		decay := computeDecay(weight, runtimeSec, isLongTerm, createdAtTime, aggressive, monotonicNow)
		newWeight := weight - decay

		// Floor
		if newWeight < -10.0 {
			newWeight = -10.0
		}
		// LTM protection: preserve memories that are explicitly marked LTM OR have weight >= 10.
		// Applying the floor BEFORE dead detection ensures LTM memories are never flagged for deletion.
		if (mpminternal.IsLTMMemory(isLongTerm, int(weight))) && newWeight < 1.0 {
			newWeight = 1.0
		}
		// Record delta for batch update
		deltas = append(deltas, weightDelta{id: id, oldWeight: int(weight), newWeight: newWeight, isLTM: mpminternal.IsLTMMemory(isLongTerm, int(weight))})

		// Dead if <= 0 (post-clamp) and not LTM — LTM memories are never eligible for deletion.
		// Also catches already-dead memories (weight already <= 0 from a previous GC)
		// that were never soft-deleted — without the oldWeight > 0 guard they'd be missed.
		isLTM := deltas[len(deltas)-1].isLTM
		if newWeight <= 0.0 && !isLTM {
			var deadContent string
			if err := dm.SQLDB().QueryRow(`SELECT SUBSTR(content, 1, 60) FROM memories WHERE id = ?`, id).Scan(&deadContent); err != nil {
				usererror.Warn("handleGC: failed to fetch dead memory content, defaulting to empty: %v", err)
			}
			deadMemories = append(deadMemories, map[string]interface{}{
				"id":      id,
				"content": deadContent,
				"weight":  int(weight),
				"decay":   decay,
			})
		}
	}

	// Batch-apply all weight updates to avoid N+1 SQL pattern.
	// Uses math.Floor for consistent rounding (not int() truncation which rounds toward zero).
	if !dryRun && len(deltas) > 0 {
		tx, txErr := dm.SQLDB().Begin()
		if txErr != nil {
			usererror.Error("failed to begin transaction: %v", txErr)
		} else {
			defer tx.Rollback() // safe no-op after Commit; covers panic / early returns
			var batchErr error
			for _, d := range deltas {
				if d.newWeight == float64(d.oldWeight) {
					continue
				}
				rounded := int(math.Round(d.newWeight))
				if d.isLTM {
					if rounded < 1 {
						rounded = 1
					}
				} else {
					if rounded < -10 {
						rounded = -10
					}
				}
				if _, err := tx.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, rounded, d.id); err != nil {
					batchErr = err
					break
				}
				updated++
			}
			if batchErr != nil {
				tx.Rollback()
				// The weight-decay batch IS the maintenance pass. A
				// rolled-back batch means no weight changed at all, so it
				// must be counted like any other failed sweep — the
				// previous shape printed the error and let the command
				// exit 0.
				pruneFailed("weight decay batch update", batchErr)
			} else if err := tx.Commit(); err != nil {
				tx.Rollback()
				pruneFailed("weight decay batch commit", err)
			}
		}
	}

	// --review mode: soft-delete all dead and zombie memories (LTM already excluded above).
	// Also catches existing zombies (weight <= 0 from previous GC runs that were never cleaned).
	if review && !dryRun {
		softDeleted, softDeleteFailures := 0, 0
		for _, m := range deadMemories {
			id, _ := m["id"].(string)
			res, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ? AND deleted_at IS NULL`, id)
			if err != nil {
				softDeleteFailures++
				slog.Warn("gc soft-delete failed", "id", id, "error", err)
				usererror.Error("gc --review: soft-delete %s failed: %v", id, err)
				continue
			}
			// Count what actually changed, not what was attempted. A
			// row that was already tombstoned (deleted concurrently)
			// matches no row under the `deleted_at IS NULL` guard and is
			// neither a soft-delete nor a failure.
			n, err := res.RowsAffected()
			if err != nil {
				softDeleteFailures++
				slog.Warn("gc soft-delete row count unavailable", "id", id, "error", err)
				usererror.Error("gc --review: cannot confirm the soft-delete of %s: %v", id, err)
				continue
			}
			if n > 0 {
				softDeleted++
			}
		}
		// Clean any lingering zombies not caught by this pass
		if _, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE weight <= 0 AND is_long_term = 0 AND deleted_at IS NULL`); err != nil {
			softDeleteFailures++
			slog.Warn("gc zombie cleanup failed", "error", err)
			usererror.Error("gc --review: zombie cleanup failed: %v", err)
		}
		// Report what actually happened. The previous shape printed
		// len(deadMemories) — the number ATTEMPTED — so a failed update
		// was reported as a successful soft delete.
		if softDeleted > 0 {
			fmt.Printf("Soft-deleted %d dead memories\n", softDeleted)
		}
		if softDeleteFailures > 0 {
			fmt.Printf("%d dead memories could NOT be soft-deleted\n", softDeleteFailures)
			maintenanceFailures += softDeleteFailures
		}
	}

	// Update last_gc_at timestamp
	if !dryRun {
		dm.SaveSystemConfig("last_gc_at", string(gcTimestampJSON), "", "")
	}

	// Output
	if jsonOutput {
		type gcResult struct {
			Scanned             int                      `json:"scanned"`
			Updated             int                      `json:"updated"`
			DeadCount           int                      `json:"dead_count"`
			DeadMemories        []map[string]interface{} `json:"dead_memories,omitempty"`
			DryRun              bool                     `json:"dry_run"`
			MaintenanceFailures int                      `json:"maintenance_failures"`
		}
		result := gcResult{
			Scanned:             scanned,
			Updated:             updated,
			DeadCount:           len(deadMemories),
			DeadMemories:        deadMemories,
			DryRun:              dryRun,
			MaintenanceFailures: maintenanceFailures,
		}
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
	} else {
		fmt.Printf("Scanned: %d | Updated: %d | Dead: %d\n", scanned, updated, len(deadMemories))
		if len(deadMemories) > 0 {
			fmt.Println("\nDead memories (weight <= 0):")
			for _, m := range deadMemories {
				content := m["content"].(string)
				if len(content) > 60 {
					content = content[:60] + "…"
				}
				fmt.Printf("  [%s] %s\n", m["id"].(string)[:8], content)
			}
		}
	}
	if maintenanceFailures > 0 {
		usererror.Error("gc: %d maintenance operation(s) failed; the store is NOT fully swept (see messages above)", maintenanceFailures)
		return 1
	}
	return 0
}

// computeDecay returns the weight decay amount for a memory.
// All math is float64; only the final value is truncated on DB write.
// Uses a pre-captured "now" timestamp to prevent clock-rollback exploits.
//
// F-D1: `runtimeSecondsSinceAccess` replaces wall-clock `daysSinceAccess`.
// Decay advances only on accumulated scheduler/CLI runtime, never on
// calendar downtime. Wall-clock vacay cannot decay memory; active
// runtime still does. The accrual step happens before this function is
// called so the counter already reflects bounded runtime since last
// access.
func computeDecay(weight float64, runtimeSecondsSinceAccess float64, isLongTerm bool, createdAt *time.Time, aggressive bool, now time.Time) float64 {
	multiplier := 1.0
	if aggressive {
		multiplier = 2.0
	}
	runtimeDays := runtimeSecondsSinceAccess / 86400.0

	if isLongTerm {
		return runtimeDays * 0.01 * multiplier
	}
	if weight >= 10 {
		return runtimeDays * 0.02 * multiplier
	}
	if weight >= 5 {
		return runtimeDays * 0.05 * multiplier
	}

	// Low-weight: decay scales with age (newer = faster decay)
	ageFactor := 1.0
	if createdAt != nil {
		// Use the captured monotonic reference: runtime-days since
		// creation. Note that createdAt is set to wall-clock insertion
		// time, so this is effectively "process uptime + wall-clock from
		// creation to first boot" — bounded enough for age-factor purposes.
		runtimeDaysSinceCreated := now.Sub(*createdAt).Seconds() / 86400.0
		if runtimeDaysSinceCreated > 30.0 {
			ageFactor = 1.0
		} else {
			ageFactor = runtimeDaysSinceCreated / 30.0
		}
	}
	baseDecay := 0.1 + 0.2*ageFactor
	return runtimeDays * baseDecay * multiplier
}

// handleRestore recovers a soft-deleted memory by clearing deleted_at and preserving
