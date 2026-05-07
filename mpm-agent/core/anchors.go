package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Anchor struct {
	ID             string
	Facts          []string
	Summary        string
	Tags           []string
	Context        string
	Weight         int
	SessionID      string
	ReferenceCount int
	ExpiresAt      string
	CreatedAt      string
	IsCondensed    bool
	AncestorIDs    []string
}

func ParseAnchorFacts(factsJSON string) []string {
	if factsJSON == "" {
		return nil
	}
	var facts []string
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return nil
	}
	return facts
}

func ParseAnchorTags(tagsJSON string) []string {
	if tagsJSON == "" {
		return nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
		return nil
	}
	return tags
}

func AnchorFactsToJSON(facts []string) string {
	if len(facts) == 0 {
		return ""
	}
	b, _ := json.Marshal(facts)
	return string(b)
}

func AnchorTagsToJSON(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	b, _ := json.Marshal(tags)
	return string(b)
}

func InsertAnchor(db *sql.DB, facts []string, summary string, tags []string, context, sessionID string, weight int) error {
	id := GenerateID()
	now := time.Now().Format(time.RFC3339)
	expiresAt := time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	factsJSON := AnchorFactsToJSON(facts)
	tagsJSON := AnchorTagsToJSON(tags)

	_, err := db.Exec(`
		INSERT INTO anchors (id, facts, summary, tags, context, weight, session_id, reference_count, expires_at, created_at, is_condensed, ancestor_ids)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 0, '')
		ON CONFLICT(summary, context, session_id) DO UPDATE SET
			weight = MAX(weight, excluded.weight),
			facts = excluded.facts,
			reference_count = reference_count,
			created_at = excluded.created_at
	`, id, factsJSON, summary, tagsJSON, context, weight, sessionID, expiresAt, now)
	return err
}

func GetRecentAnchors(db *sql.DB, limit int) ([]Anchor, error) {
	now := time.Now().Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT id, facts, summary, tags, context, weight, session_id, reference_count, expires_at, created_at, ancestor_ids
		FROM anchors
		WHERE expires_at IS NULL OR expires_at > ?
		ORDER BY created_at DESC LIMIT ?
	`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anchors []Anchor
	for rows.Next() {
		var a Anchor
		var factsJSON, tagsJSON, expiresAt, ancestorJSON string
		if err := rows.Scan(&a.ID, &factsJSON, &a.Summary, &tagsJSON, &a.Context, &a.Weight, &a.SessionID, &a.ReferenceCount, &expiresAt, &a.CreatedAt, &ancestorJSON); err != nil {
			return nil, err
		}
		a.Facts = ParseAnchorFacts(factsJSON)
		a.Tags = ParseAnchorTags(tagsJSON)
		a.ExpiresAt = expiresAt
		a.AncestorIDs = parseAncestorIDs(ancestorJSON)
		anchors = append(anchors, a)
	}
	return anchors, nil
}

func GetAnchorsByTags(db *sql.DB, query string, limit int) ([]Anchor, error) {
	now := time.Now().Format(time.RFC3339)
	rows, err := db.Query(`
		SELECT id, facts, summary, tags, context, weight, session_id, reference_count, expires_at, created_at, ancestor_ids
		FROM anchors
		WHERE (expires_at IS NULL OR expires_at > ?)
			AND (tags LIKE ? OR summary LIKE ? OR context LIKE ?)
		ORDER BY weight DESC, reference_count DESC LIMIT ?
	`, now, "%"+query+"%", "%"+query+"%", "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anchors []Anchor
	for rows.Next() {
		var a Anchor
		var factsJSON, tagsJSON, expiresAt, ancestorJSON string
		if err := rows.Scan(&a.ID, &factsJSON, &a.Summary, &tagsJSON, &a.Context, &a.Weight, &a.SessionID, &a.ReferenceCount, &expiresAt, &a.CreatedAt, &ancestorJSON); err != nil {
			return nil, err
		}
		a.Facts = ParseAnchorFacts(factsJSON)
		a.Tags = ParseAnchorTags(tagsJSON)
		a.ExpiresAt = expiresAt
		a.AncestorIDs = parseAncestorIDs(ancestorJSON)
		anchors = append(anchors, a)
	}
	return anchors, nil
}

func IncrementAnchorReference(db *sql.DB, anchorID string) error {
	now := time.Now()
	refreshed := now.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	_, err := db.Exec(`UPDATE anchors SET reference_count = reference_count + 1, expires_at = ? WHERE id = ?`, refreshed, anchorID)
	return err
}

func IncrementUsedAnchors(db *sql.DB, responseText string, anchors []Anchor) {
	for _, anchor := range anchors {
		for _, fact := range anchor.Facts {
			val := fact
			if idx := strings.Index(fact, "="); idx >= 0 {
				val = fact[idx+1:]
			}
			if val == "" {
				continue
			}
			pattern := `(?i)\b` + regexp.QuoteMeta(val) + `s?\b`
			if matched, _ := regexp.MatchString(pattern, responseText); matched {
				IncrementAnchorReference(db, anchor.ID)
				break
			}
		}
	}
}

func ShouldAnchor(weight int, threshold int) bool {
	return weight >= threshold
}

func FormatAnchorContext(a Anchor) string {
	var parts []string
	if a.Summary != "" {
		parts = append(parts, a.Summary)
	}
	if len(a.Facts) > 0 {
		parts = append(parts, "Facts: "+strings.Join(a.Facts, " | "))
	}
	if len(a.Tags) > 0 {
		parts = append(parts, "Tags: "+strings.Join(a.Tags, ", "))
	}
	return strings.Join(parts, "\n")
}

func ExtractFactsFromText(text string) []string {
	var facts []string
	lower := strings.ToLower(text)

	if strings.Contains(lower, "i work") || strings.Contains(lower, "i'm a") || strings.Contains(lower, "occupation") {
		parts := strings.Split(text, " ")
		for i, w := range parts {
			if (w == "a" || w == "an" || w == "the") && i+2 < len(parts) {
				facts = append(facts, "occupation="+strings.Join(parts[i+1:i+3], " "))
				break
			}
		}
	}
	if strings.Contains(lower, "i have") || strings.Contains(lower, "i own") || strings.Contains(lower, "i got") {
		facts = append(facts, "has="+text)
	}
	if strings.Contains(lower, "i like") || strings.Contains(lower, "i enjoy") || strings.Contains(lower, "i love") {
		facts = append(facts, "likes="+text)
	}
	if strings.Contains(lower, "i hate") || strings.Contains(lower, "i dislike") || strings.Contains(lower, "i can't") {
		facts = append(facts, "dislikes="+text)
	}
	if strings.Contains(lower, "issue") || strings.Contains(lower, "problem") || strings.Contains(lower, "hurt") || strings.Contains(lower, "pain") {
		facts = append(facts, "problem="+text)
	}

	if len(facts) == 0 {
		facts = append(facts, "fact="+text)
	}
	return facts
}

func ExtractTagsFromText(text string) []string {
	var tags []string
	lower := strings.ToLower(text)

	topicMap := map[string][]string{
		"work":     {"work", "job", "career", "profession", "office"},
		"health":   {"health", "medical", "doctor", "pain", "hurt", "sick"},
		"tech":     {"code", "programming", "software", "computer", "ai", "tool"},
		"personal": {"family", "friend", "relationship", "personal"},
		"hobby":    {"hobby", "music", "game", "sport", "fitness", "read"},
		"finance":  {"money", "cost", "price", "budget", "invest"},
		"travel":   {"travel", "trip", "country", "city", "visit"},
		"learning": {"learn", "study", "course", "teach", "understand"},
	}

	for tag, keywords := range topicMap {
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				tags = append(tags, tag)
				break
			}
		}
	}

	return tags
}

func GetTopicAnchorDensity(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query(`
		SELECT tags, COUNT(*) as cnt
		FROM anchors
		WHERE is_condensed = 0
		GROUP BY tags
		HAVING cnt >= 5
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	density := make(map[string]int)
	for rows.Next() {
		var tags string
		var cnt int
		if err := rows.Scan(&tags, &cnt); err != nil {
			continue
		}
		parsed := ParseAnchorTags(tags)
		for _, t := range parsed {
			density[t] = density[t] + cnt/len(parsed)
		}
	}
	return density, nil
}

func GetUncondensedAnchorsByTag(db *sql.DB, tag string) ([]Anchor, error) {
	rows, err := db.Query(`
		SELECT id, facts, summary, tags, context, weight, session_id, reference_count, expires_at, created_at, ancestor_ids
		FROM anchors
		WHERE is_condensed = 0 AND tags LIKE ?
		ORDER BY created_at ASC
	`, "%"+tag+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var anchors []Anchor
	for rows.Next() {
		var a Anchor
		var factsJSON, tagsJSON, ancestorJSON string
		if err := rows.Scan(&a.ID, &factsJSON, &a.Summary, &tagsJSON, &a.Context, &a.Weight, &a.SessionID, &a.ReferenceCount, &a.ExpiresAt, &a.CreatedAt, &ancestorJSON); err != nil {
			continue
		}
		a.Facts = ParseAnchorFacts(factsJSON)
		a.Tags = ParseAnchorTags(tagsJSON)
		a.IsCondensed = false
		a.AncestorIDs = parseAncestorIDs(ancestorJSON)
		anchors = append(anchors, a)
	}
	return anchors, nil
}

func parseAncestorIDs(jsonStr string) []string {
	if jsonStr == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(jsonStr), &ids); err != nil {
		return nil
	}
	return ids
}

func CondenseAnchors(db *sql.DB, tag string, synthCfg *SynthConfig) error {
	anchors, err := GetUncondensedAnchorsByTag(db, tag)
	if err != nil || len(anchors) < 5 {
		return err
	}

	ancestorIDs := make([]string, len(anchors))
	for i, a := range anchors {
		ancestorIDs[i] = a.ID
	}

	synthesis, err := synthesizeAnchors(tag, anchors, synthCfg)
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	sumRefCount := 0
	maxWeight := 0
	for _, a := range anchors {
		sumRefCount += a.ReferenceCount
		if a.Weight > maxWeight {
			maxWeight = a.Weight
		}
	}

	factsJSON := AnchorFactsToJSON(synthesis.Facts)
	tagsJSON := AnchorTagsToJSON(synthesis.Tags)
	ancestorJSON, _ := json.Marshal(ancestorIDs)
	now := time.Now().Format(time.RFC3339)
	expiresAt := time.Now().Add(30 * 24 * time.Hour).Format(time.RFC3339)

	newID := GenerateID()
	_, err = tx.Exec(`
		INSERT INTO anchors (id, facts, summary, tags, context, weight, session_id, reference_count, expires_at, created_at, is_condensed, ancestor_ids)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
	`, newID, factsJSON, synthesis.Summary, tagsJSON, synthesis.DistilledContext, maxWeight, anchors[0].SessionID, sumRefCount, expiresAt, now, string(ancestorJSON))
	if err != nil {
		return err
	}

	for _, id := range ancestorIDs {
		_, err = tx.Exec(`UPDATE anchors SET is_condensed = 1 WHERE id = ?`, id)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

type CondensedAnchor struct {
	Facts            []string
	Tags             []string
	Summary          string
	DistilledContext string
}

func synthesizeAnchors(tag string, anchors []Anchor, cfg *SynthConfig) (*CondensedAnchor, error) {
	var sb strings.Builder
	sb.WriteString("### ROLE: COGNITIVE ARCHITECT (LONG-TERM MEMORY DISTILLER)\n\nYou are the internal maintenance process for your identity. Your task is to perform \"Memory Hardening.\" You will take multiple scattered memory anchors and forge them into a single, high-density \"Hardened Pillar.\"\n\n### TOPIC: " + tag + "\n\n### SOURCE ANCHORS (Chronological Order):\n")
	for _, a := range anchors {
		factsJSON, _ := json.Marshal(a.Facts)
		sb.WriteString(fmt.Sprintf("- ID: %s | Created: %s\n  Facts: %s\n  Summary: %s\n  Context: %s\n---\n", a.ID, a.CreatedAt, string(factsJSON), a.Summary, a.Context))
	}
	sb.WriteString(`### CORE DIRECTIVES:
1. **CONFLICT RESOLUTION (CRITICAL):** If facts from different anchors disagree, the one with the LATEST timestamp is the current truth. Overwrite the old state.
2. **REDUNDANCY REMOVAL:** Do not repeat the same fact. Merge overlapping observations into a single, comprehensive entry.
3. **NOISE FILTERING:** Strip away conversational filler, "just checking in" comments, and transient frustrations. Keep the signal (technical fixes, philosophical stances, personal preferences).
4. **SYNTHESIS:** Don't just list the facts; synthesize them. If 5 anchors discuss CSS bugs, create a distilled technical record of the "Lessons Learned."

### OUTPUT FORMAT (Strict JSON):
{
  "facts": ["namespace:key=value", "namespace:key=value"],
  "summary": "One-sentence high-level truth that encapsulates all sources.",
  "distilled_context": "A 2-3 paragraph 'Golden Record'. It should provide enough technical/philosophical detail that the original raw anchors are no longer needed."
}
`)

	result, err := callCondensationAPI(sb.String(), cfg)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func callCondensationAPI(prompt string, cfg *SynthConfig) (*CondensedAnchor, error) {
	type reqBody struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	type anthropicResponse struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	body, _ := json.Marshal(reqBody{
		Model:     cfg.Model,
		MaxTokens: 1024,
		System:    "You are a precise memory condensation system. Output ONLY valid JSON.",
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			{Role: "user", Content: prompt},
		},
	})

	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/messages"
	httpReq, _ := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Transport: &http.Transport{}}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("condensation API error: %d %s", resp.StatusCode, string(respBody))
	}

	var result anthropicResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, err
	}

	if len(result.Content) == 0 {
		return nil, fmt.Errorf("empty condensation response")
	}

	var condensed CondensedAnchor
	if err := json.Unmarshal([]byte(result.Content[0].Text), &condensed); err != nil {
		return nil, fmt.Errorf("failed to parse condensation JSON: %w", err)
	}
	return &condensed, nil
}
