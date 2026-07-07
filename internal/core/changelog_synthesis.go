package internal

// changelog_synthesis.go implements the join between git-sourced
// ChangelogEntry rows (built by ParseCommitLog in changelog.go) and
// agent-sourced changelog memories (written via log_to_changelog in
// changelog_mcp.go). The synthesis engine is a pure function: given
// a ChangelogDocument and a database connection, it returns a new
// ChangelogDocument with Body fields populated from matching
// memories, plus a list of orphan memories (memories with
// #commit:<hash> tags that do not correspond to any entry in the
// document).
//
// Architecture (strict retrospective contract):
//
//   - The join key is (commit_hash, mpm_memory_id). The synthesis
//     query walks every memory in the 'changelog' collection,
//     parses its tags, finds the #commit:<hash> tag, and looks up
//     the matching ChangelogEntry by CommitHash. One-to-one: a
//     memory exists iff the commit exists, by design (LogChangelogEntry
//     rejects orphan-by-construction).
//
//   - Bodies are merged in place. If a ChangelogEntry has a Body
//     already (e.g., from --release-notes highlight), the memory
//     Body is APPENDED below it, not overwritten. The highlight
//     block is the operator's prose; the memory prose is the
//     agent's prose; they stack.
//
//   - Orphan memories are NOT silently dropped. They are returned
//     in a separate slice so the caller can render them in a
//     dedicated section at the bottom of the changelog AND emit
//     a terminal warning during the build. Both signals matter:
//     terminal warning = operator sees it during this build;
//     section in the file = human reading the file later sees it.
//
//   - The synthesis function does NOT modify git-sourced entries
//     that have no matching memory. Their body stays empty, the
//     renderer emits just the bullet. No fake "merged" output for
//     entries that were never annotated.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ChangelogMemory is the minimal shape the synthesis engine needs
// from the memories table. We do not import the full Memory struct
// to keep this file decoupled from the rest of the memory model.
type ChangelogMemory struct {
	ID      string
	Content string
	Tags    []string
	// CommitHash is the lowercased hash extracted from the
	// #commit:<hash> tag. Empty if the memory is not associated
	// with a specific commit (should not happen for log_to_changelog
	// memories, but checked defensively).
	CommitHash string
}

// ChangelogSynthesisResult bundles the synthesized document with
// the orphan list. Orphans are returned alongside so the caller
// can both render them and warn about them — keeping the warning
// and the rendered section in lockstep is easier when both come
// from the same return value.
type ChangelogSynthesisResult struct {
	// Document is the original ChangelogDocument with Body fields
	// filled in for entries that have matching changelog memories.
	Document *ChangelogDocument
	// Orphans lists changelog memories that did not match any
	// entry in the document. Reasons include: agent hallucinated
	// a hash; the commit was squashed; the branch was abandoned;
	// the changelog range was computed with --since that excluded
	// the commit.
	Orphans []ChangelogMemory
	// Matched counts the number of entries that received at least
	// one memory. Used for the build summary.
	Matched int
	// Unmatched counts the number of entries that received no
	// memory. These render as plain bullets. Also for summary.
	Unmatched int
}

// FetchChangelogMemories loads every memory in the 'changelog'
// collection, parses its tags, and extracts the commit hash from
// the #commit:<hash> tag if present. Returns the memories indexed
// by commit hash for O(1) lookup during synthesis.
//
// Memories without a #commit:<hash> tag are returned in a separate
// slice (unkeyed) so the caller can surface them as orphans too.
// A correctly-invoked log_to_changelog always sets the tag, so
// unkeyed memories indicate a misuse of the tool or a tag
// corruption event — worth surfacing.
func FetchChangelogMemories(db *sql.DB) (indexed map[string][]ChangelogMemory, unkeyed []ChangelogMemory, err error) {
	if db == nil {
		return nil, nil, fmt.Errorf("FetchChangelogMemories: database is nil")
	}
	rows, err := db.Query(
		`SELECT id, content, tags FROM memories WHERE collection = 'changelog' ORDER BY created_at`)
	if err != nil {
		return nil, nil, fmt.Errorf("FetchChangelogMemories: query: %w", err)
	}
	defer rows.Close()

	indexed = make(map[string][]ChangelogMemory)
	for rows.Next() {
		var id, content, tagsJSON string
		if err := rows.Scan(&id, &content, &tagsJSON); err != nil {
			return nil, nil, fmt.Errorf("FetchChangelogMemories: scan: %w", err)
		}
		var tags []string
		if tagsJSON != "" {
			if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
				// Tag parse failure on a single row is not fatal;
				// we skip it and continue. The caller will see
				// fewer matches but no error. Better than dropping
				// the whole batch on one corrupted row.
				continue
			}
		}
		commitHash := extractCommitHashFromTags(tags)
		mem := ChangelogMemory{
			ID:         id,
			Content:    content,
			Tags:       tags,
			CommitHash: commitHash,
		}
		if commitHash == "" {
			unkeyed = append(unkeyed, mem)
			continue
		}
		indexed[commitHash] = append(indexed[commitHash], mem)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("FetchChangelogMemories: rows.Err: %w", err)
	}
	return indexed, unkeyed, nil
}

// extractCommitHashFromTags walks a tag slice looking for the
// canonical #commit:<lowercase-hash> tag and returns the hash.
// Returns "" if no such tag is present. The lookup is O(N) where
// N is the number of tags (small, typically 2-4 per memory).
func extractCommitHashFromTags(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, CommitTagPrefix) {
			return strings.TrimPrefix(t, CommitTagPrefix)
		}
	}
	return ""
}

// SynthesizeChangelog merges agent-written changelog memories into
// a ChangelogDocument by walking every entry, looking up the
// matching memory by commit hash, and appending the memory body
// to the entry's body. Bodies are appended (not overwritten) so
// the existing --release-notes highlight block is preserved.
//
// The function does NOT mutate the input document. It returns a
// deep-enough copy: top-level slice headers are re-allocated and
// each release's Entries slice is re-allocated with new entry
// values. The Project and date fields are preserved as-is.
func SynthesizeChangelog(doc *ChangelogDocument, db *sql.DB) (*ChangelogSynthesisResult, error) {
	if doc == nil {
		return nil, fmt.Errorf("SynthesizeChangelog: document is nil")
	}
	if db == nil {
		return nil, fmt.Errorf("SynthesizeChangelog: database is nil")
	}

	indexed, unkeyed, err := FetchChangelogMemories(db)
	if err != nil {
		return nil, err
	}

	// Build a working copy. We re-allocate the Releases slice and
	// the Entries slice per release, but reuse the original entry
	// struct values as the starting point. Body is the only field
	// that gets mutated; everything else is read-only.
	working := &ChangelogDocument{
		Project: doc.Project,
		Legacy:  doc.Legacy,
	}
	working.Releases = make([]ChangelogRelease, len(doc.Releases))
	for i, rel := range doc.Releases {
		newEntries := make([]ChangelogEntry, len(rel.Entries))
		copy(newEntries, rel.Entries)
		working.Releases[i] = ChangelogRelease{
			Version: rel.Version,
			Date:    rel.Date,
			Notes:   rel.Notes,
			Entries: newEntries,
		}
	}
	// Legacy section: copy entry slice too so we can mutate bodies
	// if a legacy entry matches (rare, but possible if an agent
	// wrote a changelog memory for a commit before the --since
	// boundary that was still surfaced via --legacy).
	if working.Legacy != nil {
		newEntries := make([]ChangelogEntry, len(doc.Legacy.Entries))
		copy(newEntries, doc.Legacy.Entries)
		working.Legacy = &ChangelogRelease{
			Version: doc.Legacy.Version,
			Date:    doc.Legacy.Date,
			Notes:   doc.Legacy.Notes,
			Entries: newEntries,
		}
	}

	result := &ChangelogSynthesisResult{
		Document: working,
		Orphans:  []ChangelogMemory{},
	}

	// Walk every release's entries. For each non-highlight entry,
	// look up the memory by CommitHash (lowercased — that's how
	// LogChangelogEntry stores it) and merge.
	for ri := range working.Releases {
		for ei := range working.Releases[ri].Entries {
			entry := &working.Releases[ri].Entries[ei]
			if entry.Highlight {
				// Highlights are operator prose; never overwritten
				// by agent memory. Skip.
				continue
			}
			if entry.CommitHash == "" {
				// No commit hash, no possible join. Entry renders
				// as a plain bullet. (Non-conventional commits
				// land here; they cannot be annotated by
				// log_to_changelog because the tool requires a
				// commit_hash argument, but a non-conventional
				// commit is by definition not in the synthesis
				// scope.)
				result.Unmatched++
				continue
			}
			key := strings.ToLower(entry.CommitHash)
			mems, ok := indexed[key]
			if !ok {
				result.Unmatched++
				continue
			}
			// Merge: append each matching memory's content under
			// the entry's existing body. Multiple memories for the
			// same commit are joined with a separator. In practice
			// the strict retrospective contract + the per-commit
			// idempotency at the MCP layer means this should
			// always be 0 or 1 memory per commit; the multi-memory
			// path is defensive.
			merged := entry.Body
			for _, m := range mems {
				if merged == "" {
					merged = strings.TrimSpace(m.Content)
				} else {
					merged = merged + "\n\n" + strings.TrimSpace(m.Content)
				}
			}
			entry.Body = merged
			// Populate the join-key field that the JSON sibling
			// reserves for the synthesis engine. Lets downstream
			// readers see which memories contributed to which
			// entry without re-walking the database.
			for _, m := range mems {
				entry.MPMMemoryIDs = append(entry.MPMMemoryIDs, m.ID)
			}
			result.Matched++
		}
	}
	// Legacy section: same logic if present.
	if working.Legacy != nil {
		for ei := range working.Legacy.Entries {
			entry := &working.Legacy.Entries[ei]
			if entry.CommitHash == "" {
				continue
			}
			key := strings.ToLower(entry.CommitHash)
			if mems, ok := indexed[key]; ok {
				merged := entry.Body
				for _, m := range mems {
					if merged == "" {
						merged = strings.TrimSpace(m.Content)
					} else {
						merged = merged + "\n\n" + strings.TrimSpace(m.Content)
					}
				}
				entry.Body = merged
				for _, m := range mems {
					entry.MPMMemoryIDs = append(entry.MPMMemoryIDs, m.ID)
				}
				result.Matched++
			}
		}
	}

	// Orphans: every indexed key that did NOT match any entry in
	// the document. We walk the indexed map and emit anything
	// that wasn't consumed. The unkeyed slice (memories with no
	// #commit:<hash> tag at all) is also orphan.
	consumed := make(map[string]bool)
	for ri := range working.Releases {
		for _, e := range working.Releases[ri].Entries {
			if e.CommitHash != "" {
				consumed[strings.ToLower(e.CommitHash)] = true
			}
		}
	}
	if working.Legacy != nil {
		for _, e := range working.Legacy.Entries {
			if e.CommitHash != "" {
				consumed[strings.ToLower(e.CommitHash)] = true
			}
		}
	}
	for hash, mems := range indexed {
		if consumed[hash] {
			continue
		}
		result.Orphans = append(result.Orphans, mems...)
	}
	// Unkeyed memories (no #commit:<hash> tag) are always orphans.
	result.Orphans = append(result.Orphans, unkeyed...)

	return result, nil
}
