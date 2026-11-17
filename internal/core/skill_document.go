package internal

// skill_document.go — the shared skill-document loader.
//
// The skill `read` action returns TWO things that must agree: the
// skill's body, and — for a packaged skill — its package provenance.
// PR #243 fetched them from two independent statements, so a package
// deleted between the two could let a body return with no provenance.
// The follow-up (PR A) reads both from ONE snapshot, which means the
// skill-loading logic has to be runnable inside a caller's transaction.
//
// So it is written against the smallest read surface that BOTH *sql.DB
// and *sql.Tx satisfy, and it lives in its own file rather than in
// skill_db.go. The file is part of the reader's GUARDED set (see
// TestP6TheReaderNeverWrites): every statement it issues is declared
// here as a constant, and the guard proves each one is a single read
// statement. That matters because the `read` action's hot path runs
// through this file, and a guard that covered only the package reader
// would leave half of the read path unguarded.

import (
	"database/sql"
	"fmt"
)

// skillQueryer is the read surface a skill's rows can be loaded from.
//
// *sql.DB and *sql.Tx both satisfy it, which is the whole point: the
// loader is written once and runs either against the pool or inside a
// caller's snapshot. It is deliberately the two METHODS and not an
// interface with more: the loader issues no statement other than a
// SELECT, and the guard checks the SQL text of every one of them.
type skillQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

const (
	// skillDocumentByIDQuery reads ONE skill by its exact id.
	//
	// The `collection = 'skills'` and `deleted_at IS NULL` predicates
	// are the skill-scope rules: a memory of another collection is not
	// a skill, and a soft-shredded skill is not readable.
	skillDocumentByIDQuery = `
		SELECT id, collection, content, tags, metadata, is_global, is_prime_directive,
		       weight, created_at, deleted_at
		FROM memories
		WHERE collection = 'skills' AND deleted_at IS NULL
		  AND id = ?
		LIMIT 1
	`

	// skillDocumentQuery reads ONE skill by its exact id, without the
	// skill-scope predicates.
	//
	// It is the second half of the name→latest fallback: the candidate
	// ids come from the skill-rows query, so the row is known to be a
	// live skill already, and re-asserting the predicates here would
	// only be a second chance to fail.
	skillDocumentQuery = `
		SELECT id, collection, content, tags, metadata, is_global, is_prime_directive,
		       weight, created_at, deleted_at
		FROM memories
		WHERE id = ? AND deleted_at IS NULL
		LIMIT 1
	`

	// skillRowsByNameQuery reads the (id, version) pair of every live
	// skill row of one NAME.
	//
	// The id carries the version (`skill:<name>-v<version>`), so the
	// prefix match is the name match. The version is read from the
	// metadata rather than parsed out of the id, because that is where
	// the writer put it.
	skillRowsByNameQuery = `
		SELECT id, COALESCE(json_extract(metadata, '$.version'), '')
		FROM memories
		WHERE collection = 'skills' AND deleted_at IS NULL
		  AND substr(id, 1, length(?)) = ?
		  AND id != ?
	`
)

// readSkillDocument loads a skill and returns it together with the RAW
// document it was parsed from.
//
// The raw document is the PROJECTION — the complete `memories.content`
// column, frontmatter included. The package provenance needs it for two
// things: the recorded `projection_sha256` is the hash of exactly these
// bytes, so they are what the recorded value is validated against; and
// the body's hash is the hash of the part of these bytes that follows
// the frontmatter.
//
// The name→latest fallback is preserved exactly: an exact id wins, and
// a name resolves to the highest-semver live row of that name.
func readSkillDocument(q skillQueryer, nameOrID, version string) (*Skill, string, error) {
	var (
		id, content, tagsJSON, metaJSON, collection, createdAt string
		isGlobal, isPrime                                      int
		weight                                                 float64
		deletedAt                                              sql.NullString
	)

	// Try exact-id lookup first.
	row := q.QueryRow(skillDocumentByIDQuery, nameOrID)

	err := row.Scan(&id, &collection, &content, &tagsJSON, &metaJSON,
		&isGlobal, &isPrime, &weight, &createdAt, &deletedAt)
	if err == sql.ErrNoRows {
		// Fall back to name lookup: find the semver-max row. The
		// previous shape used ORDER BY id DESC LIMIT 1 as a proxy
		// for "highest semver" — equal-width semver sorts in the
		// same order, but unequal-width semver breaks the proxy
		// (e.g. id-sort puts v1.9.0 above v1.10.0 because '9' > '1').
		// Load the candidates and pick the semver max in Go so the
		// name-latest lookup matches the `is_latest = highest
		// semantic version` invariant.
		rows, qerr := loadActiveSkillRowsFrom(q, nameOrID, "")
		if qerr != nil {
			return nil, "", qerr
		}
		max := highestSemver(rows)
		if max.id == "" {
			err = sql.ErrNoRows
		} else {
			row = q.QueryRow(skillDocumentQuery, max.id)
			err = row.Scan(&id, &collection, &content, &tagsJSON, &metaJSON,
				&isGlobal, &isPrime, &weight, &createdAt, &deletedAt)
		}
	}
	if err == sql.ErrNoRows {
		return nil, "", fmt.Errorf("skill %q not found", nameOrID)
	}
	if err != nil {
		return nil, "", fmt.Errorf("query skill: %w", err)
	}

	fm, body, parseErr := ParseSkillFrontmatter(content)
	if parseErr != nil {
		return nil, "", fmt.Errorf("parse skill %s frontmatter: %w", id, parseErr)
	}

	meta := parseJSONMeta(metaJSON)
	isLatest, _ := meta["is_latest"].(bool)
	// content_hash is written by SaveSkill into metadata; surface it on
	// the read-side Skill struct so callers (dedup, drift checks) don't
	// have to reach into the metadata map. Matches the `is_latest`
	// extraction pattern immediately above. Absent hash → empty string.
	contentHash, _ := meta["content_hash"].(string)

	return &Skill{
		ID:          id,
		Collection:  collection,
		Tags:        parseJSONTags(tagsJSON),
		Metadata:    meta,
		IsGlobal:    isGlobal == 1,
		IsLatest:    isLatest,
		Weight:      int(weight),
		CreatedAt:   createdAt,
		Name:        fm.Name,
		Version:     fm.Version,
		WhenToUse:   fm.WhenToUse,
		Domain:      fm.Domain,
		Constraints: fm.Constraints,
		Steps:       fm.Steps,
		Frontmatter: fm,
		Body:        body,
		ContentHash: contentHash,
	}, content, nil
}

// loadActiveSkillRowsFrom loads the (id, version) pair of every live
// skill row of one NAME, excluding one id.
//
// It is the transaction-agnostic form: *sql.DB and *sql.Tx both satisfy
// skillQueryer, so the pool-scoped and the transaction-scoped callers
// share ONE statement.
func loadActiveSkillRowsFrom(q skillQueryer, name, excludeID string) ([]skillSemverRow, error) {
	prefix := "skill:" + name + "-v"
	rows, err := q.Query(skillRowsByNameQuery, prefix, prefix, excludeID)
	if err != nil {
		return nil, fmt.Errorf("query active skill rows for %s: %w", name, err)
	}
	defer rows.Close()
	var out []skillSemverRow
	for rows.Next() {
		var r skillSemverRow
		if err := rows.Scan(&r.id, &r.version); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}
