package internal

import (
	"database/sql"
	"testing"
)

// TestDeletedAt_SetProducesInteger verifies the repaired write shape
// `CAST(strftime('%s','now') AS INTEGER)` stores INTEGER and is scannable
// via sql.NullInt64 (the reader in db.go:4046 GetMemoryRevision).
//
// Note: SQLite INTEGER affinity would already coerce a numeric TEXT like
// `strftime('%s','now')` → INTEGER on storage, so the bare shape also
// appears as integer today. The CAST repair makes the type explicit and
// matches the schema invariant (INTEGER Unix epoch) rather than relying on
// affinity. A non-numeric TEXT such as CURRENT_TIMESTAMP
// ('2026-08-25 19:00:00') would stay TEXT and fail the scan — that is the
// load-bearing failure the CAST guards against.
func TestDeletedAt_SetProducesInteger(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	id, err := dm.SaveMemory("memories", "deleted_at regression probe", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	// Repaired shape: CAST wrapper → INTEGER
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`, id); err != nil {
		t.Fatalf("repaired UPDATE: %v", err)
	}
	var typ string
	if err := dm.SQLDB().QueryRow(`SELECT typeof(deleted_at) FROM memories WHERE id = ?`, id).Scan(&typ); err != nil {
		t.Fatalf("typeof: %v", err)
	}
	if typ != "integer" {
		t.Fatalf("deleted_at typeof=%q, want integer (repaired shape)", typ)
	}
	// Reader that defeated F-01: db.go:4046 NullInt64 scan must succeed.
	var deletedAt sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("NullInt64 scan failed on repaired INTEGER deleted_at: %v", err)
	}
	if !deletedAt.Valid || deletedAt.Int64 == 0 {
		t.Fatalf("NullInt64 valid=%v val=%d, want valid non-zero", deletedAt.Valid, deletedAt.Int64)
	}

	// Negative control: non-numeric TEXT (the shape CURRENT_TIMESTAMP
	// produces) must be TEXT and must fail the NullInt64 scan. Proves
	// that relying on bare TEXT writes would be fragile.
	id2, _ := dm.SaveMemory("memories", "deleted_at TEXT negative control", "", nil, nil, nil, false, 1)
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET deleted_at = '2026-08-25 19:00:00' WHERE id = ?`, id2); err != nil {
		t.Fatalf("TEXT UPDATE: %v", err)
	}
	var typ2 string
	_ = dm.SQLDB().QueryRow(`SELECT typeof(deleted_at) FROM memories WHERE id = ?`, id2).Scan(&typ2)
	if typ2 != "text" {
		t.Fatalf("negative control: TEXT literal typeof=%q, want text", typ2)
	}
	var v sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id2).Scan(&v); err == nil {
		t.Fatalf("negative control: expected NullInt64 scan to fail on TEXT deleted_at, got valid=%v val=%d", v.Valid, v.Int64)
	}
}

// TestDeletedAt_MemoryStoreDeleteProducesInteger exercises the actual
// MemoryStore.DeleteMemory write path (internal/core/memory.go:2160) which
// was repaired from `strftime('%s','now')` to `CAST(strftime… AS INTEGER)`.
// It verifies the semantic (row disappears from GetRecent / WHERE deleted_at
// IS NULL) still holds, plus the storage-type invariant.
func TestDeletedAt_MemoryStoreDeleteProducesInteger(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	store := NewMemoryStore("")
	store.SQLiteDBPath = dm.DBPath()
	store.DB = &SQLiteConnection{DB: dm.SQLDB()}
	store.DM = dm

	id, err := dm.SaveMemory("memories", "store delete probe", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	if err := store.DeleteMemory(id, "memories"); err != nil {
		t.Fatalf("DeleteMemory: %v", err)
	}

	var typ string
	if err := dm.SQLDB().QueryRow(`SELECT typeof(deleted_at) FROM memories WHERE id = ?`, id).Scan(&typ); err != nil {
		t.Fatalf("typeof after DeleteMemory: %v", err)
	}
	if typ != "integer" {
		t.Fatalf("DeleteMemory deleted_at typeof=%q, want integer", typ)
	}
	var deletedAt sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("NullInt64 scan after DeleteMemory: %v", err)
	}
	if !deletedAt.Valid {
		t.Fatalf("deleted_at not valid after DeleteMemory")
	}
	// Semantic: soft-deleted row must be excluded from active queries.
	var cnt int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ? AND deleted_at IS NULL`, id).Scan(&cnt); err != nil {
		t.Fatalf("active check: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("soft-deleted row still counted as active")
	}
}

// TestDeletedAt_SkillShredProducesInteger covers skill_db.go:534 ShredSkill.
func TestDeletedAt_SkillShredProducesInteger(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	id, err := dm.SaveMemory("skills", "skill shred probe", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory skills: %v", err)
	}

	if err := dm.ShredSkill(id); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}
	var typ string
	if err := dm.SQLDB().QueryRow(`SELECT typeof(deleted_at) FROM memories WHERE id = ?`, id).Scan(&typ); err != nil {
		t.Fatalf("typeof after ShredSkill: %v", err)
	}
	if typ != "integer" {
		t.Fatalf("ShredSkill deleted_at typeof=%q, want integer", typ)
	}
	var v sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT deleted_at FROM memories WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("NullInt64 scan after ShredSkill: %v", err)
	}
	if !v.Valid {
		t.Fatalf("deleted_at not valid after ShredSkill")
	}
}

// TestLastSynthesizedAt_CooldownProducesInteger covers
// internal/core/synthesis_auto.go:246 markMemorySynthCooldown, repaired from
// `strftime('%s','now')` to `CAST(strftime… AS INTEGER)`.
func TestLastSynthesizedAt_CooldownProducesInteger(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	id, err := dm.SaveMemory("memories", "last_synth probe", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	// Repaired shape directly (markMemorySynthCooldown is unexported; test the SQL it now uses)
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET last_synthesized_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`, id); err != nil {
		t.Fatalf("repaired UPDATE last_synthesized_at: %v", err)
	}
	var typ string
	if err := dm.SQLDB().QueryRow(`SELECT typeof(last_synthesized_at) FROM memories WHERE id = ?`, id).Scan(&typ); err != nil {
		t.Fatalf("typeof last_synthesized_at: %v", err)
	}
	if typ != "integer" {
		t.Fatalf("last_synthesized_at typeof=%q, want integer", typ)
	}
	var v sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT last_synthesized_at FROM memories WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("NullInt64 scan last_synthesized_at: %v", err)
	}
	if !v.Valid || v.Int64 == 0 {
		t.Fatalf("NullInt64 valid=%v val=%d, want valid non-zero", v.Valid, v.Int64)
	}

	// Negative control: non-numeric TEXT would be TEXT and fail the scan.
	// Proves the CAST guards against the non-numeric TEXT class.
	id2, _ := dm.SaveMemory("memories", "last_synth TEXT control", "", nil, nil, nil, false, 1)
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET last_synthesized_at = '2026-08-25 19:00:00' WHERE id = ?`, id2); err != nil {
		t.Fatalf("TEXT UPDATE: %v", err)
	}
	var typ2 string
	_ = dm.SQLDB().QueryRow(`SELECT typeof(last_synthesized_at) FROM memories WHERE id = ?`, id2).Scan(&typ2)
	if typ2 != "text" {
		t.Fatalf("negative control typeof=%q, want text", typ2)
	}
	var v2 sql.NullInt64
	if err := dm.SQLDB().QueryRow(`SELECT last_synthesized_at FROM memories WHERE id = ?`, id2).Scan(&v2); err == nil {
		t.Fatalf("negative control: expected NullInt64 scan to fail on TEXT last_synthesized_at")
	}
}
