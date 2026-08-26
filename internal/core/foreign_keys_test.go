package internal

// foreign_keys_test.go — Foreign Key Audit (2026-08-07).
//
// Two guarantees under test, both rooted in the same audit finding:
//
//  1. `_foreign_keys=1` must be hardcoded into the SQLite DSN so that EVERY
//     pooled connection opens with `PRAGMA foreign_keys = ON`. The legacy
//     `db.Exec("PRAGMA foreign_keys = ON")` only covers whichever single
//     connection the Exec grabs — database/sql lazily spins up more
//     connections as concurrency grows, and those open FK-OFF (the SQLite
//     default). Result: cascade deletes silently stop firing under agent
//     concurrency, leaving orphaned evidence, phantom revision rows, and
//     corrupted causal lineages.
//
//  2. The declared ON DELETE CASCADE edges actually fire. memory_revisions
//     references memories with ON DELETE CASCADE; memories.session_id uses
//     ON DELETE SET NULL. The evidence table deliberately has NO FK (it
//     preserves artifact evidence history after deletion — documented at
//     evidence_store.go), so evidence must SURVIVE a memory hard-delete.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// foreignKeyTestDM builds a hermetic production-path DatabaseManager —
// real file DB, created via NewDatabaseManager so the DSN's _foreign_keys=1
// actually applies (NewTestDM skips the DSN; initUnifiedSchema leaves FK
// off for connections whose DSN did not opt in, per the test-path contract
// in initUnifiedSchema).
//
// Refactor 2026-08-12: delegate to NewTestLocalOnlyDM (which sets
// MPM_WORKSPACE under t.TempDir() so the file DB never touches the live
// workspace) but still constructs the DatabaseManager via
// NewDatabaseManager("") to preserve the real DSN with _foreign_keys=1.
// NewTestDM would skip that DSN and silently let FK enforcement drop —
// exactly the regression this test guards against.
func foreignKeyTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("MPM_WORKSPACE", tmp)
	t.Setenv("MPM_SHARED_DB", "")
	dm, err := NewDatabaseManager("")
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// probeFKConns asserts FK enforcement on each of the two pinned physical
// connections: the PRAGMA value must be 1 AND an FK-violating INSERT must be
// rejected. Functional probe catches drivers that ignore the PRAGMA report.
func probeFKConns(t *testing.T, ctx context.Context, conns []*sql.Conn, label string) {
	t.Helper()
	for i, c := range conns {
		name := fmt.Sprintf("%s conn#%d", label, i+1)
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("%s: PRAGMA foreign_keys scan: %v", name, err)
		}
		if fk != 1 {
			t.Errorf("%s: PRAGMA foreign_keys = %d, want 1 — DSN _foreign_keys=1 is not structural", name, fk)
		}
		// Behavioural probe: a memory whose session_id references a
		// nonexistent session must be rejected by FK enforcement.
		badID := "fk-bogus-" + strings.ReplaceAll(label, " ", "-")
		_, err := c.ExecContext(ctx,
			"INSERT INTO memories (id, collection, content, session_id) VALUES (?, 'memories', 'abc', 'no-such-session')",
			badID)
		if err == nil {
			t.Errorf("%s: FK-violating INSERT succeeded — enforcement must be ON", name)
		} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			t.Errorf("%s: expected FOREIGN KEY failure, got: %v", name, err)
		}
	}
}

// pinTwoPooledConns borrows two *sql.Conn from the pool while holding the
// first open, forcing the second to be a physically distinct, freshly
// spawned connection — the exact one a bare `PRAGMA foreign_keys=ON` Exec
// would never have touched.
func pinTwoPooledConns(t *testing.T, db *sql.DB) (*sql.Conn, *sql.Conn) {
	t.Helper()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	ctx := context.Background()
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pool conn 1: %v", err)
	}
	t.Cleanup(func() { c1.Close() })
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pool conn 2: %v", err)
	}
	t.Cleanup(func() { c2.Close() })
	return c1, c2
}

// TestForeignKeyEnforcedOnEveryPooledConnection is the regression test for
// the DSN race: under the pre-fix code (bare PRAGMA Exec covering only the
// first pooled connection), the second physical connection reports
// foreign_keys=0 and accepts the FK-violating insert.
func TestForeignKeyEnforcedOnEveryPooledConnection(t *testing.T) {
	dm := foreignKeyTestDM(t)
	c1, c2 := pinTwoPooledConns(t, dm.SQLDB())
	probeFKConns(t, context.Background(), []*sql.Conn{c1, c2}, "main")
}

// TestNewSession_FKEnforcedOnEveryPooledConnection covers the NewSession
// path — background worker connections (synthesis, idle_dream, critic) —
// so a worker's cascade writes can never run FK-off.
func TestNewSession_FKEnforcedOnEveryPooledConnection(t *testing.T) {
	dm := foreignKeyTestDM(t)
	session, err := dm.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	c1, c2 := pinTwoPooledConns(t, session.SQLDB())
	probeFKConns(t, context.Background(), []*sql.Conn{c1, c2}, "session")
}

// TestMemoryHardDelete_CascadesRevisions is the audit's manual cascade test:
// delete a memory and assert (a) its revisions die with it (FK ON DELETE
// CASCADE firing on a hard delete), and (b) its evidence survives — the
// documented evidence_store design preserves evidence history after
// artifact deletion.
func TestMemoryHardDelete_CascadesRevisions(t *testing.T) {
	dm := foreignKeyTestDM(t)

	revisionCount := func(memoryID string) int {
		var n int
		if err := dm.db.QueryRow(
			"SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?", memoryID,
		).Scan(&n); err != nil {
			t.Fatalf("count revisions: %v", err)
		}
		return n
	}
	evidenceCount := func(artifactID string) int {
		var n int
		if err := dm.db.QueryRow(
			"SELECT COUNT(*) FROM evidence WHERE artifact_id = ?", artifactID,
		).Scan(&n); err != nil {
			t.Fatalf("count evidence: %v", err)
		}
		return n
	}

	// Insert a memory via the real write path (secret/poison scanner +
	// revision trigger on INSERT).
	memID, err := dm.SaveMemoryNode(dm, "memories",
		"FK audit cascade test memory", "", []string{"fk-audit"}, nil, nil,
		false, 5, "", "0.5", "0.5", "")
	if err != nil {
		t.Fatalf("SaveMemoryNode: %v", err)
	}
	if got := revisionCount(memID); got != 1 {
		t.Fatalf("expected 1 revision after insert, got %d", got)
	}

	// Edit → revision 2 (memories_rev_au trigger).
	if _, err := dm.ExecTracked(
		"UPDATE memories SET content = ? WHERE id = ?",
		0, "FK audit cascade test memory (edited)", memID); err != nil {
		t.Fatalf("update memory: %v", err)
	}
	if got := revisionCount(memID); got != 2 {
		t.Fatalf("expected 2 revisions after edit, got %d", got)
	}

	// Evidence — intentionally FK-less; must survive the hard delete.
	if err := AddEvidence(dm, EvidenceInput{
		ArtifactID:         memID,
		ArtifactType:       "memory",
		Type:               "observation",
		SourceGroup:        "git",
		Strength:           0.8,
		IndependenceFactor: 1.0,
		CreatedBy:          "fk-audit",
		CreatedAt:          time.Now(),
	}); err != nil {
		t.Fatalf("AddEvidence: %v", err)
	}
	if got := evidenceCount(memID); got != 1 {
		t.Fatalf("expected 1 evidence row, got %d", got)
	}

	// Soft delete: append-only — revisions SURVIVE (the au trigger logs a
	// deletion revision), the row still exists for point-in-time rebuild.
	if _, err := dm.ExecTracked(
		"UPDATE memories SET deleted_at = ? WHERE id = ?", 0, time.Now().Unix(), memID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if got := revisionCount(memID); got != 3 {
		t.Fatalf("soft delete: expected 3 revisions (append-only), got %d", got)
	}

	// Hard delete: FK ON DELETE CASCADE must remove the revisions.
	if _, err := dm.ExecTracked("DELETE FROM memories WHERE id = ?", 0, memID); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	if got := revisionCount(memID); got != 0 {
		t.Errorf("hard delete: %d memory_revisions survived — ON DELETE CASCADE did NOT fire", got)
	}

	// Evidence is the documented exception: no FK, survives deletion.
	if got := evidenceCount(memID); got != 1 {
		t.Errorf("evidence must survive memory deletion per evidence_store.go design, got %d", got)
	}
}

// TestSessionDelete_CascadesMemorySessionIDNull covers the second declared
// FK action: sessions ON DELETE SET NULL on memories.session_id.
func TestSessionDelete_CascadesMemorySessionIDNull(t *testing.T) {
	dm := foreignKeyTestDM(t)

	sessionID := "fk-audit-session"
	if _, err := dm.ExecTracked(`
		INSERT INTO sessions (id, session_id, content, content_hash)
		VALUES (?, ?, 'sess', 'hash')
	`, 0, sessionID, sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	memID, err := dm.SaveMemoryNode(dm, "memories",
		"session-cascade memory", sessionID, nil, nil, nil,
		false, 5, "", "0.5", "0.5", "")
	if err != nil {
		t.Fatalf("SaveMemoryNode: %v", err)
	}

	if _, err := dm.ExecTracked("DELETE FROM sessions WHERE id = ?", 0, sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	var sid sql.NullString
	if err := dm.db.QueryRow("SELECT session_id FROM memories WHERE id = ?", memID).Scan(&sid); err != nil {
		t.Fatalf("read back memory: %v", err)
	}
	if sid.Valid {
		t.Errorf("DELETE session must SET NULL memories.session_id, got %q", sid.String)
	}
}
