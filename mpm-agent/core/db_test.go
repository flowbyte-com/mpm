package core

import (
	"path/filepath"
	"testing"
)

func TestMiniBotDBInit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mini-bot.db")

	err := InitMiniBotDB(dbPath)
	if err != nil {
		t.Fatalf("InitMiniBotDB failed: %v", err)
	}

	db, err := OpenDBForPath(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tables := []string{"memories", "sessions", "lessons", "anchors", "tools"}
	for _, table := range tables {
		row := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table)
		var name string
		if err := row.Scan(&name); err != nil {
			t.Errorf("table %s not found: %v", table, err)
		}
	}
}