package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	blobstorefs "github.com/flowbyte-com/mpm/internal/blobstore"
	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/mpmcli"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// blobGC_DB allows test injection. If nil, handleBlobGC uses getDB().
var blobGC_DB mpminternal.CoreDB

// handleBlobGC implements `mpm blob gc`.
// Two passes: GCExpired (delete expired blobs) then GCSweepOrphans (delete orphaned files).
func handleBlobGC(args []string) int {
	// Defect 9 (2026-09-13 acceptance): mpm blob gc --help used to
	// execute a non-dry-run GC because the router's isSubcommandHelp
	// fall-through routed "help" to this handler without a short-
	// circuit. Without this guard, `--help` is a destructive mutation
	// vector. (The pre-fix acceptance observed "only showed generic
	// blob help" — but the actual code path runs the GC; with no
	// expired blobs and no orphans, the output is silent and looks
	// like help-only output. The fix ensures `--help` is inert.)
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			printBlobHelp()
			return 0
		}
	}
	dryRun := false
	for _, arg := range args {
		if arg == "--dry-run" || arg == "-n" {
			dryRun = true
		}
	}

	var dm mpminternal.CoreDB
	if blobGC_DB != nil {
		dm = blobGC_DB
	} else {
		dm = getDB()
		if dm == nil {
			return 1
		}
	}

	blobDir := os.Getenv("MPM_BLOB_DIR")
	if blobDir == "" {
		workspace := mpmcli.ResolveWorkspace()
		blobDir = filepath.Join(workspace, "blobs")
	}

	ttl := 24 * time.Hour
	bs, err := blobstorefs.NewFilesystemBackend(dm.SQLDB(), blobDir, ttl)
	if err != nil {
		usererror.Error("blob gc: NewFilesystemBackend: %v", err)
		return 1
	}

	now := time.Now()

	var stats1, stats2 blobstorefs.GCStats

	if !dryRun {
		// Pass 1: GC expired.
		stats1, err = bs.GCExpired(context.Background(), now)
		if err != nil {
			usererror.Error("blob gc: GCExpired: %v", err)
			return 1
		}

		// Pass 2: Sweep orphans.
		grace := time.Hour
		if envGrace := os.Getenv("MPM_BLOB_ORPHAN_GRACE"); envGrace != "" {
			if d, err := time.ParseDuration(envGrace); err == nil && d > 0 {
				grace = d
			}
		}
		stats2, err = bs.GCSweepOrphans(context.Background(), grace)
		if err != nil {
			usererror.Error("blob gc: GCSweepOrphans: %v", err)
			return 1
		}
	}

	if dryRun {
		fmt.Println("Dry run — no changes made")
	}
	fmt.Printf("Expired: deleted=%d, freed_bytes=%d\n", stats1.ExpiredDeleted, stats1.FreedBytes)
	fmt.Printf("Orphans: deleted=%d, skipped_grace=%d\n", stats2.OrphansDeleted, stats2.OrphansSkipped)
	return 0
}
