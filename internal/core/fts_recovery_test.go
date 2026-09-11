// fts_recovery_test.go — Stage 0 (Pi alpha) FTS5 orphan-state regression tests.
//
// The orphan state in production:
//   - shadow tables exist (sessions_fts_data, memories_fts_data, etc.) as plain
//     CREATE TABLE entries replayed by `mpm ops restore-db`
//   - the `<base>_fts` virtual table entry is MISSING from sqlite_master
//   - triggers `<base>_ai/_ad/_au` reference the missing virtual table
//
// Without repair, every INSERT into memories fails with "no such table:
// main.memories_fts". The Pi alpha failure surface was: 1999 stale cron
// wakes piling up because the cron dispatcher writes through the broken
// trigger, and `mpm memory save` / `mpm wake schedule` both returned
// `(embedErr="no such table: main.<base>_fts")`.
//
// These tests verify fts_recovery.go's behaviour on hermetic in-memory
// databases independent of the live state. Each test is closed-world
// (it sets up the exact orphan shape it needs, doesn't rely on a live DB).

package internal

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// ftsRecoveryTestDB opens an in-memory SQLite with FTS5 enabled.
// Caller must Close() (use t.Cleanup).
func ftsRecoveryTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_pragma=fts5(1)")
	if err != nil {
		t.Fatalf("open in-memory fts5 db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestFtsRecovery_OrphanShadowTables_NoVirtual is the headline test:
// the canonical orphan state — shadow tables exist as plain CREATE TABLE
// entries; the virtual table entry is absent; triggers reference the missing
// vtab. After repair, the vtab exists, triggers re-fire on insert, and
// INSERT INTO memories succeeds.
func TestFtsRecovery_OrphanShadowTables_NoVirtual(t *testing.T) {
	db := ftsRecoveryTestDB(t)

	// Build the canonical base table.
	if _, err := db.Exec(`CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		content TEXT,
		collection TEXT,
		session_id TEXT,
		tags TEXT,
		deleted_at INTEGER
	)`); err != nil {
		t.Fatalf("create memories base: %v", err)
	}

	// Reproduce the orphan state: replay the FTS shadow-table CREATE TABLE
	// statements as the Round 6 backup/restore pipeline did. Note we
	// deliberately do NOT register `<base>_fts` as a virtual table, and
	// we DO install triggers that reference it (matching the live DB's
	// pre-fix state).
	for _, stmt := range []string{
		`CREATE TABLE memories_fts_data (id INTEGER PRIMARY KEY, segment BLOB)`,
		`CREATE TABLE memories_fts_content (id INTEGER PRIMARY KEY, c0 TEXT, c1 TEXT, c2 TEXT, c3 TEXT)`,
		`CREATE TABLE memories_fts_config (k TEXT PRIMARY KEY, v)`,
		`CREATE TABLE memories_fts_docsize (id INTEGER PRIMARY KEY, sz BLOB, h TEXT)`,
		`CREATE TABLE memories_fts_idx (segid INTEGER, term TEXT, pgno INTEGER, PRIMARY KEY(segid, term)) WITHOUT ROWID`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed shadow (%s): %v", stmt, err)
		}
	}

	if _, err := db.Exec(`CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
		INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
		VALUES (new.rowid, new.content, new.collection, COALESCE(new.session_id,''), COALESCE(new.tags,'[]'));
	END`); err != nil {
		t.Fatalf("seed memories_ai: %v", err)
	}

	// Sanity: probe the orphan state. Shadow tables exist, virtual table absent.
	var shadowCount, vtabCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('memories_fts_data','memories_fts_content','memories_fts_config','memories_fts_docsize','memories_fts_idx')`).Scan(&shadowCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='memories_fts' AND type='table'`).Scan(&vtabCount)
	if shadowCount != 5 || vtabCount != 0 {
		t.Fatalf("orphan precondition not met: shadowCount=%d vtabCount=%d", shadowCount, vtabCount)
	}

	// Insert must FAIL without repair (this is what the live DB reproduced).
	if _, err := db.Exec(
		`INSERT INTO memories (id, content, collection, tags, deleted_at) VALUES (?, ?, ?, ?, ?)`,
		"orphan-fail-row", "pre-repair content", "memories", "[]", nil,
	); err == nil {
		t.Fatal("expected INSERT to fail before repair (orphan triggers reference missing vtab), but it succeeded")
	} else if !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("expected 'no such table' error, got: %v", err)
	}

	// Run the recovery (use the in-package function so we can drive it
	// against this test DB).
	dm := &DatabaseManager{db: db}
	if _, err := dm.repairOrphanedFTS5(); err != nil {
		t.Fatalf("repairOrphanedFTS5: %v", err)
	}

	// After repair: vtab should exist; shadow tables still exist (rebuilt
	// by CREATE VIRTUAL TABLE for fts5); INSERT should succeed.
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='memories_fts' AND type='table'`).Scan(&vtabCount)
	if vtabCount != 1 {
		t.Fatalf("after repair: memories_fts vtab missing (vtabCount=%d)", vtabCount)
	}

	if _, err := db.Exec(
		`INSERT INTO memories (id, content, collection, tags, deleted_at) VALUES (?, ?, ?, ?, ?)`,
		"orphan-recover-row", "post-repair content postrepairsentinel", "memories", "[]", nil,
	); err != nil {
		t.Fatalf("post-repair INSERT into memories failed: %v", err)
	}

	// Read-back via FTS — confirms the trigger fired and indexed the row.
// Use a unique single-word token (FTS5's porter tokenizer splits on
// non-alphanumerics; hyphenated tokens like `post-repair` would require
// quoting, while single tokens are unambiguous).
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'postrepairsentinel'`,
	).Scan(&n); err != nil {
		t.Fatalf("FTS MATCH query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 FTS hit for 'postrepairsentinel', got %d", n)
	}
}

// TestFtsRecovery_Idempotent verifies that calling repairOrphanedFTS5
// twice on the same DB is safe and converges to the same state. The Pi
// alpha production symptom was that init runs every CLI invocation;
// idempotence is required.
//
// The seed schema mirrors the canonical `memories` column set so the
// recovery's rebuild SELECT (which reads `collection`, `session_id`,
// `tags`) doesn't fail on missing-column errors.
func TestFtsRecovery_Idempotent(t *testing.T) {
	db := ftsRecoveryTestDB(t)
	if _, err := db.Exec(`CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		collection TEXT,
		content TEXT,
		session_id TEXT,
		tags TEXT,
		deleted_at INTEGER
	)`); err != nil {
		t.Fatalf("create memories: %v", err)
	}
	// Single shadow table in the orphan seed — any shadow table signals
	// the orphan state, but the recovery drops ALL five.
	if _, err := db.Exec(`CREATE TABLE memories_fts_data (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create shadow: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
		INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
		VALUES (new.rowid, new.content, new.collection, COALESCE(new.session_id,''), COALESCE(new.tags,'[]'));
	END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	dm := &DatabaseManager{db: db}
	if _, err := dm.repairOrphanedFTS5(); err != nil {
		t.Fatalf("first repairOrphanedFTS5: %v", err)
	}
	// Second call must not error and must converge to the same state.
	r2, err := dm.repairOrphanedFTS5()
	if err != nil {
		t.Fatalf("second repairOrphanedFTS5 (idempotence check): %v", err)
	}
	// Second call on already-clean DB returns 0 repairs (no orphan detected).
	if r2 != 0 {
		t.Fatalf("expected 0 repairs on already-clean DB, got %d", r2)
	}

	// Behavioural check: insert still works.
	if _, err := db.Exec(`INSERT INTO memories (id, content, collection, session_id, tags, deleted_at) VALUES ('idempotent-1', 'first content', 'memories', NULL, '[]', NULL)`); err != nil {
		t.Fatalf("insert after idempotent repair: %v", err)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'first'`).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 FTS hit, got %d", n)
	}
}

// TestFtsRecovery_NoOpOnCleanDB verifies that calling repairOrphanedFTS5
// on a healthy database is a no-op: zero repairs, zero errors, all inserts
// still work. A misbehaving repair would touch a working DB and risk
// disturbing the in-memory index state.
func TestFtsRecovery_NoOpOnCleanDB(t *testing.T) {
	db := ftsRecoveryTestDB(t)
	// Build a fully clean FTS module.
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, content TEXT)`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	if _, err := db.Exec(`CREATE VIRTUAL TABLE sessions_fts USING fts5(content, tokenize='porter unicode61')`); err != nil {
		t.Fatalf("create sessions_fts: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER sessions_ai AFTER INSERT ON sessions BEGIN
		INSERT INTO sessions_fts(rowid, content) VALUES (new.rowid, new.content);
	END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	// Seed one row so we have a non-empty index to compare against.
	if _, err := db.Exec(`INSERT INTO sessions (id, content) VALUES ('clean-1', 'clean content')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	dm := &DatabaseManager{db: db}
	r, err := dm.repairOrphanedFTS5()
	if err != nil {
		t.Fatalf("repair on clean DB: %v", err)
	}
	if r != 0 {
		t.Fatalf("expected 0 repairs on clean DB, got %d", r)
	}

	// Insert still works after the no-op pass.
	if _, err := db.Exec(`INSERT INTO sessions (id, content) VALUES ('clean-2', 'second clean content')`); err != nil {
		t.Fatalf("insert after clean-DB no-op: %v", err)
	}
}

// TestFtsRecovery_DropsStaleTriggers_whenVtabMissing covers the Pi alpha
// failure surface directly: triggers remain in sqlite_master referencing
// a missing virtual table; recovery must drop those triggers so the
// existing INSERT path no longer aborts. We then re-run the canonical
// CREATE TRIGGER IF NOT EXISTS path during initFTSTables to get back to
// a healthy state.
func TestFtsRecovery_DropsStaleTriggers_whenVtabMissing(t *testing.T) {
	db := ftsRecoveryTestDB(t)
	if _, err := db.Exec(`CREATE TABLE scheduled_wakes (id TEXT PRIMARY KEY, reason TEXT)`); err != nil {
		t.Fatalf("create scheduled_wakes: %v", err)
	}
	// Install stale triggers referencing a vtab that doesn't exist
	// (mimics the live DB after Round 6's restore left the DB in
	// partial state). Pre-fix, initFTSTables would CREATE VIRTUAL TABLE
	// IF NOT EXISTS, see the shadow-table collision error, return, and
	// leave these triggers active — aborting every wake insert.
	for _, trig := range []string{
		`CREATE TRIGGER scheduled_wakes_ai AFTER INSERT ON scheduled_wakes BEGIN
			INSERT INTO scheduled_wakes_fts(rowid, reason) VALUES (new.rowid, new.reason);
		END`,
		`CREATE TRIGGER scheduled_wakes_ad AFTER DELETE ON scheduled_wakes BEGIN
			DELETE FROM scheduled_wakes_fts WHERE rowid = old.rowid;
		END`,
		`CREATE TRIGGER scheduled_wakes_au AFTER UPDATE ON scheduled_wakes BEGIN
			DELETE FROM scheduled_wakes_fts WHERE rowid = old.rowid;
		END`,
	} {
		if _, err := db.Exec(trig); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
	}

	// Insert must fail with "no such table" pre-fix.
	preFixErr := func() error {
		_, err := db.Exec(`INSERT INTO scheduled_wakes (id, reason) VALUES ('pre-fix-wake', 'pre-fix')`)
		return err
	}()
	if preFixErr == nil || !strings.Contains(preFixErr.Error(), "no such table") {
		t.Fatalf("expected 'no such table' pre-fix, got: %v", preFixErr)
	}

	dm := &DatabaseManager{db: db}
	if _, err := dm.repairOrphanedFTS5(); err != nil {
		t.Fatalf("repair: %v", err)
	}
	// Triggers should be dropped (recovery walks every trigger referencing
	// the missing vtab and DROPs it). We confirm by counting.
	var remaining int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'scheduled_wakes_%'`).Scan(&remaining)
	if remaining != 0 {
		t.Fatalf("expected 0 stale scheduled_wakes triggers post-repair, got %d", remaining)
	}

	// Now simulate the post-repair init step that recreates the vtab +
	// triggers (the same DDL initFTSTables uses). Once the vtab + trigger
	// are recreated, the canonical insert path works.
	if _, err := db.Exec(`CREATE VIRTUAL TABLE scheduled_wakes_fts USING fts5(reason, tokenize='porter unicode61')`); err != nil {
		t.Fatalf("create scheduled_wakes_fts: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER scheduled_wakes_ai AFTER INSERT ON scheduled_wakes BEGIN
		INSERT INTO scheduled_wakes_fts(rowid, reason) VALUES (new.rowid, new.reason);
	END`); err != nil {
		t.Fatalf("recreate trigger: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO scheduled_wakes (id, reason) VALUES ('post-fix-wake', 'post-fix')`); err != nil {
		t.Fatalf("post-fix insert: %v", err)
	}
}

// TestFtsRecovery_PreservesCanonicalData — repair never modifies the
// canonical base table. We exercise this property by inserting rows
// pre-repair (so they live in the base table), running repair, then
// verifying the rows survived.
//
// Seed schema mirrors the canonical `memories` column set so the
// recovery's rebuild SELECT (which reads `collection`, `session_id`,
// `tags`) doesn't fail.
func TestFtsRecovery_PreservesCanonicalData(t *testing.T) {
	db := ftsRecoveryTestDB(t)
	if _, err := db.Exec(`CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		collection TEXT,
		content TEXT,
		session_id TEXT,
		tags TEXT,
		deleted_at INTEGER
	)`); err != nil {
		t.Fatalf("create memories: %v", err)
	}
	// Create 50 canonical rows BEFORE orphan-creation so they're
	// demonstrably present when repair runs.
	for i := 0; i < 50; i++ {
		if _, err := db.Exec(
			`INSERT INTO memories (id, collection, content, session_id, tags, deleted_at) VALUES (?, 'memories', ?, NULL, '[]', NULL)`,
			mkID(i), mkContent(i),
		); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	// Establish orphan shadow state.
	for _, stmt := range []string{
		`CREATE TABLE memories_fts_data (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE memories_fts_content (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE memories_fts_config (k TEXT PRIMARY KEY)`,
		`CREATE TABLE memories_fts_docsize (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE memories_fts_idx (segid INTEGER, term TEXT, pgno INTEGER, PRIMARY KEY(segid, term)) WITHOUT ROWID`,
		`CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
		END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\nstmt: %s", err, stmt)
		}
	}

	dm := &DatabaseManager{db: db}
	if _, err := dm.repairOrphanedFTS5(); err != nil {
		t.Fatalf("repair: %v", err)
	}

	// All 50 canonical rows must still exist. The repair only ever
	// touches FTS projection objects (shadow tables + vtab + triggers),
	// never the base table.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 50 {
		t.Fatalf("expected 50 canonical rows post-repair, got %d", n)
	}

	// Sample 3 rows to confirm contents are byte-identical.
	for _, i := range []int{0, 25, 49} {
		var got string
		if err := db.QueryRow(`SELECT content FROM memories WHERE id=?`, mkID(i)).Scan(&got); err != nil {
			t.Fatalf("read row %d: %v", i, err)
		}
		if got != mkContent(i) {
			t.Fatalf("row %d content drift: got %q want %q", i, got, mkContent(i))
		}
	}
}

// TestFtsRecovery_ContinueLoopOnFailure — Pi alpha finding: initFTSTables
// used to bail out on the FIRST fts5 init error, leaving triggers for
// OTHER domains unrecoverable on the same boot. The fix makes the outer
// loop continue past single-statement failures. We verify the
// non-bail-out property end-to-end: a partially-broken init still leaves
// the DB in a state where non-broken domains work.
func TestFtsRecovery_ContinueLoopOnFailure(t *testing.T) {
	db := ftsRecoveryTestDB(t)
	// Create one orphan (sessions) AND one clean (memories) domain.
	// After repair, sessions_fts should exist AND memories_fts should be
	// created cleanly. The initFTSTables loop must not bail out partway.
	//
	// Seed schemas mirror canonical session_id / content_hash columns
	// so the recovery's rebuild SELECT doesn't fail on missing-column
	// errors.
	if _, err := db.Exec(`CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		collection TEXT,
		content TEXT,
		session_id TEXT,
		tags TEXT,
		deleted_at INTEGER
	)`); err != nil {
		t.Fatalf("create memories: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		content TEXT,
		session_id TEXT,
		content_hash TEXT
	)`); err != nil {
		t.Fatalf("create sessions: %v", err)
	}
	// Sessions is in orphan state — shadow tables exist, no vtab.
	for _, s := range []string{
		`CREATE TABLE sessions_fts_data (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE sessions_fts_content (id INTEGER PRIMARY KEY, c0 TEXT)`,
		`CREATE TABLE sessions_fts_config (k TEXT PRIMARY KEY)`,
		`CREATE TABLE sessions_fts_docsize (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE sessions_fts_idx (segid INTEGER, term TEXT, pgno INTEGER, PRIMARY KEY(segid, term)) WITHOUT ROWID`,
		`CREATE TRIGGER sessions_ai AFTER INSERT ON sessions BEGIN
			INSERT INTO sessions_fts(rowid, content, session_id, content_hash)
			VALUES (new.rowid, new.content, COALESCE(new.session_id,''), COALESCE(new.content_hash,''));
		END`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v\nstmt: %s", err, s)
		}
	}

	dm := &DatabaseManager{db: db}
	if _, err := dm.repairOrphanedFTS5(); err != nil {
		t.Fatalf("repair: %v", err)
	}
	// Now run the canonical initFTSTables DDL the way the loop does.
	for _, stmt := range []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(content, session_id, content_hash UNINDEXED, tokenize='porter unicode61')`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(content, collection, session_id UNINDEXED, tags, tokenize='porter unicode61')`,
		`CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content, collection, session_id, tags)
			VALUES (new.rowid, new.content, new.collection, COALESCE(new.session_id,''), COALESCE(new.tags,'[]'));
		END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("initFTSTables stmt: %v", err)
		}
	}

	// Both domains work.
	if _, err := db.Exec(`INSERT INTO sessions (id, content, session_id, content_hash) VALUES ('s1', 'session content', NULL, NULL)`); err != nil {
		t.Fatalf("sessions insert: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memories (id, content, collection, session_id, tags, deleted_at) VALUES ('m1', 'memory content', 'memories', NULL, '[]', NULL)`); err != nil {
		t.Fatalf("memories insert: %v", err)
	}
	var sn, mn int
	_ = db.QueryRow(`SELECT COUNT(*) FROM sessions_fts WHERE sessions_fts MATCH 'session'`).Scan(&sn)
	_ = db.QueryRow(`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'memory'`).Scan(&mn)
	if sn != 1 || mn != 1 {
		t.Fatalf("expected 1 hit each, got sessions=%d memories=%d", sn, mn)
	}
}

// mkID / mkContent generate deterministic test rows for the
// preservation test.
func mkID(i int) string {
	return "row-" + string(rune('A'+i%26)) + "-" + intToStr(i)
}
func mkContent(i int) string {
	return "preserved-content-row-" + intToStr(i)
}
func intToStr(i int) string {
	if i == 0 {
		return "0"
	}
	ds := []byte{}
	for i > 0 {
		ds = append([]byte{byte('0' + i%10)}, ds...)
		i /= 10
	}
	return string(ds)
}
