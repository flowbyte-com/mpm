package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// =============================================================================
// mpm migrate — front door for non-SQLite memory migration
// =============================================================================
//
// `migrate` is a friendly alias for the markdown/json subset of `mpm ingest`.
// It runs the same staging + dedup + promote pipeline but auto-detects file
// format from extension and presents a simpler flag surface:
//
//   mpm migrate --from <path>                  (auto-detect, stage + review)
//   mpm migrate --from <path> --commit         (auto-detect, stage + commit)
//   mpm migrate --from <path> --dry-run        (parse only, no writes)
//   mpm migrate --commit-batch <batch_id>      (promote a previously-staged batch)
//   mpm migrate --undo <batch_id>              (delegates to ingest --undo)
//
// For SQLite sources (OpenClaw DBs, etc.) keep using `mpm ingest --source`.
// The ingest pipeline remains the engine; migrate is the alias.
//
// Underlying implementation lives in internal/core/migrate_md.go and
// internal/core/migrate_json.go.  Both ship ready: --from <file.json> parses
// JSON, --from <file.md> parses markdown.  The CLI shape is stable.

func handleMigrate(args []string) int {
	if len(args) < 2 {
		printMigrateHelp()
		return 1
	}

	// Top-level flags
	var fromPath string
	var dryRun bool
	var commitAfter bool
	var commitBatch string
	var undoBatch string
	var sourceLabel string

	i := 1
	for i < len(args) {
		arg := args[i]
		switch arg {
		case "--help", "-h", "help":
			printMigrateHelp()
			return 0
		case "--from":
			if i+1 >= len(args) {
				usererror.Error("--from requires a path")
				return 1
			}
			fromPath = args[i+1]
			i += 2
		case "--dry-run":
			dryRun = true
			i++
		case "--commit":
			commitAfter = true
			i++
		case "--commit-batch":
			if i+1 >= len(args) {
				usererror.Error("--commit-batch requires a batch id")
				return 1
			}
			commitBatch = args[i+1]
			i += 2
		case "--undo":
			if i+1 >= len(args) {
				usererror.Error("--undo requires a batch id")
				return 1
			}
			undoBatch = args[i+1]
			i += 2
		case "--label":
			if i+1 >= len(args) {
				usererror.Error("--label requires a value")
				return 1
			}
			sourceLabel = args[i+1]
			i += 2
		default:
			usererror.Error("Unknown migrate flag: %s", arg)
			printMigrateHelp()
			return 1
		}
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Pure-commit mode: just promote a previously-staged batch.
	if commitBatch != "" {
		n, err := dm.PromoteRawMemoryBatch(commitBatch, dryRun)
		if err != nil {
			usererror.Error("commit failed: %v", err)
			return 1
		}
		fmt.Printf("Promoted %d memories from batch %s.\n", n, commitBatch)
		return 0
	}

	// Pure-undo mode: delegate to ingest's undo.
	if undoBatch != "" {
		return handleIngest([]string{"ingest", "--undo", undoBatch})
	}

	// Stage mode: parse + stage in raw_memories.
	if fromPath == "" {
		usererror.Error("--from <path> is required")
		printMigrateHelp()
		return 1
	}

	format := detectFormat(fromPath)
	if format == "" {
		usererror.Error("could not detect format for %s (supported: .md, .markdown, .json)", fromPath)
		return 1
	}

	batchID := "migrate"
	if sourceLabel != "" {
		batchID = "migrate_" + sanitizeLabel(sourceLabel)
	} else {
		batchID = fmt.Sprintf("migrate_%s_%d", sanitizeLabel(filepath.Base(fromPath)), nowUnix())
	}

	fmt.Printf("mpm migrate: %s (%s format)\n", fromPath, format)
	fmt.Printf("  Batch ID: %s\n", batchID)
	if dryRun {
		fmt.Println("  [DRY RUN] No writes.")
	}

	var stats *mpminternal.MigrateStats
	var err error

	switch format {
	case "markdown":
		stats, err = dm.IngestFromMarkdownFile(fromPath, batchID, dryRun)
	case "json":
		stats, err = dm.IngestFromJsonFile(fromPath, batchID, dryRun)
	default:
		usererror.Error("unsupported format: %s", format)
		return 1
	}

	if err != nil {
		usererror.Error("migration failed: %v", err)
		return 1
	}

	fmt.Printf("  Read:      %d facts\n", stats.RowsRead)
	fmt.Printf("  Staged:    %d\n", stats.RowsStaged)
	fmt.Printf("  Skipped:   %d (already in memories)\n", stats.RowsSkipped)
	fmt.Printf("  Rejected:  %d\n", stats.RowsRejected)

	// Per-entry rejections: surface the file/record/reason so the operator
	// can fix the offending rows.  Only printed when there are any.
	if len(stats.Errors) > 0 {
		fmt.Println("  Per-entry:")
		for _, e := range stats.Errors {
			fmt.Printf("    - %s\n", e)
		}
	}

	if dryRun {
		fmt.Println("\n  Next step: re-run without --dry-run to stage, then use --commit to promote.")
		return 0
	}

	fmt.Printf("\n  Staged in batch %s as pending. Review with:\n", batchID)
	fmt.Printf("    mpm ingest --status\n")
	fmt.Printf("    mpm migrate --commit-batch %s    (promote to memories)\n", batchID)
	fmt.Printf("    mpm migrate --undo %s            (rollback this batch)\n", batchID)

	if commitAfter {
		fmt.Println()
		fmt.Println("  Auto-committing staged batch...")
		n, err := dm.PromoteRawMemoryBatch(batchID, false)
		if err != nil {
			usererror.Error("auto-commit failed: %v", err)
			return 1
		}
		fmt.Printf("  Promoted: %d memories.\n", n)
	}

	return 0
}

func detectFormat(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".markdown":
		return "markdown"
	case ".json":
		return "json"
	}
	return ""
}

func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func nowUnix() int64 {
	return time.Now().Unix()
}

func printMigrateHelp() {
	fmt.Println(`mpm migrate — Import memories from non-SQLite sources (markdown, json)

Usage:
    mpm migrate --from <path> [--dry-run] [--commit] [--label <name>]
    mpm migrate --commit-batch <batch_id> [--dry-run]
    mpm migrate --undo <batch_id>

Flags:
    --from <path>        Source file (markdown or json). Format auto-detected
                         from extension. .md/.markdown → markdown parser.
                         .json → JSON parser (array of objects, or wrapped
                         {"memories"|"facts"|"items"|"entries":[...]}).
                         Per-entry content/fact/text/body required;
                         optional tags (array or comma-string), weight (1-100),
                         ttl, source_id. Per-entry content capped at 256 KiB;
                         file capped at 5 MiB.
    --dry-run            Parse + stage but write nothing. Shows counts.
    --commit             Stage then immediately promote pending rows to memories.
                         Skips the review step.
    --label <name>       Source label used in the batch ID (e.g. "hermes-memories").
                         Defaults to the basename of the source file.
    --commit-batch <id>  Promote a previously-staged batch to memories.
    --undo <batch_id>    Rollback a previously-staged batch (delegates to
                         mpm ingest --undo).

Pipeline:
    migrate --from        → parse → stage in raw_memories (status=pending)
    migrate --commit-batch → promote pending rows → write memories → mark approved
    migrate --undo         → delete pending/approved rows for batch

Examples:
    # Dry-run: see what would be staged, no writes
    mpm migrate --from ~/.hermes/memories/MEMORY.md --dry-run

    # Stage then commit in one shot
    mpm migrate --from ~/.hermes/memories/MEMORY.md --commit

    # Two-phase: stage, review, then commit
    mpm migrate --from docs/notes.md
    mpm ingest --status
    mpm migrate --commit-batch migrate_notes_md_1773907957

    # JSON: array-of-objects
    mpm migrate --from lessons.json --commit

    # JSON: wrapped object
    mpm migrate --from export.json --commit    # accepts {"memories": [...]},
                                               # {"facts": [...]},
                                               # {"items": [...]},
                                               # {"entries": [...]}

Notes:
    - Dedup is via content_hash; facts already in memories are skipped.
    - Tags extracted from "Tags: a, b, c" lines in markdown, plus the heading
      slug, plus "migrated" provenance.
    - Weight defaults to 5 (mid-low) unless overridden by "Weight: N" line.
    - For SQLite sources (OpenClaw DB), keep using 'mpm ingest --source'.`)
}