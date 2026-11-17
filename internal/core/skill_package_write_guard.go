// skill_package_write_guard.go — the shared primitive that applies the
// package-immutability guard to a single-document UPDATE.
//
// WHY THIS EXISTS
// ----------------
// Two public paths replace a `memories` row's content — and its whole
// metadata column — IN PLACE, under the same id:
//
//	MemoryStore.UpdateMemory      (internal/core/memory.go)
//	DatabaseManager.UpdateMemory  (internal/core/web_db.go)
//
// For a skill that owns a package that would give one id two documents:
// `read` would return the new body while `read_file` returned the old bytes,
// `projection_sha256` would stop describing `memories.content`, and the
// package row, the file rows and the blobs would all still describe the
// previous document.
//
// So the guard is FAIL CLOSED: not the content, not the tags, not the
// metadata, not the embedding, not the content hash, not the timestamps.
//
// THE ATOMICITY
// -------------
// The guard's condition is FOLDED INTO THE UPDATE'S OWN WHERE CLAUSE. The
// check and the write are therefore one atomic statement: a package row
// that appears between the pre-check and the write cannot let the
// overwrite through, because the statement itself excludes the row.
//
// The SQL lives in named constants rather than inline, so a test can
// execute THE PRODUCTION STATEMENT — not a copy of it. A test that
// re-typed the SQL would still pass if the condition were deleted from
// the code; a test that runs the constant cannot.
//
// THE EXPLANATION OF A ZERO-ROW OUTCOME
// --------------------------------------
// A zero-row outcome has two possible causes: the row is gone, or a
// package row appeared between the pre-check and the write. Telling them
// apart needs a read, and a read in a separate statement can see a
// DIFFERENT snapshot than the write did.
//
// It is made deterministic instead of probabilistic, by reading the row's
// existence AND the package's ownership in ONE statement — and by the
// cascade invariant:
//
//	skill_packages.skill_id REFERENCES memories(id) ON DELETE CASCADE
//
// A package row can only disappear when its memories row is hard-deleted;
// no code deletes a package row directly (the blobstore hard rule keeps
// the collector away from the package tables entirely). So:
//
//   - the row is gone            -> the package is gone with it; the
//                                    outcome is "not found", and nothing
//                                    was overwritten;
//   - the row is present and the package is present -> the refusal;
//   - the row is present and the package is absent  -> impossible: the
//                                    write was excluded by a package that
//                                    cannot have vanished without taking
//                                    the row with it.
//
// No ordering argument is load-bearing, and no second snapshot is read.

package internal

import (
	"fmt"
)

// The production UPDATE statements, as constants.
//
// They are the statements the code actually runs. The regression test
// executes these constants, so removing the guard's condition from them
// breaks the test — which is the point of a predicate test.
const (
	// guardedUpdateMemoryWithEmbeddingSQL is the DatabaseManager's
	// single-document UPDATE, with a fresh embedding.
	guardedUpdateMemoryWithEmbeddingSQL = `
		UPDATE memories
		SET content = ?, tags = ?, metadata = ?, embedding = ?, content_hash = ?,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
		  AND NOT EXISTS (SELECT 1 FROM skill_packages WHERE skill_id = ?)
	`

	// guardedUpdateMemoryWithoutEmbeddingSQL is the DatabaseManager's
	// single-document UPDATE, preserving the existing embedding column.
	guardedUpdateMemoryWithoutEmbeddingSQL = `
		UPDATE memories
		SET content = ?, tags = ?, metadata = ?, content_hash = ?,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
		  AND NOT EXISTS (SELECT 1 FROM skill_packages WHERE skill_id = ?)
	`

	// guardedUpdateMemoryStoreWithEmbeddingSQL is the MemoryStore's
	// single-document UPDATE, with a fresh embedding and its provenance.
	guardedUpdateMemoryStoreWithEmbeddingSQL = `
		UPDATE memories
		SET content = ?, tags = ?, metadata = ?, embedding = ?, content_hash = ?,
		    embedding_source = ?, embedding_dimension = ?, embedding_model = ?,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
		  AND NOT EXISTS (SELECT 1 FROM skill_packages WHERE skill_id = ?)
	`

	// guardedUpdateMemoryStoreWithoutEmbeddingSQL is the MemoryStore's
	// single-document UPDATE, preserving the existing embedding column.
	guardedUpdateMemoryStoreWithoutEmbeddingSQL = `
		UPDATE memories
		SET content = ?, tags = ?, metadata = ?, content_hash = ?,
		    updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ?
		  AND NOT EXISTS (SELECT 1 FROM skill_packages WHERE skill_id = ?)
	`
)

// guardedUpdateOutcome names what a zero-row guarded UPDATE meant.
type guardedUpdateOutcome int

const (
	// guardedOutcomeNotFound — the memories row does not exist (or was
	// hard-deleted). Nothing was overwritten.
	guardedOutcomeNotFound guardedUpdateOutcome = iota

	// guardedOutcomePackageImmutable — the row exists and owns a
	// package. The write was refused.
	guardedOutcomePackageImmutable

	// guardedOutcomeUnexplained — the row exists and owns no package,
	// yet the guarded UPDATE wrote no row. The cascade invariant says
	// this cannot happen, so it is reported rather than papered over.
	guardedOutcomeUnexplained
)

// guardedUpdateExplanation is the outcome of a zero-row guarded UPDATE,
// with the package's identity when there is one.
type guardedUpdateExplanation struct {
	Outcome guardedUpdateOutcome
	Package *skillPackageIdentity
}

// explainGuardedUpdate explains a zero-row outcome of a guarded UPDATE.
//
// The row's existence and the package's ownership are read in ONE
// statement, so the answer is internally consistent — it cannot see the
// package gone and the row present, or the other way round.
func explainGuardedUpdate(query packageQuery, id string) (*guardedUpdateExplanation, error) {
	rows, err := query(`
		SELECT
			(SELECT COUNT(*) FROM memories WHERE id = ?),
			(SELECT COUNT(*) FROM skill_packages WHERE skill_id = ?),
			COALESCE((SELECT name FROM skill_packages WHERE skill_id = ?), ''),
			COALESCE((SELECT version FROM skill_packages WHERE skill_id = ?), '')
	`, id, id, id, id)
	if err != nil {
		return nil, fmt.Errorf("explain the guarded update: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("explain the guarded update: %w", err)
		}
		return &guardedUpdateExplanation{Outcome: guardedOutcomeNotFound}, nil
	}
	var rowCount, packageCount int
	var name, version string
	if err := rows.Scan(&rowCount, &packageCount, &name, &version); err != nil {
		return nil, fmt.Errorf("scan the guarded update's explanation: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("explain the guarded update: %w", err)
	}

	if rowCount == 0 {
		// The row is gone — and with it, by the cascade, any package it
		// owned. Nothing was overwritten.
		return &guardedUpdateExplanation{Outcome: guardedOutcomeNotFound}, nil
	}
	if packageCount > 0 {
		return &guardedUpdateExplanation{
			Outcome: guardedOutcomePackageImmutable,
			Package: &skillPackageIdentity{name: name, version: version},
		}, nil
	}
	return &guardedUpdateExplanation{Outcome: guardedOutcomeUnexplained}, nil
}

// packageImmutableErrorFor builds the refusal for a package identity.
func packageImmutableErrorFor(id string, pkg *skillPackageIdentity) error {
	return &PackageImmutableError{SkillID: id, Name: pkg.name, Version: pkg.version}
}
