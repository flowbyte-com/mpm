package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ParseJsonFacts parses a JSON document into atomic facts.
//
// Accepts two top-level shapes:
//   - Array of objects: [{"content": "...", ...}, ...]
//   - Wrapped object:   {"memories": [{...}, ...]}
//
// Per-object fields (all optional except content/fact):
//   - "content"  OR "fact"  (required): the fact body
//   - "tags"               (optional): []string OR comma-separated string
//   - "weight"             (optional): int 1-100, default 5
//   - "ttl"                (optional): string ("0" = permanent, "24h" = ephemeral)
//   - "source_id"          (optional): explicit source identifier; if missing,
//                                          uses "<file>#<index>"
//
// Each resulting MigratedFact is suitable for direct insertion into
// raw_memories via IngestFromJsonFile.
func ParseJsonFacts(content, sourcePath string) ([]MigratedFact, error) {
	var raw interface{}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	// Normalize to a list of map[string]interface{} entries
	entries, err := normalizeJsonEntries(raw)
	if err != nil {
		return nil, err
	}

	facts := make([]MigratedFact, 0, len(entries))
	for i, entry := range entries {
		fact, err := parseJsonEntry(entry, sourcePath, i)
		if err != nil {
			// Skip malformed entries but continue parsing — partial recovery
			// is better than failing the whole batch on one bad row.
			continue
		}
		facts = append(facts, fact)
	}

	return facts, nil
}

// normalizeJsonEntries unwraps {"memories": [...]} and validates the top-level
// shape. Returns an error for objects that don't match either accepted shape.
func normalizeJsonEntries(raw interface{}) ([]map[string]interface{}, error) {
	switch v := raw.(type) {
	case []interface{}:
		return entriesFromArray(v)
	case map[string]interface{}:
		// Look for a "memories" or "facts" or "items" field
		for _, key := range []string{"memories", "facts", "items", "entries"} {
			if arr, ok := v[key].([]interface{}); ok {
				return entriesFromArray(arr)
			}
		}
		return nil, fmt.Errorf("object has no memories/facts/items/entries array field")
	default:
		return nil, fmt.Errorf("top-level must be an array or object, got %T", raw)
	}
}

func entriesFromArray(arr []interface{}) ([]map[string]interface{}, error) {
	out := make([]map[string]interface{}, 0, len(arr))
	for i, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("entry %d is not an object (got %T)", i, item)
		}
		out = append(out, m)
	}
	return out, nil
}

// parseJsonEntry extracts a MigratedFact from one JSON object. Returns an
// error if the entry has no usable content/fact field.
func parseJsonEntry(entry map[string]interface{}, sourcePath string, index int) (MigratedFact, error) {
	// Extract content from "content" or "fact" (case-insensitive preference)
	var content string
	for _, key := range []string{"content", "fact", "text", "body"} {
		if s, ok := entry[key].(string); ok && strings.TrimSpace(s) != "" {
			content = s
			break
		}
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return MigratedFact{}, fmt.Errorf("entry %d has no content/fact/text/body field", index)
	}

	fact := MigratedFact{
		Content: content,
		Tags:    []string{},
		Weight:  5,
		TTL:     "",
		Heading: "",
	}

	// Tags: array OR comma-separated string
	if tagsRaw, ok := entry["tags"]; ok {
		switch tt := tagsRaw.(type) {
		case []interface{}:
			for _, t := range tt {
				if s, ok := t.(string); ok && strings.TrimSpace(s) != "" {
					fact.Tags = append(fact.Tags, strings.TrimSpace(s))
				}
			}
		case string:
			for _, t := range strings.Split(tt, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					fact.Tags = append(fact.Tags, t)
				}
			}
		}
	}

	// Weight: int OR float (JSON numbers come as float64)
	if w, ok := entry["weight"]; ok {
		switch ww := w.(type) {
		case float64:
			fact.Weight = clampWeight(int(ww))
		case int:
			fact.Weight = clampWeight(ww)
		}
	}

	// TTL: string
	if t, ok := entry["ttl"].(string); ok {
		fact.TTL = strings.TrimSpace(t)
	}

	// Explicit source_id, or synthesize from index
	if sid, ok := entry["source_id"].(string); ok && sid != "" {
		fact.SourceID = sid
	} else {
		fact.SourceID = fmt.Sprintf("%s#%d", sourcePath, index)
	}

	// Always tag migration provenance
	fact.Tags = append(fact.Tags, "migrated")
	fact.Tags = dedupeStrings(fact.Tags)

	return fact, nil
}

func clampWeight(w int) int {
	if w < 1 {
		return 1
	}
	if w > 100 {
		return 100
	}
	return w
}

// IngestFromJsonFile reads a JSON file, parses it into facts, and stages
// each as a row in raw_memories. Same pipeline as IngestFromMarkdownFile;
// the only difference is the parser entry point.
//
// Returns MigrateStats so callers can render the same shape as IngestOpenClaw.
func (dm *DatabaseManager) IngestFromJsonFile(sourcePath, importBatch string, dryRun bool) (*MigrateStats, error) {
	if importBatch == "" {
		importBatch = fmt.Sprintf("migrate_json_%d", time.Now().Unix())
	}

	content, err := readFileCapped(sourcePath, 5*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sourcePath, err)
	}

	facts, err := ParseJsonFacts(content, sourcePath)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", sourcePath, err)
	}

	stats := &MigrateStats{
		RowsRead:     len(facts),
		Format:       "json",
		ImportBatch:  importBatch,
		RowsRejected: 0,
		RowsSkipped:  0,
		RowsStaged:   0,
	}

	if dryRun {
		return stats, nil
	}

	for _, f := range facts {
		hash := sha256.Sum256([]byte(f.Content))
		contentHash := hex.EncodeToString(hash[:])

		exists, err := dm.contentHashExistsInMemory(contentHash)
		if err != nil {
			return nil, fmt.Errorf("dedup check: %w", err)
		}
		if exists {
			stats.RowsSkipped++
			continue
		}

		meta := map[string]interface{}{
			"weight":      f.Weight,
			"ttl":         f.TTL,
			"tags":        f.Tags,
			"source":      "migrate",
			"source_db":   "json",
			"source_id":   f.SourceID,
			"source_path": sourcePath,
			"imported_at": time.Now().UTC().Format(time.RFC3339),
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			stats.RowsRejected++
			continue
		}

		raw := &RawMemory{
			ID:          fmt.Sprintf("js_%s_%d", contentHash[:12], time.Now().UnixNano()),
			SourceID:    f.SourceID,
			SourceDB:    "json",
			ContentHash: contentHash,
			Text:        f.Content,
			Metadata:    string(metaJSON),
			IngestedAt:  float64(time.Now().Unix()),
			Status:      "pending",
			ImportBatch: importBatch,
			UpdatedAt:   float64(time.Now().Unix()),
			ExpiresAt:   0,
		}

		if err := dm.insertRawMemory(raw); err != nil {
			stats.RowsRejected++
			continue
		}
		stats.RowsStaged++
	}

	return stats, nil
}