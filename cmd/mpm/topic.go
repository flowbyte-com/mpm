package main

import (
	"strings"

	_ "github.com/mattn/go-sqlite3"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// Topic Suggestion Helpers
// =============================================================================

var stopWords = map[string]bool{
	"the":   true,
	"is":    true,
	"at":    true,
	"to":    true,
	"a":     true,
	"in":    true,
	"on":    true,
	"for":   true,
	"of":    true,
	"and":   true,
	"or":    true,
	"but":   true,
	"with":  true,
	"as":    true,
	"by":    true,
	"from":  true,
	"it":    true,
	"this":  true,
	"that":  true,
	"be":    true,
	"have":  true,
	"has":   true,
	"had":   true,
	"were":  true,
	"was":   true,
	"are":   true,
	"been":  true,
	"being": true,
}

// sanitizeContentForFTS strips markdown, filters stop-words and short words,
// joins remaining keywords with " OR " for FTS5 query.
func sanitizeContentForFTS(content string) string {
	// Strip markdown (using same patterns as recall.go)
	content = stripMarkdownHeadings.ReplaceAllString(content, "")
	content = stripMarkdownBold.ReplaceAllString(content, "$1")
	content = stripMarkdownItalic.ReplaceAllString(content, "$1")
	content = stripMarkdownCode.ReplaceAllString(content, "$1")

	// Split on whitespace
	words := strings.Fields(content)

	// Filter stop-words and words < 4 chars
	var keywords []string
	for _, w := range words {
		lower := strings.ToLower(w)
		if stopWords[lower] {
			continue
		}
		if len(w) < 4 {
			continue
		}
		keywords = append(keywords, lower)
	}

	// Need at least 2 keywords
	if len(keywords) < 2 {
		return ""
	}

	return strings.Join(keywords, " OR ")
}

// computeTopicConfidence counts how many memoryKeywords appear in topicName.
// Returns matched / total_topic_words (0 if no topic words).
func computeTopicConfidence(memoryKeywords []string, topicName string) float64 {
	if len(memoryKeywords) == 0 || topicName == "" {
		return 0
	}

	topicWords := strings.Fields(strings.ToLower(topicName))
	if len(topicWords) == 0 {
		return 0
	}

	var matched int
	for _, kw := range memoryKeywords {
		kwLower := strings.ToLower(kw)
		for _, tw := range topicWords {
			if kwLower == tw {
				matched++
				break
			}
		}
	}

	return float64(matched) / float64(len(topicWords))
}

// suggestTopicsForMemory uses FTS5 to find topics matching the content,
// scores them by confidence, and returns suggestions meeting minConfidence.
func suggestTopicsForMemory(dm *mpminternal.DatabaseManager, memoryID, content string, maxTopics int, minConfidence float64) ([]map[string]interface{}, error) {
	// Sanitize content for FTS query
	ftsQuery := sanitizeContentForFTS(content)
	if ftsQuery == "" {
		return nil, nil
	}

	// Also get memory keywords from sanitize output for confidence scoring
	memoryKeywords := strings.Fields(ftsQuery)

	// FTS5 query to find active topics matching the content.
	//
	// Structural-topic exclusion: the `decisions` and `theories` topics are
	// auto-created cross-reference anchors for the epistemology collections.
	// They have empty description and empty tags by design — surfacing them
	// as suggestions for every new memory pollutes the cognitive space. The
	// filter below mirrors internal/wake_context.recentTopicNames: a topic
	// is structural if its description is empty/null/'{}' AND its tags are
	// empty/null/'[]'; the name allowlist is defense-in-depth.
	sql := `
		SELECT t.id, t.name, t.description
		FROM topics t
		JOIN topics_fts fts ON t.rowid = fts.rowid
		WHERE topics_fts MATCH ? AND t.is_active = 1
		  AND NOT (
		    (t.description IS NULL OR t.description = '' OR t.description = '{}')
		    AND (t.tags IS NULL OR t.tags = '' OR t.tags = '[]')
		  )
		  AND t.name NOT IN ('decisions', 'theories')
		LIMIT ?`

	rows, err := dm.SQLDB().Query(sql, ftsQuery, maxTopics)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var suggestions []map[string]interface{}
	for rows.Next() {
		var id, name, description string
		if err := rows.Scan(&id, &name, &description); err != nil {
			continue
		}

		// Compute confidence using topic name (and description, take max)
		confidenceName := computeTopicConfidence(memoryKeywords, name)
		confidenceDesc := computeTopicConfidence(memoryKeywords, description)
		confidence := confidenceName
		if confidenceDesc > confidence {
			confidence = confidenceDesc
		}

		if confidence < minConfidence {
			continue
		}

		suggestions = append(suggestions, map[string]interface{}{
			"id":          id,
			"name":        name,
			"description": description,
			"confidence":  confidence,
		})
	}

	return suggestions, nil
}
