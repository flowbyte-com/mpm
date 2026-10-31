package internal

// phase2_best_effort_tail_repro_test.go — issue #178, phase 2: the
// best-effort tail.
//
// These writes stay best-effort (no signature change, no new failure
// mode for the caller) — the fix is a statement-level retry budget so
// a transient contention window does not silently drop them. Each test
// holds the WAL write lock, runs the real function, releases only
// after the retry engages, and asserts the effect landed.

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRepro_DeleteOrphanWakeSurvivesContention pins the orphan-wake
// cleanup: a delete that does not land leaves a duplicate wake that
// only the delivery cap bounds.
func TestRepro_DeleteOrphanWakeSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	if _, err := dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by) VALUES ('repro-orphan', 0, 'r', 0, 't')`); err != nil {
		t.Fatalf("seeding the orphan wake: %v", err)
	}
	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		deleteOrphanWake(dm, map[string]interface{}{"id": "repro-orphan"})
	}()

	waitFor(t, 60*time.Second, "the orphan-delete retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("deleteOrphanWake never completed")
	}

	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM scheduled_wakes WHERE id = 'repro-orphan'`).Scan(&n); err != nil {
		t.Fatalf("counting the orphan wake: %v", err)
	}
	if n != 0 {
		t.Fatalf("the orphan wake survived contention")
	}
}

// TestRepro_PromoteMalformedRowSurvivesContention pins the staging
// rejection write: a row the importer already decided to reject must
// actually be marked rejected, even when the write meets contention
// that outlasts the busy timeout. The hold (6s) exceeds the 5s
// busy_timeout, so a bare write cannot survive it — only the retry
// budget gets the row marked.
func TestRepro_PromoteMalformedRowSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	if _, err := dm.db.Exec(
		`INSERT INTO raw_memories (id, source_id, source_db, content_hash, text, metadata, status, ingested_at, updated_at)
		 VALUES ('repro-bad', 's', 'd', 'h', 'text', 'not-json{{{', 'pending', 0, 0)`); err != nil {
		t.Fatalf("seeding the malformed staging row: %v", err)
	}
	release := holdWALWriteLock(t, competitor)
	defer release()

	type result struct {
		promoted int
		err      error
	}
	done := make(chan result, 1)
	go func() {
		p, err := dm.PromoteRawMemoryBatch("", false)
		done <- result{p, err}
	}()

	// Hold past the busy timeout, then release: a bare write fails at
	// 5s; a guarded write retries into the released lock.
	time.Sleep(6 * time.Second)
	release()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("PromoteRawMemoryBatch must survive contention, got: %v", r.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("PromoteRawMemoryBatch never completed")
	}

	var status string
	if err := dm.db.QueryRow(
		`SELECT status FROM raw_memories WHERE id = 'repro-bad'`).Scan(&status); err != nil {
		t.Fatalf("reading the staging row: %v", err)
	}
	if status != "rejected" {
		t.Fatalf("the malformed row was not rejected under contention, status = %q", status)
	}
}

// TestRepro_LoadSessionsFromDirSurvivesContention pins the session
// import write: a transcript that parses must actually land its row.
func TestRepro_LoadSessionsFromDirSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	store := &MemoryStore{DB: &SQLiteConnection{DB: dm.db}, DM: dm}
	dir := t.TempDir()
	line := `{"timestamp":"2026-10-20T12:00:00Z","type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]},"modelId":"m","provider":"p"}`
	if err := os.WriteFile(filepath.Join(dir, "sess1.jsonl"), []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := store.LoadSessionsFromDir(dir)
		done <- err
	}()

	waitFor(t, 60*time.Second, "the session-import retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("LoadSessionsFromDir must survive contention, got: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("LoadSessionsFromDir never completed")
	}

	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE session_id = 'sess1'`).Scan(&n); err != nil {
		t.Fatalf("counting the session row: %v", err)
	}
	if n != 1 {
		t.Fatalf("the session row was lost under contention")
	}
}

// TestRepro_RecordReferenceInteractionSurvivesContention pins the
// admission audit write: best-effort, but not silently dropped by a
// transient lock.
func TestRepro_RecordReferenceInteractionSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	if _, err := dm.db.Exec(
		`INSERT INTO reference_docs (id, title, content) VALUES ('doc1', 't', 'c')`); err != nil {
		t.Fatalf("seeding the reference doc: %v", err)
	}
	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var chunkID *string
		dm.recordReferenceInteraction("doc1", chunkID, "q", "kind", 1, sql.NullFloat64{})
	}()

	waitFor(t, 60*time.Second, "the interaction retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("recordReferenceInteraction never completed")
	}

	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM reference_interactions WHERE doc_id = 'doc1'`).Scan(&n); err != nil {
		t.Fatalf("counting the interaction row: %v", err)
	}
	if n != 1 {
		t.Fatalf("the interaction row was lost under contention")
	}
}
