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
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
		Revived: []string{},
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
		var existingTags sql.NullString
		var existingID string
		var existingDeleted sql.NullInt64
		err := db.QueryRow(
			`SELECT id, content, tags, deleted_at FROM memories WHERE id = ?`,
			sd.StableID,
		).Scan(&existingID, &existingContent, &existingTags, &existingDeleted)
		if err != nil && err != sql.ErrNoRows {
			return summary, fmt.Errorf("seed lookup %s: %w", sd.StableID, err)
		}

		softDeleted := err == nil && existingDeleted.Valid && existingDeleted.Int64 > 0

		switch {
		case err == sql.ErrNoRows:
			// 2a. No row at all. Insert.
			if err := insertSeedRow(db, sd); err != nil {
				return summary, fmt.Errorf("insert %s: %w", sd.StableID, err)
			}
			summary.Created = append(summary.Created, sd.StableID)

		case softDeleted:
			// 2a-bis. The row exists but is soft-deleted.
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
			//
			// The restore must not destroy what the operator had
			// customised: a row whose content still matches the seed is
			// restored exactly, and a row whose content DRIFTED is
			// restored WITH the operator's content. The drifted case is
			// reported under its own bucket so the summary never claims
			// a fresh creation for a row that carried operator content.
			drifted, err := reviveSeedRow(db, sd, existingContent, existingTags.String)
			if err != nil {
				return summary, fmt.Errorf("revive %s: %w", sd.StableID, err)
			}
			if drifted {
				summary.Revived = append(summary.Revived, sd.StableID)
			} else {
				summary.Created = append(summary.Created, sd.StableID)
			}

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
// 2a-bis). A soft-deleted stable id still occupies the PRIMARY KEY, so
// a plain insert would be silently swallowed by INSERT OR IGNORE.
// UNDELETE instead.
//
// The row's CONTENT is the operator's. The restore used to overwrite
// content, tags and metadata with the seed's values, so an operator who
// had edited a baseline directive lost the edit on the next seed run —
// silently — and the summary reported the row as freshly "Created",
// the same label used for a row that had never existed.
//
// The contract the seed has always had is "ApplyDirectives never
// overwrites a local edit", and the live-row path honours it. The
// restore path now honours it too:
//
//   - content still MATCHES the seed → the row is restored exactly.
//     Nothing was customised, so nothing is lost, and the row is
//     reported under Created (the baseline is back).
//   - content DRIFTED from the seed → the row is restored WITH the
//     operator's content and tags, and reported under its own bucket so
//     the report never claims a fresh creation for a row that carried
//     operator content.
//
// The METADATA is regenerated either way: it carries the seed's scope
// and provenance, which are substrate bookkeeping rather than operator
// content (the scope is seed-owned by contract — every baseline
// directive is global).
//
// The FTS reindex and memory-revision triggers fire on the UPDATE, so
// the revived directive becomes searchable and revisioned like any
// other write.
//
// Returns drifted=true when the operator's content was preserved rather
// than refreshed from the seed.
func reviveSeedRow(db *sql.DB, sd SeedDirective, existingContent, existingTags string) (bool, error) {
	drifted := seedRowDrifted(existingContent, existingTags, sd)
	content := sd.Content
	tagsJSON := "[" + strings.Join(quoteStrings(sd.Tags), ",") + "]"
	if drifted {
		content = existingContent
		if trimmed := strings.TrimSpace(existingTags); trimmed != "" {
			tagsJSON = trimmed
		}
	}
	scope := sd.Scope
	if scope == "" {
		scope = "global"
	}
	meta := seedRowMetadata(scope)
	_, err := db.Exec(`
		UPDATE memories
		SET deleted_at = NULL, content = ?, tags = ?, metadata = ?,
		    is_prime_directive = 1, collection = 'directives'
		WHERE id = ? AND deleted_at IS NOT NULL AND deleted_at > 0`,
		content, tagsJSON, meta, sd.StableID)
	if err != nil {
		return false, err
	}
	return drifted, nil
}

// seedRowMetadata builds the metadata JSON a seeded directive row carries.
//
// It is the SINGLE writer of that metadata, and it uses the
// DirectiveProvenanceSource constant rather than a literal, so the writer
// and the reader (OrphanedDirectives, which looks for the marker) cannot
// drift apart.
//
// The metadata carries three things:
//   - is_prime_directive: the legacy column-marker, so the web/CLI read
//     paths that predate the collection column still find the row;
//   - scope: the seed's scope, which is seed-owned by contract;
//   - provenance: where the row came from. `source` is what distinguishes
//     a row the baseline seeder wrote from an operator-authored directive —
//     see IsRegistryBackedDirective.
func seedRowMetadata(scope string) string {
	return fmt.Sprintf(`{"is_prime_directive":1,"scope":%q,"provenance":{"agent":"mpm_ops_init","compute":"absolute","model":"direct","persona":"operator","source":%q}}`,
		scope, DirectiveProvenanceSource)
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
// docs/library/directives.md §3.
func insertSeedRow(db *sql.DB, sd SeedDirective) error {
	tagsJSON := "[" + strings.Join(quoteStrings(sd.Tags), ",") + "]"
	scope := sd.Scope
	if scope == "" {
		scope = "global"
	}
	meta := seedRowMetadata(scope)
	// created_at / updated_at are provided explicitly so the row
	// invariant (non-NULL created_at) holds on databases whose
	// `memories.created_at` column lacks the canonical DEFAULT
	// clause — see MigrateMemoriesCreatedAtBackfill for the legacy
	// backfill. Computing the value once keeps the two timestamps
	// consistent within a single INSERT.
	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT OR IGNORE INTO memories
		    (id, collection, content, tags, metadata, is_prime_directive, weight, confidence, retrieval_priority, importance, created_at, updated_at)
		VALUES
		    (?, 'directives', ?, ?, ?, 1, 10, 1.0, 1.0, 1.0, ?, ?)`,
		sd.StableID, sd.Content, tagsJSON, meta, now, now)
	return err
}

// seedRowDrifted reports whether a stored row differs from the seed in
// any way the restore would overwrite.
//
// CONTENT is compared verbatim (trimmed). TAGS are compared as SETS: the
// column holds a JSON array, and a row written by a different code path
// may order or space its tags differently without being customised.
//
// The tag comparison exists because the restore writes the tags as well
// as the content, and a tag the operator added is as much their
// customisation as the text they wrote. Anything the restore would
// overwrite has to be detected before it is overwritten.
//
// The LIVE-row path compares the content only. It writes nothing, so a
// tag-only difference there destroys nothing and is not flagged.
func seedRowDrifted(content, tags string, sd SeedDirective) bool {
	if strings.TrimSpace(content) != strings.TrimSpace(sd.Content) {
		return true
	}
	return !sameTagSet(tags, sd.Tags)
}

// sameTagSet reports whether a stored JSON tags column carries exactly
// the seed's tags.
func sameTagSet(stored string, seedTags []string) bool {
	var parsed []string
	if err := json.Unmarshal([]byte(stored), &parsed); err != nil {
		// Not a JSON array — fall back to a literal comparison so an
		// unreadable column is never mistaken for a match.
		return strings.TrimSpace(stored) == strings.TrimSpace("["+strings.Join(quoteStrings(seedTags), ",")+"]")
	}
	if len(parsed) != len(seedTags) {
		return false
	}
	have := make(map[string]bool, len(parsed))
	for _, t := range parsed {
		have[t] = true
	}
	for _, t := range seedTags {
		if !have[t] {
			return false
		}
	}
	return true
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
		Revived: []string{},
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