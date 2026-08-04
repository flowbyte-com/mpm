package main

import (
	"context"
	"flag"
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleBackfillSnapshots derives _epistemic_snapshot blocks for
// memories saved before the snapshot resolver existed.
//
// Usage:
//
//	mpm ops backfill-snapshots [--batch-size=500] [--dry-run]
//
// Flags:
//
//	--batch-size <n>   Memories per transaction (default 500,
//	                   max 10000). Override for resource-constrained
//	                   VMs or large migrations.
//	--dry-run          Count candidates and preview the first few
//	                   derived snapshots; don't write anything.
//
// See internal/core/backfill_snapshots.go for the implementation.
func handleBackfillSnapshots(args []string) int {
	fs := flag.NewFlagSet("backfill-snapshots", flag.ContinueOnError)
	batchSize := fs.Int("batch-size", mpminternal.DefaultBackfillBatchSize,
		"Memories per transaction (default 500, max 10000)")
	dryRun := fs.Bool("dry-run", false,
		"Count only and preview derived snapshots; don't write")
	fs.Usage = func() {
		fmt.Println("Usage: mpm ops backfill-snapshots [flags]")
		fmt.Println("\nFlags:")
		fmt.Println("  --batch-size <n>   Memories per batch transaction (default 500)")
		fmt.Println("  --dry-run           Count and preview only; no writes")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}
	defer dm.Close()

	report, err := mpminternal.BackfillSnapshots(context.Background(), dm, mpminternal.BackfillOptions{
		BatchSize: *batchSize,
		DryRun:    *dryRun,
	})
	if err != nil {
		usererror.Error("backfill: %v", err)
		return 1
	}

	fmt.Printf("\n%s\n", report.BackfillSummarize())

	if report.Errors > 0 {
		fmt.Printf("⚠ %d errors encountered (logged); inspect with `mpm ops logs`\n", report.Errors)
	}

	if report.Partial > 0 {
		fmt.Printf("ℹ %d partial snapshots: creator stamped without session_id (legacy sessions weren't tracked)\n", report.Partial)
	}

	return 0
}