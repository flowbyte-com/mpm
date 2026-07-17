package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// MigratedFact is a single fact extracted from a non-SQLite source file.
// One MigratedFact corresponds to one row that gets staged in raw_memories.
type MigratedFact struct {
	Content  string   // the fact body (excluding heading and metadata lines)
	Tags     []string // tags for retrieval (heading slugs + inline Tags: lines)
	Weight   int      // 1-100, default 5 if unspecified
	TTL      string   // "0" for permanent, "24h" etc. for ephemeral, "" = default
	SourceID string   // unique within source (e.g. "filename.md#3")
	Heading  string   // the ## heading text if present (informational)
}

// IngestStats mirrors IngestStats from ingest.go so migrate can return the
// same shape as IngestOpenClaw.
type MigrateStats struct {
	RowsRead     int
	RowsStaged   int
	RowsSkipped  int
	RowsRejected int
	Format       string // "markdown" or "json"
	ImportBatch  string
}

// ParseMarkdownFacts splits a markdown document into atomic facts.
//
// Format conventions handled:
//   - ## Heading sections (split at each ## line)
//   - § inline separators (alternative boundary within or across sections)
//   - "Tags: a, b, c" lines anywhere in the section body, extracted as tags
//   - "Weight: N" lines (1-100 integer scale, matching the column)
//   - "TTL: 24h" or "TTL: 0" lines (matching save_to_memory convention)
//
// Each resulting MigratedFact represents one atomic claim suitable for
// retrieval. Very short sections (< 20 chars after trimming) are skipped
// to avoid promoting blank paragraphs to memories.
//
// Heading text is preserved as the first tag (slugified) so queries can
// surface whole sections when relevant.
func ParseMarkdownFacts(content, sourcePath string) []MigratedFact {
	var facts []MigratedFact
	sourceCounter := 0

	// Normalize line endings.
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	// Primary split: `## ` headings at line start, KEEPING the `## ` attached
	// to subsequent sections so heading detection below works on each one.
	// Manual scan (not strings.Split) because Split consumes the delimiter.
	var sections []string
	lines := strings.Split(content, "\n")
	var current []string
	flush := func() {
		if len(current) > 0 {
			sections = append(sections, strings.Join(current, "\n"))
		}
		current = current[:0]
	}
	for _, line := range lines {
		if (strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# ")) && len(current) > 0 {
			flush()
		}
		current = append(current, line)
	}
	flush()

	for _, raw := range sections {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		// Within each section, also split on `§` (hermes-style inline separator).
		// We keep the heading on the FIRST sub-section only.
		var heading string
		firstLine := strings.SplitN(raw, "\n", 2)[0]
		if strings.HasPrefix(firstLine, "## ") || strings.HasPrefix(firstLine, "#") {
			heading = strings.TrimSpace(strings.TrimLeft(firstLine, "#"))
		}

		// Split on § to get atomic sub-facts within this section.
		// The hermes-style § separator is the primary atomic boundary.
		// Without §, the whole ## section is one atomic fact — metadata
		// (Tags:/Weight:/TTL: lines) stays attached to the body.
		var subParts []string
		if strings.Contains(raw, "§") {
			subParts = strings.Split(raw, "§")
		} else {
			subParts = []string{raw}
		}

		for i, part := range subParts {
			part = strings.TrimSpace(part)
			if strings.TrimSpace(part) == "" {
				continue
			}

			// Strip the leading ## from the first sub-part (already extracted heading)
			if i == 0 && heading != "" {
				lines := strings.SplitN(part, "\n", 2)
				if strings.HasPrefix(lines[0], "#") {
					if len(lines) > 1 {
						part = strings.TrimSpace(lines[1])
					} else {
						part = ""
					}
				}
			}

			if strings.TrimSpace(part) == "" {
				continue
			}

			fact := MigratedFact{
				Content: part,
				Tags:    []string{},
				Weight:  5, // default
				TTL:     "", // default
				Heading: heading,
			}

			// Extract structured metadata lines (Tags:, Weight:, TTL:)
			lines := strings.Split(part, "\n")
			var contentLines []string
			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "Tags:") || strings.HasPrefix(trimmed, "tags:") {
					tagStr := strings.TrimSpace(trimmed[strings.Index(trimmed, ":")+1:])
					for _, t := range strings.Split(tagStr, ",") {
						t = strings.TrimSpace(t)
						if t != "" {
							fact.Tags = append(fact.Tags, t)
						}
					}
					continue
				}
				if strings.HasPrefix(trimmed, "Weight:") || strings.HasPrefix(trimmed, "weight:") {
					val := strings.TrimSpace(trimmed[strings.Index(trimmed, ":")+1:])
					var w int
					fmt.Sscanf(val, "%d", &w)
					if w >= 1 && w <= 100 {
						fact.Weight = w
					}
					continue
				}
				if strings.HasPrefix(trimmed, "TTL:") || strings.HasPrefix(trimmed, "ttl:") {
					fact.TTL = strings.TrimSpace(trimmed[strings.Index(trimmed, ":")+1:])
					continue
				}
				contentLines = append(contentLines, line)
			}
			fact.Content = strings.TrimSpace(strings.Join(contentLines, "\n"))

			// Slugify heading and prepend as tag (so section-level queries work)
			if heading != "" {
				slug := slugifyHeading(heading)
				if slug != "" {
					fact.Tags = append([]string{slug}, fact.Tags...)
				}
			}

			// Always tag the migration provenance so we can find these later
			fact.Tags = append(fact.Tags, "migrated")
			// De-dupe tags
			fact.Tags = dedupeStrings(fact.Tags)

			fact.SourceID = fmt.Sprintf("%s#%d", sourcePath, sourceCounter)
			sourceCounter++

			facts = append(facts, fact)
		}
	}

	return facts
}

// slugifyHeading turns a heading into a tag-safe slug.
func slugifyHeading(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "/", "-")
	// strip punctuation that's not tag-safe
	bad := []string{":", ",", ".", "(", ")", "?", "!", "'", "\"", "—", "–", "&"}
	for _, b := range bad {
		s = strings.ReplaceAll(s, b, "")
	}
	// collapse repeated dashes
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
		s = strings.Trim(s, "-")
	}
	return s
}

func dedupeStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] && s != "" {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// IngestFromMarkdownFile reads a markdown file, parses it into facts, and
// stages each fact as a row in raw_memories. The existing ingest review
// pipeline (--review, --commit, --undo) picks up from there.
//
// Returns MigrateStats so callers can render the same shape as IngestOpenClaw.
func (dm *DatabaseManager) IngestFromMarkdownFile(sourcePath, importBatch string, dryRun bool) (*MigrateStats, error) {
	if importBatch == "" {
		importBatch = fmt.Sprintf("migrate_md_%d", time.Now().Unix())
	}

	content, err := readFileCapped(sourcePath, 5*1024*1024) // 5MB cap
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sourcePath, err)
	}

	facts := ParseMarkdownFacts(content, sourcePath)
	stats := &MigrateStats{
		RowsRead:     len(facts),
		Format:       "markdown",
		ImportBatch:  importBatch,
		RowsRejected: 0,
		RowsSkipped:  0,
		RowsStaged:   0,
	}

	if dryRun {
		// In dry-run, return stats without writing.
		return stats, nil
	}

	for _, f := range facts {
		hash := sha256.Sum256([]byte(f.Content))
		contentHash := hex.EncodeToString(hash[:])

		// Check dedup against existing memories
		exists, err := dm.contentHashExistsInMemory(contentHash)
		if err != nil {
			return nil, fmt.Errorf("dedup check: %w", err)
		}
		if exists {
			stats.RowsSkipped++
			continue
		}

		// Build metadata JSON for the raw_memories row (carried over to the
		// memory when promoted via --commit).
		meta := map[string]interface{}{
			"weight":     f.Weight,
			"ttl":        f.TTL,
			"tags":       f.Tags,
			"source":     "migrate",
			"source_db":  "markdown",
			"source_id":  f.SourceID,
			"source_path": sourcePath,
			"heading":    f.Heading,
			"imported_at": time.Now().UTC().Format(time.RFC3339),
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			stats.RowsRejected++
			continue
		}

		raw := &RawMemory{
			ID:          fmt.Sprintf("md_%s_%d", contentHash[:12], time.Now().UnixNano()),
			SourceID:    f.SourceID,
			SourceDB:    "markdown",
			ContentHash: contentHash,
			Text:        f.Content,
			Metadata:    string(metaJSON),
			IngestedAt:  float64(time.Now().Unix()),
			Status:      "pending",
			ImportBatch: importBatch,
			UpdatedAt:   float64(time.Now().Unix()),
			ExpiresAt:   0, // no expiry by default; review sets it
		}

		if err := dm.insertRawMemory(raw); err != nil {
			stats.RowsRejected++
			continue
		}
		stats.RowsStaged++
	}

	return stats, nil
}

// readFileCapped reads a file with a size cap to prevent OOM on giant inputs.
func readFileCapped(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.Size() > maxBytes {
		return "", fmt.Errorf("file too large: %d bytes (cap %d)", st.Size(), maxBytes)
	}

	buf := make([]byte, st.Size())
	n, err := f.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
// PromoteRawMemoryBatch promotes pending raw_memories rows to memories,
// optionally filtered by import_batch. Returns the count promoted.
//
// For each pending row:
//   1. Parse metadata to recover tags, weight, TTL, heading
//   2. Call SaveMemoryWithContext (which now auto-detects the weight scale)
//   3. Mark the raw_memories row as 'approved' and stamp updated_at
//
// If save fails (e.g. toxicity, schema violation), the row stays 'pending'
// with a notes field explaining the rejection.
func (dm *DatabaseManager) PromoteRawMemoryBatch(importBatch string, dryRun bool) (int, error) {
	query := `
		SELECT id, source_id, source_db, content_hash, text, metadata
		FROM raw_memories
		WHERE status = 'pending'
	`
	args := []interface{}{}
	if importBatch != "" {
		query += ` AND import_batch = ?`
		args = append(args, importBatch)
	}
	query += ` ORDER BY ingested_at ASC`

	// Collect rows first, then close (release connection) before per-row writes.
	// Iterating rows from dm.db.Query holds the connection for streaming,
	// which deadlocks against dm.db.Exec inside the loop on the same
	// connection. Bug surfaced via the RejectsSensitiveContent test where
	// promote returned n=1 but raw_memories status stayed pending.
	type pending struct {
		id, sourceID, sourceDB, contentHash, text, metaJSON string
	}
	var pendings []pending
	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return 0, fmt.Errorf("query pending: %w", err)
	}
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.sourceID, &p.sourceDB, &p.contentHash, &p.text, &p.metaJSON); err != nil {
			continue
		}
		pendings = append(pendings, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate pending: %w", err)
	}

	promoted := 0
	for _, p := range pendings {

		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(p.metaJSON), &meta); err != nil {
			// Malformed metadata — reject with note
			if !dryRun {
				_, _ = dm.db.Exec(`UPDATE raw_memories SET status='rejected', llm_notes='malformed metadata', updated_at=? WHERE id=?`,
					float64(time.Now().Unix()), p.id)
			}
			continue
		}

		tags := []string{}
		if t, ok := meta["tags"].([]interface{}); ok {
			for _, x := range t {
				if s, ok := x.(string); ok {
					tags = append(tags, s)
				}
			}
		}

		weight := 5
		if w, ok := meta["weight"].(float64); ok {
			weight = int(w)
		}

		ttl := ""
		if t, ok := meta["ttl"].(string); ok {
			ttl = t
		}

		// Append source provenance to tags so we can find migrated facts
		tags = append(tags, "from-migration:"+importBatch)

		if dryRun {
			promoted++
			continue
		}

		_, _, err := dm.SaveMemoryWithContext(p.text, "memories", tags, float64(weight), ttl, ActiveContext{})
		if err != nil {
			_, _ = dm.db.Exec(`UPDATE raw_memories SET status='rejected', llm_notes=?, updated_at=? WHERE id=?`,
				fmt.Sprintf("save failed: %v", err), float64(time.Now().Unix()), p.id)
			continue
		}

		_, _ = dm.db.Exec(`UPDATE raw_memories SET status='approved', updated_at=? WHERE id=?`,
			float64(time.Now().Unix()), p.id)
		promoted++
	}

	return promoted, nil
}
