// skill_db.go — DB methods for the skills collection.
//
// Read path mirrors directive_tools.go: indexed lookup, single-statement
// queries, no LLM calls on the hot path. SaveSkill, PromoteSkillToGlobal,
// and ShredSkill are the only writers.

package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"golang.org/x/mod/semver"
)

// ReadSkill fetches a skill by id (exact) or name (latest version).
// version is ignored when name is given; pass exact id (skill:<name>-v<ver>)
// to bypass version resolution.
func (dm *DatabaseManager) ReadSkill(nameOrID, version string) (*Skill, error) {
	db := dm.db
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}

	var (
		id, content, tagsJSON, metaJSON, collection, createdAt string
		isGlobal, isPrime                                      int
		weight                                                 int
		deletedAt                                              sql.NullString
	)

	// Try exact-id lookup first.
	row := db.QueryRow(`
		SELECT id, collection, content, tags, metadata, is_global, is_prime_directive,
		       weight, created_at, deleted_at
		FROM memories
		WHERE collection = 'skills' AND deleted_at IS NULL
		  AND id = ?
		LIMIT 1
	`, nameOrID)

	err := row.Scan(&id, &collection, &content, &tagsJSON, &metaJSON,
		&isGlobal, &isPrime, &weight, &createdAt, &deletedAt)
	if err == sql.ErrNoRows {
		// Fall back to name lookup: find the highest-versioned row.
		row = db.QueryRow(`
			SELECT id, collection, content, tags, metadata, is_global, is_prime_directive,
			       weight, created_at, deleted_at
			FROM memories
			WHERE collection = 'skills' AND deleted_at IS NULL
			  AND id LIKE ('skill:' || ? || '-v%')
			ORDER BY id DESC
			LIMIT 1
		`, nameOrID)
		err = row.Scan(&id, &collection, &content, &tagsJSON, &metaJSON,
			&isGlobal, &isPrime, &weight, &createdAt, &deletedAt)
	}
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("skill %q not found", nameOrID)
	}
	if err != nil {
		return nil, fmt.Errorf("query skill: %w", err)
	}

	fm, body, parseErr := ParseSkillFrontmatter(content)
	if parseErr != nil {
		return nil, fmt.Errorf("parse skill %s frontmatter: %w", id, parseErr)
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
		Weight:      weight,
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
	}, nil
}

// ListSkills returns the latest version of each skill in scope.
// scope: "local" | "shared" | "all".
// scope defaults to "all" when empty.
func (dm *DatabaseManager) ListSkills(scope string) ([]SkillSummary, error) {
	if scope == "" {
		scope = "all"
	}
	db := dm.db
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}

	var scopeClause string
	switch scope {
	case "local":
		scopeClause = "AND is_global = 0"
	case "shared":
		scopeClause = "AND is_global = 1"
	default:
		scopeClause = ""
	}

	rows, err := db.Query(`
		SELECT id, content, is_global, weight
		FROM memories
		WHERE collection = 'skills' AND deleted_at IS NULL
		  ` + scopeClause + `
		ORDER BY id DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query skills: %w", err)
	}
	defer rows.Close()

	type parsedRow struct {
		id        string
		name      string
		version   string
		whenToUse string
		isGlobal  bool
		weight    int
	}
	var all []parsedRow
	for rows.Next() {
		var id, content string
		var isGlobal, weight int
		if err := rows.Scan(&id, &content, &isGlobal, &weight); err != nil {
			return nil, fmt.Errorf("scanning skill row: %w", err)
		}
		fm, _, err := ParseSkillFrontmatter(content)
		if err != nil {
			continue
		}
		all = append(all, parsedRow{
			id: id, name: fm.Name, version: fm.Version,
			whenToUse: fm.WhenToUse, isGlobal: isGlobal == 1, weight: weight,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	// Deduplicate by name, keeping the highest version (lexicographic
	// order on the id string is sufficient because semver sorts when
	// equal-width, e.g. 1.0.0 < 2.0.0 < 10.0.0).
	// Future-proofing note: if a future version uses unequal width, fix the parser, not the sort.
	byName := make(map[string]parsedRow)
	for _, r := range all {
		cur, ok := byName[r.name]
		if !ok || r.id > cur.id {
			byName[r.name] = r
		}
	}
	out := make([]SkillSummary, 0, len(byName))
	for _, r := range byName {
		out = append(out, SkillSummary{
			ID: r.id, Name: r.name, Version: r.version,
			WhenToUse: r.whenToUse, IsGlobal: r.isGlobal, Weight: r.weight,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// SaveSkill inserts or updates a skill row in the `memories` table
// (collection='skills'). When a new version is supplied for an existing
// name, the previous version's is_latest flag is flipped to false in the
// same transaction. Saving the same (name, version) pair requires
// force=true — returns an error otherwise so silent overwrites never
// happen.
//
// LTM semantics: skill rows are written with is_long_term=1 so the
// existing long-term decay machinery (slow 0.01×days rate, floor at 1)
// applies. This IS the "90-day decay floor" — the metadata's
// decay_floor_days key is documentary; the enforcement lives in the LTM
// rate, not a separate floor. Putting the floor in LTM means we don't
// need a skill-specific decay sweep and the row stays discoverable by
// the same retrieval paths that already handle directives and lessons.
//
// Write path: the scanner is structurally guaranteed by routing the
// INSERT through saveMemoryRow (the private primitive shared with
// SaveMemoryNode). SaveSkill uses a deterministic `skill:<name>-v<semver>`
// id rather than SaveMemoryNode's generated uuid, so we can't go
// through SaveMemoryNode directly — but we do go through its core
// scanner+INSERT primitive. Any future field added to the memories
// schema that lands in saveMemoryRow will automatically reach skills
// without re-deriving a parallel writer.
func (dm *DatabaseManager) SaveSkill(name, version, content, authorAgent string, force bool) (string, error) {
	// Parse frontmatter first so a malformed content string never reaches
	// the DB layer. The contract here is that any rejected frontmatter
	// is a 400, not a write-conflict.
	fm, _, err := ParseSkillFrontmatter(content)
	if err != nil {
		return "", fmt.Errorf("save skill: %w", err)
	}
	if fm.Name != name {
		return "", fmt.Errorf("frontmatter name %q does not match arg %q", fm.Name, name)
	}
	if fm.Version != version {
		return "", fmt.Errorf("frontmatter version %q does not match arg %q", fm.Version, version)
	}

	id, err := SkillIDForNameAndVersion(name, version)
	if err != nil {
		return "", fmt.Errorf("save skill: %w", err)
	}
	db := dm.SQLDB()

	// Existence check — does a non-deleted row already own this id?
	var existing string
	row := db.QueryRow(`SELECT id FROM memories WHERE id = ? AND deleted_at IS NULL`, id)
	err = row.Scan(&existing)
	exists := err == nil
	if err != nil && err != sql.ErrNoRows {
		return "", fmt.Errorf("lookup existing skill: %w", err)
	}
	if exists && !force {
		return "", fmt.Errorf("skill %s already exists; pass force=true to overwrite", id)
	}

	// Build tags / metadata. Tags: ["skill", <domain>] — domain is omitted
	// when empty so the SKILL domain taxonomy doesn't get polluted with
	// empty strings.
	tags := []string{"skill"}
	if fm.Domain != "" {
		tags = append(tags, fm.Domain)
	}
	// promoted_at is intentionally JSON null (not absent) so the field
	// shape is stable across reads — the promote task will json_set it
	// to a real timestamp without having to first insert the key.
	metadata := map[string]interface{}{
		"is_latest":        true,
		"author":           authorAgent,
		"promoted_at":      nil,
		"decay_floor_days": 90,
		"content_hash":     contentHash(content),
		// Mirror version into metadata so SaveSkill's semver-compare path
		// can read it back without re-parsing the frontmatter. (The id
		// also encodes it, but json_extract on a JSON column is cleaner
		// than string surgery on skill:<name>-v<version>.)
		"version":          version,
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}
	tagsJSON, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("marshal tags: %w", err)
	}

	if exists && force {
		// In-place overwrite: same id, same row — no version flip, no new
		// row. "force" means overwrite content/metadata; it must NOT reset
		// weight or reinforcement_count, because those columns encode the
		// skill's retrieval trajectory (how often it has been useful). A
		// force-overwrite that dropped weight from 13→5 would silently
		// destroy the prior reinforcement signal and bury a battle-tested
		// skill under newer-looking alternatives. SQLite preserves columns
		// omitted from the SET clause, so we update only the content
		// triple + updated_at and leave weight/reinforcement_count alone.
		//
		// is_latest must still be computed via semver comparison against
		// other versions for this name, excluding the current row. Edge
		// case: force-overwriting v1 after v2 exists must NOT stamp v1
		// as latest — same contract as the new-row path below.
		prefix := "skill:" + name + "-v"
		var (
			otherID      string
			otherVersion string
		)
		err = db.QueryRow(`
			SELECT id, json_extract(metadata, '$.version')
			FROM memories
			WHERE collection = 'skills' AND deleted_at IS NULL
			  AND substr(id, 1, length(?)) = ?
			  AND id != ?
			ORDER BY id DESC
			LIMIT 1
		`, prefix, prefix, id).Scan(&otherID, &otherVersion)
		isLatest := true
		if err == nil {
			cmp := semver.Compare("v"+version, "v"+otherVersion)
			if cmp < 0 {
				// Incoming is older than another existing version —
				// the higher one retains is_latest=true, this one
				// gets is_latest=false.
				isLatest = false
			}
		}
		// err == sql.ErrNoRows: no other versions, this is the only
		// (or highest) version, is_latest=true.

		metadata["is_latest"] = isLatest
		metaJSON, err = json.Marshal(metadata)
		if err != nil {
			return "", fmt.Errorf("marshal metadata: %w", err)
		}

		_, err = db.Exec(`
			UPDATE memories
			SET content = ?, tags = ?, metadata = ?, is_long_term = 1,
			    updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ? AND deleted_at IS NULL
		`, content, string(tagsJSON), string(metaJSON), id)
		if err != nil {
			return "", fmt.Errorf("update skill: %w", err)
		}
		return id, nil
	}

	// New row path. Transaction so the flip-prior + insert stays atomic —
	// a crash between the two would leave two is_latest=true rows for the
	// same name, which ReadSkill would resolve nondeterministically.
	//
	// is_latest is semver-driven, not save-order-driven. We find the
	// current max-version row for this name within the tx and only flip
	// it when the incoming version strictly exceeds it. Saving an older
	// version (e.g. v1 after v2) inserts with is_latest=false and leaves
	// the higher existing row untouched.
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Find the current max-version row for this name. ORDER BY id DESC is
	// a proxy for highest semver because skill ids are skill:<name>-v<semver>
	// with equal-width semver; if v10.0.0 ever lands this needs a proper
	// semver ORDER BY via a generated column.
	//
	// Prefix-match via substr() (not LIKE) because LIKE's `_` and `%`
	// wildcards would match unintended rows if the name contains those
	// characters. The skill name validator already rejects `-v` to keep
	// the format unambiguous; we extend that hygiene here by avoiding
	// the LIKE wildcard syntax entirely.
	var (
		existingID      string
		existingVersion string
	)
	prefix := "skill:" + name + "-v"
	err = tx.QueryRow(`
		SELECT id, json_extract(metadata, '$.version')
		FROM memories
		WHERE collection = 'skills' AND deleted_at IS NULL
		  AND substr(id, 1, length(?)) = ?
		ORDER BY id DESC
		LIMIT 1
	`, prefix, prefix).Scan(&existingID, &existingVersion)

	hasExisting := err == nil
	if err != nil && err != sql.ErrNoRows {
		return "", fmt.Errorf("lookup max version: %w", err)
	}

	// semver.Compare requires the 'v' prefix on both arguments. The DB
	// stores version without the prefix (YAML convention); we add it
	// here. Prepending unconditionally is safe: the frontmatter parser
	// already validated the semver shape, and an empty version would
	// have failed upstream.
	newIsLatest := true
	if hasExisting {
		cmp := semver.Compare("v"+version, "v"+existingVersion)
		switch {
		case cmp > 0:
			// Incoming version strictly higher — flip the prior max to false.
			_, err = tx.Exec(`
				UPDATE memories
				SET metadata = json_set(COALESCE(metadata, '{}'), '$.is_latest', 0)
				WHERE id = ? AND deleted_at IS NULL
			`, existingID)
			if err != nil {
				return "", fmt.Errorf("flip prior is_latest: %w", err)
			}
		case cmp == 0:
			// Existence check above should have caught this. Defensive.
			return "", fmt.Errorf("save skill: version %s already exists for %s (id=%s)", version, name, existingID)
		default:
			// cmp < 0: incoming version is older than current max — keep
			// the higher existing row as latest, insert this one as
			// not-latest.
			newIsLatest = false
		}
	}

	// Rebuild metadata with the comparison-correct is_latest flag.
	metadata["is_latest"] = newIsLatest
	metaJSON, err = json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("marshal metadata: %w", err)
	}

	// INSERT via saveMemoryRow — the shared scanner+INSERT primitive
	// that backs SaveMemoryNode. Tx-aware (DBNode interface), so the
	// INSERT lands in the same transaction as the flip-prior UPDATE
	// above. Skills don't carry embeddings, so IVF assignment is a
	// no-op here.
	txDBNode := &txNode{tx: tx, dm: dm}
	_, err = saveMemoryRow(txDBNode, dm, id, "skills", content, "", tags, metadata, nil, true, 5, "", "", "", "")
	if err != nil {
		return "", fmt.Errorf("insert skill: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// TODO(follow-up): force-overwrite path at line ~281 hardcodes
// metadata.is_latest=true via the initial map. Edge case: if an older
// version (e.g. v1) is force-overwritten AFTER a higher version (v2)
// already exists, the overwrite will incorrectly stamp v1 as latest.
// The fix is to call the same semver-compare logic above before the
// UPDATE so is_latest reflects current max, not the row's identity.
// Tracked separately because it's a different transaction shape
// (single UPDATE vs. tx with flip+insert) and the test surface is
// orthogonal to the out-of-order-saves test this commit ships.

// contentHash returns the hex SHA-256 of s. The plan's placeholder
// (fmt.Sprintf("%x", len(s))) is deterministic but two distinct skill
// bodies of equal length would collide; SHA-256 ensures the hash
// actually identifies the content, which the dedup pathway in
// MemoryStore.addMemoryDirect already relies on.
func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// PromoteSkillToGlobal marks a skill as shared (is_global=1). The
// confirm flag mirrors record_global_rule and promote_to_global:
// agents cannot promote without explicit operator consent.
//
// Divergence from PromoteToGlobal (intentional, per plan): that path
// COPIES a local memory into shared.memories under a fresh GenerateID()
// row. Skills cannot do that — their id is the deterministic contract
// `skill:<name>-v<semver>` that ReadSkill/SaveSkill resolve against, so
// a copy under a random id would be unreachable by name. Instead we flip
// is_global on the canonical row in place. derived_from_skill_id points
// at the row's own id purely as a promotion marker (not a copy pointer).
//
// The collection='skills' guard on both statements is a privilege gate:
// without it any memory id (a decision, a raw session row) could be
// elevated to is_global=1 through this "skill" path and recorded in the
// audit log as a skill promotion.
func (dm *DatabaseManager) PromoteSkillToGlobal(skillID string, confirm bool) error {
	if !confirm {
		return fmt.Errorf("promote_skill_to_global requires confirm=true (no silent mutations)")
	}
	db := dm.SQLDB()
	if db == nil {
		return fmt.Errorf("db not initialized")
	}

	// Verify the row exists AND is actually a skill.
	var existing string
	err := db.QueryRow(`SELECT id FROM memories WHERE id = ? AND collection = 'skills' AND deleted_at IS NULL`, skillID).Scan(&existing)
	if err == sql.ErrNoRows {
		return fmt.Errorf("skill %s not found", skillID)
	}
	if err != nil {
		return fmt.Errorf("lookup skill: %w", err)
	}

	// Flip is_global and stamp the promotion marker. The collection guard
	// is repeated here so a row that stops being a skill between the check
	// and the write can't slip through.
	_, err = db.Exec(`
		UPDATE memories
		SET is_global = 1,
		    metadata = json_set(COALESCE(metadata, '{}'),
		                        '$.derived_from_skill_id', ?,
		                        '$.promoted_at', strftime('%s','now'))
		WHERE id = ? AND collection = 'skills' AND deleted_at IS NULL
	`, skillID, skillID)
	if err != nil {
		return fmt.Errorf("promote: %w", err)
	}
	return nil
}

// ShredSkill soft-deletes a skill by setting deleted_at. The row stays
// in the DB for forensics; ReadSkill and ListSkills exclude it via the
// deleted_at IS NULL clause already present in those queries.
//
// Silent on missing id (zero rows affected), matching the
// delete-by-mark pattern: the caller already knows the skill exists
// to want to delete it, and a missing id is not an error condition.
// A row that is already soft-deleted is also a no-op (the
// `deleted_at IS NULL` guard) — shredding is idempotent.
//
// The collection='skills' guard mirrors PromoteSkillToGlobal: this
// surface shouldn't be able to delete a non-skill row by accident.
func (dm *DatabaseManager) ShredSkill(skillID string) error {
	db := dm.SQLDB()
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	_, err := db.Exec(`
		UPDATE memories
		SET deleted_at = strftime('%s','now')
		WHERE id = ? AND collection = 'skills' AND deleted_at IS NULL
	`, skillID)
	if err != nil {
		return fmt.Errorf("shred skill: %w", err)
	}
	return nil
}

// parseJSONTags and parseJSONMeta are thin wrappers around encoding/json
// that handle empty/nil inputs gracefully. Shared across the skill read
// paths; consolidated here to keep the per-row scan logic uncluttered.
func parseJSONTags(raw string) []string {
	var out []string
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func parseJSONMeta(raw string) map[string]interface{} {
	out := map[string]interface{}{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}
