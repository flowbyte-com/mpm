// gc_tools.go — DM methods for the GC maintenance pass.
//
// The CLI handler (cmd/mpm/handlers.go handleGC) keeps its full flag
// surface for operator use (--review / --purge / --shred-negative).
// The MCP tool (`gc_run`) and `mpm call gc_run` use this entry point
// with safe defaults — DryRun=true unless explicitly disabled.
//
// Decay math (gcComputeDecay) lives here as a single canonical
// implementation. The CLI handler's inline `computeDecay` is
// intentionally separate (it operates on a different memory store
// reference path); if decay rules change, both must move together.
package internal

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// GCOptions configures a single GC run invoked via mpm call / MCP. The
// safe defaults (DryRun=true, Aggressive=false, MaxAgeHours=24) make
// accidental damage unlikely. Agents wanting destructive runs must
// explicitly set DryRun=false. The full CLI flag surface (--review,
// --purge, --shred-negative) is intentionally NOT exposed here — those
// modes are operationally distinct and stay on the `mpm gc` CLI where
// humans can see what they're doing.
type GCOptions struct {
	DryRun         bool // when true, no writes — pure stats
	Aggressive     bool // doubled decay rate
	MaxAgeHours    int  // cooldown between successive GC runs
	StaleTheoryDays int // pending theories older than this (days) are flagged/resolved; <= 0 disables
	// Policies overrides the per-collection decay protection map. Nil
	// uses DefaultDecayPolicies: collections with DecayPercent <= 0
	// (decisions — the append-only audit trail) are exempt from the GC
	// forgetting curve, matching the maintenance-path semantics.
	Policies map[string]DecayPolicy
}

// GCRunResult is the structured output of dm.RunGC. Every numeric field
// is the count of affected rows; booleans signal what was actually
// attempted vs skipped. DeadMemories is populated on dry runs so
// agents can decide whether to escalate (purge/shred).
type GCRunResult struct {
	Ran                   bool    // false if cooldown skipped the run
	CooldownSkip          bool    // true if cooldown blocked this run
	LastGCRan             *int64  // populated only on cooldown skip (Unix-epoch seconds)
	Scanned               int
	Updated               int
	SoftDeleted           int
	Shredded              int
	AuditPruned           int
	HandoffPruned         int
	CascadeOutboxPruned   int
	DeadMemories          []map[string]interface{} // first 50 dead for inspection
	StaleTheories         []map[string]interface{} // pending theories older than StaleTheoryDays
	StaleTheoriesResolved int                     // theories auto-resolved as disproven (non-dry-run only)
}

// RunGC executes one maintenance pass with the standard cooldown cap.
// Stats are returned regardless of dry-run flag so agents can plan
// follow-up actions (e.g., "30 memories would die — escalate to shred?").
// Safe for concurrent invocation: the cooldown claim is atomic.
//
// Failure modes:
//   - cooldown active → Ran=false, CooldownSkip=true, LastGCRan set
//   - SQL error → error returned, no partial writes (single transaction
//     for the batch update so a mid-run failure rolls everything back)
func (dm *DatabaseManager) RunGC(opts GCOptions) (*GCRunResult, error) {
	if opts.MaxAgeHours <= 0 {
		opts.MaxAgeHours = 24
	}
	result := &GCRunResult{}

	// Atomic cooldown claim — same SQL as the test pins. See
	// cmd/mpm/handlers.go for the multi-case contract.
	now := time.Now()
	gcTimestampJSON, _ := json.Marshal(map[string]string{"updated_at": now.Format(time.RFC3339)})
	claim, err := dm.db.Exec(`
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
	`, string(gcTimestampJSON), strconv.Itoa(opts.MaxAgeHours))
	if err != nil {
		return nil, fmt.Errorf("gc: cooldown claim: %w", err)
	}
	rowsAffected, _ := claim.RowsAffected()
	if rowsAffected == 0 {
		result.CooldownSkip = true
		if lastGC, gerr := dm.GetSystemConfig("last_gc_at"); gerr == nil {
			// GetSystemConfig returns updated_at as int64 Unix-epoch seconds
			// (see migration timestamps_unified_v1).
			if t, ok := lastGC["updated_at"].(int64); ok && t > 0 {
				v := t
				result.LastGCRan = &v
			}
		}
		return result, nil
	}

	// Audit + handoff + cascade-outbox retention sweeps (always run on a
	// successful claim — these are cheap and central to the GC contract).
	// The cascade outbox grows monotonically with every shred event;
	// without a sweep the table accumulates forever on long-running daemons.
	if pruned, err := dm.PruneAuditLog(30); err == nil {
		result.AuditPruned = int(pruned)
	}
	if pruned, err := dm.PruneHandoffs(90); err == nil {
		result.HandoffPruned = int(pruned)
	}
	if pruned, err := dm.PruneCascadeOutbox(30); err == nil {
		result.CascadeOutboxPruned = int(pruned)
	}

	// Compute decay for every non-deleted memory.
	policies := opts.Policies
	if policies == nil {
		policies = DefaultDecayPolicies
	}
	rows, err := dm.db.Query(`
		SELECT id, collection, weight, last_accessed_at, created_at, is_long_term
		FROM memories WHERE deleted_at IS NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("gc: scan memories: %w", err)
	}
	defer rows.Close()

	type delta struct {
		id        string
		newWeight float64
		isLTM     bool
	}
	var deltas []delta
	monotonicNow := time.Now()

	for rows.Next() {
		result.Scanned++
		var id, collection string
		var weight float64
		var lastAccessed, createdAt *int64
		var isLTM bool
		if err := rows.Scan(&id, &collection, &weight, &lastAccessed, &createdAt, &isLTM); err != nil {
			return nil, fmt.Errorf("scanning GC candidate memory row: %w", err)
		}
		// Zero-decay collections (append-only audit trails like
		// decisions) are exempt from the forgetting curve — the
		// maintenance path protects them, and so does GC.
		if p, ok := policies[collection]; ok && p.DecayPercent <= 0 {
			continue
		}
		last := lastAccessed
		if last == nil {
			last = createdAt
		}
		if last == nil {
			continue
		}
		lastTime := time.Unix(*last, 0)
		days := monotonicNow.Sub(lastTime).Hours() / 24.0
		var createdTime *time.Time
		if createdAt != nil {
			t := time.Unix(*createdAt, 0)
			createdTime = &t
		}
		decay := gcComputeDecay(weight, days, isLTM, createdTime, opts.Aggressive, monotonicNow)
		newW := weight - decay
		if newW < -10.0 {
			newW = -10.0
		}
		if (IsLTMMemory(isLTM, int(weight))) && newW < 1.0 {
			newW = 1.0
		}
		lifetimeLTM := IsLTMMemory(isLTM, int(weight))
		deltas = append(deltas, delta{id: id, newWeight: newW, isLTM: lifetimeLTM})

		if newW <= 0.0 && !lifetimeLTM {
			var preview string
			if err := dm.db.QueryRow(`SELECT SUBSTR(content, 1, 60) FROM memories WHERE id = ?`, id).Scan(&preview); err != nil {
				preview = ""
			}
			result.DeadMemories = append(result.DeadMemories, map[string]interface{}{
				"id":      id,
				"content": preview,
				"weight":  int(weight),
			})
			// Cap the preview list — full list available via SQL for agents
			// that want to enumerate.
			if len(result.DeadMemories) >= 50 {
				break
			}
		}
	}

	// Stale-theory sweep: pending hypotheses that have aged past the
	// staleness window are flagged (dry-run) or auto-resolved as
	// disproven (non-dry-run). This is the buildup guard for the theory
	// lifecycle — agent discipline alone leaves stale "pending" rows
	// accumulating indefinitely.
	//
	// Expired-but-pending rows (e.g. legacy expires_at=0 artifacts) are
	// INCLUDED deliberately: they are invisible to ResolveTheory and
	// would otherwise accumulate as unactionable ghosts. The sweep
	// clears expires_at in the same transaction it resolves them, so
	// the resolution lands on a row the API can see again.
	if opts.StaleTheoryDays > 0 {
		staleRows, err := dm.db.Query(`
			SELECT id, content, created_at FROM memories
			WHERE collection = 'theories' AND deleted_at IS NULL
			  AND json_extract(metadata, '$.status') = 'pending'
			  AND created_at < strftime('%s','now') - ? * 86400
		`, opts.StaleTheoryDays)
		if err != nil {
			return nil, fmt.Errorf("gc: query stale theories: %w", err)
		}
		type staleTheory struct {
			id        string
			content   string
			createdAt int64
		}
		var stale []staleTheory
		for staleRows.Next() {
			var t staleTheory
			if err := staleRows.Scan(&t.id, &t.content, &t.createdAt); err != nil {
				staleRows.Close()
				return nil, fmt.Errorf("gc: scan stale theory: %w", err)
			}
			stale = append(stale, t)
		}
		staleRows.Close()

		for _, t := range stale {
			days := int(monotonicNow.Sub(time.Unix(t.createdAt, 0)).Hours() / 24.0)
			result.StaleTheories = append(result.StaleTheories, map[string]interface{}{
				"id":             t.id,
				"content":        previewContent(t.content),
				"days_pending":   days,
			})
			if !opts.DryRun {
				if resolved, err := dm.resolveStaleTheory(t.id); err != nil {
					return nil, fmt.Errorf("gc: resolve stale theory %s: %w", t.id, err)
				} else if resolved {
					result.StaleTheoriesResolved++
				}
			}
		}
	}

	// Batch-apply weight updates in a single transaction. Skip entirely
	// on dry-run — the whole point of dry-run is "show me what would
	// happen without changing anything."
	if !opts.DryRun && len(deltas) > 0 {
		tx, txErr := dm.db.Begin()
		if txErr != nil {
			return nil, fmt.Errorf("gc: begin tx: %w", txErr)
		}
		rolledBack := false
		defer func() {
			if !rolledBack {
				_ = tx.Rollback()
			}
		}()
		for _, d := range deltas {
			rounded := int(math.Round(d.newWeight))
			if d.isLTM && rounded < 1 {
				rounded = 1
			} else if !d.isLTM && rounded < -10 {
				rounded = -10
			}
			if _, err := tx.Exec(`UPDATE memories SET weight = ? WHERE id = ?`, rounded, d.id); err != nil {
				_ = tx.Rollback()
				rolledBack = true
				return nil, fmt.Errorf("gc: update weight %s: %w", d.id, err)
			}
			result.Updated++
		}
		if err := tx.Commit(); err != nil {
			rolledBack = true
			return nil, fmt.Errorf("gc: commit: %w", err)
		}
	}

	result.Ran = true
	return result, nil
}

// resolveStaleTheory marks a pending theory as disproven on GC's behalf,
// mirroring ResolveTheory's transition semantics (same in-tx UPDATE with a
// status='pending' filter, same cascade hook on a real pending→disproven
// transition) but attributed to GC rather than a manual resolve, and with
// no reinforcement bump — GC is closing the loop on an unvalidated claim,
// not promoting it.
//
// Idempotent: a theory already resolved (or never pending) updates zero
// rows and returns (false, nil). Returns (true, nil) only on a real
// transition.
func (dm *DatabaseManager) resolveStaleTheory(theoryID string) (bool, error) {
	patch, err := json.Marshal(map[string]interface{}{
		"status":          "disproven",
		"conclusion":      "unvalidated — no resolution within the GC staleness window",
		"resolved_at":     time.Now().UTC().Format(time.RFC3339),
		"resolved_by":     "gc:stale",
		"resolved_by_via": "gc",
	})
	if err != nil {
		return false, fmt.Errorf("marshal stale-theory patch: %w", err)
	}

	var transitioned bool
	err = dm.WithTx(func(node DBNode) error {
		res, err := node.ExecTracked(`
			UPDATE memories
			SET expires_at = NULL,
			    metadata = json_patch(COALESCE(metadata, '{}'), ?),
			    last_accessed_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ? AND collection = 'theories' AND deleted_at IS NULL
			  AND json_extract(metadata, '$.status') = 'pending'
		`, 0, string(patch), theoryID)
		if err != nil {
			return fmt.Errorf("resolve stale theory: %w", err)
		}
		rows, _ := res.RowsAffected()
		transitioned = rows > 0

		if transitioned {
			if _, err := dm.EnqueueCascadeInvalidation(
				node.Tx(),
				theoryID, "theory",
				"theory_disproven", "", 0,
			); err != nil {
				return fmt.Errorf("cascade enqueue: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if transitioned {
		dm.LogAudit(AuditInfo, "epistemology",
			fmt.Sprintf("gc auto-resolved stale theory %s", theoryID), "",
			AuditContext{"theory_id": theoryID, "resolved_by": "gc:stale"})
	}
	return transitioned, nil
}

// previewContent returns a single-line preview of a memory/theory body,
// matching the DeadMemories preview convention (first 60 chars).
func previewContent(content string) string {
	preview := strings.ReplaceAll(content, "\n", " ")
	if len(preview) > 60 {
		preview = preview[:60]
	}
	return preview
}

// gcComputeDecay computes per-memory weight decay. Mirrors
// cmd/mpm/handlers.go:computeDecay verbatim — both implementations must
// stay in lockstep. The duplication exists because the CLI handler
// operates on a different memory store path (MemoryStore vs raw *sql.DB);
// centralizing would require either changing the CLI's data flow or
// threading the DM through the CLI's command-dispatch loop. Until that
// refactor happens, this is the lowest-friction duplication.
//
// Rules:
//   - LTM memory: very slow decay (0.01 × days)
//   - High-weight (>=10, non-LTM): slow (0.02 × days)
//   - Medium-weight (5-9): moderate (0.05 × days)
//   - Low-weight: faster, scaled by age (newer memories decay faster)
//
// `aggressive` doubles every rate. `createdAt` is required for the
// low-weight branch (age factor); nil falls back to the base rate.
func gcComputeDecay(weight float64, daysSinceAccess float64, isLongTerm bool, createdAt *time.Time, aggressive bool, now time.Time) float64 {
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
	ageFactor := 1.0
	if createdAt != nil {
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