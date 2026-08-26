package internal

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// buildLegacyPreAffinityFixture constructs a faithful pre-affinity-migration
// database: the D2 reproduction fixture from the 2026-08-25 Stage 5 audit.
//
// Shape (pre-timestamps_unified_v1 / pre-affinity-rebuild):
//   - memories declares DATETIME timestamp columns and stores TEXT values
//     like '2026-04-01 08:00:00'
//   - lessons exists as a plain TABLE (not yet lessons_base + view)
//   - NO FTS tables, NO views, NO INSTEAD OF triggers
//
// Regression context: migrateLessonsToView created INSTEAD OF triggers whose
// bodies reference lessons_fts before lessons_fts existed; the subsequent
// RebuildMemoriesColumnAffinity ALTER RENAME forced a schema re-parse and
// failed hard ("error in trigger lessons_instead_of_insert: no such table:
// main.lessons_fts"), wedging every future invocation behind
// "failed to initialize schema".
func buildLegacyPreAffinityFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "src", "db", "mpm.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()

	ddl := []string{
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, content TEXT NOT NULL,
			content_hash TEXT UNIQUE NOT NULL,
			created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
			source_path TEXT, metadata JSON, embedding BLOB
		);`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_session_id ON sessions(session_id);`,
		`CREATE TABLE topics (
			id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT,
			parent_topic_id TEXT, tags JSON, is_active INTEGER DEFAULT 1,
			created_at INTEGER, updated_at INTEGER, embedding BLOB
		);`,
		`CREATE TABLE topic_memberships (
			memory_id TEXT, session_id TEXT, topic_id TEXT NOT NULL,
			role TEXT DEFAULT 'related', created_at INTEGER,
			PRIMARY KEY (memory_id, topic_id)
		);`,
		`CREATE TABLE memories (
			id TEXT PRIMARY KEY, collection TEXT NOT NULL, content TEXT NOT NULL,
			session_id TEXT, tags JSON, metadata JSON, embedding BLOB,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			source_db TEXT, source_id TEXT, promoted_at REAL
		);
		ALTER TABLE memories ADD COLUMN weight REAL DEFAULT 1;
		ALTER TABLE memories ADD COLUMN deleted_at DATETIME;
		ALTER TABLE memories ADD COLUMN reinforcement_count INTEGER DEFAULT 0;
		ALTER TABLE memories ADD COLUMN is_long_term INTEGER DEFAULT 0;
		ALTER TABLE memories ADD COLUMN is_global INTEGER DEFAULT 0;
		ALTER TABLE memories ADD COLUMN last_accessed_at DATETIME;
		ALTER TABLE memories ADD COLUMN expires_at DATETIME;`,
		`CREATE TABLE system_config (
			key TEXT PRIMARY KEY, raw_json TEXT NOT NULL, content_hash TEXT NOT NULL,
			updated_at INTEGER
		);`,
		`CREATE TABLE schema_migrations (id TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);`,
		`CREATE TABLE lessons (
			id TEXT PRIMARY KEY, type TEXT NOT NULL DEFAULT 'insight',
			content TEXT NOT NULL, tags JSON,
			reinforcement_count INTEGER DEFAULT 1, source_session_id TEXT,
			created TEXT NOT NULL, content_hash TEXT
		);`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture DDL: %v\nstmt: %s", err, stmt)
		}
	}

	rows := []struct{ q string }{
		{`INSERT INTO sessions VALUES ('row1','s1','did things','hash1',1780000000,NULL,NULL,NULL);`},
		// m1: full live row with all four INTEGER-target timestamps as legacy TEXT
		// NOTE: session_id must satisfy the memories→sessions(id) FK.
		{`INSERT INTO memories (id,collection,content,tags,metadata,session_id,created_at,updated_at,last_accessed_at)
		  VALUES ('m1','memories','pre-upgrade memory alpha','[]','{}','row1',
		          '2026-04-01 08:00:00','2026-04-02 09:00:00','2026-04-03 10:00:00');`},
		// m2: NULL last_accessed_at + NULL deleted_at (nullable columns)
		{`INSERT INTO memories (id,collection,content,tags,metadata,session_id,created_at,updated_at,last_accessed_at)
		  VALUES ('m2','memories','pre-upgrade memory beta','[]','{}','row1',
		          '2026-05-01 08:00:00','2026-05-02 09:00:00',NULL);`},
		// m3: soft-deleted row with legacy TEXT deleted_at
		{`INSERT INTO memories (id,collection,content,tags,metadata,session_id,created_at,deleted_at)
		  VALUES ('m3','memories','pre-upgrade memory gamma (deleted)','[]','{}','row1',
		          '2026-03-01 08:00:00','2026-06-01 12:00:00');`},
		{`INSERT INTO lessons VALUES ('l1','warning','pre-upgrade lesson','[]',2,NULL,'2026-04-05T10:00:00Z','h1');`},
	}
	for _, r := range rows {
		if _, err := db.Exec(r.q); err != nil {
			t.Fatalf("fixture seed: %v\nq: %s", err, r.q)
		}
	}
	return root
}

// TestLegacyUpgrade_FullBootAfterAffinityRebuild is the D2 regression test:
// a faithful pre-migration DB must survive NewDatabaseManager (the real
// production boot path), come out usable, keep correct converted timestamps,
// support ordinary memory operations, and remain idempotent on reopen.
func TestLegacyUpgrade_FullBootAfterAffinityRebuild(t *testing.T) {
	root := buildLegacyPreAffinityFixture(t)

	// --- first startup: must not fail ---
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("first startup failed (D2 wedge): %v", err)
	}

	// --- dependent schema objects must all exist ---
	var count int
	for _, probe := range []string{
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lessons_base'`,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='view' AND name='lessons'`,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='lessons_fts'`,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='lessons_instead_of_insert'`,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memories_fts'`,
	} {
		if err := dm.SQLDB().QueryRow(probe).Scan(&count); err != nil {
			t.Fatalf("probe %q: %v", probe, err)
		}
		if count != 1 {
			t.Errorf("object missing after migration: %s", probe)
		}
	}

	// --- timestamp values converted correctly (TEXT datetime → epoch) ---
	type tsRow struct {
		id         string
		createdAt  sql.NullInt64
		updatedAt  sql.NullInt64
		deletedAt  sql.NullInt64
		lastAccess sql.NullInt64
	}
	rows := map[string]tsRow{}
	rs, err := dm.SQLDB().Query(`SELECT id, created_at, updated_at, deleted_at, last_accessed_at FROM memories ORDER BY id`)
	if err != nil {
		t.Fatalf("timestamp query: %v", err)
	}
	for rs.Next() {
		var r tsRow
		if err := rs.Scan(&r.id, &r.createdAt, &r.updatedAt, &r.deletedAt, &r.lastAccess); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows[r.id] = r
	}
	rs.Close()

	// 2026-04-01 08:00:00 etc: SQLite's strftime('%s', …) parses legacy
	// datetime strings as UTC, so the expected epoch is the UTC interpretation.
	expect := func(datetime string) int64 {
		t.Helper()
		tt, err := time.Parse("2006-01-02 15:04:05", datetime)
		if err != nil {
			t.Fatalf("parse %q: %v", datetime, err)
		}
		return tt.Unix()
	}
	if got := rows["m1"]; !got.createdAt.Valid || got.createdAt.Int64 != expect("2026-04-01 08:00:00") {
		t.Errorf("m1.created_at = %v, want %d", got.createdAt, expect("2026-04-01 08:00:00"))
	}
	if got := rows["m1"]; !got.lastAccess.Valid || got.lastAccess.Int64 != expect("2026-04-03 10:00:00") {
		t.Errorf("m1.last_accessed_at = %v, want %d", got.lastAccess, expect("2026-04-03 10:00:00"))
	}
	if got := rows["m3"]; !got.deletedAt.Valid || got.deletedAt.Int64 != expect("2026-06-01 12:00:00") {
		t.Errorf("m3.deleted_at = %v, want %d", got.deletedAt, expect("2026-06-01 12:00:00"))
	}
	if got := rows["m2"]; got.lastAccess.Valid && got.lastAccess.Int64 != 0 {
		t.Errorf("m2.last_accessed_at should stay NULL after rebuild, got %d", got.lastAccess.Int64)
	}
	// D2 liveness invariant: legacy rows had deleted_at = NULL; the rebuild
	// must not fabricate 0 (which `deleted_at IS NULL` readers treat as
	// soft-deleted — silent data loss).
	for _, id := range []string{"m1", "m2"} {
		if got := rows[id]; got.deletedAt.Valid {
			t.Errorf("%s.deleted_at = %d after rebuild, want NULL (live row must stay live)", id, got.deletedAt.Int64)
		}
	}
	var liveCount int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE content LIKE 'pre-upgrade memory%' AND deleted_at IS NULL`).Scan(&liveCount); err != nil {
		t.Fatalf("live count: %v", err)
	}
	if liveCount != 2 {
		t.Errorf("live pre-upgrade rows after first boot = %d, want 2", liveCount)
	}
	// Boot-time directive seeding must land integer timestamps on the
	// rebuilt table (canonical DEFAULT restored), matching fresh installs.
	var seedBad int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id LIKE 'mpm-seed-%' AND (typeof(created_at)!='integer' OR created_at IS NULL)`).Scan(&seedBad); err != nil {
		t.Fatalf("seed ts probe: %v", err)
	}
	if seedBad != 0 {
		t.Errorf("%d seeded directives lack integer created_at on rebuilt table", seedBad)
	}
	// deleted_at / last_accessed_at / expires_at must have NO default:
	// a seeded row omitting them must stay live.
	var seedDeleted int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id LIKE 'mpm-seed-%' AND deleted_at IS NOT NULL`).Scan(&seedDeleted); err != nil {
		t.Fatalf("seed liveness probe: %v", err)
	}
	if seedDeleted != 0 {
		t.Errorf("%d seeded directives are self-soft-deleted (deleted_at default regression)", seedDeleted)
	}
	// Storage types must be INTEGER, not TEXT.
	var badTypes int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE typeof(created_at)!='integer' OR typeof(updated_at)!='integer'`).Scan(&badTypes); err != nil {
		t.Fatalf("typeof check: %v", err)
	}
	if badTypes != 0 {
		t.Errorf("%d rows still have non-INTEGER created_at/updated_at storage", badTypes)
	}

	// --- sentinels written ---
	for _, sentinel := range []string{"deleted_at_unified_v1", "timestamps_unified_v1", memoriesColumnAffinitySentinel} {
		if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE id=?`, sentinel).Scan(&count); err != nil {
			t.Fatalf("sentinel probe %s: %v", sentinel, err)
		}
		if count != 1 {
			t.Errorf("sentinel %s not recorded", sentinel)
		}
	}

	// --- ordinary memory operations work through canonical paths ---
	id, err := dm.SaveMemory("memories", "post-upgrade write probe", "", nil, nil, nil, false, 1)
	if err != nil {
		t.Fatalf("SaveMemory post-upgrade: %v", err)
	}
	mem, err := dm.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory post-upgrade: %v", err)
	}
	if mem["content"] != "post-upgrade write probe" {
		t.Errorf("post-upgrade read-back mismatch: %v", mem["content"])
	}

	// Lessons flow exercises the migrated view + INSTEAD OF triggers + FTS sync.
	lesson, err := dm.AddLesson("post-upgrade lesson probe", LessonTypeWarning, []string{"d2"}, "sess")
	if err != nil {
		t.Fatalf("AddLesson via migrated view: %v", err)
	}
	var lFts int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM lessons_fts WHERE content LIKE 'post-upgrade lesson probe%'`).Scan(&lFts); err != nil {
		t.Fatalf("lessons_fts probe: %v", err)
	}
	if lFts == 0 {
		t.Errorf("lesson insert did not reach lessons_fts (INSTEAD OF trigger broken)")
	}
	_ = lesson

	if err := dm.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// --- second startup: idempotent, data intact ---
	dm2, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("second startup failed: %v", err)
	}
	defer dm2.Close()
	var liveCount2 int
	if err := dm2.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE content LIKE 'pre-upgrade memory%' AND deleted_at IS NULL`).Scan(&liveCount2); err != nil {
		t.Fatalf("reopen count: %v", err)
	}
	if liveCount2 != 2 {
		t.Errorf("live pre-upgrade rows after reopen = %d, want 2", liveCount2)
	}
	// Reopen must not fabricate deleted_at on the still-live rows.
	var resurrected int
	if err := dm2.SQLDB().QueryRow(`SELECT COUNT(*) FROM memories WHERE id IN ('m1','m2') AND deleted_at IS NOT NULL`).Scan(&resurrected); err != nil {
		t.Fatalf("deleted_at drift probe: %v", err)
	}
	if resurrected != 0 {
		t.Errorf("%d formerly-live rows gained deleted_at across boots", resurrected)
	}

	// --- third startup for good measure (idempotence stability) ---
	dm3, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("third startup failed: %v", err)
	}
	dm3.Close()
}

// TestFreshDB_BootStillWorks guards the fix's flip side: the early
// lessons_fts creation must not break or duplicate state on a fresh install.
func TestFreshDB_BootStillWorks(t *testing.T) {
	root := t.TempDir()
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("fresh boot: %v", err)
	}
	defer dm.Close()

	var count int
	// Exactly one lessons_fts (IF NOT EXISTS dedup between early creation and
	// initFTSTables).
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='lessons_fts'`).Scan(&count); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if count != 1 {
		t.Errorf("lessons_fts count = %d, want 1", count)
	}

	if _, err := dm.SaveMemory("memories", "fresh boot probe", "", nil, nil, nil, false, 1); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	hits, err := dm.SearchMemories("fresh boot probe", "", false, 5, 0)
	if err != nil {
		t.Fatalf("SearchMemories: %v", err)
	}
	if len(hits) == 0 {
		t.Errorf("search returned no hits for freshly saved memory")
	}
}
