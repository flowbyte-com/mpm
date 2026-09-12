// migration_tags_json_normalize.go — repair legacy corrupted `memories.tags`
// rows produced by SupersedeDecision/InvalidateDecision before the JSON-array
// fix landed. Pre-fix those writers concatenated CSV strings
// (`["a","b"],superseded,superseded-by:<id>`) into a column whose schema is
// JSON, breaking every `mpm memory list` with "failed to unmarshal tags"
// warnings. Idempotent via the `tags_json_normalize_v1` sentinel.
//
// Detection
// ─────────
// A row needs repair when its `tags` column is not a valid JSON array:
//   - NULL/empty           → already canonical (skip)
//   - literal `null`       → legacy state; replace with `[]`
//   - any non-array JSON   → defensive: replace with `[]`
//   - non-JSON text        → repair: parse the legacy CSV-suffix into a
//                            real JSON array, preserving `superseded*`
//                            markers (the ranking-discount signal).
//
// Trade-off
// ─────────
// The `superseded-by:<id>` link is part of the chain's discount signal
// (HybridSearch Phase5b ×0.25). Losing it would silently re-rank
// already-superseded decisions above the chain's current reading. The
// migration preserves every token that has the prefix `superseded` or
// `superseded-by:` and drops the rest. This is lossy for any other tags
// that happened to be in the row, but those rows were always
// Supersede/Invalidate outputs whose only legitimate tag set is the
// supersede marker chain — the operator's `--tags` value never landed
// there pre-fix (it went to the REPLACEMENT row's tags via
// `RecordDecision`).
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const tagsJSONNormalizeSentinel = "tags_json_normalize_v1"

// MigrateTagsJSONNormalize repairs the `memories.tags` column on every
// row whose stored value is not a valid JSON array of strings.
// Idempotent via the schema_migrations sentinel.
func MigrateTagsJSONNormalize(tx *sql.Tx) error {
	if tx == nil {
		return nil
	}
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		tagsJSONNormalizeSentinel,
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check %s sentinel: %w", tagsJSONNormalizeSentinel, err)
	}
	if applied > 0 {
		return nil
	}

	// Find every row whose tags column is NULL, empty, or non-JSON,
	// or is valid JSON but not an array (e.g. literal `null`).
	rows, err := tx.Query(`
		SELECT id, tags FROM memories
		WHERE deleted_at IS NULL
		  AND (
		    tags IS NULL
		    OR tags = ''
		    OR NOT json_valid(tags)
		    OR json_type(tags) != 'array'
		  )
	`)
	if err != nil {
		return fmt.Errorf("scan corrupted tags: %w", err)
	}
	type pending struct {
		id   string
		tags string
	}
	var affected []pending
	for rows.Next() {
		var id, tags string
		if err := rows.Scan(&id, &tags); err != nil {
			rows.Close()
			return fmt.Errorf("scan row: %w", err)
		}
		affected = append(affected, pending{id: id, tags: tags})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows.Err: %w", err)
	}
	for _, p := range affected {
		fixed := repairLegacyTags(p.tags)
		if _, err := tx.Exec(`UPDATE memories SET tags = ? WHERE id = ?`, fixed, p.id); err != nil {
			return fmt.Errorf("update %s: %w", p.id, err)
		}
	}

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		tagsJSONNormalizeSentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", tagsJSONNormalizeSentinel, err)
	}
	return nil
}

// repairLegacyTags maps a corrupted `tags` value into a canonical JSON
// array. Inputs and outputs:
//
//	NULL/empty/garbage     → "[]"
//	`null`                 → "[]"   (the literal JSON null)
//	`null,superseded`      → `["superseded"]`
//	`["a","b"],superseded` → `["superseded"]`   (drop pre-fix CSV suffix junk)
//	`a,b,c`                → `["a","b","c"]`     (CSV-like legacy)
//
// The function is conservative: it preserves `superseded*` markers (the
// ranking-discount signal) and drops everything else, on the assumption
// that the legacy row's only legitimate content was the supersede chain.
func repairLegacyTags(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return "[]"
	}

	// Pre-fix bug wrote `["foo","bar"],superseded,superseded-by:<id>`.
	// Detect a leading JSON-array prefix and pull supersede markers from
	// the tail if the head is itself valid JSON.
	if strings.HasPrefix(raw, "[") {
		end := strings.LastIndex(raw, "]")
		if end > 0 {
			head := raw[:end+1]
			tail := raw[end+1:]
			if json.Valid([]byte(head)) {
				tags := extractSupersedeTagsFromTail(tail)
				if len(tags) == 0 {
					return head
				}
				return mergeJSONArrayWithTags(head, tags)
			}
		}
	}

	// No JSON head — treat the whole string as CSV.
	return csvLikeToJSONArray(raw)
}

// csvLikeToJSONArray splits s on commas, keeps tokens starting with
// `superseded`, returns a JSON array of the kept tokens.
func csvLikeToJSONArray(s string) string {
	var keep []string
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" || tok == "null" {
			continue
		}
		if strings.HasPrefix(tok, "superseded") {
			keep = append(keep, tok)
		}
	}
	if len(keep) == 0 {
		return "[]"
	}
	out, _ := json.Marshal(keep)
	return string(out)
}

// extractSupersedeTagsFromTail returns the comma-separated tokens in
// tail (which begins with `,` from the head-strip) that begin with
// `superseded`. Returns nil when the tail has no recognised markers.
func extractSupersedeTagsFromTail(tail string) []string {
	if tail == "" {
		return nil
	}
	// tail starts with `,tag1[,tag2...]` per the pre-fix format.
	clean := strings.TrimPrefix(tail, ",")
	var keep []string
	for _, tok := range strings.Split(clean, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.HasPrefix(tok, "superseded") {
			keep = append(keep, tok)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return keep
}

// mergeJSONArrayWithTags takes a valid JSON array (head) and appends the
// `tags` to it. Done via json.Marshal round-trip for correctness.
func mergeJSONArrayWithTags(head string, tags []string) string {
	var existing []string
	_ = json.Unmarshal([]byte(head), &existing)
	for _, t := range tags {
		// Dedupe (preserves order, drops exact duplicates).
		dup := false
		for _, e := range existing {
			if e == t {
				dup = true
				break
			}
		}
		if !dup {
			existing = append(existing, t)
		}
	}
	out, _ := json.Marshal(existing)
	return string(out)
}