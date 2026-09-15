package main

import (
	"context"
	"fmt"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/synth"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// mpm synthesize — Find near-duplicate memories and merge via LLM synthesis.
// Unlike the auto-synthesis hook (which fires after every ingest), this
// standalone command scans the entire memories table for potential merges.
//
// Subcommands:
//
//	mpm synthesize                Full-table synthesis scan (default)
//	mpm synthesize status         Show recent synthesis telemetry (last 20)
//	mpm synthesize failures       Show only failed/skipped synthesis events
//	mpm synthesize --dry-run      Scan without calling the LLM
func handleSynthesize(args []string) int {
	// Subcommand dispatch: status / failures route to watchdog reader.
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return handleSynthesizeStatus(20)
		case "failures":
			return handleSynthesizeFailures(50)
		case "help", "-h", "--help":
			fmt.Println("Usage: mpm synthesize [status|failures] [--dry-run]")
			return 0
		}
	}

	dryRun := false
	for _, a := range args {
		if a == "--dry-run" || a == "--dryrun" {
			dryRun = true
			break
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	client := synth.NewSynthClient()

	// Scan all non-deleted, non-LTM memories for near-miss clusters
	rows, err := dm.SQLDB().Query(`
		SELECT id, content FROM memories
		WHERE deleted_at IS NULL
		  AND is_long_term = 0
		ORDER BY created_at DESC
	`)
	if err != nil {
		usererror.Error("Query failed: %v", err)
		return 1
	}
	defer rows.Close()

	type memInfo struct {
		ID      string
		Content string
	}
	var all []memInfo
	scanErr := func() error {
		for rows.Next() {
			var m memInfo
			if err := rows.Scan(&m.ID, &m.Content); err != nil {
				return fmt.Errorf("scanning memory row for synthesize: %w", err)
			}
			all = append(all, m)
		}
		return nil
	}()
	if scanErr != nil {
		usererror.Warn("handleSynthesize: %v", scanErr)
		return 1
	}

	if len(all) == 0 {
		fmt.Println("No memories found for synthesis scan.")
		return 0
	}

	// 2026-09-14 tightening pass: BOUNDED CONTINUATION. The
	// safeguard's per-invocation stage ceiling is 8. The CLI
	// scan processes up to that many memories per call,
	// reports Remaining, and exits cleanly. Re-invocation
	// resumes from canonical substrate state — content-hash
	// dedup is durable, so already-synthesized rows are
	// skipped on the next run. Continuation is the bounded
	// shape; rejection is not.
	ceiling := mpminternal.MaxSemanticStagesPerInvocation
	total := len(all)
	if total > ceiling {
		all = all[:ceiling]
	}

	processed := 0
	for _, m := range all {
		if dryRun {
			fmt.Printf("[dry-run] Would check %s for near-misses\n", shortID(m.ID))
			processed++
			continue
		}
		mpminternal.AutoSynthesize(context.Background(), dm, client, m.ID, m.Content)
		processed++
		time.Sleep(1 * time.Second) // rate-limit: 1 call/sec
	}

	remaining := total - processed
	if dryRun {
		fmt.Printf("Dry-run: %d memory(ies) processed; %d remaining.\n", processed, remaining)
	} else if remaining > 0 {
		// 2026-09-15 accounting-naming nit: the bounded-
		// continuation report now uses unit-exact labels.
		// The unit is *memories*: one AutoSynthesize call
		// per non-deleted, non-LTM memory row. The CLI
		// processes up to MaxSemanticStagesPerInvocation
		// (= 8) per call; re-invocation resumes from
		// canonical substrate state. The math invariant
		// `processed + remaining == total` holds in code
		// regardless of N (verified by
		// TestRunawaySafeguard_ContinuationArithmeticInvariant).
		fmt.Println()
		fmt.Println("MPM · Synthesis")
		fmt.Println("Unit: memories (one check per non-deleted, non-LTM memory row)")
		fmt.Printf("  Memories processed: %d\n", processed)
		fmt.Printf("  Memories remaining: %d\n", remaining)
		fmt.Println()
		fmt.Println("Bounded execution limit reached for this invocation.")
		fmt.Println("Run `mpm synthesize` again to continue.")
	} else {
		fmt.Printf("✅ Synthesis scan complete: %d memory(ies) checked.\n", processed)
	}
	return 0
}

// handleSynthesizeStatus prints the most recent synthesis_* entries from the
// watchdog log. There is no in-memory DLQ — the watchdog JSONL is the
// synthesis event store.
func handleSynthesizeStatus(limit int) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	ops, err := dm.RecentWatchdogOps(limit, "synthesize_")
	if err != nil {
		usererror.Error("Watchdog read failed: %v", err)
	}
	if len(ops) == 0 {
		fmt.Println("No synthesis telemetry yet.")
		return 0
	}

	fmt.Printf("Synthesis telemetry (last %d of %d entries):\n\n", len(ops), len(ops))
	for _, op := range ops {
		ts, _ := op["timestamp"].(string)
		name, _ := op["op"].(string)
		reason, _ := op["reason"].(string)
		errStr, _ := op["error"].(string)
		memID, _ := op["memory_id"].(string)
		fmt.Printf("  %s  %s", ts, name)
		if memID != "" {
			fmt.Printf("  memory=%s", shortID(memID))
		}
		if reason != "" {
			fmt.Printf("  reason=%s", reason)
		}
		if errStr != "" {
			fmt.Printf("  err=%s", errStr)
		}
		fmt.Println()
	}
	return 0
}

// handleSynthesizeFailures prints watchdog entries with op="synthesize_error"
// or a non-empty error field. Useful for "why are my merges not happening?"
// investigations.
func handleSynthesizeFailures(limit int) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	ops, err := dm.RecentWatchdogOps(limit, "synthesize_")
	if err != nil {
		usererror.Error("Watchdog read failed: %v", err)
	}

	fmted := 0
	for _, op := range ops {
		name, _ := op["op"].(string)
		errStr, _ := op["error"].(string)
		if name != "synthesize_error" && errStr == "" {
			continue
		}
		ts, _ := op["timestamp"].(string)
		fmt.Printf("  %s  %s", ts, name)
		if memID, ok := op["memory_id"].(string); ok && memID != "" {
			fmt.Printf("  memory=%s", shortID(memID))
		}
		if reason, ok := op["reason"].(string); ok && reason != "" {
			fmt.Printf("  reason=%s", reason)
		}
		if errStr != "" {
			fmt.Printf("  err=%s", errStr)
		}
		fmt.Println()
		fmted++
	}
	if fmted == 0 {
		fmt.Println("No synthesis failures recorded. ✅")
	}
	return 0
}
