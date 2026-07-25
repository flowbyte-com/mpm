// skill_db.go — DB methods for the skills collection.
//
// Read path mirrors directive_tools.go: indexed lookup, single-statement
// queries, no LLM calls on the hot path. The full save / promote / shred
// surface lands in Tasks 3, 10, and 12; this file owns only the read side
// for now (and the three stub methods required to satisfy the CoreDB
// interface contract — they return an explicit "not yet implemented" error
// until their owning tasks ship).

package internal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrSkillNotImplemented is returned by the save / promote / shred
// methods that ship in Tasks 3, 10, and 12. Surfaced here so consumers
// that import the CoreDB interface (CLI, MCP) get a typed error rather
// than a generic one when they accidentally call a not-yet-implemented
// method on a build that omits the later tasks.
var ErrSkillNotImplemented = errors.New("skill method not yet implemented (see Skills layer plan)")

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
		isGlobal, isPrime                                     int
		weight                                                int
		deletedAt                                             sql.NullString
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

	return &Skill{
		ID:          id,
		Collection:  collection,
		Tags:        parseJSONTags(tagsJSON),
		Metadata:    parseJSONMeta(metaJSON),
		IsGlobal:    isGlobal == 1,
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
	}, nil
}

// ListSkills returns the latest version of each skill in scope.
// scope: "local" | "shared" | "all" — default "all".
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
		  `+scopeClause+`
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
			continue
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

// SaveSkill persists a new skill row. Stubbed here so the CoreDB
// interface compiles; the real implementation lands in Task 3
// (skill authoring).
func (dm *DatabaseManager) SaveSkill(name, version, content, authorAgent string, force bool) (string, error) {
	return "", fmt.Errorf("SaveSkill(%q, %q, ...): %w", name, version, ErrSkillNotImplemented)
}

// PromoteSkillToGlobal moves a local skill into the shared DB so other
// agents can read it. Stubbed here; real implementation lands in Task 10.
func (dm *DatabaseManager) PromoteSkillToGlobal(skillID string, confirm bool) error {
	return fmt.Errorf("PromoteSkillToGlobal(%q): %w", skillID, ErrSkillNotImplemented)
}

// ShredSkill hard-deletes a skill row. Stubbed here; real implementation
// lands in Task 12.
func (dm *DatabaseManager) ShredSkill(skillID string) error {
	return fmt.Errorf("ShredSkill(%q): %w", skillID, ErrSkillNotImplemented)
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