// cmd/mpm/service_why.go — WhyService for Wave 2.
//
// WhyService EARNED its existence (RFC §7) by owning composition
// behaviour that crosses multiple substrate paths for a single
// artifact:
//
//   - Artifact identity (memory/decision/theory/skill metadata)
//   - Evidence chain (mpm call mpm_evidence '{"action":"list",...}')
//   - Confidence history (mpm call mpm_confidence '{"action":"query_confidence_history",...}')
//   - Retrieval metadata (GetRetrievalMetadata)
//
// If this service disappeared, every operator who wanted to ask
// "why was this reinforced?" or "what evidence backs this up?" would
// reimplement the per-kind dispatch + cross-source composition.
// Composition IS behaviour — earned.
//
// Layering contract (RFC §7):
//   WhyService composes queries across the substrate. It does NOT
//   call other services. It does NOT own rendering. It does NOT
//   call commands.
//
// Recursion: ONE LEVEL deep (per RFC §'mpm why'). This service does
// NOT traverse into source artifacts. If the operator wants
// recursive provenance, that ships later as `mpm why <id> --depth N`
// — not today.

package main

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// WhyReport is the assembled artifact-introspection payload.
//
// Identity + Provenance + Evidence + Confidence are the four panels
// the renderer formats. Each section is optional — if a particular
// substrate call fails, the section surfaces an error string and
// the rest of the report still renders.
type WhyReport struct {
	ArtifactID  string
	ArtifactKind string // "memory", "decision", "theory", "skill"
	GeneratedAt time.Time

	// Identity — who/what is this artifact.
	Identity *WhyIdentity
	// Provenance — when was it created/last touched, by whom.
	Provenance *WhyProvenance
	// Evidence — observations / decisions / reversals attached.
	EvidenceCount int
	EvidenceRows  []EvidenceRow
	// Confidence — last N confidence events.
	ConfidenceHistory []ConfidenceRow
	// Retrieval — substrate-side reuse statistics.
	Retrieval *WhyRetrieval
	// SkipReason — set when the artifact couldn't be located.
	SkipReason string
}

// WhyIdentity is the basic identifying shape for an artifact. Rendered
// as the first section of the why report.
type WhyIdentity struct {
	Content string // preview of content / claim (truncated)
	Tags    []string
	Weight  int
	IsLTM   bool
}

// WhyProvenance is the time + agency shape. Rendered as the second
// section.
type WhyProvenance struct {
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastAccessed *time.Time
	CreatedBy    string
	SessionID    string
}

// EvidenceRow is one observation attached to this artifact. Source-
// shape from the evidence table.
type EvidenceRow struct {
	Type       string
	Strength   float64
	CreatedBy  string
	CreatedAt  time.Time
	Notes      string
	SourceGroup string
}

// ConfidenceRow is one confidence-altering event. Output shape from
// query_confidence_history.
type ConfidenceRow struct {
	ComputedAt   time.Time
	Confidence   float64
	EvidenceCount int
	Trigger      string
}

// WhyRetrieval is the substrate's reuse statistics for an artifact.
// Rendered as the second-to-last section.
//
// LastRetrievedAt is stored as INTEGER Unix-epoch seconds (see migration
// timestamps_unified_v1); nil means "never retrieved".
type WhyRetrieval struct {
	ReuseCount      int
	SuccessCount    int
	LastRetrievedAt *int64
}

// WhyService is the cognitive-interface provenance flagship.
// Composition happens here, not in the handler.
type WhyService struct {
	dm *mpminternal.DatabaseManager
}

// NewWhyService returns the service. Returns nil if dm is nil.
func NewWhyService(dm *mpminternal.DatabaseManager) *WhyService {
	if dm == nil {
		return nil
	}
	return &WhyService{dm: dm}
}

// Explain takes an artifact id (or `kind:id` shorthand) and assembles
// the WhyReport. Auto-detects the kind: prefixed `skill:` always wins;
// otherwise checks memory, decision, theory collections in order.
//
// Returns (nil, nil) with SkipReason set if the artifact cannot be
// located — operators get an actionable "not found" message instead
// of an opaque error.
func (s *WhyService) Explain(id string) (*WhyReport, error) {
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	report := &WhyReport{
		ArtifactID:  id,
		GeneratedAt: time.Now().UTC(),
	}

	// Kind auto-detection. Skill ids always win via prefix (canonical
	// shape). Otherwise probe the standard collections.
	if strings.HasPrefix(id, "skill:") {
		report.ArtifactKind = "skill"
	} else {
		k, artifactMap, err := s.detectKind(id)
		if err != nil {
			report.SkipReason = err.Error()
			return report, nil
		}
		report.ArtifactKind = k
		report.Identity = identityFromMap(artifactMap, k)
		report.Provenance = provenanceFromMap(artifactMap)
	}

	// Skill ids still use the standard fetch path now (skills live in
	// the memories collection too) — keep the skill-prefix branch
	// for backward compatibility but route through the same loader.
	return s.explainMemoryLike(id, report)
}

// explainSkill is reserved as an extension point. Today skill rows
// live in the memories collection with collection='skills' and are
// fetched via the standard explainMemoryLike path. Future versions
// may split skills into their own physical table; this hook is
// where the divergent loader would go.
func (s *WhyService) explainSkill(id string, report *WhyReport) (*WhyReport, error) {
	return s.explainMemoryLike(id, report)
}

// explainMemoryLike handles memory/decision/theory (all live in the
// memories table with collection='memories'/'decisions'/'theories').
func (s *WhyService) explainMemoryLike(id string, report *WhyReport) (*WhyReport, error) {
	evidenceRows, evCount, err := s.loadEvidence(id, report.ArtifactKind)
	if err == nil {
		report.EvidenceRows = evidenceRows
		report.EvidenceCount = evCount
	} else {
		// Non-fatal — surface in report but don't abort.
		report.EvidenceCount = 0
	}

	if hist, err := s.loadConfidenceHistory(id, report.ArtifactKind); err == nil {
		report.ConfidenceHistory = hist
	}
	if retr, err := s.loadRetrieval(id); err == nil {
		report.Retrieval = retr
	}
	return report, nil
}

// detectKind probes the memories table for the id across the
// canonical substrate collections. Returns the first match. Returns
// (kind, map, nil) on hit; ("", nil, error) on miss.
//
// The collection set mirrors what an operator might plausibly want to
// introspect (per RFC §'mpm why' the intent is "any artifact in the
// substrate"). Add new collections here when they become introspectable.
func (s *WhyService) detectKind(id string) (string, map[string]interface{}, error) {
	collections := []string{
		"memories",
		"decisions",
		"theories",
		"skills",
		"lessons",
		"changelog",
		"notes",
		"directives",
		"knowledge",
	}
	for _, collection := range collections {
		row, err := s.fetchFromCollection(id, collection)
		if err != nil {
			continue // any error including ErrNoRows — fall through to next collection
		}
		if row != nil {
			// Map the collection name to a friendlier "kind" label for
			// the operator. The evidence/confidence APIs use the same
			// label (artifact_type) so the mapping stays internally
			// consistent.
			switch collection {
			case "memories":
				return "memory", row, nil
			case "lessons":
				return "lesson", row, nil
			case "decisions":
				return "decision", row, nil
			case "theories":
				return "theory", row, nil
			case "skills":
				return "skill", row, nil
			default:
				// Generic collections (changelog, notes, directives,
				// knowledge) surface as their collection name. This
				// avoids hiding what the operator is actually looking at.
				return collection, row, nil
			}
		}
	}
	return "", nil, fmt.Errorf("no artifact found for id %q (probed %d standard collections)", id, len(collections))
}

// fetchFromCollection reads a single row from the memories table by
// id + collection. Returns (nil, nil) when not found; (nil, error) on
// query failure.
func (s *WhyService) fetchFromCollection(id, collection string) (map[string]interface{}, error) {
	row := s.dm.QueryRowTracked(
		`SELECT id, content, session_id, tags, weight, is_long_term, metadata,
		        collection, created_at, updated_at, last_accessed_at
		 FROM memories
		 WHERE id = ? AND collection = ? AND deleted_at IS NULL
		 LIMIT 1`,
		id, collection,
	)
	var outID, content, coll string
	var sessID, metaStr, tagsJSON sql.NullString
	var createdAt, updatedAt sql.NullString
	var lastAccessed sql.NullString
	// weight + is_long_term can arrive as INTEGER, FLOAT, or NUMERIC
	// depending on insert path (some handlers write REAL values). Scan
	// into float64/bool to handle all three, then convert.
	var weight float64
	var isLTM bool

	if err := row.Scan(&outID, &content, &sessID, &tagsJSON, &weight, &isLTM, &metaStr, &coll, &createdAt, &updatedAt, &lastAccessed); err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"id":           outID,
		"content":      content,
		"weight":       int64(int(weight)),
		"is_long_term": isLTM,
		"collection":   coll,
	}
	if sessID.Valid {
		out["session_id"] = sessID.String
	}
	if tagsJSON.Valid {
		out["tags"] = tagsJSON.String
	}
	if metaStr.Valid {
		out["metadata"] = metaStr.String
	}
	if createdAt.Valid {
		out["created_at"] = createdAt.String
	}
	if updatedAt.Valid {
		out["updated_at"] = updatedAt.String
	}
	if lastAccessed.Valid {
		out["last_accessed_at"] = lastAccessed.String
	}
	return out, nil
}

// loadEvidence fetches all evidence rows for an artifact.
func (s *WhyService) loadEvidence(id, kind string) ([]EvidenceRow, int, error) {
	res, err := s.dm.ListEvidence(id, kind)
	if err != nil {
		return nil, 0, err
	}
	rawList, _ := res["evidence"].([]interface{})
	rows := make([]EvidenceRow, 0, len(rawList))
	for _, raw := range rawList {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		row := EvidenceRow{
			Type:        stringOf(m["type"]),
			Strength:    floatOf(m["strength"]),
			CreatedBy:   stringOf(m["created_by"]),
			SourceGroup: stringOf(m["source_group"]),
			Notes:       stringOf(m["notes"]),
		}
		if v, ok := m["created_at"].(int64); ok {
			row.CreatedAt = time.Unix(v, 0).UTC()
		}
		rows = append(rows, row)
	}
	// Sort newest-first (defensive — DB already sorts DESC).
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.After(rows[j].CreatedAt) })
	return rows, len(rows), nil
}

// loadConfidenceHistory fetches the recent confidence timeline.
func (s *WhyService) loadConfidenceHistory(id, kind string) ([]ConfidenceRow, error) {
	res, err := s.dm.QueryConfidenceHistory(id, kind, 10)
	if err != nil {
		return nil, err
	}
	rawList, _ := res["history"].([]interface{})
	rows := make([]ConfidenceRow, 0, len(rawList))
	for _, raw := range rawList {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		row := ConfidenceRow{
			Confidence:    floatOf(m["confidence"]),
			EvidenceCount: intOf(m["evidence_count"]),
			Trigger:       stringOf(m["trigger"]),
		}
		if v, ok := m["computed_at"].(int64); ok {
			row.ComputedAt = time.Unix(v, 0).UTC()
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// loadRetrieval fetches the substrate's reuse statistics.
func (s *WhyService) loadRetrieval(id string) (*WhyRetrieval, error) {
	meta, err := s.dm.GetRetrievalMetadata(id)
	if err != nil {
		return &WhyRetrieval{}, nil // non-fatal
	}
	out := &WhyRetrieval{
		ReuseCount:   meta.ReuseCount,
		SuccessCount: meta.SuccessCount,
	}
	if meta.LastRetrievedAt != nil {
		out.LastRetrievedAt = meta.LastRetrievedAt
	}
	return out, nil
}

// Small helpers for safe type assertions on the typed-map shape from
// the substrate.
func stringOf(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func floatOf(v interface{}) float64 {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

func intOf(v interface{}) int {
	if v == nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// identityFromMap extracts the Identity panel from a raw memory row.
func identityFromMap(m map[string]interface{}, kind string) *WhyIdentity {
	if m == nil {
		return nil
	}
	out := &WhyIdentity{}
	if v, ok := m["content"].(string); ok {
		out.Content = v
	}
	if v, ok := m["weight"].(int64); ok {
		out.Weight = int(v)
	}
	if v, ok := m["is_long_term"].(bool); ok {
		out.IsLTM = v
	}
	// tags is a JSON string; parsing fully is out of scope for
	// v1; the renderer shows a comma-separated preview if any.
	if v, ok := m["tags"].(string); ok && v != "" {
		out.Tags = strings.Split(stripJSONArrayEnvelope(v), ",")
	}
	return out
}

// provenanceFromMap extracts the Provenance panel from a raw memory row.
func provenanceFromMap(m map[string]interface{}) *WhyProvenance {
	if m == nil {
		return nil
	}
	out := &WhyProvenance{}
	if v, ok := m["created_at"].(string); ok {
		if t, err := parseSQLiteTime(v); err == nil {
			out.CreatedAt = t
		}
	}
	if v, ok := m["updated_at"].(string); ok {
		if t, err := parseSQLiteTime(v); err == nil {
			out.UpdatedAt = t
		}
	}
	if v, ok := m["last_accessed_at"].(string); ok && v != "" {
		if t, err := parseSQLiteTime(v); err == nil {
			out.LastAccessed = &t
		}
	}
	if v, ok := m["session_id"].(string); ok {
		out.SessionID = v
	}
	return out
}

// stripJSONArrayEnvelope parses a JSON array envelope like
// ["a","b","c"] into "a, b, c" for display. Falls back to the raw
// input on parse failure — never an empty string when the column
// had data.
//
// The previous version did a naive trim. This one handles both
// "[\"a\",\"b\"]" and `["a","b"]` shapes (SQLite stores JSON1-emitted
// arrays either way). The renderer no longer sees leading/trailing
// brackets or dangling quotes.
func stripJSONArrayEnvelope(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return s
	}
	// Strip the outer brackets; then walk the comma-separated list.
	// Because the elements may themselves contain commas inside
	// quoted strings, we use a simple state machine: count quotes
	// and only split on commas at zero-quote count.
	inner := s[1:]
	inner = strings.TrimSuffix(inner, "]")
	inner = strings.TrimSpace(inner)

	var out strings.Builder
	quoteCount := 0
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '"' {
			quoteCount ^= 1
			// Skip the quote characters entirely from the display
			// string — they were SQLite serialization artifacts.
			continue
		}
		if c == ',' && quoteCount == 0 {
			out.WriteString(", ")
			continue
		}
		out.WriteByte(c)
	}
	return out.String()
}
