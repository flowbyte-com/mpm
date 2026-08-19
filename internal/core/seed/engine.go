// Package seed (engine.go) — the runtime that performs the seeding.
//
// ApplyDirectives walks the SeedDirectives registry and idempotently
// inserts each entry as a row in the memories table (collection='directives').
// The contract:
//
//   - StableID present, content matches → Skipped (no-op)
//   - StableID present, content drifted → Updated flag (operator's edit preserved, flagged for visibility)
//   - StableID absent → Created (INSERT OR IGNORE on id PRIMARY KEY)
//
// ApplyDirectives never overwrites a local edit. The "Updated" bucket
// is for visibility — the operator sees that their local copy differs
// from the upstream seed and can decide to reconcile manually. This
// preserves the "less auto-noise" voice: the system never silently
// mutates operator content.
//
// The function is safe to call concurrently from multiple processes
// against the same DB only if SQLite's locking is properly serialized
// (which it is by default in WAL mode for single-writer).
package seed

import (
	"database/sql"
	"fmt"
	"strings"
)

// ApplyDirectives walks the SeedDirectives registry and inserts any
// missing rows into the memories table. Returns a SeedSummary that
// the CLI command prints as a human-readable report.
//
// dm must be a connected DatabaseManager (any path — local DB or
// attached shared DB). The function uses dm.SQLDB() for the direct
// INSERT OR IGNORE — it does not call the SaveMemoryWithContext
// helper because that path generates a fresh id (we want our
// StableID as the primary key so the seeded row is identifiable
// across re-runs).
func ApplyDirectives(dm interface {
	SQLDB() *sql.DB
}) (SeedSummary, error) {
	summary := SeedSummary{
		Created: []string{},
		Skipped: []string{},
		Updated: []string{},
	}

	db := dm.SQLDB()
	if db == nil {
		return summary, fmt.Errorf("seed.ApplyDirectives: db not initialized")
	}

	for _, sd := range SeedDirectives {
		// 1. Look up the row by id, including soft-deleted rows.
		// Liveness is sentinel-agnostic: NULL and 0 both mean "live"
		// (legacy installs wrote deleted_at = 0; current code writes
		// NULL and soft-deletes with a real Unix-epoch value).
		var existingContent string
		var existingID string
		var existingDeleted sql.NullInt64
		err := db.QueryRow(
			`SELECT id, content, deleted_at FROM memories WHERE id = ?`,
			sd.StableID,
		).Scan(&existingID, &existingContent, &existingDeleted)
		if err != nil && err != sql.ErrNoRows {
			return summary, fmt.Errorf("seed lookup %s: %w", sd.StableID, err)
		}

		softDeleted := err == nil && existingDeleted.Valid && existingDeleted.Int64 > 0

		switch {
		case err == sql.ErrNoRows || softDeleted:
			// 2a. No live row. Insert if truly absent, otherwise revive
			// the soft-deleted row with the current seed content.
			//
			// INSERT OR IGNORE alone cannot restore a shredded baseline:
			// the stable-id PRIMARY KEY is still occupied by the dead
			// row, so the insert is silently swallowed while the summary
			// reports "Created". The tiered-fallback contract says the
			// baseline is non-negotiable ("standalone runtime is never
			// directive-blind"), so re-init must UNDELETE rather than
			// collide. This also makes `mpm ops init directives` actually
			// usable as the documented recovery path after an accidental
			// shred.
			if softDeleted {
				if err := reviveSeedRow(db, sd); err != nil {
					return summary, fmt.Errorf("revive %s: %w", sd.StableID, err)
				}
			} else if err := insertSeedRow(db, sd); err != nil {
				return summary, fmt.Errorf("insert %s: %w", sd.StableID, err)
			}
			summary.Created = append(summary.Created, sd.StableID)

		case err != nil:
			// 2b. Real DB error.
			return summary, fmt.Errorf("lookup %s: %w", sd.StableID, err)

		default:
			// 2c. Live row exists — compare content.
			if strings.TrimSpace(existingContent) == strings.TrimSpace(sd.Content) {
				summary.Skipped = append(summary.Skipped, sd.StableID)
			} else {
				// Operator's local edit diverged from seed. Preserve
				// the local edit; flag in summary for visibility.
				summary.Updated = append(summary.Updated, sd.StableID)
			}
		}
	}

	return summary, nil
}

// reviveSeedRow restores a soft-deleted seed row (see ApplyDirectives
// 2a). A soft-deleted stable id still occupies the PRIMARY KEY, so a
// plain insert would be silently swallowed by INSERT OR IGNORE. UNDELETE
// instead and refresh the row with the current seed content/tags/scope.
// The FTS reindex and memory-revision triggers fire on the UPDATE, so
// the revived directive becomes searchable and revisioned like any
// other write.
func reviveSeedRow(db *sql.DB, sd SeedDirective) error {
	tagsJSON := "[" + strings.Join(quoteStrings(sd.Tags), ",") + "]"
	scope := sd.Scope
	if scope == "" {
		scope = "global"
	}
	meta := fmt.Sprintf(`{"is_prime_directive":1,"scope":%q,"provenance":{"agent":"mpm_ops_init","compute":"absolute","model":"direct","persona":"operator","source":"baseline_cognitive_bootstrap"}}`, scope)
	_, err := db.Exec(`
		UPDATE memories
		SET deleted_at = NULL, content = ?, tags = ?, metadata = ?,
		    is_prime_directive = 1, collection = 'directives'
		WHERE id = ? AND deleted_at IS NOT NULL AND deleted_at > 0`,
		sd.Content, tagsJSON, meta, sd.StableID)
	return err
}

// insertSeedRow writes one seed row. Uses INSERT OR IGNORE on the
// primary key so a race between two concurrent `mpm ops init directives`
// runs is benign — only one wins, the other sees ErrNoRows on lookup
// and skips. Collection is 'directives' (matches the MCP read path);
// is_prime_directive is set to 1 for legacy web/CLI read compatibility.
//
// Scope is materialised into metadata JSON. Empty/unset scope defaults
// to "global" at write time so legacy seeded rows (no scope in metadata)
// match the same predicate in ReadDirectivesForFramework via the
// json_extract(metadata, '$.scope') IS NULL branch. See
// docs/architecture/directives.md §3.
func insertSeedRow(db *sql.DB, sd SeedDirective) error {
	tagsJSON := "[" + strings.Join(quoteStrings(sd.Tags), ",") + "]"
	scope := sd.Scope
	if scope == "" {
		scope = "global"
	}
	meta := fmt.Sprintf(`{"is_prime_directive":1,"scope":%q,"provenance":{"agent":"mpm_ops_init","compute":"absolute","model":"direct","persona":"operator","source":"baseline_cognitive_bootstrap"}}`, scope)
	_, err := db.Exec(`
		INSERT OR IGNORE INTO memories
		    (id, collection, content, tags, metadata, is_prime_directive, weight, confidence, retrieval_priority, importance)
		VALUES
		    (?, 'directives', ?, ?, ?, 1, 10, 1.0, 1.0, 1.0)`,
		sd.StableID, sd.Content, tagsJSON, meta)
	return err
}

// quoteStrings wraps each tag in JSON double quotes. Replaces the
// strings/encoding/json import for a 4-line dependency we don't
// otherwise need. Safe for ASCII tags (the registry only uses
// alphanumeric + underscores in tag names).
func quoteStrings(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return out
}

// ApplySkills walks the SeedSkills registry and idempotently inserts
// each entry as a skill row (collection='skills'). The contract:
//
//   - Row absent → Created (SaveSkill is the canonical writer)
//   - Row present, content matches → Skipped (no-op)
//   - Row present, content drifted → Updated flag (operator's edit
//     preserved; surfaced in summary for visibility)
//
// ApplySkills never overwrites a local edit. The "Updated" bucket
// is for visibility — the operator sees that their local copy
// differs from the upstream seed and can decide to reconcile
// manually. This matches the ApplyDirectives voice: the system
// never silently mutates operator content.
//
// We route through SaveSkill (not raw INSERT OR IGNORE) so the
// scanner, frontmatter validation, content_hash, LTM-by-default,
// and version-flip machinery all run — same guarantees any operator
// SaveSkill invocation gets, with no shortcut.
func ApplySkills(dm interface {
	SQLDB() *sql.DB
	SaveSkill(name, version, content, authorAgent string, force bool) (string, error)
}) (SeedSummary, error) {
	summary := SeedSummary{
		Created: []string{},
		Skipped: []string{},
		Updated: []string{},
	}

	db := dm.SQLDB()
	if db == nil {
		return summary, fmt.Errorf("seed.ApplySkills: db not initialized")
	}

	for _, s := range SeedSkills {
		savedID, err := s.SavedID()
		if err != nil {
			return summary, fmt.Errorf("seed %s: %w", s.StableID, err)
		}

		// Look up existing row by saved id. The deleted_at IS NULL
		// filter matches ApplyDirectives — soft-deleted rows are
		// treated as absent so a re-init after an accidental shred
		// can recover them.
		var existingContent string
		err = db.QueryRow(
			`SELECT content FROM memories WHERE id = ? AND deleted_at IS NULL`,
			savedID,
		).Scan(&existingContent)

		switch {
		case err == sql.ErrNoRows:
			// No existing row — insert via the canonical writer.
			id, saveErr := dm.SaveSkill(s.Name, s.Version, s.Content, "seed:baseline", false)
			if saveErr != nil {
				return summary, fmt.Errorf("save %s: %w", s.StableID, saveErr)
			}
			summary.Created = append(summary.Created, id)

		case err != nil:
			return summary, fmt.Errorf("lookup %s: %w", s.StableID, err)

		default:
			// Row exists — compare content. TrimSpace so trailing
			// newlines (SaveSkill is stricter than the registry
			// string literals) don't trigger false drift reports.
			if strings.TrimSpace(existingContent) == strings.TrimSpace(s.Content) {
				summary.Skipped = append(summary.Skipped, savedID)
			} else {
				summary.Updated = append(summary.Updated, savedID)
			}
		}
	}

	return summary, nil
}