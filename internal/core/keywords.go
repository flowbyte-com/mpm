package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

var englishStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "should": true, "could": true,
	"may": true, "might": true, "must": true, "can": true, "this": true,
	"that": true, "these": true, "those": true, "i": true, "you": true,
	"he": true, "she": true, "it": true, "we": true, "they": true,
	"and": true, "or": true, "but": true, "if": true, "then": true,
	"so": true, "as": true, "for": true, "not": true, "with": true,
	"from": true, "by": true, "on": true, "at": true, "to": true,
	"in": true, "of": true,
}

func ExtractConversationKeywords(text string, maxTokens int) []string {
	enc, err := getTiktokenEncoder()
	if err != nil {
		return nil
	}

	tokens := enc.Encode(text, nil, nil)
	if len(tokens) > maxTokens {
		tokens = tokens[len(tokens)-maxTokens:]
	}

	decoded := enc.Decode(tokens)
	words := strings.Fields(strings.ToLower(decoded))

	var keywords []string
	for _, w := range words {
		w = strings.TrimFunc(w, func(r rune) bool {
			return r == '.' || r == ',' || r == '!' || r == '?' || r == '"' || r == '\'' || r == '(' || r == ')' || r == ':' || r == ';'
		})
		if _, ok := englishStopwords[w]; ok || len(w) < 3 {
			continue
		}
		keywords = append(keywords, w)
	}
	return keywords
}

func FindEpistemologyOverlaps(dm *DatabaseManager, keywords []string, limit int, threshold float64) ([]map[string]interface{}, error) {
	if len(keywords) == 0 {
		return nil, nil
	}

	sanitised := make([]string, 0, len(keywords))
	for _, w := range keywords {
		w = strings.TrimFunc(w, func(r rune) bool {
			return r == '*' || r == '"' || r == '(' || r == ')'
		})
		up := strings.ToUpper(w)
		if up == "AND" || up == "OR" || up == "NOT" || up == "NEAR" || w == "" {
			continue
		}
		sanitised = append(sanitised, w)
	}
	if len(sanitised) == 0 {
		return nil, nil
	}
	query := strings.Join(sanitised, " ")

	if limit <= 0 {
		limit = 3
	}

	rows, err := dm.SQLDB().Query(`
		SELECT m.id, m.content, m.collection, m.tags, m.metadata, m.created_at,
		       bm25(memories_fts) as score
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.deleted_at IS NULL
		  AND m.collection IN ('theories', 'decisions')
		ORDER BY score
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("FTS5 epistemology overlap query failed: %w", err)
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, content, collection string
		var tags, metadata sql.NullString
		var createdAt string
		var score float64

		if err := rows.Scan(&id, &content, &collection, &tags, &metadata, &createdAt, &score); err != nil {
			continue
		}

		if score >= threshold {
			continue
		}

		record := map[string]interface{}{
			"id":         id,
			"content":    content,
			"collection": collection,
			"tags":       tags.String,
			"created_at": createdAt,
			"score":      score,
		}

		if metadata.Valid && metadata.String != "" {
			var metaMap map[string]interface{}
			if err := json.Unmarshal([]byte(metadata.String), &metaMap); err == nil {
				record["metadata"] = metaMap
			}
		}

		results = append(results, record)
	}
	if results == nil {
		results = []map[string]interface{}{}
	}
	return results, nil
}
