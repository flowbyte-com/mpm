//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	db, err := sql.Open("sqlite3", "/home/v/.openclaw/workspace/flowbyte/mpm/src/db/mpm.db")
	if err != nil {
		fmt.Printf("DB open error: %v\n", err)
		return
	}
	defer db.Close()

	// Check if memories_fts exists
	var tableCount int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memories_fts'").Scan(&tableCount)
	fmt.Printf("memories_fts exists: %v\n", tableCount > 0)

	// Check FTS5 availability
	var fts5Available bool
	db.QueryRow("SELECT 1 FROM pragma_compile_options WHERE compile_options LIKE '%FTS5%'").Scan(&fts5Available)
	fmt.Printf("FTS5 available: %v\n", fts5Available)

	// Count rows in memories_fts
	var ftsCount int
	db.QueryRow("SELECT COUNT(*) FROM memories_fts").Scan(&ftsCount)
	fmt.Printf("Rows in memories_fts: %d\n", ftsCount)

	// Count rows in memories
	var memCount int
	db.QueryRow("SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL").Scan(&memCount)
	fmt.Printf("Rows in memories (non-deleted): %d\n", memCount)

	// Try a simple FTS5 query
	rows, err := db.Query("SELECT * FROM memories_fts LIMIT 3")
	if err != nil {
		fmt.Printf("FTS5 query error: %v\n", err)
	} else {
		fmt.Println("FTS5 query works!")
		rows.Close()
	}
}
