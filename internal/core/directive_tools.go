// directive_tools.go — DM methods for the prime-directives + proactive
// recall hint system.
//
// Two methods covering the agent's bootstrap awareness:
//   - ReadDirectives: every prime directive in the system, in insertion
//     order. Used at session start (mpm wake) and on every mpm call.
//   - ProactiveRecallHint: keyword-matched decisions/theories surfaced
//     automatically when the agent's conversation context overlaps.
//     Skills are a fourth hint source: any skill whose when_to_use
//     field overlaps a conversation keyword is appended (best-effort;
//     ListSkills errors are non-fatal — the epistemology overlap hits
//     are still surfaced).
package internal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// ReadDirectives returns all memories that are prime directives.
// A memory is a directive if either it was ingested with
// collection='directives' (the MCP path) or has is_prime_directive=1
// (the legacy column-based path). Both identifiers reach the same set
// once either is set — see the directives section of docs/SPEC.md.
//
// ReadDirectives returns ALL directives regardless of scope; it is the
// admin / CLI / web-DB view. The runtime agent wake path uses
// ReadDirectivesForFramework instead, which filters by scope.
func (dm *DatabaseManager) ReadDirectives() ([]map[string]interface{}, error) {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, metadata, created_at FROM memories
		WHERE (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)
		  AND deleted_at IS NULL
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query directives: %w", err)
	}
	defer rows.Close()
	var directives []map[string]interface{}
	for rows.Next() {
		var id, content, createdAt string
		var metaJSON *string
		if err := rows.Scan(&id, &content, &metaJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning directive row: %w", err)
		}
		var meta map[string]interface{}
		if metaJSON != nil && *metaJSON != "" {
			json.Unmarshal([]byte(*metaJSON), &meta)
		}
		directives = append(directives, map[string]interface{}{
			"id":         id,
			"content":    content,
			"metadata":   meta,
			"created_at": createdAt,
		})
	}
	return directives, nil
}

// ReadDirectivesForFramework returns the prime directives that apply to
// the given agent framework. Scope filtering is the substrate's job, not
// the plugin's — every framework id reaches MPM via MPM_FRAMEWORK env at
// mpm-mcp startup (see internal/core/mpmcli.ActiveContextFromEnv) and is
// passed to this function by handleReadDirectives.
//
// Selection rule (additive union, see docs/archive/directives.md §4):
//
//	directives where scope IS NULL  (legacy rows pre-scope)
//	OR scope = "global"
//	OR scope = "framework:<fw>"
//
// Empty `fw` falls back to "global only" (the unset-MPM_FRAMEWORK default
// is the safe one — never accidentally surface a framework-specific
// directive to an unidentified caller).
//
// Tiered sources (tiered fallback seeding, 2026-08-19):
//
//  1. Local constitutional baseline — always present (seeded at boot by
//     initUnifiedSchema via seed.ApplyDirectives).
//  2. Shared overlay — when MPM_SHARED_DB is attached, shared.memories
//     directive rows are merged additively. Rows are deduplicated by
//     stable id; a shared row with the same id as a local row wins
//     (org-wide policy overrides the local copy).
//
// Returned rows are sorted by id ASC for deterministic agent wake order.
func (dm *DatabaseManager) ReadDirectivesForFramework(fw string) ([]map[string]interface{}, error) {
	var scopedVal interface{}
	if fw != "" {
		scopedVal = "framework:" + fw
	} else {
		// When framework is unset, no row matches the "framework:<fw>"
		// branch (sqlite parameter NULL ≠ any string literal). Global
		// + null-scope rows still match. This is the intended behaviour.
		scopedVal = nil
	}

	local, err := dm.queryDirectivesScope("", scopedVal)
	if err != nil {
		return nil, fmt.Errorf("query directives for framework %q: %w", fw, err)
	}

	// Shared overlay: additive union when a shared DB is attached.
	if dm.SharedAttached() != "" {
		shared, err := dm.queryDirectivesScope("shared.", scopedVal)
		if err != nil {
			// Shared attachment is an additive layer; a broken shared
			// schema must not blind the local constitutional baseline.
			slog.Warn("shared directive overlay query failed; continuing local-only", "error", err.Error())
		} else {
			byID := make(map[string]map[string]interface{}, len(local)+len(shared))
			for _, d := range local {
				byID[d["id"].(string)] = d
			}
			for _, d := range shared {
				// Shared wins on id collision (org-wide policy override).
				byID[d["id"].(string)] = d
			}
			merged := make([]map[string]interface{}, 0, len(byID))
			for _, d := range byID {
				merged = append(merged, d)
			}
			sort.Slice(merged, func(i, j int) bool {
				return merged[i]["id"].(string) < merged[j]["id"].(string)
			})
			return merged, nil
		}
	}
	return local, nil
}

// queryDirectivesScope runs the scope-filtered directive query against
// the given schema prefix ("" for the local memories table, "shared."
// for an attached shared DB). The scan/parse logic is shared between the
// local baseline and the shared overlay so both tiers surface identical
// row shapes.
func (dm *DatabaseManager) queryDirectivesScope(schemaPrefix string, scopedVal interface{}) ([]map[string]interface{}, error) {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, metadata, created_at FROM `+schemaPrefix+`memories
		WHERE (collection = 'directives' OR COALESCE(is_prime_directive, 0) = 1)
		  AND COALESCE(deleted_at, 0) = 0
		  AND (
		    json_extract(metadata, '$.scope') IS NULL
		    OR json_extract(metadata, '$.scope') = 'global'
		    OR json_extract(metadata, '$.scope') = ?
		  )
		ORDER BY id ASC
	`, scopedVal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var directives []map[string]interface{}
	for rows.Next() {
		var id, content, createdAt string
		var metaJSON *string
		if err := rows.Scan(&id, &content, &metaJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning directive row: %w", err)
		}
		var meta map[string]interface{}
		if metaJSON != nil && *metaJSON != "" {
			json.Unmarshal([]byte(*metaJSON), &meta)
		}
		directives = append(directives, map[string]interface{}{
			"id":         id,
			"content":    content,
			"metadata":   meta,
			"created_at": createdAt,
		})
	}
	return directives, nil
}

// ProactiveRecallHint extracts keywords from a conversation snippet and
// finds matching decisions/theories, then layers skill-matching on top:
// any skill whose when_to_use field contains one of the same keywords
// is appended as a hint. Skill scan errors are non-fatal; the
// decision/theory overlaps are returned even if ListSkills fails.
func (dm *DatabaseManager) ProactiveRecallHint(conversationText string, maxHints int, minScore float64) ([]map[string]interface{}, error) {
	if maxHints <= 0 {
		maxHints = 3
	}
	keywords := ExtractConversationKeywords(conversationText, 50)
	overlaps, err := FindEpistemologyOverlaps(dm, keywords, maxHints, minScore)
	if err != nil {
		return nil, fmt.Errorf("find overlaps: %w", err)
	}

	// Build the keyword set once for substring matching against each
	// skill's when_to_use haystack. Skills use case-insensitive
	// substring rather than the BM25 path decisions/theories use —
	// when_to_use is a short free-form taxonomy, not a long document,
	// so token-level BM25 would over-fragment ("wp" vs "wp-deploy").
	keywordSet := make(map[string]bool, len(keywords))
	for _, k := range keywords {
		keywordSet[k] = true
	}

	skills, skillErr := dm.ListSkills("all")
	if skillErr == nil {
		for _, s := range skills {
			if !keywordOverlap(keywordSet, s.WhenToUse) {
				continue
			}
			overlaps = append(overlaps, map[string]interface{}{
				"content": FormatSkillHint(s),
				"type":    "skill",
				"name":    s.Name,
				"version": s.Version,
				// SkillHintScoreSentinel is a sentinel, not a relevance
				// signal: the keyword overlap pass has no BM25 analogue
				// and shouldn't pretend to. Consumers that re-rank by
				// score should treat skill hints as boolean triggers,
				// not comparable to BM25-decision/theory hits.
				"score": SkillHintScoreSentinel,
			})
		}
	}

	// Trim to maxHints. Appending skills after the BM25-ranked
	// decision/theory hits can displace the tail of the BM25 ranking
	// when more overlapping skills exist than the budget allows; that
	// displacement is intentional — `when_to_use` is author-curated
	// taxonomy and a stronger signal than BM25 on short documents, so
	// surfaced skills take priority over low-ranked BM25 hits.
	if len(overlaps) > maxHints {
		overlaps = overlaps[:maxHints]
	}
	return overlaps, nil
}

// keywordOverlap returns true when any keyword appears as a
// case-insensitive substring of haystack. Returns false on the first
// miss for an empty keyword set so callers don't have to special-case
// the "no keywords extracted" path.
func keywordOverlap(keywords map[string]bool, haystack string) bool {
	if len(keywords) == 0 || haystack == "" {
		return false
	}
	lowered := strings.ToLower(haystack)
	for kw := range keywords {
		if strings.Contains(lowered, kw) {
			return true
		}
	}
	return false
}

// FormatSkillHint renders the human-readable one-liner surfaced as the
// hint's `content` field. Kept terse on purpose — the MCP consumer
// (`handleProactiveRecallHint`) shows this verbatim, and the wake-
// context catalogue is the richer view; this is just the trigger line
// that prompts the agent to call read_skill.
func FormatSkillHint(s SkillSummary) string {
	return fmt.Sprintf("skill %q (v%s) applies here. when_to_use: %s. Read with mpm call read_skill.", s.Name, s.Version, s.WhenToUse)
}

// SkillHintScoreSentinel is the score assigned to skill hints surfaced
// by ProactiveRecallHint. It is NOT a relevance signal — see the
// package doc. Exported so consumers can grep for the sentinel value
// and treat skill hints as boolean triggers rather than re-ranking by
// score.
const SkillHintScoreSentinel = 0.5
