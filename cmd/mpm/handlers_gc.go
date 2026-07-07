package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"github.com/flowbyte-com/mpm-core/usererror"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ============================================================================
// Handler: gc (garbage collection / memory decay)
// ============================================================================

func handleGC(args []string) int {
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

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		usererror.Error("%v", err)
	}
	defer dm.Close()

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
			usererror.Error("%v", err)
		}
		if len(negMemories) == 0 {
			fmt.Println("No negative-weight memories found.")
			return 0
		}
		fmt.Printf("Found %d negative-weight memories:\n\n", len(negMemories))
		var shredded int
		for _, m := range negMemories {
			id, _ := m["id"].(string)
			weight, _ := m["weight"].(int)
			theory, err := dm.GetProvenTheoryForMemory(id)
			if err != nil {
				usererror.Warn("error checking theory: %s: %v", id, err)
				continue
			}
			if theory != nil {
				if dryRun {
					fmt.Printf("  [%s] weight=%d → WOULD SHRED (proven theory %s)\n", id, weight, theory["id"])
				} else {
					if err := dm.ShredMemory(id); err != nil {
						usererror.Warn("shred error: %s: %v", id, err)
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
		  updated_at = CURRENT_TIMESTAMP
		WHERE (
		  system_config.raw_json IS NULL
		  OR json_extract(system_config.raw_json, '$.updated_at') IS NULL
		  OR datetime(json_extract(system_config.raw_json, '$.updated_at')) < datetime('now', '-' || ? || ' hours')
		)
	`, string(gcTimestampJSON), strconv.Itoa(maxAgeHours))
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

	// Purge mode: hard delete old reviewed memories and exit
	if purge {
		result, err := dm.SQLDB().Exec(`
			DELETE FROM memories
			WHERE deleted_at IS NOT NULL
			AND deleted_at < datetime('now', '-30 days')
		`)
		if err != nil {
			usererror.Error("%v", err)
		}
		purged, _ := result.RowsAffected()
		fmt.Printf("Purged %d old deleted memories\n", purged)
		return 0
	}

	// Audit log retention sweep — drop entries older than 30 days. Wired
	// into the GC cycle rather than a separate cron because GC is the
	// canonical cleanup pass and we want one place to tune retention.
	if pruned, err := dm.PruneAuditLog(30); err != nil {
		slog.Warn("audit prune failed", "error", err.Error(), "retention_days", 30)
	} else if pruned > 0 {
		fmt.Printf("Pruned %d audit log entries older than 30 days\n", pruned)
	}

	// Session handoffs retention sweep — 90 days. Handoffs are
	// higher-signal, lower-volume than audit log, so they get a longer
	// retention window. The next session may need to look back more than
	// 30 days to understand a long-running project.
	if pruned, err := dm.PruneHandoffs(90); err != nil {
		slog.Warn("handoff prune failed", "error", err.Error(), "retention_days", 90)
	} else if pruned > 0 {
		fmt.Printf("Pruned %d session handoffs older than 90 days\n", pruned)
	}

	// Get all non-deleted memories
	rows, err := dm.SQLDB().Query(`
		SELECT id, weight, last_accessed_at, created_at, is_long_term
		FROM memories WHERE deleted_at IS NULL
	`)
	if err != nil {
		usererror.Error("%v", err)
	}
	defer rows.Close()

	// Capture monotonic clock offset once at start of GC run to prevent clock-rollback exploits.
	// Using a captured "now" ensures all time calculations within this GC pass use the same
	// reference point, even if the system clock goes backward mid-run.
	monotonicNow := time.Now()
	var deadMemories []map[string]interface{}
	var updated, scanned int

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
		var id string
		var weight int
		var lastAccessed, createdAt *time.Time
		var isLongTerm bool

		rows.Scan(&id, &weight, &lastAccessed, &createdAt, &isLongTerm)

		// Compute days since access using captured monotonic time
		lastAccessTime := lastAccessed
		if lastAccessTime == nil {
			lastAccessTime = createdAt
		}
		if lastAccessTime == nil {
			// Both timestamps are NULL — skip this row
			continue
		}
		daysSinceAccess := monotonicNow.Sub(*lastAccessTime).Hours() / 24.0

		// Compute decay amount (float64 throughout)
		decay := computeDecay(float64(weight), daysSinceAccess, isLongTerm, createdAt, aggressive, monotonicNow)
		newWeight := float64(weight) - decay

		// Floor
		if newWeight < -10.0 {
			newWeight = -10.0
		}
		// LTM protection: preserve memories that are explicitly marked LTM OR have weight >= 10.
		// Applying the floor BEFORE dead detection ensures LTM memories are never flagged for deletion.
		if (mpminternal.IsLTMMemory(isLongTerm, weight)) && newWeight < 1.0 {
			newWeight = 1.0
		}
		// Record delta for batch update
		deltas = append(deltas, weightDelta{id: id, oldWeight: weight, newWeight: newWeight, isLTM: mpminternal.IsLTMMemory(isLongTerm, weight)})

		// Dead if <= 0 (post-clamp) and not LTM — LTM memories are never eligible for deletion.
		// Also catches already-dead memories (weight already <= 0 from a previous GC)
		// that were never soft-deleted — without the oldWeight > 0 guard they'd be missed.
		isLTM := deltas[len(deltas)-1].isLTM
		if newWeight <= 0.0 && !isLTM {
			var deadContent string
			dm.SQLDB().QueryRow(`SELECT SUBSTR(content, 1, 60) FROM memories WHERE id = ?`, id).Scan(&deadContent)
			deadMemories = append(deadMemories, map[string]interface{}{
				"id":      id,
				"content": deadContent,
				"weight":  weight,
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
				usererror.Error("batch update failed, rolled back: %v", batchErr)
			} else if err := tx.Commit(); err != nil {
				tx.Rollback()
				usererror.Error("batch commit failed, rolled back: %v", err)
			}
		}
	}

	// --review mode: soft-delete all dead and zombie memories (LTM already excluded above).
	// Also catches existing zombies (weight <= 0 from previous GC runs that were never cleaned).
	if review && !dryRun {
		for _, m := range deadMemories {
			if _, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, m["id"]); err != nil {
				slog.Warn("gc soft-delete failed", "id", m["id"], "error", err)
			}
		}
		// Clean any lingering zombies not caught by this pass
		if _, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE weight <= 0 AND is_long_term = 0 AND deleted_at IS NULL`); err != nil {
			slog.Warn("gc zombie cleanup failed", "error", err)
		}
		if len(deadMemories) > 0 {
			fmt.Printf("Soft-deleted %d dead memories\n", len(deadMemories))
		}
	}

	// Update last_gc_at timestamp
	if !dryRun {
		dm.SaveSystemConfig("last_gc_at", string(gcTimestampJSON), "", "")
	}

	// Output
	if jsonOutput {
		type gcResult struct {
			Scanned      int                      `json:"scanned"`
			Updated      int                      `json:"updated"`
			DeadCount    int                      `json:"dead_count"`
			DeadMemories []map[string]interface{} `json:"dead_memories,omitempty"`
			DryRun       bool                     `json:"dry_run"`
		}
		result := gcResult{
			Scanned:      scanned,
			Updated:      updated,
			DeadCount:    len(deadMemories),
			DeadMemories: deadMemories,
			DryRun:       dryRun,
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
	return 0
}

// computeDecay returns the weight decay amount for a memory.
// All math is float64; only the final value is truncated on DB write.
// Uses a pre-captured "now" timestamp to prevent clock-rollback exploits.
func computeDecay(weight float64, daysSinceAccess float64, isLongTerm bool, createdAt *time.Time, aggressive bool, now time.Time) float64 {
	multiplier := 1.0
	if aggressive {
		multiplier = 2.0
	}

	if isLongTerm {
		return daysSinceAccess * 0.01 * multiplier
	}
	if weight >= 10 {
		return daysSinceAccess * 0.02 * multiplier
	}
	if weight >= 5 {
		return daysSinceAccess * 0.05 * multiplier
	}

	// Low-weight: decay scales with age (newer = faster decay)
	ageFactor := 1.0
	if createdAt != nil {
		// Use the captured monotonic reference: daysSinceCreated based on the pre-captured
		// timestamp, not wall-clock time. This prevents clock-rollback from slowing decay.
		daysSinceCreated := now.Sub(*createdAt).Hours() / 24.0
		if daysSinceCreated > 30.0 {
			ageFactor = 1.0
		} else {
			ageFactor = daysSinceCreated / 30.0
		}
	}
	baseDecay := 0.1 + 0.2*ageFactor
	return daysSinceAccess * baseDecay * multiplier
}

// handleRestore recovers a soft-deleted memory by clearing deleted_at and preserving
