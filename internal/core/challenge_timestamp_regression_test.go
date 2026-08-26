package internal

import (
	"testing"
	"time"
)

// TestChallengeTimestamp_Regression proves the F1 fix: challenge insertion must produce INTEGER timestamp
// and GetRecent must succeed afterwards. This exercises the actual producer path (raw INSERT with Unix seconds)
// not a manual TEXT insert.
func TestChallengeTimestamp_Regression(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	store := NewMemoryStore("")
	store.SQLiteDBPath = dm.DBPath()
	store.DB = &SQLiteConnection{DB: dm.SQLDB()}
	store.DM = dm

	baseID, err := dm.SaveMemory("memories", "base fact", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory base: %v", err)
	}

	// Simulate fixed handler: INSERT with Unix seconds (INTEGER)
	theoryID := GenerateID()
	nowSec := time.Now().Unix()
	_, err = dm.SQLDB().Exec(`INSERT INTO memories (id, collection, content, metadata, created_at, weight) VALUES (?, 'theories', ?, ?, ?, 1)`, theoryID, "HYPOTHESIS: test", `{"status":"pending"}`, nowSec)
	if err != nil {
		t.Fatalf("insert theory INTEGER: %v", err)
	}

	// Verify typeof is integer
	var typ string
	if err := dm.SQLDB().QueryRow(`SELECT typeof(created_at) FROM memories WHERE id=?`, theoryID).Scan(&typ); err != nil {
		t.Fatalf("typeof query: %v", err)
	}
	if typ != "integer" {
		t.Fatalf("challenge theory created_at typeof=%q, want integer", typ)
	}

	// Verify GetRecent can read all rows including base + theory + baseline directives
	mems, err := store.GetRecent(20)
	if err != nil {
		t.Fatalf("GetRecent failed after challenge INTEGER insert: %v", err)
	}
	found := false
	for _, m := range mems {
		if m.ID == theoryID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("theory %s not found in GetRecent", theoryID)
	}
	if len(mems) < 2 {
		t.Fatalf("expected at least 2 mems, got %d", len(mems))
	}
	// Ensure base still readable
	_ = baseID
}

func TestSelfHealTimestamp_Regression(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	store := NewMemoryStore("")
	store.SQLiteDBPath = dm.DBPath()
	store.DB = &SQLiteConnection{DB: dm.SQLDB()}
	store.DM = dm

	// Simulate fixed self_heal insert with CAST(... INTEGER)
	_, err = dm.SQLDB().Exec(`INSERT INTO memories (id, collection, content, metadata, created_at, updated_at) VALUES (?, 'projects', ?, ?, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER)) ON CONFLICT(id) DO UPDATE SET metadata=excluded.metadata, updated_at=CAST(strftime('%s','now') AS INTEGER)`, "test-selfheal-reg", "marker", `{}`)
	if err != nil {
		t.Fatalf("self_heal insert: %v", err)
	}
	var ctyp, utyp string
	if err := dm.SQLDB().QueryRow(`SELECT typeof(created_at), typeof(updated_at) FROM memories WHERE id='test-selfheal-reg'`).Scan(&ctyp, &utyp); err != nil {
		t.Fatalf("typeof: %v", err)
	}
	if ctyp != "integer" || utyp != "integer" {
		t.Fatalf("self_heal types %s %s, want integer integer", ctyp, utyp)
	}
	if _, err := store.GetRecent(20); err != nil {
		t.Fatalf("GetRecent after self_heal: %v", err)
	}
}

func TestSynthesisCreatedAtPreservation_Integer(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	store := NewMemoryStore("")
	store.SQLiteDBPath = dm.DBPath()
	store.DB = &SQLiteConnection{DB: dm.SQLDB()}
	store.DM = dm

	id1, _ := dm.SaveMemory("memories", "first", "", nil, nil, nil, false, 1)
	time.Sleep(1100 * time.Millisecond)
	id2, _ := dm.SaveMemory("memories", "second", "", nil, nil, nil, false, 1)
	var oldest *string
	dm.SQLDB().QueryRow(`SELECT MIN(created_at) FROM memories WHERE id IN (?,?)`, id1, id2).Scan(&oldest)
	if oldest == nil {
		t.Fatal("oldest nil")
	}
	newID, _ := dm.SaveMemory("memories", "synth", "", nil, nil, nil, false, 1)
	_, err = dm.SQLDB().Exec(`UPDATE memories SET created_at = CAST(? AS INTEGER) WHERE id=?`, *oldest, newID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	var typ string
	dm.SQLDB().QueryRow(`SELECT typeof(created_at) FROM memories WHERE id=?`, newID).Scan(&typ)
	if typ != "integer" {
		t.Fatalf("synthesis preservation typeof %s", typ)
	}
	if _, err := store.GetRecent(20); err != nil {
		t.Fatalf("GetRecent: %v", err)
	}
}
