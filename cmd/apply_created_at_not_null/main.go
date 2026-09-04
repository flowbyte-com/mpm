// One-shot operator tool: invoke EnforceMemoriesCreatedAtNotNull
// against the live workspace mpm.db. The migration is wired into
// initUnifiedSchema so it auto-applies on every daemon restart,
// but the daemon was already running when this fix landed and we
// want the schema hardening applied without bouncing the scheduler.
//
// Usage:
//   MPM_DB_PATH=/home/v/workspace/projects/mpm/src/db/mpm.db go run ./cmd/apply_created_at_not_null
//
// Or just run it from the repo root after the binary is built:
//   go run ./cmd/apply_created_at_not_null
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

func defaultDBPath() string {
	if v := os.Getenv("MPM_DB_PATH"); v != "" {
		return v
	}
	return "/home/v/workspace/projects/mpm/src/db/mpm.db"
}