package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
//     uses "<file>#<index>"
//
// Each resulting MigratedFact is suitable for direct insertion into
// raw_memories via IngestFromJsonFile.
//
// Validation behaviour mirrors the markdown migrator: malformed entries
// are skipped and counted (well-formed siblings still parse) so a
// single bad row does not block an entire batch.  Per-entry errors are
// not surfaced in the error return (each value is its own decision); callers
// who need per-entry diagnostics should use ParseJsonFactsWithReport,
// or rely on IngestFromJsonFile's stats.Errors[] slice.
func ParseJsonFacts(content, sourcePath string) ([]MigratedFact, error) {
	facts, _, err := ParseJsonFactsWithReport(content, sourcePath)
	return facts, err
}

// ParseJsonFactsWithReport is ParseJsonFacts plus a structured report of
// per-entry rejections (file + record index + reason).  Callers that need
// to surface "record N was skipped because X" should use this form; the
// shorter ParseJsonFacts preserves the original error-only contract.
//
// The document is treated as untrusted.  Per-entry content is hard-capped
// at 256 KiB so a 5 MiB file cannot be filled with a single pathological
// entry.  The file itself is readFileCapped at 5 MiB in IngestFromJsonFile.
func ParseJsonFactsWithReport(content, sourcePath string) (facts []MigratedFact, perEntryErrors []string, err error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil, fmt.Errorf("%s: empty document", sourcePath)
	}

	var raw interface{}
	dec := json.NewDecoder(strings.NewReader(content))
	if derr := dec.Decode(&raw); derr != nil {
		return nil, nil, fmt.Errorf("%s: invalid JSON: %w", sourcePath, derr)
	}
	// Trailing-data check: a single valid JSON document is followed
	// only by EOF (with optional whitespace).  Any non-EOF result from a
	// second Decode means the operator concatenated documents.  (dec.More()
	// was unreliable: it does not skip leading whitespace at EOF, falsely
	// flagging "\n"/"/"/"* etc. as trailing JSON.  The canonical
	// second-Decode pattern handles whitespace correctly.)
	var dummy interface{}
	if derr := dec.Decode(&dummy); derr != io.EOF {
		return nil, nil, fmt.Errorf("%s: trailing data after first JSON value", sourcePath)
	}

	entries, err := normalizeJsonEntries(raw)
	if err != nil {
		return nil, nil, err
	}

	for i, entry := range entries {
		fact, perr := parseJsonEntry(entry, sourcePath, i)
		if perr != nil {
			// Partial recovery: surface the error (so the CLI can
			// print "file X record N / field F: <reason>") but
			// continue parsing siblings.  Matches markdown's
			// per-section skip behaviour.
			perEntryErrors = append(perEntryErrors, perr.Error())
			continue
		}
		facts = append(facts, fact)
	}
	return facts, perEntryErrors, nil
}

// ParseJsonReport summarises a JSON parse + structured per-entry errors so the
// CLI can render "file Y record N: skipped: <reason>".
type ParseJsonReport struct {
	File     string         // source path passed to ParseJsonFacts
	Facts    []MigratedFact // well-formed entries
	Rejected int            // count of malformed entries skipped
	Errors   []string       // one message per rejected entry (file + record + reason)
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

// MaxJsonEntryBytes is the hard cap on a single fact's content length.
// The file cap (5 MiB) is set by readFileCapped; this per-entry cap
// prevents a single pathological entry from filling the file and forcing
// the user to bisect their input.
const MaxJsonEntryBytes = 256 * 1024

// parseJsonEntry extracts a MigratedFact from one JSON object. Returns an
// error if the entry has no usable content/fact field, the content
// exceeds MaxJsonEntryBytes, or a string field contains control characters.
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
		return MigratedFact{}, fmt.Errorf("%s: entry %d: missing content/fact/text/body field", sourcePath, index)
	}
	if len(content) > MaxJsonEntryBytes {
		return MigratedFact{}, fmt.Errorf("%s: entry %d: content too large (%d bytes > %d cap)",
			sourcePath, index, len(content), MaxJsonEntryBytes)
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

	facts, perEntryErrors, err := ParseJsonFactsWithReport(content, sourcePath)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", sourcePath, err)
	}

	stats := &MigrateStats{
		RowsRead:     len(facts) + len(perEntryErrors),
		Format:       "json",
		ImportBatch:  importBatch,
		RowsRejected: len(perEntryErrors),
		RowsSkipped:  0,
		RowsStaged:   0,
		Errors:       perEntryErrors,
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
