// One-shot operator tool: invoke EnforceMemoriesCreatedAtNotNull
// against the live workspace mpm.db. The migration is wired into
// initUnifiedSchema so it auto-applies on every daemon restart,
// but the daemon was already running when this fix landed and we
// want the schema hardening applied without bouncing the scheduler.
//
// Usage:
//
//	MPM_DB_PATH=/path/to/mpm.db go run ./cmd/apply_created_at_not_null
//
// Or just run it from the repo root after the binary is built:
//
//	go run ./cmd/apply_created_at_not_null
//
// The script is intentionally minimal — it opens the DB, calls the
// migration function (same one initUnifiedSchema calls), prints the
// result. Safe to run multiple times (sentinel-guarded).
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"

	"github.com/flowbyte-com/mpm-core"
)

func main() {
	dbPath := flag.String("db", defaultDBPath(), "path to the mpm.db file to harden")
	dryRun := flag.Bool("dry-run", false, "report what would happen without running the migration")
	flag.Parse()

	if _, err := os.Stat(*dbPath); err != nil {
		log.Fatalf("DB not found at %s: %v", *dbPath, err)
	}

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_foreign_keys=1", *dbPath))
	if err != nil {
		log.Fatalf("sql.Open(%s): %v", *dbPath, err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("ping %s: %v", *dbPath, err)
	}

	dm := internal.NewDatabaseManagerForDB(db)
	defer dm.Close()

	// Probe the schema before reporting.
	var preNotNull int
	if err := db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('memories') WHERE name='created_at'`,
	).Scan(&preNotNull); err != nil {
		log.Fatalf("probe memories.created_at: %v", err)
	}
	fmt.Printf("Pre-migration: memories.created_at notnull=%d\n", preNotNull)
	if preNotNull == 1 {
		fmt.Println("Schema already enforces NOT NULL on memories.created_at — nothing to do.")
		return
	}

	if *dryRun {
		fmt.Println("Dry-run: would call EnforceMemoriesCreatedAtNotNull(db).")
		return
	}

	if err := internal.EnforceMemoriesCreatedAtNotNull(db); err != nil {
		log.Fatalf("EnforceMemoriesCreatedAtNotNull: %v", err)
	}

	var postNotNull int
	if err := db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('memories') WHERE name='created_at'`,
	).Scan(&postNotNull); err != nil {
		log.Fatalf("probe post-migration: %v", err)
	}
	fmt.Printf("Post-migration: memories.created_at notnull=%d\n", postNotNull)

	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id='created_at_not_null_v1'`,
	).Scan(&sentinelCount); err != nil {
		log.Fatalf("probe sentinel: %v", err)
	}
	fmt.Printf("Sentinel: created_at_not_null_v1 present=%d\n", sentinelCount)
}

// defaultDBPath returns the canonical mpm.db path used when neither
// the -db flag nor MPM_DB_PATH is supplied.
//
// Resolution order:
//  1. MPM_DB_PATH (explicit operator override)
//  2. $MPM_WORKSPACE/src/db/mpm.db — project-relative, the same
//     canonical layout the rest of MPM uses
//  3. $HOME/.mpm/src/db/mpm.db — the user-level install root, used
//     when neither override is set
//
// The previous behaviour silently fell back to the original author's
// checkout ("/home/v/workspace/projects/mpm/src/db/mpm.db"), which
// pointed at a path that does not exist for any other user. Operators
// who actually want to hit the original author's checkout can still
// do so by setting MPM_DB_PATH explicitly.
func defaultDBPath() string {
	if v := os.Getenv("MPM_DB_PATH"); v != "" {
		return v
	}
	if ws := os.Getenv("MPM_WORKSPACE"); ws != "" {
		return filepath.Join(ws, "src", "db", "mpm.db")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".mpm", "src", "db", "mpm.db")
	}
	// Last-ditch fallback: an empty string lets the -db default
	// surface the missing-config problem via the os.Stat check in
	// main() ("DB not found at : stat: no such file or directory"),
	// which is a more honest error than pointing at someone else's
	// checkout.
	return ""
}
