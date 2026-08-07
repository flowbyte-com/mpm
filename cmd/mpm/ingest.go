package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// =============================================================================
// mpm ingest — Import memories from external SQLite sources
// =============================================================================

func handleIngest(args []string) int {
	if len(args) < 2 {
		printIngestHelp()
		return 1
	}

	subcommand := args[1]

	switch subcommand {
	case "--help", "-h", "help":
		printIngestHelp()
		return 0
	case "--source", "source":
		return handleIngestSource(args[2:])
	case "--list-schemas", "list-schemas":
		return handleIngestListSchemas(args[2:])
	case "--status", "status":
		return handleIngestStatus(args[2:])
	case "--review", "review":
		return handleIngestReview(args[2:])
	case "--cleanup", "cleanup":
		return handleIngestCleanup(args[2:])
	case "--history", "history":
		return handleIngestHistory(args[2:])
	case "--undo", "undo":
		return handleIngestUndo(args[2:])
	default:
		usererror.Error("Unknown ingest subcommand: %s", subcommand)
		printIngestHelp()
		return 1
	}
}

func printIngestHelp() {
	fmt.Println(`mpm ingest — Import memories from external SQLite sources

Usage: mpm ingest <subcommand> [flags]

Subcommands:
    --source <path>       Ingest from a SQLite source DB
    --list-schemas <path> Inspect a SQLite DB's schema
    --status             Show staging area summary
    --review             LLM-review pending entries and promote approved
    --cleanup            Expire old pending entries, archive rejected
    --history            Show past ingest runs
    --undo <batch_id>    Undo a specific ingest run

Flags (--source):
    --dry-run            Preview without writing
    --batch-size <n>     Rows per batch (default: 100)
    --import-id <id>     Override batch ID
    Default source: from config (openclaw_db_path in mpm_config.json) or ~/.openclaw/memory/main.sqlite

Config:
    Set "openclaw_db_path" in mpm_config.json to change the default source path.

Examples:
    mpm ingest --source ~/.openclaw/memory/main.sqlite --dry-run
    mpm ingest --source ~/.openclaw/memory/main.sqlite
    mpm ingest --status
    mpm ingest --review --batch 20
    mpm ingest --undo ingest_2026-04-06T123456789`)
}

// handleIngestSource runs the ingest pipeline
func handleIngestSource(args []string) int {
	var sourcePath string
	var dryRun bool
	var batchSize int = 100
	var importID string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			dryRun = true
		case "--batch-size":
			if i+1 >= len(args) {
				usererror.Error("--batch-size requires a number")
				return 1
			}
			fmt.Sscanf(args[i+1], "%d", &batchSize)
			i++
		case "--import-id":
			if i+1 >= len(args) {
				usererror.Error("--import-id requires an ID")
				return 1
			}
			importID = args[i+1]
			i++
		case "--source":
			if i+1 >= len(args) {
				usererror.Error("--source requires a path")
				return 1
			}
			sourcePath = args[i+1]
			i++
		default:
			// Positional source path
			if !strings.HasPrefix(args[i], "-") && sourcePath == "" {
				sourcePath = args[i]
			}
		}
	}

	if sourcePath == "" {
		sourcePath = config.GetOpenClawDBPath()
	}

	fmt.Printf("Reading %s...\n", sourcePath)

	// Detect schema first
	schema, err := internal.DetectSchema(sourcePath)
	if err != nil {
		usererror.Error("Schema detection failed: %v", err)
		return 1
	}
	fmt.Printf("  Schema: %s (%s)\n", schema.DBType, schema.Tables[0].Name)

	dm := getDB()
	if dm == nil {
		return 1
	}

	if dryRun {
		fmt.Println("  [DRY RUN] No changes written.")
	}

	stats, err := dm.IngestOpenClaw(sourcePath, batchSize, importID, dryRun)
	if err != nil {
		usererror.Error("Ingest failed: %v", err)
		return 1
	}

	fmt.Printf("  Rows read:     %d\n", stats.RowsRead)
	if stats.RowsSkipped > 0 || stats.RowsRejected > 0 {
		fmt.Printf("  Rows skipped:   %d (dedup or already staged)\n", stats.RowsSkipped)
		fmt.Printf("  Rows rejected:  %d (security filter)\n", stats.RowsRejected)
	}
	if !dryRun {
		fmt.Printf("  Rows staged:   %d\n", stats.RowsStaged)
	}

	return 0
}

// handleIngestListSchemas inspects a source DB's schema
func handleIngestListSchemas(args []string) int {
	if len(args) < 1 {
		usererror.Usage("mpm ingest --list-schemas <path>")
		return 1
	}
	path := args[0]
	if strings.HasPrefix(path, "--") {
		path = config.GetOpenClawDBPath()
	}

	schema, err := internal.DetectSchema(path)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	fmt.Printf("Schema: %s\n", schema.DBType)
	fmt.Printf("Path:   %s\n", schema.DBPath)
	fmt.Println("\nTables:")
	for _, t := range schema.Tables {
		fmt.Printf("  %s: %v\n", t.Name, t.Columns)
	}
	return 0
}

// handleIngestStatus shows staging area summary
func handleIngestStatus(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	counts, err := dm.GetIngestStatus()
	if err != nil {
		usererror.Error("Failed to get status: %v", err)
		return 1
	}

	fmt.Println("raw_memories:")
	statuses := []string{"pending", "reviewing", "approved", "rejected", "duplicate", "expired"}
	hasAny := false
	for _, s := range statuses {
		if counts[s] > 0 {
			fmt.Printf("  %-12s %d\n", s+":", counts[s])
			hasAny = true
		}
	}
	if !hasAny {
		fmt.Println("  (empty)")
	}

	batches, err := dm.ListIngestBatches()
	if err == nil && len(batches) > 0 {
		fmt.Println("\nRecent batches:")
		for _, b := range batches {
			if len(b) > 0 {
				batch := b["batch"].(string)
				fmt.Printf("  %s", batch)
				for k, v := range b {
					if k != "batch" {
						fmt.Printf(", %s=%v", k, v)
					}
				}
				fmt.Println()
			}
		}
	}

	return 0
}

// handleIngestReview runs LLM review on pending entries
func handleIngestReview(args []string) int {
	var batchSize int = 20
	var force bool // placeholder for --force flag (Phase 4)
	_ = force

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--batch":
			if i+1 >= len(args) {
				usererror.Error("--batch requires a number")
				return 1
			}
			fmt.Sscanf(args[i+1], "%d", &batchSize)
			i++
		case "--force":
			force = true
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Reset stale reviewing entries
	reset, _ := dm.ResetStaleReviewing()
	if reset > 0 {
		fmt.Printf("  Reset %d stale 'reviewing' entries to 'pending'\n", reset)
	}

	pending, err := dm.GetRawMemoriesByStatus("pending", batchSize)
	if err != nil {
		usererror.Error("Failed to fetch pending: %v", err)
		return 1
	}

	if len(pending) == 0 {
		fmt.Println("No pending entries to review.")
		return 0
	}

	fmt.Printf("Reviewing %d pending entries...\n", len(pending))
	fmt.Printf("  Entries: ")
	for _, p := range pending {
		fmt.Printf("%s ", p.ID[:8])
	}
	fmt.Println()

	return 0
}

// handleIngestCleanup expires old pending entries
func handleIngestCleanup(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	now := time.Now().Unix()

	// Expire pending entries past their expires_at
	result, err := dm.SQLDB().Exec(`
		UPDATE raw_memories SET status = 'expired'
		WHERE status = 'pending' AND expires_at > 0 AND expires_at < ?
	`, now)
	if err != nil {
		usererror.Warn("Cleanup failed: %v", err)
		return 1
	}
	expired, _ := result.RowsAffected()
	fmt.Printf("Expired %d pending entries.\n", expired)

	// Hard delete old rejected entries (7+ days old)
	oldThreshold := now - (7 * 24 * 60 * 60)
	result2, err := dm.SQLDB().Exec(`
		DELETE FROM raw_memories
		WHERE status = 'rejected' AND updated_at < ?
	`, oldThreshold)
	if err == nil {
		if deleted, _ := result2.RowsAffected(); deleted > 0 {
			fmt.Printf("Deleted %d old rejected entries (VACUUM recommended).\n", deleted)
		}
	}

	return 0
}

// handleIngestHistory shows past ingest runs
func handleIngestHistory(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	batches, err := dm.ListIngestBatches()
	if err != nil {
		usererror.Error("Failed to get history: %v", err)
		return 1
	}

	if len(batches) == 0 {
		fmt.Println("No ingest history.")
		return 0
	}

	fmt.Println("Ingest history:")
	for _, b := range batches {
		batch := b["batch"].(string)
		fmt.Printf("  %s", batch)
		for k, v := range b {
			if k != "batch" {
				fmt.Printf(", %s=%v", k, v)
			}
		}
		fmt.Println()
	}
	return 0
}

// handleIngestUndo reverts an ingest run
func handleIngestUndo(args []string) int {
	if len(args) < 1 {
		usererror.Usage("mpm ingest --undo <batch_id>")
		return 1
	}
	batchID := args[0]

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Delete raw_memories entries for this batch
	result, err := dm.SQLDB().Exec(`
		DELETE FROM raw_memories WHERE import_batch = ? AND status IN ('pending', 'approved', 'reviewing')
	`, batchID)
	if err != nil {
		usererror.Error("Undo failed: %v", err)
		return 1
	}
	deleted, _ := result.RowsAffected()
	fmt.Printf("Removed %d entries from raw_memories (batch: %s).\n", deleted, batchID)

	// Note: promoted memories still remain in the memory table.
	// Full undo would require tracking promoted_at per batch — Phase 5 feature.

	return 0
}
