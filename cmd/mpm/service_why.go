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
	"errors"
	"fmt"
	"sort"
	"strings"
	"strconv"
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
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastAccessed    *time.Time
	CreatedBy       string
	SessionID       string
	FrameworkName    string // from artifact_provenance.framework_name
	FrameworkAdapter string // from artifact_provenance.framework_adapter (e.g., "opencode-mcp")
	ModelName        string // from artifact_provenance.model_name
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

	// Overlay framework/model provenance from artifact_provenance when available.
	if prov := s.loadArtifactProvenance(id, report.ArtifactKind); prov != nil {
		if report.Provenance == nil {
			report.Provenance = prov
		} else {
			report.Provenance.FrameworkName    = prov.FrameworkName
			report.Provenance.FrameworkAdapter = prov.FrameworkAdapter
			report.Provenance.ModelName        = prov.ModelName
		}
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

// loadArtifactProvenance fetches framework_name / framework_adapter /
// model_name from artifact_provenance for the given artifact. Returns
// an empty struct (NOT an error) when no provenance row exists — that
// is the common case for legacy rows pre-dating the schema migration.
// Errors are non-fatal: a transient SQL hiccup must not blank the
// rest of the why report.
func (s *WhyService) loadArtifactProvenance(id, kind string) *WhyProvenance {
	if s.dm == nil {
		return &WhyProvenance{}
	}
	var fw, adapter, model sql.NullString
	err := s.dm.QueryRowTracked(`
		SELECT framework_name, framework_adapter, model_name
		FROM artifact_provenance
		WHERE artifact_id = ? AND artifact_type = ?
		LIMIT 1
	`, id, kind).Scan(&fw, &adapter, &model)
	if err != nil {
		return &WhyProvenance{} // no row OR transient error — both surface as "(unknown)"
	}
	return &WhyProvenance{
		FrameworkName:    fw.String,
		FrameworkAdapter: adapter.String,
		ModelName:        model.String,
	}
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
// canonical substrate collections, then falls back to first-class
// non-memory tables (lessons_base, works). Returns the first match.
// Returns (kind, map, nil) on hit; ("", nil, error) on miss.
//
// The collection set mirrors what an operator might plausibly want to
// introspect (per RFC §'mpm why' the intent is "any artifact in the
// substrate"). Add new collections here when they become introspectable.
//
// T64 2026-09-11: lessons now live in a dedicated `lessons_base` table
// (a sibling of `memories`), reached via an `INSTEAD OF` trigger on the
// `lessons` view. The pre-fix code only probed the `memories` table
// with collection='lessons', which the new schema never writes to — so
// every lesson id returned "no artifact found" even though the row was
// trivially resolvable in lessons_base. The fix probes lessons_base
// after the memories loop misses, returning kind="lesson" so the rest
// of the why report (evidence, confidence, retrieval) works the same
// way it does for memory kinds.
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
	// T64 fix: probe the lessons_base table directly. Lessons have
	// lived in a dedicated table since the migrateLessonsToView
	// migration; the `lessons` view (backed by INSTEAD OF triggers on
	// lessons_base) is the canonical read path but is not queryable
	// by the id-only probe used here. fetchLesson handles both NULL
	// session_id / content_hash columns and returns a map in the
	// same shape fetchFromCollection does for memory kinds so the
	// downstream provenance / evidence loaders stay generic.
	if row, lerr := s.fetchLesson(id); lerr == nil && row != nil {
		return "lesson", row, nil
	}
	// F15: works are first-class artifacts with their own table — probe it
	// so `mpm why <work-id>` resolves instead of reporting "(unknown)".
	if row, werr := s.fetchWork(id); werr == nil && row != nil {
		return "work", row, nil
	}
	return "", nil, fmt.Errorf("no artifact found for id %q (probed %d standard collections + lessons_base + works)", id, len(collections))
}

// fetchLesson reads a lessons_base row by id for `mpm why`. The lessons
// table is the canonical home of lesson content (lessons collection in
// memories is legacy and the new schema writes only to lessons_base
// via the migrateLessonsToView migration). The map shape mirrors
// fetchFromCollection so the downstream provenance / evidence loaders
// stay generic.
//
// T64 2026-09-11: the pre-fix why probe only looked at `memories`
// where collection='lessons', which the new schema never populates —
// so every valid lesson id returned "no artifact found". This fetcher
// is the dedicated fallback after the memories loop misses.
//
// Lesson schema (verified 2026-09-11 from the restored pre-probe dump):
//   id TEXT PRIMARY KEY,
//   type TEXT NOT NULL DEFAULT 'insight',
//   content TEXT NOT NULL,
//   tags JSON,
//   reinforcement_count INTEGER,
//   source_session_id TEXT,    (nullable)
//   created TEXT NOT NULL,
//   content_hash TEXT,         (nullable)
//   retrieval_priority REAL,
//   importance REAL,
//   confidence REAL,
//   deleted_at INTEGER.        (nullable)
//
// The `created` column is TEXT (ISO8601), not INTEGER like memories —
// that's intentional in the lessons schema and we emit it verbatim in
// the output map.
func (s *WhyService) fetchLesson(id string) (map[string]interface{}, error) {
	row := s.dm.QueryRowTracked(`
		SELECT id, type, content, tags, reinforcement_count, source_session_id,
		       created, content_hash, retrieval_priority, importance, confidence, deleted_at
		FROM lessons_base WHERE id = ? AND deleted_at IS NULL LIMIT 1
	`, id)
	var outID, lessonType, content, created string
	var tagsJSON, sessID, contentHash sql.NullString
	var reinf int
	var retrievalPriority, importance, confidence float64
	var deletedAt sql.NullInt64
	if err := row.Scan(&outID, &lessonType, &content, &tagsJSON, &reinf, &sessID,
		&created, &contentHash, &retrievalPriority, &importance, &confidence, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // not a lesson — expected miss
		}
		return nil, fmt.Errorf("lesson probe scan: %w", err)
	}
	out := map[string]interface{}{
		"id":             outID,
		"collection":     "lessons",
		"type":           lessonType,
		"content":        content,
		"weight":         int64(int(retrievalPriority)),
		"importance":     importance,
		"confidence":     confidence,
		"reinforcement_count": reinf,
		"created":        created,
	}
	if sessID.Valid {
		out["session_id"] = sessID.String
	}
	if tagsJSON.Valid {
		out["tags"] = tagsJSON.String
	}
	return out, nil
}

// fetchWork reads a works row by id for `mpm why`. Timestamps are INTEGER
// Unix-epoch seconds; they are emitted under the same map keys the memory
// path uses so provenanceFromMap handles both uniformly. Returns nil when
// not found or on query failure (probe semantics).
//
// T64 2026-09-11 fix: the pre-fix scan declared `verification` and
// `session_id` as raw `string`, which failed with `converting NULL to
// string is unsupported` whenever the work had no verification row or
// no session id. The error returned from Scan is NOT sql.ErrNoRows, so
// detectKind's `continue` swallowed it as a "not a work" miss and the
// why probe reported "no artifact found" for otherwise-valid works.
// Both columns are now sql.NullString — the map only includes the
// verification key when it's actually populated.
func (s *WhyService) fetchWork(id string) (map[string]interface{}, error) {
	row := s.dm.QueryRowTracked(`
		SELECT id, title, content, status, verification, session_id, created_at, updated_at
		FROM works WHERE id = ? LIMIT 1
	`, id)
	var outID, title, status string
	var content sql.NullString
	var verification sql.NullString
	var sessID sql.NullString
	var createdAt, updatedAt sql.NullInt64
	if err := row.Scan(&outID, &title, &content, &status, &verification, &sessID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // id is not a work artifact — expected miss
		}
		return nil, fmt.Errorf("work probe scan: %w", err)
	}
	out := map[string]interface{}{
		"id":         outID,
		"collection": "works",
		"title":      title,
		"status":     status,
	}
	if content.Valid && content.String != "" {
		out["content"] = content.String
	} else {
		// Title is the work's identifying text; use it for the identity
		// preview rather than rendering an empty artifact.
		out["content"] = title
	}
	if verification.Valid {
		out["verification"] = verification.String
	}
	if sessID.Valid {
		out["session_id"] = sessID.String
	}
	if createdAt.Valid && createdAt.Int64 > 0 {
		out["created_at_unix"] = createdAt.Int64
		out["created_at"] = strconv.FormatInt(createdAt.Int64, 10)
	}
	if updatedAt.Valid && updatedAt.Int64 > 0 {
		out["updated_at_unix"] = updatedAt.Int64
		out["updated_at"] = strconv.FormatInt(updatedAt.Int64, 10)
	}
	return out, nil
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
	// ListEvidence returns []map[string]interface{} not []interface{},
	// so we need to handle both cases for type safety.
	var rawList []map[string]interface{}
	switch v := res["evidence"].(type) {
	case []map[string]interface{}:
		rawList = v
	case []interface{}:
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				rawList = append(rawList, m)
			}
		}
	}
	rows := make([]EvidenceRow, 0, len(rawList))
	for _, m := range rawList {
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
//
// F15: timestamp columns hold INTEGER Unix-epoch seconds since the
// timestamps_unified_v1 migration. The driver surfaces those as int64
// (scanned here through NullString as decimal strings) — so both the
// numeric form and legacy text forms are parsed. An unparseable or absent
// timestamp stays zero and the renderer reports it explicitly instead of
// printing a misleading "(unknown)" for data that exists.
func provenanceFromMap(m map[string]interface{}) *WhyProvenance {
	if m == nil {
		return nil
	}
	out := &WhyProvenance{}
	if t, ok := sqliteTimeFromMap(m, "created_at"); ok {
		out.CreatedAt = t
	}
	if t, ok := sqliteTimeFromMap(m, "updated_at"); ok {
		out.UpdatedAt = t
	}
	if t, ok := sqliteTimeFromMap(m, "last_accessed_at"); ok && !t.IsZero() {
		tt := t
		out.LastAccessed = &tt
	}
	if v, ok := m["session_id"].(string); ok {
		out.SessionID = v
	}
	return out
}

// sqliteTimeFromMap resolves a timestamp field that may be stored as an
// integer unix-epoch (post-migration), a numeric string, or legacy
// CURRENT_TIMESTAMP / RFC3339 text.
func sqliteTimeFromMap(m map[string]interface{}, key string) (time.Time, bool) {
	switch v := m[key].(type) {
	case int64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(v, 0).UTC(), true
	case float64:
		if v <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(v), 0).UTC(), true
	case string:
		if v == "" {
			return time.Time{}, false
		}
		// Integer-epoch decimal string?
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sec <= 0 {
				return time.Time{}, false
			}
			return time.Unix(sec, 0).UTC(), true
		}
		if t, err := parseSQLiteTime(v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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
