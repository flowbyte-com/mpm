// directive_tools.go — DM methods for the prime-directives + proactive
// recall hint system.
//
// Two methods covering the agent's bootstrap awareness:
//   - ReadDirectives: every prime directive in the system, in insertion
//     order. Used at session start (mpm wake) and on every mpm call.
//   - ProactiveRecallHint: keyword-matched decisions/theories surfaced
//     automatically when the agent's conversation context overlaps.
package internal

import (
	"encoding/json"
	"fmt"
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
// finds matching decisions/theories.
func (dm *DatabaseManager) ProactiveRecallHint(conversationText string, maxHints int, minScore float64) ([]map[string]interface{}, error) {
	if maxHints <= 0 {
		maxHints = 3
	}
	keywords := ExtractConversationKeywords(conversationText, 50)
	overlaps, err := FindEpistemologyOverlaps(dm, keywords, maxHints, minScore)
	if err != nil {
		return nil, fmt.Errorf("find overlaps: %w", err)
	}
	return overlaps, nil
}