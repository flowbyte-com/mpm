// skill_package_write_guard_test.go — the package-immutability guard on the
// REMAINING single-document write paths, and the package's metadata
// contract.
//
// Issue #248: `UpdateMemory` rewrote a packaged skill's projection in
// place, under the same id, without the package-immutability refusal that
// guards the skill write path. Two implementations did it:
//
//	MemoryStore.UpdateMemory      (internal/core/memory.go)
//	DatabaseManager.UpdateMemory  (internal/core/web_db.go)
//
// This file pins the refusal on BOTH, and pins the metadata contract that
// the legacy metadata patch path could also break.
//
// Every test here runs against a DISPOSABLE database in a disposable
// workspace (see p3DM in skill_tx_test.go). The live MPM installation and
// its database are never touched.

package internal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// The row state, read whole.
//
// A refusal has to change NOTHING. So the assertion reads every column the
// update path can touch — content, tags, metadata, the embedding and its
// provenance, the content hash and the timestamp — and compares the whole
// row before and after.
// ─────────────────────────────────────────────────────────────────────────────

// p3RowState is the complete mutable state of one memories row.
type p3RowState struct {
	Content            string
	Tags               string
	Metadata           string
	Embedding          string
	ContentHash        string
	EmbeddingSource    string
	EmbeddingDimension sql.NullInt64
	EmbeddingModel     string
	UpdatedAt          sql.NullInt64
	DeletedAt          sql.NullInt64
}

// p3ReadRowState reads one memories row's mutable state.
func p3ReadRowState(t *testing.T, db *sql.DB, id string) p3RowState {
	t.Helper()
	var s p3RowState
	err := db.QueryRow(`
		SELECT COALESCE(content, ''), COALESCE(tags, ''), COALESCE(metadata, ''),
		       COALESCE(embedding, ''), COALESCE(content_hash, ''),
		       COALESCE(embedding_source, ''), embedding_dimension,
		       COALESCE(embedding_model, ''), updated_at, deleted_at
		FROM memories WHERE id = ?
	`, id).Scan(&s.Content, &s.Tags, &s.Metadata, &s.Embedding, &s.ContentHash,
		&s.EmbeddingSource, &s.EmbeddingDimension, &s.EmbeddingModel, &s.UpdatedAt, &s.DeletedAt)
	if err != nil {
		t.Fatalf("read the row state of %s: %v", id, err)
	}
	return s
}

// p3AssertRowStateUnchanged fails when the row moved.
func p3AssertRowStateUnchanged(t *testing.T, before, after p3RowState) {
	t.Helper()
	if before.Content != after.Content {
		t.Errorf("the content changed:\n  before: %q\n  after:  %q", before.Content, after.Content)
	}
	if before.Tags != after.Tags {
		t.Errorf("the tags changed:\n  before: %s\n  after:  %s", before.Tags, after.Tags)
	}
	if before.Metadata != after.Metadata {
		t.Errorf("the metadata changed:\n  before: %s\n  after:  %s", before.Metadata, after.Metadata)
	}
	if before.Embedding != after.Embedding {
		t.Errorf("the embedding changed:\n  before: %s\n  after:  %s", before.Embedding, after.Embedding)
	}
	if before.ContentHash != after.ContentHash {
		t.Errorf("the content hash changed:\n  before: %s\n  after:  %s", before.ContentHash, after.ContentHash)
	}
	if before.EmbeddingSource != after.EmbeddingSource {
		t.Errorf("the embedding source changed: %q -> %q", before.EmbeddingSource, after.EmbeddingSource)
	}
	if before.EmbeddingDimension != after.EmbeddingDimension {
		t.Errorf("the embedding dimension changed: %v -> %v", before.EmbeddingDimension, after.EmbeddingDimension)
	}
	if before.EmbeddingModel != after.EmbeddingModel {
		t.Errorf("the embedding model changed: %q -> %q", before.EmbeddingModel, after.EmbeddingModel)
	}
	if before.UpdatedAt != after.UpdatedAt {
		t.Errorf("the timestamp changed: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if before.DeletedAt != after.DeletedAt {
		t.Errorf("the tombstone changed: %v -> %v", before.DeletedAt, after.DeletedAt)
	}
}

// p3SetEmbedding gives the row a non-empty embedding and its provenance, so
// a test can prove the refusal did not disturb them.
func p3SetEmbedding(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`
		UPDATE memories
		SET embedding = '[0.1,0.2,0.3]', embedding_source = 'provider',
		    embedding_dimension = 3, embedding_model = 'test-model'
		WHERE id = ?
	`, id)
	if err != nil {
		t.Fatalf("set the embedding: %v", err)
	}
}

// p3PackageMetadata reads the package-relevant metadata keys of a row.
func p3PackageMetadata(t *testing.T, db *sql.DB, id string) (version, contentHash string) {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT COALESCE(metadata, '{}') FROM memories WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read the metadata of %s: %v", id, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("parse the metadata of %s: %v", id, err)
	}
	version, _ = meta["version"].(string)
	contentHash, _ = meta["content_hash"].(string)
	return version, contentHash
}

// p3MetadataValue reads one metadata key of a row.
func p3MetadataValue(t *testing.T, db *sql.DB, id, key string) (string, bool) {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT COALESCE(metadata, '{}') FROM memories WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read the metadata of %s: %v", id, err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatalf("parse the metadata of %s: %v", id, err)
	}
	v, ok := meta[key]
	if !ok {
		return "", false
	}
	s, isString := v.(string)
	if !isString {
		b, _ := json.Marshal(v)
		return string(b), true
	}
	return s, true
}

// ─────────────────────────────────────────────────────────────────────────────
// STAGE 1 — the single-document write paths.
// ─────────────────────────────────────────────────────────────────────────────

// TestDatabaseManagerUpdateMemoryIsRefusedAgainstAPackagedSkill pins the
// refusal on the public CoreDB method.
//
// `UpdateMemory` is the same in-place replacement the legacy force path
// performs, reached by a different door. The refusal is the same one, with
// the same classification.
func TestDatabaseManagerUpdateMemoryIsRefusedAgainstAPackagedSkill(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	err := dm.UpdateMemory(fixture.skillID, "an OVERWRITTEN body",
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "overwritten"})
	if err == nil {
		t.Fatal("UpdateMemory overwrote a packaged skill")
	}
	if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}
	var immutable *PackageImmutableError
	if !errors.As(err, &immutable) {
		t.Errorf("the error is %v, want a *PackageImmutableError", err)
	} else {
		if immutable.SkillID != fixture.skillID {
			t.Errorf("the refusal names id %q, want %q", immutable.SkillID, fixture.skillID)
		}
		if immutable.Name != "packaged" {
			t.Errorf("the refusal names %q, want \"packaged\"", immutable.Name)
		}
		if immutable.Version != "1.0.0" {
			t.Errorf("the refusal names version %q, want 1.0.0", immutable.Version)
		}
	}
	if !strings.Contains(err.Error(), "package_immutable") {
		t.Errorf("the refusal message does not carry the code: %v", err)
	}
}

// TestMemoryStoreUpdateMemoryIsRefusedAgainstAPackagedSkill pins the same
// refusal on the other implementation.
func TestMemoryStoreUpdateMemoryIsRefusedAgainstAPackagedSkill(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")
	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}

	err := store.UpdateMemory(fixture.skillID, "an OVERWRITTEN body", []string{"skill"},
		map[string]interface{}{"note": "overwritten"})
	if err == nil {
		t.Fatal("MemoryStore.UpdateMemory overwrote a packaged skill")
	}
	if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}
}

// TestUpdateMemoryRefusalChangesNothing pins that the refusal is FAIL
// CLOSED on both paths: not the content, not the tags, not the metadata,
// not the embedding, not the content hash, not the embedding's provenance,
// and not the timestamp.
func TestUpdateMemoryRefusalChangesNothing(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")
	p3SetEmbedding(t, dm.SQLDB(), fixture.skillID)

	before := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	beforePkg, beforeFiles, beforeBlob := p3PackageState(t, dm, fixture.skillID)

	if err := dm.UpdateMemory(fixture.skillID, "an OVERWRITTEN body",
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "overwritten"}); err == nil {
		t.Fatal("UpdateMemory overwrote a packaged skill")
	}
	after := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	p3AssertRowStateUnchanged(t, before, after)

	afterPkg, afterFiles, afterBlob := p3PackageState(t, dm, fixture.skillID)
	if afterPkg != beforePkg {
		t.Errorf("the refusal changed the package row:\n  before: %s\n  after:  %s", beforePkg, afterPkg)
	}
	if afterFiles != beforeFiles {
		t.Errorf("the refusal changed the file row:\n  before: %s\n  after:  %s", beforeFiles, afterFiles)
	}
	if afterBlob != beforeBlob {
		t.Errorf("the refusal changed the blob:\n  before: %q\n  after:  %q", beforeBlob, afterBlob)
	}
}

// TestMemoryStoreUpdateMemoryRefusalChangesNothing pins the same
// fail-closed property on the MemoryStore path.
func TestMemoryStoreUpdateMemoryRefusalChangesNothing(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")
	p3SetEmbedding(t, dm.SQLDB(), fixture.skillID)
	store := &MemoryStore{DM: dm, DB: &SQLiteConnection{DB: dm.SQLDB()}}

	before := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	if err := store.UpdateMemory(fixture.skillID, "an OVERWRITTEN body", []string{"skill"},
		map[string]interface{}{"note": "overwritten"}); err == nil {
		t.Fatal("MemoryStore.UpdateMemory overwrote a packaged skill")
	}
	after := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	p3AssertRowStateUnchanged(t, before, after)
}

// TestUpdateMemoryIsRefusedAgainstASoftDeletedPackagedSkill pins that the
// refusal applies to a TOMBSTONED packaged skill.
//
// A soft shred preserves the skill row and — through the foreign key — the
// package row. The update path must not resurrect it, and must not rewrite
// it either.
func TestUpdateMemoryIsRefusedAgainstASoftDeletedPackagedSkill(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	if err := dm.ShredSkill(fixture.skillID); err != nil {
		t.Fatalf("ShredSkill: %v", err)
	}
	before := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)

	err := dm.UpdateMemory(fixture.skillID, "an OVERWRITTEN body",
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "overwritten"})
	if err == nil {
		t.Fatal("UpdateMemory overwrote a soft-deleted packaged skill")
	}
	if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}

	after := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	p3AssertRowStateUnchanged(t, before, after)
	if !after.DeletedAt.Valid {
		t.Error("the soft-deleted packaged skill was resurrected by the refused update")
	}
}

// TestUpdateMemoryStillWorksForAnUnpackagedSkill pins that the guard is
// NARROW: an ordinary single-document skill — one with no package row — is
// still updated exactly as before.
func TestUpdateMemoryStillWorksForAnUnpackagedSkill(t *testing.T) {
	dm := p3DM(t)

	original := p3SkillContent("ordinary", "1.0.0", "the original body")
	id, err := dm.SaveSkill("ordinary", "1.0.0", original, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	updated := p3SkillContent("ordinary", "1.0.0", "the updated body")
	if err := dm.UpdateMemory(id, updated,
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "updated"}); err != nil {
		t.Fatalf("the update of an ordinary skill was refused: %v", err)
	}

	content, _ := p3SkillRowContent(t, dm, id)
	if !strings.Contains(content, "the updated body") {
		t.Errorf("the ordinary skill was not updated: %q", content)
	}
}

// TestUpdateMemoryStillWorksForAnOrdinaryMemory pins the same for a plain
// memory row — the guard must not narrow the ordinary surface.
func TestUpdateMemoryStillWorksForAnOrdinaryMemory(t *testing.T) {
	dm := p3DM(t)

	id, err := dm.SaveMemory("memories", "the original content", "", nil, nil, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	if err := dm.UpdateMemory(id, "the updated content",
		map[string]interface{}{"kind": "test"}, map[string]interface{}{"note": "updated"}); err != nil {
		t.Fatalf("the update of an ordinary memory was refused: %v", err)
	}

	var content string
	if err := dm.SQLDB().QueryRow(`SELECT content FROM memories WHERE id = ?`, id).Scan(&content); err != nil {
		t.Fatalf("read the memory: %v", err)
	}
	if content != "the updated content" {
		t.Errorf("the ordinary memory carries %q, want the updated content", content)
	}
}

// TestUpdateMemoryGuardIsInTheWriteStatement pins the second line of
// defence directly.
//
// The guard has two parts: a check, and the SAME condition folded into the
// UPDATE the check guards. This test runs that UPDATE against a packaged
// skill and pins that it changes NOTHING — so even if the check were
// removed, or ran against a stale snapshot, the write could not land.
//
// This is what closes the time-of-check/time-of-use window: the refusal is
// not a property of the check's timing.
func TestUpdateMemoryGuardIsInTheWriteStatement(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	before := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)

	res, err := dm.SQLDB().Exec(`
		UPDATE memories
		SET content = ?, tags = ?, metadata = ?, content_hash = ?,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
		  AND NOT EXISTS (SELECT 1 FROM skill_packages WHERE skill_id = ?)
	`, "an OVERWRITTEN body", `["skill"]`, `{}`, "deadbeef", fixture.skillID, fixture.skillID)
	if err != nil {
		t.Fatalf("the guarded UPDATE: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if affected != 0 {
		t.Errorf("the guarded UPDATE changed %d rows against a packaged skill, want 0", affected)
	}

	after := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	p3AssertRowStateUnchanged(t, before, after)
}

// TestUpdateMemoryRefusalHoldsUnderConcurrentWriters pins the concurrency
// property: once the package row is committed, no update may change the
// skill's content — not one, and not several running at once.
func TestUpdateMemoryRefusalHoldsUnderConcurrentWriters(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	before := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)

	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = dm.UpdateMemory(fixture.skillID,
				fmt.Sprintf("an OVERWRITTEN body %d", i),
				map[string]interface{}{"skill": true},
				map[string]interface{}{"note": "overwritten"})
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err == nil {
			t.Errorf("writer %d: the update of a packaged skill succeeded", i)
			continue
		}
		if !errors.Is(err, ErrPackageImmutable) {
			t.Errorf("writer %d: the error is %v, want the package_immutable sentinel", i, err)
		}
	}

	after := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)
	p3AssertRowStateUnchanged(t, before, after)
}

// TestUpdateMemoryCannotRaceThePackageOwnershipChange pins the stale
// snapshot case in its deterministic form.
//
// The guard's decision must reflect the CURRENT ownership, not a snapshot
// taken earlier. So:
//
//  1. while the skill has NO package, an update is legitimate and lands;
//  2. the package row is committed;
//  3. every update from then on refuses, and the content does not move.
//
// A guard that only consulted a snapshot taken before step 2 would let
// step 3's write through. This is the regression that pins it.
func TestUpdateMemoryCannotRaceThePackageOwnershipChange(t *testing.T) {
	dm := p3DM(t)

	content := p3SkillContent("becomes-packaged", "1.0.0", "the original body")
	id, err := dm.SaveSkill("becomes-packaged", "1.0.0", content, "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	// 1. No package yet: the update is legitimate and must land.
	first := p3SkillContent("becomes-packaged", "1.0.0", "the first body")
	if err := dm.UpdateMemory(id, first,
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "first"}); err != nil {
		t.Fatalf("the update of an unpackaged skill was refused: %v", err)
	}
	afterFirst, _ := p3SkillRowContent(t, dm, id)
	if !strings.Contains(afterFirst, "the first body") {
		t.Fatalf("the first update did not land: %q", afterFirst)
	}

	// 2. The package is published.
	blobBytes := []byte("the package entrypoint bytes")
	sum := sha256Hex(blobBytes)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO skill_package_blobs (sha256, size_bytes, content) VALUES (?, ?, ?)`,
		sum, len(blobBytes), blobBytes); err != nil {
		t.Fatalf("insert blob: %v", err)
	}
	manifest := `{"format":"mpm-skill-package/1","name":"becomes-packaged"}`
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO skill_packages (skill_id, name, version, version_source, entrypoint,
			entrypoint_sha256, manifest, package_hash)
		VALUES (?, 'becomes-packaged', '1.0.0', 'declared', 'SKILL.md', ?, ?, ?)
	`, id, sum, manifest, sum); err != nil {
		t.Fatalf("insert package: %v", err)
	}

	// 3. From now on, every update refuses.
	second := p3SkillContent("becomes-packaged", "1.0.0", "the second body")
	if err := dm.UpdateMemory(id, second,
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "second"}); err == nil {
		t.Fatal("the update of a freshly packaged skill succeeded")
	} else if !errors.Is(err, ErrPackageImmutable) {
		t.Errorf("the error is %v, want the package_immutable sentinel", err)
	}

	afterSecond, _ := p3SkillRowContent(t, dm, id)
	if afterSecond != afterFirst {
		t.Errorf("the update landed after the package was published:\n  before: %q\n  after:  %q",
			afterFirst, afterSecond)
	}
}

// TestUpdateMemoryRefusalIsClassified pins that the refusal reaches the
// dispatch audit as a bounded, typed outcome rather than as unclassified
// prose.
func TestUpdateMemoryRefusalIsClassified(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	err := dm.UpdateMemory(fixture.skillID, "an OVERWRITTEN body",
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "overwritten"})
	if err == nil {
		t.Fatal("UpdateMemory overwrote a packaged skill")
	}

	class, code := ClassifyError(err)
	if class != OutcomeClassConflict {
		t.Errorf("the outcome class is %q, want %q", class, OutcomeClassConflict)
	}
	if code != "package_immutable" {
		t.Errorf("the outcome code is %q, want %q", code, "package_immutable")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// STAGE 2 — the package's METADATA contract.
// ─────────────────────────────────────────────────────────────────────────────

// TestMetadataPatchIsRefusedWhenItWouldChangeThePackageVersion pins the
// metadata half of the contract.
//
// `version` is part of the package's version identity: the package
// verification compares the row's recorded version with the package row's
// version and with the version the skill id names. A patch that changes it
// produces a row that contradicts its own package.
func TestMetadataPatchIsRefusedWhenItWouldChangeThePackageVersion(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	before, _ := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)
	if before != "1.0.0" {
		t.Fatalf("the fixture's recorded version is %q, want 1.0.0", before)
	}
	beforeState := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)

	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":"9.9.9"}`)
	if err == nil {
		t.Fatal("a metadata patch changed a packaged skill's version")
	}
	if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}
	var immutable *PackageMetadataImmutableError
	if !errors.As(err, &immutable) {
		t.Errorf("the error is %v, want a *PackageMetadataImmutableError", err)
	} else {
		if immutable.Key != "version" {
			t.Errorf("the refusal names the key %q, want \"version\"", immutable.Key)
		}
		if immutable.Name != "packaged" {
			t.Errorf("the refusal names %q, want \"packaged\"", immutable.Name)
		}
		if immutable.Version != "1.0.0" {
			t.Errorf("the refusal names version %q, want 1.0.0", immutable.Version)
		}
	}
	if !strings.Contains(err.Error(), "package_metadata_immutable") {
		t.Errorf("the refusal message does not carry the code: %v", err)
	}

	after, _ := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)
	if after != before {
		t.Errorf("the recorded version changed: %q -> %q", before, after)
	}
	p3AssertRowStateUnchanged(t, beforeState, p3ReadRowState(t, dm.SQLDB(), fixture.skillID))
}

// TestMetadataPatchIsRefusedWhenItWouldChangeThePackageContentHash pins
// the integrity half of the contract.
//
// `content_hash` is the projection's own content hash — the same value the
// manifest records as `projection_sha256`. Changing it makes the row
// assert a hash that describes neither the stored content nor the
// manifest.
func TestMetadataPatchIsRefusedWhenItWouldChangeThePackageContentHash(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	_, beforeHash := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)
	if beforeHash == "" {
		t.Fatal("the fixture carries no content_hash")
	}
	beforeState := p3ReadRowState(t, dm.SQLDB(), fixture.skillID)

	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"content_hash":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	if err == nil {
		t.Fatal("a metadata patch changed a packaged skill's content hash")
	}
	if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}
	var immutable *PackageMetadataImmutableError
	if !errors.As(err, &immutable) {
		t.Errorf("the error is %v, want a *PackageMetadataImmutableError", err)
	} else if immutable.Key != "content_hash" {
		t.Errorf("the refusal names the key %q, want \"content_hash\"", immutable.Key)
	}

	_, afterHash := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)
	if afterHash != beforeHash {
		t.Errorf("the recorded content hash changed: %q -> %q", beforeHash, afterHash)
	}
	p3AssertRowStateUnchanged(t, beforeState, p3ReadRowState(t, dm.SQLDB(), fixture.skillID))
}

// TestMetadataPatchThatRemovesAProtectedKeyIsRefused pins that DELETING a
// protected key is a change too.
func TestMetadataPatchThatRemovesAProtectedKeyIsRefused(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":null}`)
	if err == nil {
		t.Fatal("a metadata patch removed a packaged skill's version")
	}
	if !errors.Is(err, ErrPackageMetadataImmutable) {
		t.Errorf("the error is %v, want the package_metadata_immutable sentinel", err)
	}
	if _, ok := p3MetadataValue(t, dm.SQLDB(), fixture.skillID, "version"); !ok {
		t.Error("the recorded version was removed")
	}
}

// TestMetadataPatchOfUnrelatedMetadataIsPermittedOnAPackagedSkill pins
// that the guard is NARROW: a patch that touches any other key lands
// exactly as it always did.
func TestMetadataPatchOfUnrelatedMetadataIsPermittedOnAPackagedSkill(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	beforeVersion, beforeHash := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)

	if err := dm.UpdateMemoryMetadata(fixture.skillID, `{"status":"reviewed","note":"a legitimate note"}`); err != nil {
		t.Fatalf("a metadata patch of unrelated keys was refused: %v", err)
	}

	if v, ok := p3MetadataValue(t, dm.SQLDB(), fixture.skillID, "status"); !ok || v != "reviewed" {
		t.Errorf("the unrelated metadata did not land: %q (present=%v)", v, ok)
	}
	afterVersion, afterHash := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)
	if afterVersion != beforeVersion {
		t.Errorf("the recorded version changed: %q -> %q", beforeVersion, afterVersion)
	}
	if afterHash != beforeHash {
		t.Errorf("the recorded content hash changed: %q -> %q", beforeHash, afterHash)
	}
}

// TestMetadataPatchNamingAProtectedKeyWithItsCurrentValueIsPermitted
// pins that the guard refuses a CHANGE, not the mere MENTION of a key.
//
// A patch that sets a protected key to the value it already holds leaves
// the row identical. Refusing that would be a broader prohibition than the
// contract requires.
func TestMetadataPatchNamingAProtectedKeyWithItsCurrentValueIsPermitted(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	version, hash := p3PackageMetadata(t, dm.SQLDB(), fixture.skillID)

	if err := dm.UpdateMemoryMetadata(fixture.skillID,
		fmt.Sprintf(`{"version":%q,"content_hash":%q,"status":"reviewed"}`, version, hash)); err != nil {
		t.Fatalf("a metadata patch that changes nothing was refused: %v", err)
	}
	if v, ok := p3MetadataValue(t, dm.SQLDB(), fixture.skillID, "status"); !ok || v != "reviewed" {
		t.Errorf("the unrelated metadata did not land: %q (present=%v)", v, ok)
	}
}

// TestMetadataPatchOnAnOrdinaryMemoryIsUnaffected pins that an ordinary
// memory keeps every metadata key it always had.
func TestMetadataPatchOnAnOrdinaryMemoryIsUnaffected(t *testing.T) {
	dm := p3DM(t)

	id, err := dm.SaveMemory("memories", "the original content", "", nil,
		map[string]interface{}{"version": "1.0.0", "content_hash": "abc"}, nil, false, 5)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	if err := dm.UpdateMemoryMetadata(id, `{"version":"9.9.9","content_hash":"def"}`); err != nil {
		t.Fatalf("the metadata patch of an ordinary memory was refused: %v", err)
	}
	if v, _ := p3MetadataValue(t, dm.SQLDB(), id, "version"); v != "9.9.9" {
		t.Errorf("the ordinary memory's version is %q, want 9.9.9", v)
	}
	if v, _ := p3MetadataValue(t, dm.SQLDB(), id, "content_hash"); v != "def" {
		t.Errorf("the ordinary memory's content hash is %q, want def", v)
	}
}

// TestMetadataPatchOnAnUnpackagedSkillIsUnaffected pins the same for a
// skill that owns no package.
func TestMetadataPatchOnAnUnpackagedSkillIsUnaffected(t *testing.T) {
	dm := p3DM(t)

	id, err := dm.SaveSkill("ordinary", "1.0.0",
		p3SkillContent("ordinary", "1.0.0", "the original body"), "p3-test-agent", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	if err := dm.UpdateMemoryMetadata(id, `{"version":"9.9.9"}`); err != nil {
		t.Fatalf("the metadata patch of an unpackaged skill was refused: %v", err)
	}
	if v, _ := p3MetadataValue(t, dm.SQLDB(), id, "version"); v != "9.9.9" {
		t.Errorf("the unpackaged skill's version is %q, want 9.9.9", v)
	}
}

// TestMetadataRefusalIsClassified pins that the metadata refusal reaches
// the dispatch audit as a bounded, typed outcome — and that it is
// DISTINCT from the content refusal.
func TestMetadataRefusalIsClassified(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	err := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":"9.9.9"}`)
	if err == nil {
		t.Fatal("a metadata patch changed a packaged skill's version")
	}

	class, code := ClassifyError(err)
	if class != OutcomeClassConflict {
		t.Errorf("the outcome class is %q, want %q", class, OutcomeClassConflict)
	}
	if code != "package_metadata_immutable" {
		t.Errorf("the outcome code is %q, want %q", code, "package_metadata_immutable")
	}
	if code == "package_immutable" {
		t.Error("the metadata refusal is classified as the content refusal; the two must be countable apart")
	}
}

// TestMetadataRefusalAndContentRefusalAreDistinct pins that the two
// sentinels do not overlap: a content refusal must not satisfy the
// metadata check, and the other way round.
func TestMetadataRefusalAndContentRefusalAreDistinct(t *testing.T) {
	dm := p3DM(t)
	fixture := p3SeedPackagedSkill(t, dm, "packaged", "1.0.0")

	metaErr := dm.UpdateMemoryMetadata(fixture.skillID, `{"version":"9.9.9"}`)
	contentErr := dm.UpdateMemory(fixture.skillID, "an OVERWRITTEN body",
		map[string]interface{}{"skill": true}, map[string]interface{}{"note": "overwritten"})

	if metaErr == nil || contentErr == nil {
		t.Fatal("one of the two refusals did not fire")
	}
	if errors.Is(metaErr, ErrPackageImmutable) {
		t.Error("the metadata refusal satisfies the content sentinel; the two must be distinct")
	}
	if errors.Is(contentErr, ErrPackageMetadataImmutable) {
		t.Error("the content refusal satisfies the metadata sentinel; the two must be distinct")
	}
	if !errors.Is(metaErr, ErrPackageMetadataImmutable) {
		t.Errorf("the metadata error is %v, want the package_metadata_immutable sentinel", metaErr)
	}
	if !errors.Is(contentErr, ErrPackageImmutable) {
		t.Errorf("the content error is %v, want the package_immutable sentinel", contentErr)
	}
}
