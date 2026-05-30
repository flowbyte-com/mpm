package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	mpminternal "mpm/internal"
)

// handleDLQReview implements 'mpm ops dlq:review' and 'dlq:clear' commands.
// DLQ = Dead Letter Queue for failed synthesis events awaiting retry.
func handleDLQReview(args []string) int {
	fs := flag.NewFlagSet("dlq:review", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Println("Usage: mpm ops dlq:review [subcommand]")
		fmt.Println("\nSubcommands:")
		fmt.Println("  review    Show DLQ entries pending retry (default)")
		fmt.Println("  clear     Remove all entries from DLQ")
		fmt.Println("  retry     Force-retry all pending entries now")
		fmt.Println("\nExamples:")
		fmt.Println("  mpm ops dlq:review        # show pending entries")
		fmt.Println("  mpm ops dlq:review clear  # purge DLQ")
		fmt.Println("  mpm ops dlq:review retry  # flag entries for immediate retry")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	subCmd := "review"
	if fs.NArg() > 0 {
		subCmd = fs.Arg(0)
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	defer dm.Close()

	if err := mpminternal.EnsureDLQSchema(dm.SQLDB()); err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to ensure DLQ schema: %v\n", err)
		return 1
	}

	switch subCmd {
	case "review":
		return dlqReview(dm.SQLDB())
	case "clear":
		return dlqClear(dm.SQLDB())
	case "retry":
		return dlqRetry(dm.SQLDB())
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subCmd)
		fs.Usage()
		return 1
	}
}

func dlqReview(db *sql.DB) int {
	count, oldest, err := mpminternal.DLQStats(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading DLQ stats: %v\n", err)
		return 1
	}

	fmt.Printf("⚡ Synthesis DLQ — %d pending\n", count)
	if oldest.IsZero() {
		fmt.Println("   Status: empty")
		return 0
	}
	fmt.Printf("   Oldest entry: %s (%s ago)\n", oldest.Format(time.RFC3339), time.Since(oldest).Round(time.Minute))

	all, err := dlqAllEntries(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading all DLQ entries: %v\n", err)
		return 1
	}

	if len(all) == 0 {
		fmt.Println("   No entries.")
		return 0
	}

	ready := 0
	stopped := 0
	for _, e := range all {
		if e.NextRetry.Before(time.Now()) || e.NextRetry.IsZero() {
			ready++
		}
		if e.Attempt >= 5 {
			stopped++
		}
	}

	fmt.Println()
	fmt.Printf("   %-16s %-10s %-8s %-35s %s\n", "MEMORY_ID", "AGE", "ATTEMPT", "LAST_ERROR", "STATUS")
	fmt.Println("   " + string('-' * 85))
	for _, e := range all {
		age := time.Since(e.CreatedAt).Round(time.Minute)
		errStr := e.LastError
		if len(errStr) > 33 {
			errStr = errStr[:33] + "..."
		}
		status := "READY"
		if e.NextRetry.After(time.Now()) {
			status = "scheduled " + e.NextRetry.Format("15:04")
		}
		if e.Attempt >= 5 {
			status = "HARD-STOPPED"
		}
		fmt.Printf("   %-16s %-10s %-8d %-35s %s\n",
			e.MemoryID[:12]+"...",
			age,
			e.Attempt,
			errStr,
			status,
		)
	}

	fmt.Println()
	if ready > 0 {
		fmt.Printf("   ℹ️  %d ready for immediate retry (next_retry <= now)\n", ready)
	}
	fmt.Printf("   ℹ️  %d hard-stopped (attempt >= 5, requires manual intervention)\n", stopped)
	return 0
}

func dlqAllEntries(db *sql.DB) ([]mpminternal.DLQEntry, error) {
	rows, err := db.Query(`
		SELECT id, memory_id, content, tags, attempt, last_error, created_at, next_retry
		FROM synthesis_dlq
		ORDER BY created_at ASC
		LIMIT 50
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []mpminternal.DLQEntry
	for rows.Next() {
		var e mpminternal.DLQEntry
		var tagsRaw, lastErr, nextRetry, createdAt string
		if err := rows.Scan(&e.ID, &e.MemoryID, &e.Content, &tagsRaw, &e.Attempt, &lastErr, &createdAt, &nextRetry); err != nil {
			continue
		}
		e.LastError = lastErr
		if nextRetry != "" {
			t, _ := time.Parse(time.RFC3339, nextRetry)
			e.NextRetry = t
		}
		if createdAt != "" {
			t, _ := time.Parse(time.RFC3339, createdAt)
			e.CreatedAt = t
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func dlqClear(db *sql.DB) int {
	res, err := db.Exec(`DELETE FROM synthesis_dlq`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error clearing DLQ: %v\n", err)
		return 1
	}
	n, _ := res.RowsAffected()
	fmt.Printf("⚡ DLQ cleared — %d entries removed\n", n)
	return 0
}

func dlqRetry(db *sql.DB) int {
	_, err := db.Exec(`UPDATE synthesis_dlq SET next_retry = datetime('now')`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error forcing DLQ retry: %v\n", err)
		return 1
	}
	fmt.Println("⚡ All DLQ entries flagged for immediate retry on next tick.")
	return 0
}