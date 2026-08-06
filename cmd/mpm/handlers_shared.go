// mpm ops shared — multi-agent shared epistemology status & management.
//
// Phase 1 of the WISHLIST.md arc. Reads MPM_SHARED_DB / MPM_SHARED_READONLY
// env vars and reports whether the shared DB is attached, where, and how
// many rows it currently holds. Future phases add query_global_rules,
// record_global_rule, and promote_to_global here.

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/flowbyte-com/mpm-core/usererror"
)

func handleOpsShared(args []string) int {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Println("Usage: mpm ops shared [status]")
		fmt.Println()
		fmt.Println("Reports the status of the multi-agent shared DB attachment.")
		fmt.Println("Set MPM_SHARED_DB to a file path to enable shared mode.")
		fmt.Println("Set MPM_SHARED_READONLY=1 to attach the shared DB read-only.")
		return 0
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	envPath := os.Getenv("MPM_SHARED_DB")
	attached := dm.SharedAttached()

	fmt.Println("Shared Epistemology — status")
	fmt.Println("─────────────────────────────")
	if envPath == "" {
		fmt.Println("  MPM_SHARED_DB:        (not set — local-only mode)")
	} else {
		fmt.Printf("  MPM_SHARED_DB:        %s\n", envPath)
	}
	if os.Getenv("MPM_SHARED_READONLY") == "1" {
		fmt.Println("  MPM_SHARED_READONLY:  1 (read-only)")
	} else {
		fmt.Println("  MPM_SHARED_READONLY:  (unset — read/write)")
	}

	if attached == "" {
		fmt.Println("  Attached:             no (check MPM_SHARED_DB or see prior warnings)")
		return 0
	}
	fmt.Printf("  Attached:             yes (%s)\n", attached)

	// Count rows in the shared schema's memories table.
	var count int
	row := dm.SQLDB().QueryRow("SELECT COUNT(*) FROM shared.memories")
	if err := row.Scan(&count); err != nil {
		fmt.Printf("  shared.memories:      error: %v\n", err)
		return 0
	}
	fmt.Printf("  shared.memories rows: %d\n", count)

	// Count is_global rows specifically.
	var globalCount int
	row = dm.SQLDB().QueryRow("SELECT COUNT(*) FROM shared.memories WHERE is_global = 1")
	if err := row.Scan(&globalCount); err != nil {
		usererror.Warn("handleOpsShared: failed to count shared.memories is_global rows, defaulting to 0: %v", err)
	} else {
		fmt.Printf("  is_global rows:       %d\n", globalCount)
	}

	// Show file size for operator context.
	if info, err := os.Stat(filepath.Clean(attached)); err == nil {
		fmt.Printf("  shared.db size:       %d bytes\n", info.Size())
	}

	fmt.Println()
	fmt.Println("Next: Phase 2 (read tools) and Phase 3 (operator-gated writes) are")
	fmt.Println("tracked in WISHLIST.md. For now the shared DB exists but no")
	fmt.Println("application code reads or writes to it.")
	return 0
}
