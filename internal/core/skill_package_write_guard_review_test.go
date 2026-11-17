// skill_package_write_guard_review_test.go — the regression tests for the
// defects the independent review of the package-immutability guard found.
//
// Every finding in this file is a defect that was REAL: each one is
// reproduced here against the code as it stands, so the test fails if the
// defect returns. They are written as regressions, not as restatements of
// the implementation.
//
// The defects:
//
//	D1  The standalone store's schema had no `skill_packages` table, so
//	    the guard's own WHERE clause broke an ORDINARY update with "no
//	    such table: skill_packages" — the guard broke a write instead of
//	    protecting one.
//
//	D2  A store with `DM` set and `DB` nil dereferenced a nil handle in
//	    the guard's pre-check. The pre-change code executed its UPDATE
//	    through the MANAGER's pool, so that shape worked before.
//
//	D3  The metadata comparison decoded the WHOLE metadata document into a
//	    Go map. A document that is valid JSON but holds a number outside
//	    float64's range fails that decode, and the comparison then saw two
//	    empty maps — so a real change to `version` was ALLOWED.
//
//	D4  The zero-row outcome was explained by a read in a separate
//	    statement, which could see a different snapshot than the write.
//
//	D5  The predicate test re-typed the guarded UPDATE, so it would still
//	    have passed if the guard's condition were deleted from the code.
//
//	D6  The ownership-change test was not a race.
//
// Every test here runs against a DISPOSABLE database in a disposable
// workspace. The live MPM installation and its database are never touched.

package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// D1 — the standalone store's schema.
// ─────────────────────────────────────────────────────────────────────────────

// p3StandaloneStore builds a MemoryStore with NO DatabaseManager behind
// it — the shape a standalone consumer gets — on a database it owns.
//
// It is the shape that exposed the schema omission: the store installs its
// own schema through InitSQLite, and that path did not create the package
// tables.
func p3StandaloneStore(t *testing.T) *MemoryStore {
	t.Helper()
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_SHARED_DB", "")
	return &MemoryStore{
		SQLiteDBPath: filepath.Join(ws, "standalone.db"),
		MirrorFile:   filepath.Join(ws, "mirror.jsonl"),
	}
}

// TestStandaloneStoreHasThePackageTables pins D1 directly.
//
// The guard's condition names `skill_packages` in its own WHERE clause, so
// a schema without that table makes the guard's statement itself fail. The
// standalone path must install the same local-only package tables the
// managed path does.
func TestStandaloneStoreHasThePackageTables(t *testing.T) {
	store := p3StandaloneStore(t)
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite: %v", err)
	}

	for _, table := range []string{"skill_packages", "skill_package_files", "skill_package_blobs"} {
		var n int
		if err := store.DB.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n == 0 {
			t.Errorf("the standalone store has no %s table; the guard's own statement would fail with \"no such table\"", table)
		}
	}
}

// TestStandaloneStoreStillUpdatesAnOrdinaryMemory is the regression that
// matters most for D1.
//
// Before the fix, an ordinary update against a standalone store failed
// with "no such table: skill_packages" — the guard broke a write it was
// never meant to touch. This test pins that an ordinary memory on a
// standalone store is updated exactly as before.
func TestStandaloneStoreStillUpdatesAnOrdinaryMemory(t *testing.T) {
	store := p3StandaloneStore(t)
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite: %v", err)
	}

	mem, err := store.AddMemory("the original content", "memories", []string{"test"},
		map[string]interface{}{"kind": "test"}, "", "")
	if err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	id := mem.ID

	if err := store.UpdateMemory(id, "the updated content", []string{"test"},
		map[string]interface{}{"kind": "test", "note": "updated"}); err != nil {
		t.Fatalf("the ordinary update on a standalone store failed: %v", err)
	}

	var content string
	if err := store.DB.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read the memory: %v", err)
	}
	if content != "the updated content" {
		t.Errorf("the ordinary memory carries %q, want the updated content", content)
	}
}

// TestStandaloneStoreRefusesAPackagedSkill pins that the standalone path
// carries the refusal too — a store without a manager is not an escape
// from the package contract.
func TestStandaloneStoreRefusesAPackagedSkill(t *testing.T) {
	store := p3StandaloneStore(t)
	if err := store.InitSQLite(); err != nil {
		t.Fatalf("InitSQLite: %v", err)
	}

	id := p3SeedStandalonePackagedSkill(t, store, "standalone", "1.0.0")

	var beforeContent string
	if err := store.DB.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&beforeContent); err != nil {
		t.Fatalf("read the skill row: %v", err)
	}

	err := store.UpdateMemory(id, "an OVERWRITTEN body", []string{"skill"},
		map[string]interface{}{"note": "overwritten"})
	if err == nil {
		t.Fatal("the standalone store overwrote a packaged skill")
	}
	if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}

	var afterContent string
	if err := store.DB.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&afterContent); err != nil {
		t.Fatalf("read the skill row: %v", err)
	}
	if afterContent != beforeContent {
		t.Errorf("the refusal changed the content:\n  before: %q\n  after:  %q", beforeContent, afterContent)
	}
}

// p3SeedStandalonePackagedSkill saves a skill through a standalone store
// and gives it a package.
func p3SeedStandalonePackagedSkill(t *testing.T, store *MemoryStore, name, version string) string {
	t.Helper()

	content := p3SkillContent(name, version, "the original body")
	mem, err := store.AddMemory(content, "skills", []string{"skill"}, map[string]interface{}{
		"version":          version,
		"content_hash":     "seed-hash",
		"is_latest":        true,
		"author":           "p3-test-agent",
		"decay_floor_days": 90,
	}, "", "")
	if err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	id := mem.ID

	blobBytes := []byte("the package entrypoint bytes")
	sum := sha256Hex(blobBytes)
	if _, err := store.DB.Exec(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES (?, ?, ?)`,
		sum, len(blobBytes), blobBytes); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	if _, err := store.DB.Exec(`
		INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash)
		VALUES (?, ?, ?, 'declared', 'SKILL.md', ?, ?, ?)
	`, id, name, version, sum, `{"format":"mpm-skill-package/1"}`, sum); err != nil {
		t.Fatalf("insert package: %v", err)
	}
	return id
}

// ─────────────────────────────────────────────────────────────────────────────
// D2 — the DM-set / DB-nil store.
// ─────────────────────────────────────────────────────────────────────────────

// TestMemoryStoreWithDMSetAndDBNilRefusesAPackagedSkill pins D2.
//
// A store backed by a DatabaseManager can have `DM` set and `DB` nil:
// InitSQLite returns early in that case and never assigns DB. The
// pre-change code executed its UPDATE through `s.execTracked`, which
// routes to the MANAGER's pool, so that shape worked. The guard's
// pre-check must follow the same routing.
func TestMemoryStoreWithDMSetAndDBNilRefusesAPackagedSkill(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	store := &MemoryStore{DM: dm, DB: nil}
	if store.DB != nil {
		t.Fatal("the fixture's store has a DB handle; the case under test is DB == nil")
	}

	err := store.UpdateMemory(fixture.skillID, "an OVERWRITTEN body", []string{"skill"},
		map[string]interface{}{"note": "overwritten"})
	if err == nil {
		t.Fatal("the DM-backed store overwrote a packaged skill")
	}
	if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}

	content, _ := p3SkillRowContent(t, dm, fixture.skillID)
	if !strings.Contains(content, "the original body") {
		t.Errorf("the refusal changed the content: %q", content)
	}
}

// TestMemoryStoreWithDMSetAndDBNilStillUpdatesAnOrdinaryMemory pins that
// the DM-set / DB-nil shape still updates an ordinary memory — the shape
// that worked before the guard and must keep working.
func TestMemoryStoreWithDMSetAndDBNilStillUpdatesAnOrdinaryMemory(t *testing.T) {
	dm := p3DM(t)

	id, err := dm.SaveMemory("memories", "the original content", "", nil, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	store := &MemoryStore{DM: dm, DB: nil}
	if err := store.UpdateMemory(id, "the updated content", []string{"test"},
		map[string]interface{}{"note": "updated"}); err != nil {
		t.Fatalf("the ordinary update on a DM-backed store failed: %v", err)
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
// D3 — the metadata comparison must not depend on a Go map decode.
// ─────────────────────────────────────────────────────────────────────────────

// TestMetadataGuardSeesAChangeThroughANumberOutsideFloat64sRange pins D3.
//
// The metadata document is valid JSON, and it holds a number no float64
// can represent. A comparison that decodes the whole document into a Go
// map loses the entire document to that one undecodable number — so it
// sees no `version` before and no `version` after, and reports "no
// change". The rewrite of `version` would be ALLOWED.
//
// The comparison must therefore be SQLite's: it reads the one key it
// cares about, with SQLite's own JSON functions.
func TestMetadataGuardSeesAChangeThroughANumberOutsideFloat64sRange(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	// A number beyond float64's range, in an UNRELATED key. The document
	// stays valid JSON.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(metadata, '$.huge', 1e400) WHERE id = ?`,
		fixture.skillID); err != nil {
		t.Fatalf("set the out-of-range number: %v", err)
	}

	var raw string
	if err := dm.SQLDB().QueryRow(`SELECT metadata FROM memories WHERE id = ?`, fixture.skillID).Scan(&raw); err != nil {
		t.Fatalf("read the metadata: %v", err)
	}
	if !strings.Contains(raw, "huge") {
		t.Fatalf("the fixture carries no out-of-range number: %s", raw)
	}

	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":"9.9.9"}`)
	if err == nil {
		t.Fatal("a metadata patch changed a packaged skill's version through an out-of-range number")
	}
	if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}

	version, _ := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)
	if version != "1.0.0" {
		t.Errorf("the recorded version changed to %q, want 1.0.0", version)
	}
}

// TestMetadataGuardDistinguishesAnAbsentKeyFromAJSONNull pins the second
// half of the value comparison.
//
// SQLite's `json_extract` returns SQL NULL both for an ABSENT path and
// for a path whose value is JSON null. The two are different states, and
// a patch that replaces a protected key with null is a change — the key
// is gone. The comparison reads the key's TYPE as well as its value, so
// it tells them apart.
func TestMetadataGuardDistinguishesAnAbsentKeyFromAJSONNull(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	// The row carries no `content_hash` at all.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_remove(metadata, '$.content_hash') WHERE id = ?`,
		fixture.skillID); err != nil {
		t.Fatalf("remove the content hash: %v", err)
	}

	// A patch that ADDS the key changes the row — from absent to present.
	if err := dm.UpdateMemoryMetadata(fixture.skillID, `{"content_hash":"abc"}`); err == nil {
		t.Error("adding an absent protected key was allowed")
	} else if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}
}

// TestMetadataGuardRefusesAChangeOfType pins that the comparison is
// type-aware: a text version and a numeric version are different values,
// even when they render alike.
func TestMetadataGuardRefusesAChangeOfType(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	if err := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":1}`); err == nil {
		t.Error("a metadata patch changed the protected key's TYPE and was allowed")
	} else if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// D4 — the zero-row explanation must be ONE consistent read.
// ─────────────────────────────────────────────────────────────────────────────

// TestExplainGuardedUpdateDistinguishesTheThreeOutcomes pins D4.
//
// A zero-row guarded UPDATE has two possible causes — the row is gone, or
// a package row appeared — and telling them apart needs a read. The read
// must be consistent: the row's existence and the package's ownership
// are read TOGETHER, so the explanation cannot describe a state the
// write could not have seen.
//
// The third outcome — the row exists and owns no package, yet the guarded
// UPDATE wrote no row — is impossible under the cascade invariant, and is
// reported rather than papered over.
func TestExplainGuardedUpdateDistinguishesTheThreeOutcomes(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	// A packaged skill: the refusal.
	explanation, err := explainGuardedUpdate(dm.SQLDB().Query, fixture.skillID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if explanation.Outcome != guardedOutcomePackageImmutable {
		t.Errorf("the outcome is %v, want the package-immutable outcome", explanation.Outcome)
	}
	if explanation.Package == nil || explanation.Package.name != "packaged" {
		t.Errorf("the explanation carries no package identity: %+v", explanation.Package)
	}

	// A row that does not exist: not found.
	explanation, err = explainGuardedUpdate(dm.SQLDB().Query, "skill:does-not-exist-v1.0.0")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if explanation.Outcome != guardedOutcomeNotFound {
		t.Errorf("the outcome is %v, want the not-found outcome", explanation.Outcome)
	}

	// A row that exists and owns no package: the impossible outcome, which
	// must be reported rather than read as "not found".
	ordinaryID, err := dm.SaveMemory("memories", "an ordinary memory", "", nil, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	explanation, err = explainGuardedUpdate(dm.SQLDB().Query, ordinaryID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if explanation.Outcome != guardedOutcomeUnexplained {
		t.Errorf("the outcome is %v, want the unexplained outcome", explanation.Outcome)
	}
}

// TestExplainGuardedUpdateReadsOneConsistentSnapshot pins the consistency
// property directly.
//
// The explanation's own SELECT reads the row's existence and the package's
// ownership in ONE statement. There is therefore no ordering in which the
// row is present and the package is absent *because the package vanished*
// — the cascade takes the row with it — and the explanation reports that
// combination as the impossible outcome rather than as "not found".
func TestExplainGuardedUpdateReadsOneConsistentSnapshot(t *testing.T) {
	dm := p3DM(t)

	// The memories row is deleted HARD, so the package row goes with it
	// through the foreign key.
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")
	if _, err := dm.SQLDB().Exec(`DELETE FROM memories WHERE id = ?`, fixture.skillID); err != nil {
		t.Fatalf("delete the row: %v", err)
	}

	var packages int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM skill_packages WHERE skill_id = ?`, fixture.skillID).Scan(&packages); err != nil {
		t.Fatalf("count the package rows: %v", err)
	}
	if packages != 0 {
		t.Fatalf("the package row survived the cascade (%d rows); the cascade invariant does not hold", packages)
	}

	explanation, err := explainGuardedUpdate(dm.SQLDB().Query, fixture.skillID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if explanation.Outcome != guardedOutcomeNotFound {
		t.Errorf("the outcome is %v, want the not-found outcome", explanation.Outcome)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// D5 — the predicate test must run the PRODUCTION statement.
// ─────────────────────────────────────────────────────────────────────────────

// TestProductionGuardedStatementsCarryTheGuard pins D5.
//
// The four UPDATE statements the code actually runs are named constants.
// This test executes THOSE CONSTANTS — not a copy of them — against a
// packaged skill, and pins that they change nothing.
//
// A test that re-typed the SQL would still have passed after the guard's
// condition was deleted from the code, because the copy would still
// carry it. A test that runs the constant cannot.
func TestProductionGuardedStatementsCarryTheGuard(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	before := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)

	statements := []struct {
		name  string
		stmt  string
		args  []interface{}
	}{
		{
			name: "the manager's UPDATE with an embedding",
			stmt: guardedUpdateMemoryWithEmbeddingSQL,
			args: []interface{}{"an OVERWRITTEN body", `["skill"]`, `{}`, `[0.1]`, "deadbeef", fixture.skillID, fixture.skillID},
		},
		{
			name: "the manager's UPDATE without an embedding",
			stmt: guardedUpdateMemoryWithoutEmbeddingSQL,
			args: []interface{}{"an OVERWRITTEN body", `["skill"]`, `{}`, "deadbeef", fixture.skillID, fixture.skillID},
		},
		{
			name: "the store's UPDATE with an embedding",
			stmt: guardedUpdateMemoryStoreWithEmbeddingSQL,
			args: []interface{}{"an OVERWRITTEN body", `["skill"]`, `{}`, `[0.1]`, "deadbeef", "provider", 3, "test-model", fixture.skillID, fixture.skillID},
		},
		{
			name: "the store's UPDATE without an embedding",
			stmt: guardedUpdateMemoryStoreWithoutEmbeddingSQL,
			args: []interface{}{"an OVERWRITTEN body", `["skill"]`, `{}`, "deadbeef", fixture.skillID, fixture.skillID},
		},
	}

	for _, s := range statements {
		res, err := dm.SQLDB().Exec(s.stmt, s.args...)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("%s: rows affected: %v", s.name, err)
		}
		if affected != 0 {
			t.Errorf("%s changed %d rows against a packaged skill, want 0", s.name, affected)
		}
	}

	after := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	p3AssertRowStateUnchanged(t, before, after)
}

// TestProductionGuardedStatementsStillWriteForAnUnpackagedRow pins the
// other half: the same constants must still update a row that owns no
// package. A guard that blocks everything is not a guard.
func TestProductionGuardedStatementsStillWriteForAnUnpackagedRow(t *testing.T) {
	dm := p3DM(t)

	id, err := dm.SaveMemory("memories", "the original content", "", nil, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	res, err := dm.SQLDB().Exec(guardedUpdateMemoryWithoutEmbeddingSQL,
		"the updated content", `[]`, `{}`, "deadbeef", id, id)
	if err != nil {
		t.Fatalf("the guarded UPDATE: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if affected != 1 {
		t.Errorf("the guarded UPDATE changed %d rows for an unpackaged row, want 1", affected)
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
// D6 — the ownership-change test must be a RACE.
// ─────────────────────────────────────────────────────────────────────────────

// TestUpdateMemoryOwnershipRaceIsConsistent pins D6 as a real race.
//
// Writers update the skill CONTINUOUSLY while the package is published
// underneath them. Two properties must hold:
//
//  1. once the package row is committed, no update can succeed. A writer
//     whose call BEGAN after the package was published must be refused —
//     if it were not, the guard would be a snapshot check rather than a
//     guard.
//
//  2. the content must not move after the package was published. The
//     write's own WHERE clause excludes a packaged row, so an update
//     that was in flight when the package landed is excluded too.
//
// The consistency assertion is the second one: the content read
// immediately after the package is committed must be the content at the
// end.
func TestUpdateMemoryOwnershipRaceIsConsistent(t *testing.T) {
	dm := p3DM(t)

	content := p3SkillContent("raced", "1.0.0", "the original body")
	id, err := dm.SaveSkill("raced", "1.0.0", content, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	var published atomic.Bool
	var stop atomic.Bool
	var wg sync.WaitGroup

	var mu sync.Mutex
	var successAfterPublish []string
	var unexpectedErrors []string

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
						mu.Lock()
						successAfterPublish = append(successAfterPublish,
							fmt.Sprintf("worker %d attempt %d", worker, n))
						mu.Unlock()
					}
					continue
				}
				if !errors.Is(err, ErrPackageImmutable) {
					mu.Lock()
					unexpectedErrors = append(unexpectedErrors, err.Error())
					mu.Unlock()
				}
			}
		}(i)
	}

	// Let the writers get going, then publish the package under them.
	time.Sleep(150 * time.Millisecond)

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

	// The package is committed from here on.
	published.Store(true)
	contentAtPublish := p3ReadContent(t, dm.SQLDB(), id)

	// Keep the writers running: every one of them must now be refused.
	time.Sleep(400 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	contentAtEnd := p3ReadContent(t, dm.SQLDB(), id)

	mu.Lock()
	defer mu.Unlock()
	if len(successAfterPublish) > 0 {
		t.Errorf("%d updates succeeded AFTER the package was published: %v",
			len(successAfterPublish), successAfterPublish)
	}
	if len(unexpectedErrors) > 0 {
		t.Errorf("%d updates failed with an error other than the refusal: %v",
			len(unexpectedErrors), unexpectedErrors)
	}
	if contentAtEnd != contentAtPublish {
		t.Errorf("the content moved after the package was published:\n  at publish: %q\n  at end:    %q",
			contentAtPublish, contentAtEnd)
	}
}

// p3ReadContent reads one row's content.
func p3ReadContent(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var content string
	if err := db.QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read the content of %s: %v", id, err)
	}
	return content
}

// ─────────────────────────────────────────────────────────────────────────────
// The protected-key list must not be writable from outside.
// ─────────────────────────────────────────────────────────────────────────────

// TestProtectedMetadataKeysAreNotExported is a compile-time statement
// made executable: the list is unexported, so no package in the tree can
// rewrite the guard's protected keys.
//
// The test pins the list's CONTENT, which is what a reader of the code
// needs to know: exactly two keys, and the two the package contract
// names.
func TestProtectedMetadataKeysAreNotExported(t *testing.T) {
	want := []string{"version", "content_hash"}
	if len(packageProtectedMetadataKeys) != len(want) {
		t.Fatalf("the protected key list is %v, want %v", packageProtectedMetadataKeys, want)
	}
	for i, key := range want {
		if packageProtectedMetadataKeys[i] != key {
			t.Errorf("the protected key list is %v, want %v", packageProtectedMetadataKeys, want)
		}
	}
	// `is_latest` is deliberately NOT protected: the substrate's own
	// semver bookkeeping, read by no package check.
	for _, key := range packageProtectedMetadataKeys {
		if key == "is_latest" {
			t.Error("is_latest is protected; the substrate's own save path owns it and no package check reads it")
		}
	}
}
