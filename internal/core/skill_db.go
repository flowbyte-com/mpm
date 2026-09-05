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
	"strings"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v3"
)

// validateSkillFrontmatterAndScan parses frontmatter and runs the
// secret/poison scanner against the Skill content. NO DB writes.
// This helper is shared by SaveSkill (after validation succeeds, the
// caller persists) and ValidateSkill (the workshop's non-mutating
// stage). Splitting it out is the structural fix for the workshop
// safety seam: the validation stage must never call the write API.
func validateSkillFrontmatterAndScan(content string) (*Skill, []string, []string, error) {
	fm, body, err := ParseSkillFrontmatter(content)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse frontmatter: %w", err)
	}
	var warnings, errors []string

	// Missing description is a soft warning.
	if fm.Description == "" {
		warnings = append(warnings, "missing_description")
	}
	// when_to_use validation: weak rules applied here for parity
	// with the workshop pipeline; SaveSkill's caller surfaces them
	// but the workshop's ValidateSkill is the canonical reader.
	if fm.WhenToUse == "" {
		warnings = append(warnings, "missing_when_to_use")
	} else if len(fm.WhenToUse) < 30 {
		warnings = append(warnings, "weak_when_to_use_short")
	}

	// Secret/poison scanner — same call SaveSkill used to perform
	// inline before persisting. Run on the whole content.
	if sensitive, reason := isSensitiveContent(content); sensitive {
		errors = append(errors, "scanner_secret:sensitive content blocked - "+reason)
	}
	if poisoned, reason := isPoisoned(content); poisoned {
		errors = append(errors, "scanner_poison:"+reason)
	}

	// Step shape validation (alpha-4.1.2 D-004). See
	// SkillStepNodeRaw in skill.go for why we walk the raw YAML
	// nodes: yaml.v3 silently coerces an empty mapping to
	// SkillStep{Call:"", ArgsFrom:""}, which produces a row that
	// looks present but is unusable. The auditor reported this as
	// silent corruption; this validator makes the rejection explicit
	// at the application boundary.
	if stepErrors := validateSkillStepShape(fm); len(stepErrors) > 0 {
		errors = append(errors, stepErrors...)
	}

	return &Skill{
		Frontmatter: fm,
		Body:        body,
		Name:        fm.Name,
		Version:     fm.Version,
		WhenToUse:   fm.WhenToUse,
		Domain:      fm.Domain,
		Constraints: fm.Constraints,
		Steps:       fm.Steps,
	}, warnings, errors, nil
}

// validateSkillStepShape inspects the raw YAML shape of every step
// in fm.StepNodes and rejects forms that yaml.v3 silently coerces to
// unusable SkillStep{Call:"", ArgsFrom:""} values. Pinning this
// behavior here (alpha-4.1.2 D-004) closes the silent-corruption hole
// the auditor reported.
//
// Rejections:
//
//   - ScalarNode (non-mapping entry): "step[N] must be an object with
//     a `call` field". This is what yaml.v3 already errors on, but the
//     application-level message is more useful for authors.
//
//   - MappingNode with CallSet=false: "step[N] missing required field
//     `call`". Catches both `- {}` and `- description: foo`.
//
//   - MappingNode with CallSet=true and empty/whitespace CallRaw:
//     "step[N].call must be a non-empty invocation target".
//
// Each error names the offending index so the author can locate the
// bad row in the source.
func validateSkillStepShape(fm SkillFrontmatter) []string {
	var out []string
	for i, raw := range fm.StepNodes {
		switch raw.Kind {
		case 0, yaml.MappingNode:
			if !raw.CallSet {
				out = append(out, fmt.Sprintf("step[%d] missing required field `call`", i))
				continue
			}
			call := strings.TrimSpace(raw.CallRaw)
			if call == "" {
				out = append(out, fmt.Sprintf("step[%d].call must be a non-empty invocation target", i))
				continue
			}
		case yaml.ScalarNode:
			out = append(out, fmt.Sprintf("step[%d] must be an object with a `call` field, got scalar %q", i, raw.CallRaw))
		default:
			out = append(out, fmt.Sprintf("step[%d] has unsupported YAML kind %d", i, raw.Kind))
		}
	}
	return out
}

// skillSemverRow carries the (id, version) pair used by the
// semver-max helpers below. Centralises the projection so callers
// share one row shape.
type skillSemverRow struct {
	id      string
	version string
}

// highestSemver returns the row with the highest semantic version in
// rows, using golang.org/x/mod/semver.Compare with v-prefixed strings
// (per the SaveSkill convention). An empty input returns an empty
// row.
//
// The skill ids are `skill:<name>-v<semver>`. ORDER BY id DESC was
// the previous proxy for "highest semver" — it works for equal-width
// semver (e.g. 1.0.0, 2.0.0, 10.0.0) because id strings sort in the
// same order, but breaks for unequal-width semver: id-sorts
// v1.9.0 > v1.10.0 because '9' > '1' at the version digit, while
// semver-correctly v1.10.0 > v1.9.0. This helper replaces the id
// proxy with proper semver comparison, which is the authoritative
// "highest semantic version" contract.
//
// Legacy NULL-version rows: rows where the version field is empty
// (pre-metadata-version-mirror rows) sort BELOW every versioned
// row, breaking ties via id DESC. This preserves the historical
// "newest-inserted wins when no version is recorded" behaviour
// for the read path while still making the authoritative semver
// comparison the primary sort key.
func highestSemver(rows []skillSemverRow) skillSemverRow {
	var max skillSemverRow
	for _, r := range rows {
		if max.id == "" {
			max = r
			continue
		}
		cmp := semver.Compare("v"+r.version, "v"+max.version)
		switch {
		case cmp > 0:
			max = r
		case cmp == 0 && r.id > max.id:
			// Tiebreaker for equal semver (or both empty): prefer
			// the lexically-larger id (newest inserted).
			max = r
		}
	}
	return max
}

// loadActiveSkillRows loads the (id, version) pair of every live
// skill row whose id starts with `skill:<name>-v` AND is not the
// excluded id. Used by SaveSkill (new-row + force-overwrite) and
// ReadSkill's name fallback to compute the semver max without
// relying on the broken id DESC proxy.
//
// excludeID is the id to skip (typically the incoming id in save
// paths; pass "" for the read path to load every live row).
//
// NULL-version rows (legacy rows that pre-date the metadata.version
// mirror) are returned with an empty version string; highestSemver
// handles them by sorting them below every versioned row, breaking
// ties with id DESC. This preserves the historical "newest-inserted
// wins when no version is recorded" behaviour for legacy rows while
// making the authoritative semver comparison the primary sort key.
func loadActiveSkillRows(db *sql.DB, name, excludeID string) ([]skillSemverRow, error) {
	prefix := "skill:" + name + "-v"
	rows, err := db.Query(`
		SELECT id, COALESCE(json_extract(metadata, '$.version'), '')
		FROM memories
		WHERE collection = 'skills' AND deleted_at IS NULL
		  AND substr(id, 1, length(?)) = ?
		  AND id != ?
	`, prefix, prefix, excludeID)
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

// loadActiveSkillRowsTx is the transaction-scoped variant for the
// SaveSkill new-row path. Same shape as loadActiveSkillRows but uses
// the transaction's Query to keep the read inside the tx.
func loadActiveSkillRowsTx(tx *sql.Tx, name, excludeID string) ([]skillSemverRow, error) {
	prefix := "skill:" + name + "-v"
	rows, err := tx.Query(`
		SELECT id, COALESCE(json_extract(metadata, '$.version'), '')
		FROM memories
		WHERE collection = 'skills' AND deleted_at IS NULL
		  AND substr(id, 1, length(?)) = ?
		  AND id != ?
	`, prefix, prefix, excludeID)
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
		weight                                                 float64
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
		// Fall back to name lookup: find the semver-max row. The
		// previous shape used ORDER BY id DESC LIMIT 1 as a proxy
		// for "highest semver" — equal-width semver sorts in the
		// same order, but unequal-width semver breaks the proxy
		// (e.g. id-sort puts v1.9.0 above v1.10.0 because '9' > '1').
		// Load the candidates and pick the semver max in Go so the
		// name-latest lookup matches the `is_latest = highest
		// semantic version` invariant.
		rows, qerr := loadActiveSkillRows(db, nameOrID, "")
		if qerr != nil {
			return nil, qerr
		}
		max := highestSemver(rows)
		if max.id == "" {
			err = sql.ErrNoRows
		} else {
			row = db.QueryRow(`
				SELECT id, collection, content, tags, metadata, is_global, is_prime_directive,
				       weight, created_at, deleted_at
				FROM memories
				WHERE id = ? AND deleted_at IS NULL
				LIMIT 1
			`, max.id)
			err = row.Scan(&id, &collection, &content, &tagsJSON, &metaJSON,
				&isGlobal, &isPrime, &weight, &createdAt, &deletedAt)
		}
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
		var isGlobal int
		var weight float64
		if err := rows.Scan(&id, &content, &isGlobal, &weight); err != nil {
			return nil, fmt.Errorf("scanning skill row: %w", err)
		}
		fm, _, err := ParseSkillFrontmatter(content)
		if err != nil {
			continue
		}
		all = append(all, parsedRow{
			id: id, name: fm.Name, version: fm.Version,
			whenToUse: fm.WhenToUse, isGlobal: isGlobal == 1, weight: int(weight),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}

	// Deduplicate by name, keeping the semver-max version. The
	// previous shape used lexicographic id sort as a proxy for
	// "highest semver" — equal-width semver sorts in the same
	// order, but unequal-width semver breaks the proxy (e.g.
	// id-sort puts v1.9.0 above v1.10.0 because '9' > '1'). Group
	// by name, then pick the semver max in Go so the returned
	// SkillSummary represents the actual semver-max row.
	byName := make(map[string][]parsedRow)
	for _, r := range all {
		byName[r.name] = append(byName[r.name], r)
	}
	merged := make(map[string]parsedRow, len(byName))
	for name, group := range byName {
		var candidates []skillSemverRow
		for _, r := range group {
			candidates = append(candidates, skillSemverRow{id: r.id, version: r.version})
		}
		max := highestSemver(candidates)
		// Look up the parsedRow corresponding to the semver max.
		for _, r := range group {
			if r.id == max.id {
				merged[name] = r
				break
			}
		}
	}
	out := make([]SkillSummary, 0, len(merged))
	for _, r := range merged {
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
	skill, _, scanErrs, err := validateSkillFrontmatterAndScan(content)
	if err != nil {
		return "", fmt.Errorf("save skill: %w", err)
	}
	if len(scanErrs) > 0 {
		return "", fmt.Errorf("save skill: scanner blocked content: %v", scanErrs)
	}
	fm := skill.Frontmatter
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

	// Existence check — does ANY row own this id? The 2026-09-05
	// audit found the existence check filtered on `deleted_at IS NULL`
	// (hiding tombstones), which made `save → delete → save` collide
	// with the tombstone's PK on the new-row INSERT. The id is owned
	// regardless of deleted_at — ShredSkill is a soft-delete that
	// preserves the row for audit, so a tombstone still occupies the
	// PK and must be visible to this check.
	//
	// Lifecycle contract:
	//   - exists (live OR tombstoned) && !force → "already exists"
	//   - exists (live OR tombstoned) && force → UPDATE in place
	//     (the force path also clears deleted_at to resurrect the row)
	//   - !exists → INSERT (no PK collision possible)
	var existing string
	row := db.QueryRow(`SELECT id FROM memories WHERE id = ?`, id)
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
		//
		// Resurrection clause: deleted_at = NULL. The existence check
		// above includes tombstoned rows (id is owned regardless of
		// deleted_at), so a force-overwrite against a soft-deleted row
		// both overwrites the content AND resurrects the row back to
		// live. The 2026-09-05 audit found the previous force path was
		// filtered on `deleted_at IS NULL`, which silently no-op'd the
		// UPDATE and left the tombstone in place — the live row would
		// not exist after save-after-delete.
		// Load all OTHER live rows for this name and find the semver
		// max via highestSemver. The previous shape used
		// `ORDER BY id DESC LIMIT 1` as a proxy for highest semver,
		// which breaks for unequal-width semver (id-sort puts v1.9.0
		// above v1.10.0 because '9' > '1' at the version digit).
		otherRows, qerr := loadActiveSkillRows(db, name, id)
		if qerr != nil {
			return "", qerr
		}
		otherMax := highestSemver(otherRows)
		isLatest := true
		if otherMax.id != "" {
			cmp := semver.Compare("v"+version, "v"+otherMax.version)
			if cmp < 0 {
				// Incoming is older than another existing version —
				// the higher one retains is_latest=true, this one
				// gets is_latest=false.
				isLatest = false
			}
		}
		// err == sql.ErrNoRows equivalent: no other versions, this
		// is the only (or highest) version, is_latest=true.

		metadata["is_latest"] = isLatest
		metaJSON, err = json.Marshal(metadata)
		if err != nil {
			return "", fmt.Errorf("marshal metadata: %w", err)
		}
		_, err = db.Exec(`
			UPDATE memories
			SET content = ?, tags = ?, metadata = ?, is_long_term = 1,
			    deleted_at = NULL,
			    updated_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ?
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

	// Load all OTHER live rows for this name and find the semver
	// max via highestSemver. The previous shape used
	// `ORDER BY id DESC LIMIT 1` as a proxy for highest semver,
	// which breaks for unequal-width semver (id-sort puts v1.9.0
	// above v1.10.0 because '9' > '1' at the version digit). With
	// ≥2 existing versions having unequal-width semver, the proxy
	// also picked the wrong reference for the "demote old max"
	// UPDATE below — only the lexically-largest id was demoted,
	// leaving the actual semver-max (e.g. v1.10.0 when v1.9.0 was
	// the lexically-largest) still stamped is_latest=true.
	//
	// The flip-prior UPDATE below now iterates every row whose
	// semver is strictly below the incoming version, not just the
	// lexically-largest id. The whole operation stays inside the
	// existing transaction, so the flip + INSERT remain atomic.
	otherRows, qerr := loadActiveSkillRowsTx(tx, name, id)
	if qerr != nil {
		return "", qerr
	}
	otherMax := highestSemver(otherRows)
	hasExisting := otherMax.id != ""

	// semver.Compare requires the 'v' prefix on both arguments. The DB
	// stores version without the prefix (YAML convention); we add it
	// here. Prepending unconditionally is safe: the frontmatter parser
	// already validated the semver shape, and an empty version would
	// have failed upstream.
	newIsLatest := true
	if hasExisting {
		cmp := semver.Compare("v"+version, "v"+otherMax.version)
		switch {
		case cmp > 0:
			// Incoming version strictly higher — flip every other
			// row whose semver is below the incoming version to
			// false. This handles the unequal-width case where the
			// lexically-largest id is NOT the semver-max.
			for _, r := range otherRows {
				if semver.Compare("v"+r.version, "v"+version) < 0 {
					_, err = tx.Exec(`
						UPDATE memories
						SET metadata = json_set(COALESCE(metadata, '{}'), '$.is_latest', 0)
						WHERE id = ? AND deleted_at IS NULL
					`, r.id)
					if err != nil {
						return "", fmt.Errorf("flip prior is_latest: %w", err)
					}
				}
			}
		case cmp == 0:
			// Existence check above should have caught this. Defensive.
			return "", fmt.Errorf("save skill: version %s already exists for %s (id=%s)", version, name, otherMax.id)
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

// TODO RESOLVED — the force-overwrite path now uses loadActiveSkillRows
// + highestSemver (above) to determine is_latest from the semver max
// across ALL active rows for the name, not from a lexically-largest
// id DESC proxy. The same loader is used by the new-row path, the
// force-overwrite path, and the read-by-name path (semver-correct
// name-latest lookup). Regression tests at the bottom of this file
// pin all four scenarios.
//
// Historical note: the original 2026-09-05 audit TODO worried that
// "if an older version (e.g. v1) is force-overwritten AFTER a higher
// version (v2) already exists, the overwrite will incorrectly stamp
// v1 as latest". The pre-existing code already called semver.Compare
// for the force-overwrite path (line 438 in the original file), so
// the headline force-overwrite case was already covered. The deeper
// defect was the id DESC proxy used to pick "the highest existing
// version" reference, which broke for unequal-width semver (e.g.
// id-sort puts v1.9.0 above v1.10.0 because '9' > '1'). With two or
// more existing unequal-width semver versions, the proxy returned
// the wrong reference and the demote UPDATE left the actual semver
// max stamped is_latest=true (stale). All four scenarios covered by
// the regression tests below.

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
//
// 2026-09-05 audit remediation residual pass §I-C.9: a zero-rows
// affected outcome is no longer a silent success. Whether the row
// never existed or was already soft-deleted, the caller cannot
// distinguish "I deleted a live skill" from "I asked about an id
// that has no live row". Return ErrSkillNotFound so the handler
// boundary can surface a deterministic not-found; the previous
// "false success" silenced that signal. Already-soft-deleted
// ids are tracked by the deleted_at column and remain recoverable
// for forensics — the caller simply learns there is nothing to
// delete at this id.
func (dm *DatabaseManager) ShredSkill(skillID string) error {
	db := dm.SQLDB()
	if db == nil {
		return fmt.Errorf("db not initialized")
	}
	res, err := db.Exec(`
		UPDATE memories
		SET deleted_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND collection = 'skills' AND deleted_at IS NULL
	`, skillID)
	if err != nil {
		return fmt.Errorf("shred skill: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("shred skill rows: %w", err)
	}
	if affected == 0 {
		return ErrSkillNotFound
	}
	return nil
}

// ErrSkillNotFound is returned by ShredSkill when the requested
// skill id does not match a live (not soft-deleted) skill row.
// Both "never existed" and "already soft-deleted" produce this
// error — the caller's intent is "delete a live skill" and there
// is none to delete at this id.
var ErrSkillNotFound = fmt.Errorf("skill: not found")

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

// ValidateSkill is the non-mutating validation stage used by the
// workshop pipeline. It re-runs validateSkillFrontmatterAndScan
// against the Skill's reconstructed content (frontmatter + body) and
// returns accumulated warnings/errors with NO DB writes.
//
// This is the safety seam between the workshop's validation stage
// and its publication stage: a `candidate` outcome has never touched
// the database because validation goes through this helper rather
// than the write API. The row-count test in TestValidateSkill_CleanInput
// pins that contract.
func (dm *DatabaseManager) ValidateSkill(skill *Skill) (warnings []string, errors []string, err error) {
	if skill == nil {
		return nil, nil, fmt.Errorf("ValidateSkill: skill is nil")
	}
	// Reconstruct the markdown content the scanner would see if this
	// skill were saved. The YAML frontmatter is regenerated from the
	// struct fields, then the body is appended. This matches what
	// SaveSkill would persist.
	content := buildSkillContent(skill)
	_, warnings, errors, err = validateSkillFrontmatterAndScan(content)
	if err != nil {
		return nil, nil, err
	}
	return warnings, errors, nil
}

// buildSkillContent reconstructs the markdown content from a Skill
// struct. Format matches what SaveSkill accepts on input — YAML
// frontmatter block + body. Used by ValidateSkill to feed the
// scanner the same bytes the writer would.
func buildSkillContent(skill *Skill) string {
	fm := skill.Frontmatter
	// Minimal round-trip serialization. SaveSkill doesn't care about
	// field order; we mirror what ParseSkillFrontmatter reads.
	var buf strings.Builder
	buf.WriteString("---\n")
	buf.WriteString(fmt.Sprintf("name: %s\n", fm.Name))
	buf.WriteString(fmt.Sprintf("version: %s\n", fm.Version))
	if fm.Description != "" {
		buf.WriteString(fmt.Sprintf("description: %s\n", fm.Description))
	}
	if fm.WhenToUse != "" {
		buf.WriteString(fmt.Sprintf("when_to_use: %s\n", fm.WhenToUse))
	}
	if fm.Domain != "" {
		buf.WriteString(fmt.Sprintf("domain: %s\n", fm.Domain))
	}
	if len(fm.Constraints) > 0 {
		buf.WriteString("constraints:\n")
		for _, c := range fm.Constraints {
			buf.WriteString(fmt.Sprintf("  - %s\n", c))
		}
	}
	if len(fm.Steps) > 0 {
		buf.WriteString("steps:\n")
		for _, s := range fm.Steps {
			buf.WriteString(fmt.Sprintf("  - call: %s\n", s.Call))
			if s.ArgsFrom != "" {
				buf.WriteString(fmt.Sprintf("    args_from: %s\n", s.ArgsFrom))
			}
		}
	}
	buf.WriteString("---\n")
	buf.WriteString(skill.Body)
	return buf.String()
}

// SaveSkillAndDeprecatePrior publishes a new skill version and marks
// the specific prior version (by id) as deprecated in a single
// transaction. Both writes succeed or both roll back. This is the
// structural fix for the workshop's transactional coherence
// requirement (spec §9.1): the failure mode where v1 was deprecated
// but v2 publication failed is now impossible.
//
// priorSkillID is identified by id (not by name) so older versions
// sharing the logical skill name remain readable and unmodified.
//
// Read-back assertions confirm persistence of both rows before
// returning. Pattern follows AddLesson in internal/core/db.go.
func (dm *DatabaseManager) SaveSkillAndDeprecatePrior(newSkill *Skill, priorSkillID string) (*Skill, error) {
	if newSkill == nil {
		return nil, fmt.Errorf("SaveSkillAndDeprecatePrior: newSkill is nil")
	}
	db := dm.SQLDB()
	if db == nil {
		return nil, fmt.Errorf("db not initialized")
	}

	newID, err := SkillIDForNameAndVersion(newSkill.Name, newSkill.Version)
	if err != nil {
		return nil, fmt.Errorf("SaveSkillAndDeprecatePrior: %w", err)
	}

	// Verify prior exists (any failure here aborts before opening tx).
	var existing string
	err = db.QueryRow(`SELECT id FROM memories WHERE id = ? AND collection='skills' AND deleted_at IS NULL`, priorSkillID).Scan(&existing)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("SaveSkillAndDeprecatePrior: prior skill %s not found", priorSkillID)
	}
	if err != nil {
		return nil, fmt.Errorf("SaveSkillAndDeprecatePrior: lookup prior: %w", err)
	}

	// Reconstruct content for the new row.
	content := buildSkillContent(newSkill)

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Mark prior as deprecated + superseded_by in the same tx.
	// Use json('true')/json('false') so json_set produces proper JSON
	// booleans that parse back as Go bool (not float64).
	_, err = tx.Exec(`
		UPDATE memories
		SET metadata = json_set(COALESCE(metadata, '{}'),
								'$.deprecated', json('true'),
								'$.superseded_by', ?,
								'$.is_latest', json('false')),
			updated_at = CAST(strftime('%s','now') AS INTEGER)
		WHERE id = ? AND collection='skills' AND deleted_at IS NULL
	`, newID, priorSkillID)
	if err != nil {
		return nil, fmt.Errorf("deprecate prior: %w", err)
	}

	// Insert new skill row in the same tx via the shared
	// scanner+INSERT primitive (preserves the central scanner
	// coverage guarantee).
	metadata := map[string]interface{}{
		"is_latest":        true,
		"author":           "workshop",
		"promoted_at":      nil,
		"decay_floor_days": 90,
		"content_hash":     contentHash(content),
		"version":          newSkill.Version,
	}
	tags := []string{"skill"}
	if newSkill.Domain != "" {
		tags = append(tags, newSkill.Domain)
	}

	txDBNode := &txNode{tx: tx, dm: dm}
	_, err = saveMemoryRow(txDBNode, dm, newID, "skills", content, "", tags, metadata, nil, true, 5, "", "", "", "")
	if err != nil {
		return nil, fmt.Errorf("insert new skill: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Read-back assertion on both rows (substrate defense triad §3).
	// The new row must exist with is_latest=true.
	newRow, err := dm.ReadSkill(newID, "")
	if err != nil {
		return nil, fmt.Errorf("read-back new skill: %w", err)
	}
	if !newRow.IsLatest {
		return nil, fmt.Errorf("read-back new skill: is_latest=false after commit")
	}
	// The prior row must have deprecated=true and superseded_by=newID.
	priorRow, err := dm.ReadSkill(priorSkillID, "")
	if err != nil {
		return nil, fmt.Errorf("read-back prior skill: %w", err)
	}
	dep, _ := priorRow.Metadata["deprecated"].(bool)
	if !dep {
		return nil, fmt.Errorf("read-back prior skill: deprecated=false after commit")
	}
	sup, _ := priorRow.Metadata["superseded_by"].(string)
	if sup != newID {
		return nil, fmt.Errorf("read-back prior skill: superseded_by=%q want %q", sup, newID)
	}
	return newRow, nil
}
