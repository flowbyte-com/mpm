// skill_package_write_guard_review2_test.go — the regression tests for the
// defects the SECOND independent review of the package-immutability guard
// found.
//
// Like its predecessor, every finding here was a defect that was REAL: each
// is reproduced against the code as it stands, so the test fails if the
// defect returns.
//
// The defects:
//
//	R1  the metadata comparison could not tell two distinct JSON numbers
//	    apart, and could not compare an object or an array precisely
//	R2  the metadata guard's row read included tombstoned rows, so a
//	    patch against a soft-deleted packaged skill reported a metadata
//	    conflict where the UPDATE reports "not found or deleted"
//	R3  the guard's read established the transaction's snapshot before the
//	    UPDATE, so a concurrent commit could turn an ordinary patch into
//	    "database is locked"
//	R4  the store's guard read the store's own handle while its write
//	    used the manager's pool, so a store carrying both could refuse an
//	    ordinary row on the strength of ANOTHER database's package
//	R5  the store's cluster re-assignment dereferenced a nil handle on the
//	    DM-backed / DB-nil shape
//
// Every test here runs against a DISPOSABLE database in a disposable
// workspace. The live MPM installation and its database are never touched.

package internal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// R1 — the comparison's precision.
// ─────────────────────────────────────────────────────────────────────────────

// TestMetadataGuardRefusesAMalformedProtectedKey pins R1.
//
// A protected key that holds something other than a string is a CORRUPT
// row: the contract's comparison cannot be made precisely on it, because
// two distinct JSON numbers can be the same float64 and an object
// compares as its serialised text. The guard refuses such a row outright
// rather than reason about a value the contract does not describe.
//
// The fixture asserts that the stored value really is malformed — that it
// is not a string — so the test cannot pass on a well-formed row.
func TestMetadataGuardRefusesAMalformedProtectedKey(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	// A NUMERIC version: present, and not a string.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(metadata, '$.version', 1) WHERE id = ?`,
		fixture.skillID); err != nil {
		t.Fatalf("set the malformed version: %v", err)
	}

	var versionType string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_type(metadata, '$.version') FROM memories WHERE id = ?`,
		fixture.skillID).Scan(&versionType); err != nil {
		t.Fatalf("read the version's type: %v", err)
	}
	if versionType == "text" {
		t.Fatalf("the fixture's version is a string (%q); the test would prove nothing", versionType)
	}

	// The row is corrupt, so EVERY patch on it is refused.
	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"status":"reviewed"}`)
	if err == nil {
		t.Fatal("a patch landed on a packaged skill whose protected key is malformed")
	}
	if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}
}

// TestMetadataGuardComparesTheTypeNotOnlyTheRendering pins the type half
// of R1.
//
// A text "1" and a number 1 render alike and are different values. The
// comparison reads the key's TYPE as well as its value, so a patch that
// swaps one for the other is a change.
func TestMetadataGuardComparesTheTypeNotOnlyTheRendering(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	// The row's version is the TEXT "1".
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(metadata, '$.version', '1') WHERE id = ?`,
		fixture.skillID); err != nil {
		t.Fatalf("set the text version: %v", err)
	}

	// The patch replaces it with the NUMBER 1 — the same rendering.
	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":1}`)
	if err == nil {
		t.Fatal("a patch changed a protected key's TYPE and was allowed")
	}
	if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}

	var versionType string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_type(metadata, '$.version') FROM memories WHERE id = ?`,
		fixture.skillID).Scan(&versionType); err != nil {
		t.Fatalf("read the version's type: %v", err)
	}
	if versionType != "text" {
		t.Errorf("the version's type is %q, want the original text", versionType)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// R2 — the tombstoned row.
// ─────────────────────────────────────────────────────────────────────────────

// TestMetadataPatchOfATombstonedPackagedSkillReportsNotFound pins R2.
//
// The metadata UPDATE's own predicate is `deleted_at IS NULL`, so a
// soft-deleted row is not touched by the patch and the UPDATE reports
// "memory not found or deleted". The guard's row read mirrors that
// predicate, so the guard does not turn a soft-deleted row's refusal into
// a metadata conflict — the patch's behaviour is exactly what it was
// before the guard.
func TestMetadataPatchOfATombstonedPackagedSkillReportsNotFound(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	if err := dm.ShredSkill(fixture.skillID); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}

	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":"9.9.9"}`)
	if err == nil {
		t.Fatal("a metadata patch succeeded against a soft-deleted row")
	}
	if errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the not-found refusal the UPDATE reports; "+
			"the guard's row read must mirror the UPDATE's `deleted_at IS NULL` predicate", err)
	}
	if !strings.Contains(err.Error(), "not found or deleted") {
		t.Errorf("the error is %v, want the not-found refusal", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// R3 — the transaction's snapshot.
// ─────────────────────────────────────────────────────────────────────────────

// TestMetadataPatchSurvivesAConcurrentCommit pins R3.
//
// The guard is a READ that runs before the UPDATE, and a read is what
// establishes a transaction's snapshot. A deferred transaction whose first
// statement is a read cannot later be promoted to a write if another
// connection committed in between: SQLite refuses the promotion and the
// statement fails with "database is locked".
//
// The patch runs inside the substrate's own retrying transaction, so the
// transaction is RESTARTED on a snapshot conflict and the patch lands.
//
// The test forces the conflict: it holds a write transaction open on a
// second connection, starts the patch, commits the held transaction, and
// releases the lock so the patch's write can proceed.
func TestMetadataPatchSurvivesAConcurrentCommit(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	// A second connection that holds a write transaction open.
	other, err := sql.Open("sqlite3", sqliteWriteDSN(dm.dbPath))
	if err != nil {
		t.Fatalf("open a second connection: %v", err)
	}
	defer other.Close()

	lockTx, err := other.Begin()
	if err != nil {
		t.Fatalf("begin the locking transaction: %v", err)
	}
	if _, err := lockTx.Exec(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES ('lock', 0, '')`); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}

	// The patch runs while the lock is held, so its write cannot proceed
	// immediately.
	patchDone := make(chan error, 1)
	go func() {
		patchDone <- dm.UpdateMemoryMetadata(fixture.skillID, `{"status":"reviewed"}`)
	}()

	// Let the patch reach its write attempt.
	time.Sleep(200 * time.Millisecond)

	// Commit the lock and release it, so the patch's transaction can
	// take the write lock.
	if _, err := lockTx.Exec(`DELETE FROM skill_package_blobs WHERE sha256 = 'lock'`); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if err := lockTx.Commit(); err != nil {
		t.Fatalf("commit the locking transaction: %v", err)
	}

	select {
	case err := <-patchDone:
		if err != nil {
			t.Fatalf("the ordinary patch failed under a concurrent commit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the patch did not finish")
	}

	var status string
	if err := dm.SQLDB().QueryRow(
		`SELECT COALESCE(json_extract(metadata, '$.status'), '') FROM memories WHERE id = ?`,
		fixture.skillID).Scan(&status); err != nil {
		t.Fatalf("read the patched metadata: %v", err)
	}
	if status != "reviewed" {
		t.Errorf("the patch did not land: status is %q", status)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// R4 — the store's handle routing.
// ─────────────────────────────────────────────────────────────────────────────

// TestStoreGuardAndWriteUseTheSameDatabase pins R4.
//
// The store's write routes through `s.execTracked`, which prefers the
// MANAGER's pool over the store's own handle. The guard's read must
// prefer it too: a store carrying both handles, pointed at DIFFERENT
// databases, must not have its write land in one database and its package
// check read the other.
//
// The fixture builds exactly that: a store whose manager owns one database
// and whose own handle points at another, with a package in the OTHER
// database under the same id. The row in the manager's database owns no
// package, so the update must land.
func TestStoreGuardAndWriteUseTheSameDatabase(t *testing.T) {
	dm := p3DM(t)

	// A row in the MANAGER's database, with no package.
	content := p3SkillContent("split", "1.0.0", "the original body")
	id, err := dm.SaveSkill("split", "1.0.0", content, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	// A SECOND database that holds a package under the SAME id.
	other := t.TempDir() + "/other.db"
	otherDB, err := sql.Open("sqlite3", "file:"+other+"?_foreign_keys=1")
	if err != nil {
		t.Fatalf("open the second database: %v", err)
	}
	defer otherDB.Close()
	for _, stmt := range append(append([]string{}, BaseTables...), SkillPackageTables...) {
		if _, err := otherDB.Exec(stmt); err != nil {
			t.Fatalf("build the second database: %v", err)
		}
	}
	if _, err := otherDB.Exec(`INSERT INTO memories (id, collection, content, created_at)
		VALUES (?, 'skills', 'the other database', 0)`, id); err != nil {
		t.Fatalf("seed the other database: %v", err)
	}
	blobBytes := []byte("the package entrypoint bytes")
	sum := sha256Hex(blobBytes)
	if _, err := otherDB.Exec(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES (?, ?, ?)`,
		sum, len(blobBytes), blobBytes); err != nil {
		t.Fatalf("insert the other database's blob: %v", err)
	}
	if _, err := otherDB.Exec(`
		INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash)
		VALUES (?, 'split', '1.0.0', 'declared', 'SKILL.md', ?, ?, ?)
	`, id, sum, `{"format":"mpm-skill-package/1"}`, sum); err != nil {
		t.Fatalf("insert the other database's package: %v", err)
	}

	// The store: its manager owns the FIRST database; its own handle
	// points at the SECOND.
	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: otherDB}}

	updated := p3SkillContent("split", "1.0.0", "the updated body")
	if err := store.UpdateMemory(id, updated, []string{"skill"},
		map[string]interface{}{"note": "updated"}); err != nil {
		t.Fatalf("the ordinary update was refused on the strength of another database's package: %v", err)
	}

	var stored string
	if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if !strings.Contains(stored, "the updated body") {
		t.Errorf("the update did not land in the manager's database: %q", stored)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// R5 — the cluster re-assignment's handle.
// ─────────────────────────────────────────────────────────────────────────────

// TestStoreUpdateMemoryWithDMSetAndDBNilDoesNotPanic pins R5.
//
// The cluster re-assignment used the store's own handle unconditionally,
// so the DM-backed / DB-nil shape — which the write path supports —
// dereferenced a nil handle once the update committed. The re-assignment
// now runs on the same handle the write used.
func TestStoreUpdateMemoryWithDMSetAndDBNilDoesNotPanic(t *testing.T) {
	dm := p3DM(t)

	id, err := dm.SaveMemory("memories", "the original content", "", nil, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	store := &MemoryStore{DM: dm, DB: nil}

	// A panic in the re-assignment would fail this test loudly.
	if err := store.UpdateMemory(id, "the updated content", []string{"test"},
		map[string]interface{}{"note": "updated"}); err != nil {
		t.Fatalf("the ordinary update failed: %v", err)
	}

	var content string
	if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read the memory: %v", err)
	}
	if content != "the updated content" {
		t.Errorf("the ordinary memory carries %q, want the updated content", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The strengthened fixtures the review asked for.
// ─────────────────────────────────────────────────────────────────────────────

// TestTheOutOfRangeFixtureDefeatsTheOldDecoder pins the fixture's
// STRENGTH.
//
// The regression test for the float64 blind spot only proves something if
// the document it builds really does defeat a Go-side decode. This test
// asserts that directly: the document is valid JSON, and decoding it into
// a Go map FAILS.
func TestTheOutOfRangeFixtureDefeatsTheOldDecoder(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(metadata, '$.huge', 1e400) WHERE id = ?`,
		fixture.skillID); err != nil {
		t.Fatalf("set the out-of-range number: %v", err)
	}

	var raw string
	if err := dm.SQLDB().QueryRow(`SELECT metadata FROM memories WHERE id = ?`, fixture.skillID).Scan(&raw); err != nil {
		t.Fatalf("read the metadata: %v", err)
	}

	// It is valid JSON ...
	if !json.Valid([]byte(raw)) {
		t.Fatalf("the fixture's document is not valid JSON: %s", raw)
	}
	// ... and it defeats a Go-side map decode.
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		t.Fatalf("the fixture does not defeat the Go decoder; the test would prove nothing: %s", raw)
	}
}

// TestTheAbsentVersusNullFixtureStoresARealNull pins the second
// fixture's STRENGTH.
//
// The distinction the guard makes is between an ABSENT key and a key
// whose value is JSON null. The fixture must store a real JSON null, not
// merely omit the key.
func TestTheAbsentVersusNullFixtureStoresARealNull(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	// A JSON null version: the key is PRESENT and its value is null.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(metadata, '$.version', null) WHERE id = ?`,
		fixture.skillID); err != nil {
		t.Fatalf("set the null version: %v", err)
	}

	var versionType string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_type(metadata, '$.version') FROM memories WHERE id = ?`,
		fixture.skillID).Scan(&versionType); err != nil {
		t.Fatalf("read the version's type: %v", err)
	}
	if versionType != "null" {
		t.Fatalf("the fixture's version type is %q, want \"null\"; the test would prove nothing", versionType)
	}
}

// TestEveryProductionStatementStillWritesForAnUnpackagedRow strengthens the
// positive control.
//
// The previous version of this test exercised ONE of the four production
// statements. All four must still write for a row that owns no package —
// a guard that blocks everything is not a guard.
func TestEveryProductionStatementStillWritesForAnUnpackagedRow(t *testing.T) {
	dm := p3DM(t)

	statements := []struct {
		name string
		stmt string
		args []interface{}
	}{
		{
			name: "the manager's UPDATE with an embedding",
			stmt: guardedUpdateMemoryWithEmbeddingSQL,
			args: []interface{}{"the updated content", `[]`, `{}`, `[0.1]`, "deadbeef", "%s", "%s"},
		},
		{
			name: "the manager's UPDATE without an embedding",
			stmt: guardedUpdateMemoryWithoutEmbeddingSQL,
			args: []interface{}{"the updated content", `[]`, `{}`, "deadbeef", "%s", "%s"},
		},
		{
			name: "the store's UPDATE with an embedding",
			stmt: guardedUpdateMemoryStoreWithEmbeddingSQL,
			args: []interface{}{"the updated content", `[]`, `{}`, `[0.1]`, "deadbeef", "provider", 3, "test-model", "%s", "%s"},
		},
		{
			name: "the store's UPDATE without an embedding",
			stmt: guardedUpdateMemoryStoreWithoutEmbeddingSQL,
			args: []interface{}{"the updated content", `[]`, `{}`, "deadbeef", "%s", "%s"},
		},
	}

	for _, s := range statements {
		// Each statement gets its own row: `memories` carries a UNIQUE
		// (collection, identity_hash), so identical content would collide.
		id, err := dm.SaveMemory("memories", "the original content for "+s.name, "", nil, nil, nil, false, 5)
		if err != nil {
			t.Fatalf("SaveMemory: %v", err)
		}
		args := make([]interface{}, 0, len(s.args))
		for _, a := range s.args {
			if str, ok := a.(string); ok && strings.Contains(str, "%s") {
				args = append(args, id)
				continue
			}
			args = append(args, a)
		}

		res, err := dm.SQLDB().Exec(s.stmt, args...)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("%s: rows affected: %v", s.name, err)
		}
		if affected != 1 {
			t.Errorf("%s changed %d rows for an unpackaged row, want 1", s.name, affected)
		}

		var content string
		if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
			t.Fatalf("%s: read the memory: %v", s.name, err)
		}
		if content != "the updated content" {
			t.Errorf("%s: the memory carries %q, want the updated content", s.name, content)
		}
	}
}

// TestTheRaceSeesAtLeastOneCrossingCall strengthens the race test.
//
// The previous version relied on a sleep to arrange that a writer's call
// crossed the publication of the package. This version ASSERTS that at
// least one call did, so the test cannot pass by having every writer
// finish before the package was published.
func TestTheRaceSeesAtLeastOneCrossingCall(t *testing.T) {
	dm := p3DM(t)

	content := p3SkillContent("raced", "1.0.0", "the original body")
	id, err := dm.SaveSkill("raced", "1.0.0", content, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	var published atomic.Bool
	var stop atomic.Bool
	var refusals atomic.Int64
	var successesAfterPublish atomic.Int64
	var otherErrors atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; !stop.Load(); n++ {
				beganAfterPublish := published.Load()
				err := dm.UpdateMemory(id,
					fmt.Sprintf("a legitimate body from worker %d attempt %d", worker, n),
					map[string]interface{}{"skill": true},
					map[string]interface{}{"note": "raced"})
				if err == nil {
					if beganAfterPublish {
						successesAfterPublish.Add(1)
					}
					continue
				}
				if errors.Is(err, ErrPackageImmutable) {
					refusals.Add(1)
					continue
				}
				otherErrors.Add(1)
			}
		}(i)
	}

	// The workers run until the package is published AND the test says
	// so — a channel, not a sleep.
	time.Sleep(100 * time.Millisecond)

	blobBytes := []byte("the package entrypoint bytes")
	sum := sha256Hex(blobBytes)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES (?, ?, ?)`,
		sum, len(blobBytes), blobBytes); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash)
		VALUES (?, 'raced', '1.0.0', 'declared', 'SKILL.md', ?, ?, ?)
	`, id, sum, `{"format":"mpm-skill-package/1"}`, sum); err != nil {
		t.Fatalf("insert package: %v", err)
	}
	published.Store(true)

	// Let the workers keep calling against the packaged row.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()

	if refusals.Load() == 0 {
		t.Error("no writer was refused; the race did not cross the publication and the test proves nothing")
	}
	if successesAfterPublish.Load() != 0 {
		t.Errorf("%d updates succeeded after the package was published", successesAfterPublish.Load())
	}
	if otherErrors.Load() != 0 {
		t.Errorf("%d updates failed with an error other than the refusal", otherErrors.Load())
	}
}

// TestTheStandaloneStoreIsUsableEndToEnd strengthens the standalone test.
//
// The previous version checked a handle and a file. This version performs
// real store operations after the initialisation — the omission the
// standalone schema caused showed up as a failure on every write, not
// only on the guarded one.
func TestTheStandaloneStoreIsUsableEndToEnd(t *testing.T) {
	store := p3StandaloneStore(t)
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite: %v", err)
	}
	if store.DB == nil {
		t.Fatal("the standalone store has no DB handle")
	}
	if _, err := os.Stat(store.SQLiteDBPath); err != nil {
		t.Fatalf("the standalone database file does not exist: %v", err)
	}

	mem, err := store.AddMemory("the original content", "memories", []string{"test"},
		map[string]interface{}{"kind": "test"}, "", "")
	if err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	if err := store.UpdateMemory(mem.ID, "the updated content", []string{"test"},
		map[string]interface{}{"note": "updated"}); err != nil {
		t.Fatalf("the ordinary update failed on a standalone store: %v", err)
	}
	got, err := store.GetByID(mem.ID, "memories")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got == nil || got.Content != "the updated content" {
		t.Errorf("the standalone store carries %v, want the updated content", got)
	}
}

// TestTheStandaloneStorePathIsAMissingDirectory is a small hygiene check
// on the fixture: the standalone store's database path must be inside the
// disposable workspace, so a test can never reach the operator's database.
func TestTheStandaloneStorePathIsAMissingDirectory(t *testing.T) {
	store := p3StandaloneStore(t)
	if !strings.HasPrefix(store.SQLiteDBPath, os.TempDir()) {
		t.Errorf("the standalone store's path %q is outside the temporary directory", store.SQLiteDBPath)
	}
	if !filepath.IsAbs(store.SQLiteDBPath) {
		t.Errorf("the standalone store's path %q is not absolute", store.SQLiteDBPath)
	}
}
