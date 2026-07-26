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
	"strings"
)

// ReadDirectives returns all memories that are prime directives.
// A memory is a directive if either it was ingested with
// collection='directives' (the MCP path) or has is_prime_directive=1
// (the legacy column-based path). Both identifiers reach the same set
// once either is set — see the directives section of README.md.
func (dm *DatabaseManager) ReadDirectives() ([]map[string]interface{}, error) {
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, metadata, created_at FROM memories
		WHERE (collection = 'directives' OR is_prime_directive = 1)
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
			continue
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
