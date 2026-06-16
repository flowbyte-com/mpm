package main

import (
	"context"
	"fmt"
	"os"
	"time"

	mpminternal "mpm/internal"
)

// mpm synthesize — Find near-duplicate memories and merge via LLM synthesis.
// Unlike the auto-synthesis hook (which fires after every ingest), this
// standalone command scans the entire memories table for potential merges.
func handleSynthesize(args []string) int {
	dryRun := false
	for _, a := range args {
		if a == "--dry-run" || a == "--dryrun" {
			dryRun = true
			break
		}
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB open failed: %v\n", err)
		return 1
	}
	defer dm.Close()

	client := mpminternal.NewSynthClient()

	// Scan all non-deleted, non-LTM memories for near-miss clusters
	rows, err := dm.SQLDB().Query(`
		SELECT id, content FROM memories
		WHERE deleted_at IS NULL
		  AND is_long_term = 0
		ORDER BY created_at DESC
	`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Query failed: %v\n", err)
		return 1
	}

	type memInfo struct {
		ID      string
		Content string
	}
	var all []memInfo
	for rows.Next() {
		var m memInfo
		if err := rows.Scan(&m.ID, &m.Content); err != nil {
			continue
		}
		all = append(all, m)
	}
	rows.Close()

	if len(all) == 0 {
		fmt.Println("No memories found for synthesis scan.")
		return 0
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

	if dryRun {
		fmt.Printf("Dry-run: %d memory(ies) would be checked for near-misses.\n", processed)
	} else {
		fmt.Printf("✅ Synthesis scan complete: %d memory(ies) checked.\n", processed)
	}
	return 0
}
