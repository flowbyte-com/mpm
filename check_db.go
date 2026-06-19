//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"os"
	"os/exec"
)

func main() {
	// Use the same path resolution as recall.go
	mpmDir := "/home/v/.openclaw/workspace/flowbyte/mpm"
	dbPath := mpmDir + "/src/db/mpm.db"
	
	fmt.Printf("DB path: %s\n", dbPath)
	
	// Check if file exists
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Printf("DB file does not exist: %v\n", err)
		return
	}
	
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Printf("DB open error: %v\n", err)
		return
	}
	defer db.Close()
	
	// Check if we can query
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL").Scan(&count)
	if err != nil {
		fmt.Printf("Query error: %v\n", err)
		return
	}
	fmt.Printf("Total non-deleted memories: %d\n", count)
	
	// Try the LIKE query from recall
	var id, content string
	likePattern := "%fox%"
	err = db.QueryRow("SELECT id, content FROM memories WHERE deleted_at IS NULL AND content LIKE ? LIMIT 1", likePattern).Scan(&id, &content)
	if err != nil {
		fmt.Printf("LIKE query error: %v\n", err)
	} else {
		fmt.Printf("LIKE query found: id=%s, content=%s...\n", id, content[:100])
	}
	
	// Check FTS5 table
	var ftsCount int
	err = db.QueryRow("SELECT COUNT(*) FROM memories_fts").Scan(&ftsCount)
	if err != nil {
		fmt.Printf("FTS query error: %v\n", err)
	} else {
		fmt.Printf("FTS rows: %d\n", ftsCount)
	}
	
	// Try a simple FTS5 query
	rows, err := db.Query("SELECT * FROM memories_fts LIMIT 2")
	if err != nil {
		fmt.Printf("FTS select error: %v\n", err)
	} else {
		fmt.Println("FTS query works!")
		rows.Close()
	}
	
	// Use mpm itself to check
	cmd := exec.Command("./mpm", "memory", "list")
	cmd.Dir = mpmDir
	output, _ := cmd.CombinedOutput()
	fmt.Printf("\nmpm memory list output:\n%s\n", string(output[:min(500, len(output))]))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
