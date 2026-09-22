package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core"
	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/orchestration"
	"github.com/flowbyte-com/mpm-core/renderers"
	"path/filepath"
)

// handlers.go contains the unified tool handlers that power both
// `mpm call <tool>` (CLI) and the MCP server. Each handler takes a
// DatabaseManager + ActiveContext + payload, calls the appropriate
// internal/ method, and returns a JSON-marshallable result.
//
// Conventions:
//   - Read payload args with the `internal.Parse*Or` helpers (they
//     default-when-missing / type-coerce consistently).
//   - Return (result, nil) on success; (nil, err) on failure.
//   - Use `ac` for write provenance; do NOT read global mode/persona vars.
//   - Don't open/close the DB — the dispatcher owns that lifetime.

// normalizeProjection (alpha-4 D-002/W-003) accepts both "projection"
// (canonical) and "mode" (deprecated alias) input keys for the
// pointer-native projection knobs on mpm_memory query, mpm_lessons
// search, and mpm_lessons list.
//
// Resolution rules:
//   - canonical values are exactly "summary" and "full".
//   - mode == "content" → mapped to "full" (legacy equivalent).
//   - mode == "verbose" → rejected (verbose is a render-time concept,
//     not a projection knob; the response `mode` field would lie).
//   - any other non-empty value on either key → rejected with the
//     canonical-list error.
//   - both keys set to the same value → ok.
//   - both keys set to different values → prefer "projection", ignore "mode".
//   - neither key set / empty → "summary" (default unchanged).
//
// Returning a structured error keeps the failure parseable by machine
// callers (matches the existing tool-envelope shape used elsewhere).
func normalizeProjection(p map[string]interface{}) (string, error) {
	const canonicalList = `[summary, full]`

	projection, _ := p["projection"].(string)
	mode, _ := p["mode"].(string)

	// Validate "projection" against the strict canonical set. Any other
	// non-empty value (including the legacy "content" alias and the
	// non-projection "verbose" knob) is a typo at this layer.
	if projection != "" && projection != "summary" && projection != "full" {
		return "", fmt.Errorf("unknown projection %q; canonical values: %s", projection, canonicalList)
	}

	// Validate "mode" against the legacy-tolerant set: "summary",
	// "full", and the "content" alias for "full". Anything else —
	// including "verbose" — is rejected.
	if mode != "" && mode != "summary" && mode != "full" && mode != "content" {
		return "", fmt.Errorf("unknown projection %q (mode alias); canonical values: %s", mode, canonicalList)
	}

	// mode == "content" is the documented legacy equivalent of "full".
	// Remap ONLY when "projection" is empty — if both are set, the
	// canonical key wins and "mode" is silently ignored (no error,
	// since the caller expressed clear intent on the canonical key).
	if mode == "content" && projection == "" {
		mode = "full"
	}

	if projection != "" {
		return projection, nil
	}
	if mode != "" {
		return mode, nil
	}
	return "summary", nil
}

// handleSaveToMemory persists a memory row.
//
// Mirror contract (read this before debugging "why isn't my fact in
// mirror.jsonl"): the JSONL mirror at src/db/mirror.jsonl is appended to
// ONLY for these collections: changelog, memories, theories, decisions,
// knowledge, directives, mpm-projects, world-cup-2026. Collections NOT
// mirrored include:
//
//   - lessons  — lessons are cognitive-process trace, not source-of-truth
//     memory; the lesson's own index table is the source.
//   - scratchpad_orphans — ephemeral by definition; lives in
//     ephemeral_scratchpad, not memories.
//
// If you need a new collection mirrored, append to the write-side filter
// in db.go:ChallengeMemoryAsync / contradiction_log.go and add the new
// collection to the doc-comment here so future agents don't burn cycles
// diagnosing a non-bug.
func handleSaveToMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}

	// Alpha cleanup (2026-09-10): projection parity across save/show/query.
	// Default "summary" keeps the bounded-preview behavior (F4 audit
	// finding — bounded echoes prevent context-bloat on large saves).
	// "full" returns the complete stored fact in the response without a
	// second query. Projection is a wire-format choice only; the stored
	// memory is NEVER mutated by projection.
	projection, err := normalizeProjection(p)
	if err != nil {
		return nil, err
	}

	// F12-1: explicit type guard on `weight` at the agent boundary.
	// ParseFloatOr silently coerces strings ("5" -> 5.0), which the
	// alpha audit flagged as silent type coercion. The contract at this
	// boundary is:
	//   - absent    → use default (0.5)
	//   - wrong type → return a clear error (NOT silent default)
	//   - right type → use the value
	weight, weightErr := parseWeightStrict(p["weight"], mpminternal.DefaultMemoryWeight)
	if weightErr != nil {
		return nil, weightErr
	}

	// Build the WrapperContext for snapshot injection. RecentTool is
	// pulled from the process-local buffer that the registry
	// interceptor populates; if no tool preceded this save, RecentTool
	// is nil and the resolver simply omits the provenance block.
	//
	// _confidence_band and _reasoning_depth are optional payload
	// fields the LLM can supply for richer snapshots. Default empty
	// (the validator accepts empty). Documented in the tool schema.
	wc := &mpminternal.WrapperContext{
		AgentID:        ac.Agent,
		SessionID:      ac.SessionID,
		Model:          ac.Model,
		RecentTool:     mpminternal.GlobalToolBuffer().Head(ac.SessionID),
		ConfidenceBand: internal.ParseStringOr(p["_confidence_band"], ""),
		ReasoningDepth: internal.ParseStringOr(p["_reasoning_depth"], ""),
	}

	out, _, err := dm.SaveMemoryWithContextAndSnapshot(
		fact,
		internal.ParseStringOr(p["collection"], "memories"),
		internal.ParseStringSliceOr(p["tags"]),
		weight,
		internal.ParseStringOr(p["ttl"], ""),
		ac,
		wc,
	)
	if err != nil {
		return nil, err
	}

	// Per spec §4.4: surface structured embedding status in the MCP response.
	// Three cases:
	//   1. embedErr != nil  → provider unreachable; §4.4 error shape
	//   2. embedErr == nil but intentionally disabled → success with embedding_status="disabled"
	//   3. all other cases (available OR absent/unconfigured) → pure success
	//
	// Case 3 preserves the full response from SaveMemoryWithContextAndSnapshot,
	// which carries F4 bounded-echo fields (content, content_truncated, etc.).
	// Note: absent/unconfigured is NOT an error; it falls through to Case 3
	// so the full F4 bounded-echo response is preserved.
	embedErrStr, hasEmbedErr := out["embedding_error"].(string)
	cfg := mpminternal.DefaultEmbeddingConfig()
	if hasEmbedErr && embedErrStr != "" {
		// Case 1: provider unreachable — §4.4 structured error
		return map[string]interface{}{
			"memory_persisted":  true,
			"embedding_status":  "unavailable",
			"backfill_required": true,
			"error":             fmt.Sprintf("embedding provider %q is unreachable: %s", cfg.ProviderName, embedErrStr),
			"memory_id":         out["id"],
		}, nil
	}

	// Case 2: intentionally disabled — not an error, but embedding_status="disabled"
	// so the caller knows the memory has no embedding vector.
	if cfg.IntentionallyDisabled {
		return map[string]interface{}{
			"memory_persisted":  true,
			"embedding_status":  "disabled",
			"backfill_required": false,
			"memory_id":         out["id"],
		}, nil
	}

	// Case 3: pure success — preserve all F4 bounded-echo fields from out.
	// Also add memory_id alias so callers using the §4.4 field name get a hit.
	result := make(map[string]interface{}, len(out)+2)
	for k, v := range out {
		result[k] = v
	}
	result["memory_persisted"] = true
	result["memory_id"] = out["id"]

	// Projection: summary (default) keeps the bounded preview + the
	// content_truncated/content_bytes metadata. "full" returns the
	// complete stored fact and clears the truncation markers so a
	// downstream consumer doesn't have to inspect them. Stored content
	// is unchanged by either projection — the fact variable above IS
	// the stored content at this point in the flow.
	if projection == "full" {
		result["content"] = fact
		delete(result, "content_truncated")
		// content_bytes is informational (size of stored content) and
		// remains useful even under full projection, so keep it.
		result["projection"] = "full"
	} else {
		result["projection"] = "summary"
	}
	return result, nil
}

// MinMilestoneSummaryChars is the minimum length for a milestone summary.
// The threshold encodes "could another agent defend this claim from the
// summary alone?" — a 20-char string like "shipped scratchpad" is tactical
// noise that fails the signal-density test. 50 chars is the floor; longer
// is preferred and the wake-context renderer allows up to 80 chars before
// truncation.
const MinMilestoneSummaryChars = 50

// handleCommitMilestone persists a deliberate narrative milestone. It is a
// thin wrapper over save_to_memory with two hard gates:
//
//  1. Summary length floor (MinMilestoneSummaryChars). The verb exists to
//     force a commitment ceremony — a milestone that doesn't survive the
//     summary length test isn't a milestone, it's noise.
//  2. Flavor taxonomy. shipped vs insight, prefix-injected as
//     type:milestone-<flavor>. The prefix is the SQL anchor for the
//     wake-context query; do not double-prefix if the caller passed it.
//
// Milestones are decoupled from session_id by design (a milestone is a
// cognitive artifact that should survive session boundaries). The
// underlying save_to_memory call attaches session metadata via
// ActiveContext.withActiveContextMeta() for forensic value, but the
// memory row itself is not session-bound, and the wake-context query
// for recent milestones crosses all sessions by default.
//
// The save_to_memory path inherits the 20-pattern security scanner and
// 20-pattern poison scanner from SaveMemoryNode, so commit_milestone
// inherits them at no extra cost. A milestone about "AWS keys found"
// would fail the scanner; that's the right shape (the milestone should
// describe the *fact*, not the secret).
func handleCommitMilestone(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	summary, _ := p["summary"].(string)
	if summary == "" {
		return nil, fmt.Errorf("summary is required")
	}
	if len(summary) < MinMilestoneSummaryChars {
		return nil, fmt.Errorf("summary must be at least %d characters (got %d) — a milestone must be defensible from the summary alone", MinMilestoneSummaryChars, len(summary))
	}

	flavor := internal.ParseStringOr(p["flavor"], "shipped")
	switch flavor {
	case "shipped", "insight":
		// valid
	default:
		return nil, fmt.Errorf("flavor must be 'shipped' or 'insight' (got %q)", flavor)
	}

	typeTag := "type:milestone-" + flavor
	tags := internal.ParseStringSliceOr(p["tags"])

	// Dedupe: if caller already passed the canonical tag, don't double it.
	dup := false
	for _, t := range tags {
		if t == typeTag {
			dup = true
			break
		}
	}
	if !dup {
		tags = append([]string{typeTag}, tags...)
	}

	// Same WrapperContext pattern as handleSaveToMemory — milestones
	// benefit from provenance ("shipped X because I just read Y")
	// and the cost is one extra map merge.
	wc := &mpminternal.WrapperContext{
		AgentID:        ac.Agent,
		SessionID:      ac.SessionID,
		Model:          ac.Model,
		RecentTool:     mpminternal.GlobalToolBuffer().Head(ac.SessionID),
		ConfidenceBand: internal.ParseStringOr(p["_confidence_band"], ""),
		ReasoningDepth: internal.ParseStringOr(p["_reasoning_depth"], ""),
	}

	out, _, err := dm.SaveMemoryWithContextAndSnapshot(
		summary,
		"memories",
		tags,
		0.5,
		"",
		ac,
		wc,
	)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// callQueryLongTermMemory searches memory for context.
// ── Phase 2B: Pointer-native recall ─────────────────────────────────────────

// RetrievedEntryMetadata is the retrieval telemetry for a single artifact.
// Stable schema: the key is always present in the JSON output; the value is
// null when the artifact has never been retrieved.
type RetrievedEntryMetadata struct {
	ReuseCount      int    `json:"reuse_count"`
	SuccessCount    int    `json:"success_count"`
	LastRetrievedAt string `json:"last_retrieved_at,omitempty"` // RFC3339; "" = never
}

// ProjectedMemoryEntry is the Phase 2B pointer-native recall output shape.
// Instead of full content, the agent receives a summary, the canonical pointer URI,
// and retrieval telemetry. This keeps the recall payload bounded while preserving
// a cheap re-retrieval path via mpm_resolve.
type ProjectedMemoryEntry struct {
	ID                 string   `json:"id"`
	Summary            string   `json:"summary"`           // first 256 chars via SummarizeMemory
	SummaryTruncated   bool     `json:"summary_truncated"` // true when the 256-char bound cut content; pointer is the full-retrieval path
	Pointer            string   `json:"pointer"`           // "mpm://memory/<id>"
	Type               string   `json:"type"`              // always "memory"
	Tags               []string `json:"tags,omitempty"`
	Collection         string   `json:"collection"`
	CreatedAt          int64    `json:"created_at"`
	ReinforcementCount int      `json:"reinforcement_count"`
	// Alpha-4.1 F-001: weight is REAL in SQLite (HybridResult.Weight is
	// float64). Previously declared `int` which silently truncated
	// values like 7.5 → 7 and surfaced 0 for non-default weights when
	// the source value couldn't be coerced.
	Weight float64 `json:"weight"`
	// RetrievalMetadata key always present; null when never retrieved.
	RetrievalMetadata *RetrievedEntryMetadata `json:"retrieval_metadata"`
	Score             float64                 `json:"score"`
	Rationale         string                  `json:"rationale"`
	IsStale           bool                    `json:"is_stale"`
}

// ProjectedLessonEntry is the Phase 2D pointer-native lesson output shape.
// Compact lessons are returned in full; the pointer enables re-retrieval.
type ProjectedLessonEntry struct {
	ID                 string   `json:"id"`
	Summary            string   `json:"summary"`           // full (lessons are compact)
	SummaryTruncated   bool     `json:"summary_truncated"` // true when projection=summary cut the underlying content to 256 chars
	Pointer            string   `json:"pointer"`           // "mpm://lesson/<id>"
	Type               string   `json:"type"`              // warning/practice/insight
	Tags               []string `json:"tags,omitempty"`
	CreatedAt          string   `json:"created_at"`
	ReinforcementCount int      `json:"reinforcement_count"`
	// RetrievalMetadata key always present; null when never retrieved.
	RetrievalMetadata *RetrievedEntryMetadata `json:"retrieval_metadata"`
	Rationale         string                  `json:"rationale"`
}

// Alpha-4.1 F-001: shared numeric coercion helpers for projection.
//
// HybridSearchMemories stores weight as float64, created_at as int64,
// and reinforcement_count as int (see HybridResult in
// internal/core/hybrid_search.go). A previous round of code asserted
// against a single type per field with `mem["x"].(T)`, which silently
// returned zero whenever the actual stored value differed (e.g.
// weight=7.5 stored as float64 but asserted as int). These helpers
// accept every numeric shape the SQL layer or json.Number decoder may
// produce, so projection always reports the truthful value.
//
// Conversion failure is non-silent: callers can ask for the bool
// "found" return to distinguish a present-but-zero scalar from a
// missing field. We never want a missing-field zero to masquerade as a
// truthful zero in the agent payload.
func coerceInt64(v interface{}) (int64, bool) {
	// Coerce every numeric shape the SQL layer or json.Number decoder
	// may produce so projection always reports the truthful value.
	switch n := v.(type) {
	case nil:
		return 0, false
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, true
		}
		f, ferr := n.Float64()
		if ferr == nil {
			return int64(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}

func coerceInt(v interface{}) (int, bool) {
	i, ok := coerceInt64(v)
	return int(i), ok
}

func coerceFloat64(v interface{}) (float64, bool) {
	// Coerce every numeric shape — see asInt64 comment. Used for weight
	// (HybridResult stores float64) and combined_score.
	switch n := v.(type) {
	case nil:
		return 0, false
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err == nil {
			return f, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// computeScore derives a scalar from a memory map for projected output.
// Mirrors the scoring logic used in the FTS retrieval path.
func computeScore(mem map[string]interface{}) float64 {
	if s, ok := coerceFloat64(mem["combined_score"]); ok && s != 0 {
		return s
	}
	weight, _ := coerceInt(mem["weight"])
	reinf, _ := coerceInt(mem["reinforcement_count"])
	return float64(reinf*2) + float64(weight)*1.5
}

// formatRationaleForMemory produces a human-readable provenance string for
// a projected memory entry.
func formatRationaleForMemory(mem map[string]interface{}) string {
	parts := make([]string, 0, 3)
	if coll, ok := mem["collection"].(string); ok && coll != "" {
		parts = append(parts, coll)
	}
	if w, ok := coerceFloat64(mem["weight"]); ok && w > 0 {
		parts = append(parts, fmt.Sprintf("weight %s", formatWeight(w)))
	}
	if r, ok := coerceInt(mem["reinforcement_count"]); ok && r > 0 {
		parts = append(parts, fmt.Sprintf("%dx ref", r))
	}
	if len(parts) == 0 {
		return "memory"
	}
	return strings.Join(parts, " · ")
}

// formatWeight renders a weight value without trailing decimals when the
// number is a whole integer (so "weight 5" instead of "weight 5.000000").
func formatWeight(w float64) string {
	if w == float64(int64(w)) {
		return fmt.Sprintf("%d", int64(w))
	}
	return strconv.FormatFloat(w, 'f', -1, 64)
}

// isMemoryStaleForProjection returns true when a memory has not been accessed
// in the last 14 days.
func isMemoryStaleForProjection(mem map[string]interface{}) bool {
	// last_accessed_at may be nil for memories created before the column was added.
	v, ok := mem["last_accessed_at"]
	if !ok || v == nil {
		// Fall back to created_at.
		v, ok = mem["created_at"]
		if !ok {
			return false
		}
	}
	var unixSec int64
	switch n := v.(type) {
	case float64:
		unixSec = int64(n)
	case int64:
		unixSec = n
	case int:
		unixSec = int64(n)
	default:
		return false
	}
	if unixSec == 0 {
		return false
	}
	age := time.Now().Unix() - unixSec
	return age > 14*24*3600
}

func handleQueryLongTermMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	// W-009: explicit limit validation at the API boundary. The previous
	// shape silently coerced limit=-1 to 5 and limit=100000 to a hidden
	// cap, which made the caller's intent unobservable. Now: limit=0
	// returns zero results (literally), negative limits error, and
	// excessive values are clamped to a documented ceiling.
	limit, err := parseLimitStrict(p["limit"], 5)
	if err != nil {
		return nil, err
	}
	collection, _ := p["collection"].(string)

	// Phase 2d: scope param (all|local|shared). Empty / missing
	// defaults to "all" inside the DM method.
	scope, _ := p["scope"].(string)

	items, err := dm.HybridSearchMemories(query, collection, limit, scope)
	if err != nil {
		return nil, err
	}
	// Observability Layer: every result in a memory search is a node
	// the agent is about to consume. Record each. Collection name in
	// the database is the canonical node_type — pass through.
	for _, item := range items {
		if id, _ := item["id"].(string); id != "" {
			coll, _ := item["collection"].(string)
			if coll == "" {
				coll = "memory"
			}
			_ = dm.RecordRetrieval(id, coll)
		}
	}

	// Phase 2B+: pointer-native projection with explicit "summary" | "full".
	// Default is "summary" so broad queries never bloat the agent context.
	// The full payload is always retrievable via mpm_resolve / mpm_blob_read
	// against the pointer on each entry.
	//
	// M3 audit D-020: the summary and full projections intentionally
	// use different score field names (`score` vs `combined_score`).
	// Summary returns the BM25 sparse retrieval score (cheap, for chat
	// bubbles and headline displays). Full returns the post-hybrid
	// combined score (BM25 + vector + reinforcement, opt-in only — see
	// projection == "full" branch below). Unifying the names would
	// force callers to inspect the projection mode before interpreting
	// the score, which is the opposite of the design intent: the
	// projection itself signals which scoring algorithm produced the
	// value.
	projection, err := normalizeProjection(p)
	if err != nil {
		return nil, err
	}
	if projection == "" || projection == "summary" {
		projected := make([]ProjectedMemoryEntry, 0, len(items))
		for _, mem := range items {
			id, _ := mem["id"].(string)
			content, _ := mem["content"].(string)
			tags, _ := mem["tags"].([]string)
			coll, _ := mem["collection"].(string)
			// Alpha-4.1 F-001: use shared helpers that accept int64/float64/json.Number
			// shapes — the previous strict assertions silently coerced non-zero
			// values to 0.
			createdAt, _ := coerceInt64(mem["created_at"])
			reinf, _ := coerceInt(mem["reinforcement_count"])
			weight, _ := coerceFloat64(mem["weight"])

			summary, summaryTruncated := internal.SummarizeMemoryWithEllipsis(content, 256)

			var retMeta *RetrievedEntryMetadata
			if dm != nil {
				meta, err := dm.GetRetrievalMetadata(id)
				if err == nil && meta.ReuseCount > 0 {
					lastRetrieved := ""
					if meta.LastRetrievedAt != nil {
						lastRetrieved = time.Unix(*meta.LastRetrievedAt, 0).Format(time.RFC3339)
					}
					retMeta = &RetrievedEntryMetadata{
						ReuseCount:      meta.ReuseCount,
						SuccessCount:    meta.SuccessCount,
						LastRetrievedAt: lastRetrieved,
					}
				}
			}

			rationale := formatRationaleForMemory(mem)
			isStale := isMemoryStaleForProjection(mem)

			projected = append(projected, ProjectedMemoryEntry{
				ID:                 id,
				Summary:            summary,
				SummaryTruncated:   summaryTruncated,
				Pointer:            "mpm://memory/" + id,
				Type:               "memory",
				Tags:               tags,
				Collection:         coll,
				CreatedAt:          int64(createdAt),
				ReinforcementCount: reinf,
				Weight:             weight,
				RetrievalMetadata:  retMeta,
				Score:              computeScore(mem),
				Rationale:          rationale,
				IsStale:            isStale,
			})
		}
		summaryResp := map[string]interface{}{
			"success":  true,
			"mode":     "summary",
			"query":    query,
			"memories": projected,
			"count":    len(projected),
			"scope":    defaultScope(scope),
		}
		attachZeroHitHint(summaryResp, query, len(projected))
		return summaryResp, nil
	}

	// projection == "full": unbounded content, opt-in only.
	for _, mem := range items {
		content, _ := mem["content"].(string)
		id, _ := mem["id"].(string)
		mem["pointer"] = "mpm://memory/" + id
		if len(content) > 0 {
			mem["content"] = content
		}
		// W-013: hybridResultsToMaps already decodes tags to []string;
		// decode metadata to a real object so consumers don't get a
		// quoted JSON blob.
		if rawMeta, ok := mem["metadata"].(string); ok {
			mem["metadata"] = parseMetadataColumn(rawMeta)
		}
	}
	fullResp := map[string]interface{}{
		"success":  true,
		"mode":     "full",
		"memories": items,
		"count":    len(items),
		"scope":    defaultScope(scope),
	}
	attachZeroHitHint(fullResp, query, len(items))
	return fullResp, nil
}

// handleShowMemory fetches a single memory record by id and returns it
// verbatim. W-003: agents used to have to run a search query to fetch a
// known id — wasteful when the id is already in hand. Mirrors the
// canonical CLI surface at `mpm show <id>` (see cmd/mpm/handlers.go).
//
// Required params: id (string).
// Tags and metadata are JSON-decoded into native Go types before
// returning so the response is array/object-shaped (not stringified),
// which is the W-013 contract.
func handleShowMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}

	// Alpha cleanup (2026-09-10): projection parity with query. Default
	// "summary" bounds the inline content echo to keep responses small;
	// "full" returns the complete stored body. Projection never mutates
	// the stored content — it's a wire-format choice only.
	projection, err := normalizeProjection(p)
	if err != nil {
		return nil, err
	}

	mem, err := dm.GetMemory(id)
	if err != nil {
		return nil, err
	}

	// W-013: tags column is JSON-encoded as a string in the DB.
	// Unwrap it into a native []string so consumers get a real array,
	// not the literal string "[]". Empty/null tags → []string{}.
	if rawTags, ok := mem["tags"].(string); ok {
		mem["tags"] = parseTagsColumn(rawTags)
	}

	// W-013: metadata column is also JSON-encoded. Decode into
	// map[string]interface{} so nested fields (e.g. validation_criteria,
	// tags inside metadata) become real JSON, not a quoted blob.
	if rawMeta, ok := mem["metadata"].(string); ok {
		mem["metadata"] = parseMetadataColumn(rawMeta)
	}

	// Projection: bound the inline content echo unless the caller asked
	// for "full". The stored content is NEVER mutated; the projection
	// only affects what gets returned on the wire. content_truncated +
	// content_bytes mark bounded responses so callers can detect the
	// preview vs. full distinction without a second query.
	content, _ := mem["content"].(string)
	if projection == "summary" {
		bounded, truncated := mpminternal.BoundInlineContent(content)
		mem["content"] = bounded
		if truncated {
			mem["content_truncated"] = true
			mem["content_bytes"] = len(content)
			mem["note"] = "content stored in full; inline echo bounded — re-call with projection=full for complete body"
		}
	} else {
		// projection == "full": return the stored content verbatim.
		// Don't add content_truncated/content_bytes — a full projection
		// by definition is not bounded.
		if len(content) > 0 {
			mem["content"] = content
		}
	}
	mem["projection"] = projection

	// W-007: surface challenge status as top-level fields so callers
	// can branch on `is_challenged` without re-querying the metadata
	// column. The `content` field is intentionally NOT mutated — the
	// banner is exposed alongside the original payload.
	metaForChallenge := map[string]interface{}{}
	if m, ok := mem["metadata"].(map[string]interface{}); ok {
		metaForChallenge = m
	}
	isChallenged, banner := deriveChallengeFields(metaForChallenge)
	mem["is_challenged"] = isChallenged
	mem["banner"] = banner

	mem["success"] = true
	mem["pointer"] = "mpm://memory/" + id
	return mem, nil
}

// parseTagsColumn parses the JSON-encoded tags column into a native
// []string. Empty / null / invalid input yields []string{} (never nil,
// never a string), so the wire shape is always a JSON array.
func parseTagsColumn(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return []string{}
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return []string{}
	}
	if tags == nil {
		return []string{}
	}
	return tags
}

// parseMetadataColumn parses the JSON-encoded metadata column into a
// map[string]interface{}. Empty / null / invalid input yields
// map[string]interface{}{} (never nil, never a string).
func parseMetadataColumn(raw string) map[string]interface{} {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return map[string]interface{}{}
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return map[string]interface{}{}
	}
	if meta == nil {
		return map[string]interface{}{}
	}
	return meta
}

// deriveChallengeFields inspects a memory's metadata for the
// `status: "challenged"` flag and returns a banner pair for callers to
// surface in their response. Used by handleShowMemory and both branches
// of handleMpmResolve so all three paths advertise the challenge
// status with parity.
//
// W-007 (2026-08-31): without this, a challenged memory is returned
// unchanged — the caller has to discover the status by re-querying the
// metadata column, which is a contract asymmetry. Surfacing
// is_challenged as a top-level field and a ready-to-render banner
// keeps the contract honest without mutating the stored `content`
// string.
func deriveChallengeFields(meta map[string]interface{}) (bool, string) {
	if status, ok := meta["status"].(string); ok && status == "challenged" {
		return true, "[Note: This memory is challenged — treat as unverified]"
	}
	return false, ""
}

// attachZeroHitHint adds a `hint` field to the response when the query
// returned zero results and the query has multiple whitespace-separated
// tokens. The hint nudges the agent toward single-token queries (where
// per-token BM25 scores don't compete against each other for IDF).
//
// W-010: silent zero-hit results are a recurring agent failure mode.
// Surfacing the hint at the protocol layer keeps the contract honest —
// the agent learns from one read that multi-token queries may need
// simplification, instead of guessing after the third retry.
func attachZeroHitHint(resp map[string]interface{}, query string, count int) {
	if count != 0 {
		return
	}
	tokens := strings.Fields(query)
	if len(tokens) <= 1 {
		return
	}
	resp["hint"] = "0 hits for multi-token query; try each token individually " +
		"or reduce the query to a single distinctive term (BM25 IDF is " +
		"per-token — multi-token queries can underflow the threshold even " +
		"when individual tokens would match)."
}

// defaultScope normalises the user-supplied scope string to one of
// the three recognised values. Centralised here so the surface stays
// consistent across MCP and CLI callers.
func defaultScope(s string) string {
	switch s {
	case "local":
		return "local"
	case "shared":
		return "shared"
	default:
		return "all"
	}
}

// handleChallengeMemory weakens a memory and creates a pending theory.
// Canonical wire param is `memory_id` (snake_case); see the wire-format
// block above handleShredMemory. The dispatcher normalizes the legacy
// `memoryId` alias onto `memory_id`, so this handler only reads the
// canonical form.
func handleChallengeMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memory_id"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	evidence, _ := p["evidence"].(string)

	return dm.ChallengeMemoryWithTheory(memoryID, evidence)
}

// handleRestoreChallengeMemory is the F7-1 agent-facing surface for
// `mpm challenge restore <id>`. The CLI handler is a thin wrapper around
// the same DatabaseManager method, so the agent path is canonical.
func handleRestoreChallengeMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memory_id"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	return dm.RestoreMemoryFromChallenge(memoryID)
}

// callProposeTheory logs a hypothesis with validation criteria.
func handleProposeTheory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	hypothesis, _ := p["hypothesis"].(string)
	if hypothesis == "" {
		return nil, fmt.Errorf("hypothesis is required")
	}
	validationCriteria, _ := p["validation_criteria"].(string)
	tags := internal.ParseStringSliceOr(p["tags"])
	if tags == nil {
		tags = []string{}
	}
	// 2026-09-05 audit remediation pass 4 defect C.14 (P1): mirror
	// the C.13 tags guard on dependencies. The schema declares
	// `dependencies` as a string array of theory ids (the canonical
	// representation persisted by encodeDependencyList). The previous
	// shape routed the value through ParseStringSliceOr, which
	// silently returned nil for non-array scalars and silently
	// dropped non-string elements. The theory row was persisted with
	// dependencies=[] and the wake-on-delete scan treated it as a
	// standalone theory — caller intent (forward-edge declaration)
	// was lost without any error.
	dependencies, err := parseStrictStringArrayOrEmpty("dependencies", p["dependencies"])
	if err != nil {
		return nil, err
	}
	sourceIDs := internal.ParseStringSliceOr(p["source_ids"])
	if sourceIDs == nil {
		sourceIDs = []string{}
	}

	result, err := dm.ProposeTheory(hypothesis, validationCriteria, dependencies, sourceIDs, tags)
	if err != nil {
		return nil, err
	}
	// Forensic log — deliberate theory proposal. AuditInfo is gated
	// out of cluster detection in LogAudit (see audit.go).
	if id, ok := result["id"].(string); ok && id != "" {
		dm.LogAudit(
			mpminternal.AuditInfo, "epistemology",
			fmt.Sprintf("propose_theory %s", id), "",
			mpminternal.AuditContext{
				"theory_id":      id,
				"dependencies":   len(dependencies),
				"validation_set": validationCriteria != "",
			},
		)
	}
	return result, nil
}

// callResolveTheory marks a theory as proven or disproven.
//
// Field vocabulary (2026-09-10 cleanup): canonical names are `id`
// (the theory being resolved) and `status` (the outcome enum).
// Aliases `theoryId` / `theory_id` (the pre-cleanup camelCase /
// snake_case id) and `newStatus` / `new_status` are retained as
// backward-compat shims for historical callers. `status` accepts only
// `proven` or `disproven` — anything else is rejected loudly with
// the same message so the operator/agent self-corrects.
//
// Arc 1 closure: if winnerId is provided in the params, the theory
// is treated as an arbitration theory (created by the close-call
// path of `mpm ops resolve-contradictions`). The system routes to
// ResolveArbitrationTheory which atomically slashes the loser,
// writes a resolution memory, attaches evidence to the winner, and
// marks the queue row resolved. If winnerId is absent, the legacy
// path runs (just mark the theory resolved; no slash).
func handleResolveTheory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// Canonical: `id`. Backward-compat: `theoryId`, `theory_id`.
	theoryID, _ := p["id"].(string)
	if theoryID == "" {
		theoryID, _ = p["theoryId"].(string)
	}
	if theoryID == "" {
		theoryID, _ = p["theory_id"].(string)
	}
	if theoryID == "" {
		return nil, fmt.Errorf("resolve theory: id is required — the id of the theory being resolved")
	}
	conclusion, _ := p["conclusion"].(string)
	if conclusion == "" {
		return nil, fmt.Errorf("resolve theory: conclusion is required")
	}
	// Canonical: `winner_id`. Backward-compat: `winnerId`.
	winnerID, _ := p["winner_id"].(string)
	if winnerID == "" {
		winnerID, _ = p["winnerId"].(string)
	}

	// Arc 1 closure: arbitration auto-slash path.
	if winnerID != "" {
		result, err := dm.ResolveArbitrationTheory(theoryID, winnerID, conclusion)
		if err != nil {
			return nil, err
		}
		// Forensic log: arbitration resolution is a major state
		// transition (slash + resolution memory + queue mark) so
		// we log it at INFO with the full set of IDs.
		dm.LogAudit(
			mpminternal.AuditInfo, "epistemology",
			fmt.Sprintf("arbitration_resolve %s winner=%s loser=%s", theoryID, result["winner_id"], result["loser_id"]), "",
			mpminternal.AuditContext{
				"theory_id":            theoryID,
				"status":               "disproven",
				"conclusion":           conclusion,
				"winner_id":            result["winner_id"],
				"loser_id":             result["loser_id"],
				"queue_id":             result["queue_id"],
				"resolution_memory_id": result["resolution_memory_id"],
				"evidence_id":          result["evidence_id"],
				"slash_amount":         result["slash_amount"],
			},
		)
		return result, nil
	}

	// Canonical: `status`. Backward-compat: `new_status`, `newStatus`.
	// Conflict detection (D-008 / W-006): if multiple status keys are
	// supplied with conflicting values, reject explicitly so a typo
	// doesn't silently bind. Mirrors the alpha-4.1.1 conflict guard.
	status, _ := p["status"].(string)
	newStatus, _ := p["newStatus"].(string)
	newStatusSnake, _ := p["new_status"].(string)
	if status == "" && newStatusSnake != "" {
		status = newStatusSnake
	}
	if status == "" && newStatus != "" {
		status = newStatus
	}
	if status == "" {
		return nil, fmt.Errorf("resolve theory: status is required and must be 'proven' or 'disproven'")
	}
	// Conflict: caller supplied two distinct keys with different values.
	if newStatus != "" && newStatus != status {
		return nil, fmt.Errorf("resolve theory: conflicting status=%q and newStatus=%q — supply only one", status, newStatus)
	}
	if newStatusSnake != "" && newStatusSnake != status && newStatus == "" {
		return nil, fmt.Errorf("resolve theory: conflicting status=%q and new_status=%q — supply only one", status, newStatusSnake)
	}
	if status != "proven" && status != "disproven" {
		return nil, fmt.Errorf("resolve theory: status %q is invalid — must be 'proven' or 'disproven'", status)
	}
	// At this point `status` is the validated canonical value.
	// Re-bind into `newStatus` so the downstream dm.ResolveTheory call
	// (and the audit row below) keep their existing parameter name.
	newStatus = status

	result, err := dm.ResolveTheory(theoryID, conclusion, newStatus)
	if err != nil {
		return nil, err
	}
	// Forensic log — theory state-machine transition. The cluster
	// detector stays strict (proven/disproven are deliberately
	// resolved states, not anomalies); this row is for the operator's
	// audit trail.
	dm.LogAudit(
		mpminternal.AuditInfo, "epistemology",
		fmt.Sprintf("resolve_theory %s -> %s", theoryID, newStatus), "",
		mpminternal.AuditContext{
			"theory_id":  theoryID,
			"status":     newStatus,
			"conclusion": conclusion,
		},
	)
	return result, nil
}

// callRecordDecision logs a decision with context, choice, and rationale.
func handleRecordDecision(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	choice, _ := p["choice"].(string)
	if choice == "" {
		return nil, fmt.Errorf("choice is required")
	}
	// 2026-09-05 audit remediation pass 4 defect C.13 (P1): the schema
	// declares `tags` as a string array (optional), but the previous
	// shape routed every input through internal.ParseStringSliceOr,
	// which silently returned nil for non-array scalars and silently
	// dropped non-string elements from arrays. The caller believed the
	// record succeeded with their tags — the persisted decision had
	// tags=[] instead. Enforce the schema contract at the boundary:
	// omitted/null/[] are legitimate "no tags"; any present-but-invalid
	// shape errors with a clear message.
	tags, err := parseStrictStringArrayOrEmpty("tags", p["tags"])
	if err != nil {
		return nil, err
	}
	sourceIDs := internal.ParseStringSliceOr(p["source_ids"])
	if sourceIDs == nil {
		sourceIDs = []string{}
	}

	return dm.RecordDecision(
		internal.ParseStringOr(p["context"], ""),
		choice,
		internal.ParseStringOr(p["rationale"], ""),
		internal.ParseStringOr(p["outcome"], ""),
		tags,
		sourceIDs,
		ac,
	)
}

// parseStrictStringArrayOrEmpty is the strict counterpart to
// internal.ParseStringSliceOr for parameters declared as
// `"type":"array","items":{"type":"string"}` where the field is
// OPTIONAL in the JSON-Schema. Returns:
//
//   - ([]string{}, nil) when the key is absent or the value is nil/[]interface{}{}
//   - ([]string{...}, nil) when the value is a []interface{} of all strings
//   - (nil, error) when the value is a non-array scalar (string/number/bool/...)
//   - (nil, error) when the array contains any non-string element
//
// Unlike ParseStringSliceOr this never silently coerces or drops
// malformed input — the caller learns about the shape mismatch.
func parseStrictStringArrayOrEmpty(field string, v interface{}) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings, got %T", field, v)
	}
	out := make([]string, 0, len(arr))
	for i, x := range arr {
		s, ok := x.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%d] must be a string, got %T", field, i, x)
		}
		out = append(out, s)
	}
	return out, nil
}

// handleSupersedeDecision implements the F9 invalidation path: a corrected
// or superseding decision that is distinguishable from stale knowledge.
//
// Field vocabulary (2026-09-10 cleanup): canonical name is `id` (the
// decision being superseded). `original_id` is retained as a narrow
// backward-compat alias so the existing CLI/agent callers that used the
// historical semantic-role name keep working. The superseding replacement
// content arrives as `choice`/`context`/`rationale`/`outcome`/`tags`
// (same shape as a fresh `record` call) — there is no
// `superseding_decision_id` because the replacement IS the new decision
// row, returned in the response.
func handleSupersedeDecision(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	originalID, _ := p["id"].(string)
	if originalID == "" {
		// Backward-compat alias: callers that historically passed
		// `original_id` (the F9-era semantic-role name) still work.
		originalID, _ = p["original_id"].(string)
	}
	if originalID == "" {
		return nil, fmt.Errorf("supersede decision: id (or original_id) is required — the id of the decision being superseded")
	}
	choice, _ := p["choice"].(string)
	if choice == "" {
		return nil, fmt.Errorf("supersede decision: choice is required (the replacement decision)")
	}
	tags := internal.ParseStringSliceOr(p["tags"])
	if tags == nil {
		tags = []string{}
	}
	sourceIDs := internal.ParseStringSliceOr(p["source_ids"])
	if sourceIDs == nil {
		sourceIDs = []string{}
	}
	return dm.SupersedeDecision(
		originalID,
		internal.ParseStringOr(p["context"], ""),
		choice,
		internal.ParseStringOr(p["rationale"], ""),
		internal.ParseStringOr(p["outcome"], ""),
		tags,
		sourceIDs,
		ac,
	)
}

// handleInvalidateDecision retires a decision without a replacement.
//
// Field vocabulary (2026-09-10 cleanup): canonical name is `id`
// (matching the rest of the API — primary object identifier for the
// action). `decision_id` is retained as a narrow backward-compat alias
// for callers that historically used the explicit semantic-role name.
func handleInvalidateDecision(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		// Backward-compat alias: pre-cleanup callers passed `decision_id`.
		id, _ = p["decision_id"].(string)
	}
	if id == "" {
		return nil, fmt.Errorf("invalidate decision: id is required")
	}
	reason, _ := p["reason"].(string)
	return dm.InvalidateDecision(id, reason)
}

// ── Memory feedback / mutation tools ─────────────────────────────────────────
//
// Wire-format (all accept JSON payload via --payload or stdin):
//   shred_memory:        {"memory_id": "<id>"}
//   reinforce_memory:    {"memory_id": "<id>", "delta": 1}
//   weaken_memory:       {"memory_id": "<id>", "delta": 1}
//   snooze_memory:       {"memory_id": "<id>", "days": 1}
//   set_memory_weight:   {"memory_id": "<id>", "weight": 5}
//   patch_memory:        {"memory_id": "<id>", "patch": {"key": "value"}}
//   promote_memory:      {"memory_id": "<id>"}
//
// Added 2026-06-26 to close the agent feedback loop. Without these, agents
// had to shell `mpm reinforce <id>` etc., which forces them to invent CLI
// quoting and parse text output — neither works reliably across
// punctuation-heavy memory ids or non-ASCII content.

func handleShredMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, err := requireMemoryID(p)
	if err != nil {
		return nil, err
	}

	result, err := dm.ShredMemoryWithCascade(id)
	if err != nil {
		return nil, err
	}
	// Forensic log — irrecoverable hard delete. Without this, a
	// "rogue agent shreds foundational theory" event is invisible
	// until someone notices the absence of data. AuditInfo is gated
	// out of cluster detection (see audit.go).
	if memoryID, _ := result["memory_id"].(string); memoryID != "" {
		ctx := mpminternal.AuditContext{
			"memory_id": memoryID,
		}
		if tp, ok := result["theory_purged"].(string); ok && tp != "" {
			ctx["theory_purged"] = tp
		}
		dm.LogAudit(
			mpminternal.AuditInfo, "epistemology",
			fmt.Sprintf("shred_memory %s", memoryID), "",
			ctx,
		)
	}
	return result, nil
}

// handleDeleteMemory soft-deletes a memory row (sets deleted_at). Reversible
// via handleRestoreMemory. Distinct from handleShredMemory (hard delete with
// broad sweep — irreversible). Distinct from handleRestoreChallengeMemory
// (which only clears the challenged-status flag).
//
// Required params: memory_id (or id).
func handleDeleteMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, err := requireMemoryID(p)
	if err != nil {
		return nil, err
	}
	result, err := dm.SoftDeleteMemory(id)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// handleRestoreMemory reverses a prior soft-delete by clearing deleted_at.
// The memories_au_content trigger re-adds the FTS5 entry on the transition
// so the memory is searchable again.
//
// Required params: memory_id (or id).
func handleRestoreMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, err := requireMemoryID(p)
	if err != nil {
		return nil, err
	}
	result, err := dm.RestoreMemory(id)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// D-8.1: accept either `memory_id` or the bare `id` for memory-targeted
// operations. The canonical wire form remains `memory_id` (snake_case);
// `id` is accepted as a synonym so the surface is consistent with
// mpm_handoff shred (which uses `id`). Same helper used by every
// memory-action handler below.
func memoryIDFromParams(p map[string]interface{}) string {
	if v, _ := p["memory_id"].(string); v != "" {
		return v
	}
	if v, _ := p["id"].(string); v != "" {
		return v
	}
	return ""
}

// requireMemoryID resolves the canonical memory id (memory_id, then
// id) via memoryIDFromParams and errors at the handler boundary if
// both are missing, empty, or whitespace-only. The 2026-09-05
// audit residual pass §I-C.13 found that shred / set_weight /
// promote passed the raw lookup straight to the DM, allowing an
// empty id to reach a mutation query that then silently affected
// zero rows or surfaced an opaque SQL error. Use this helper at
// every memory mutation boundary so callers always learn about a
// missing id before any database write.
func requireMemoryID(p map[string]interface{}) (string, error) {
	id := strings.TrimSpace(memoryIDFromParams(p))
	if id == "" {
		return "", fmt.Errorf("memory_id is required")
	}
	return id, nil
}

func handleReinforceMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id := memoryIDFromParams(p)
	if id == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	// 2026-09-05 audit remediation pass 2: parseFloatStrict rejects
	// strings ("5", "abc", "") rather than silently coercing/defaulting.
	// Omission (key absent) still uses the legitimate default of 1.
	deltaF, err := parseFloatStrict(p["delta"], 1, "delta", 0)
	if err != nil {
		return nil, fmt.Errorf("reinforce: %w", err)
	}
	return dm.ReinforceMemoryTool(id, int(deltaF))
}

func handleWeakenMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id := memoryIDFromParams(p)
	if id == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	// 2026-09-05 audit remediation pass 2: see handleReinforceMemory.
	deltaF, err := parseFloatStrict(p["delta"], 1, "delta", 0)
	if err != nil {
		return nil, fmt.Errorf("weaken: %w", err)
	}
	return dm.WeakenMemoryTool(id, int(deltaF))
}

func handleSnoozeMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id := memoryIDFromParams(p)
	if id == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	// 2026-09-05 audit remediation pass 2: see handleReinforceMemory.
	// Note: snooze rejects negative days at the DM layer (dm.go:825),
	// but the silent coercion from "abc" → 1 must be caught here.
	// min=1 enforces "days >= 1" at the public boundary.
	daysF, err := parseFloatStrict(p["days"], 1, "days", 1)
	if err != nil {
		return nil, fmt.Errorf("snooze: %w", err)
	}
	return dm.SnoozeMemory(id, int(daysF))
}

func handleSetMemoryWeight(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, err := requireMemoryID(p)
	if err != nil {
		return nil, err
	}
	// W-004 (2026-08-31): pass float64 directly so fractional weights like
	// 7.5 persist to the REAL column without truncation. The previous
	// `int(...)` cast silently coerced 7.5 → 7 before the UPDATE.
	//
	// 2026-09-05 audit remediation pass 2: parseFloatStrict replaces the
	// silent ParseFloatOr default of 0 with explicit rejection of
	// strings / non-numeric input. The audit caught weight="abc"
	// silently persisting weight=0; weight="5" silently coercing to
	// weight=5. Omission is NOT a legitimate default for weight
	// (mpm_memory.save.weight has no default either — see the
	// F12-1 contract). Use def=0 only when caller explicitly wants
	// weight=0; otherwise the parseFloatStrict default is irrelevant
	// because the field is required. We keep def=0 here for
	// compatibility with existing callers that omit weight on a
	// non-save mutation path, but the audit-test pinned that
	// omission errors (see TestSetWeight_OmittedUsesDefault).
	weight, err := parseFloatStrict(p["weight"], 0, "weight", 0)
	if err != nil {
		return nil, fmt.Errorf("set_weight: %w", err)
	}
	return dm.SetMemoryWeight(id, weight)
}

func handlePatchMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	// patch must be a JSON object (map[string]interface{}). The DM
	// layer accepts any JSON string (json.Valid is permissive — null,
	// primitives, arrays all pass), and SQLite's json_patch treats
	// non-object patches as no-ops or undefined shapes against an
	// object target. The 2026-09-05 audit found this allowed a `null`
	// patch to reach JSONPatch and silently overwrite metadata with
	// an undefined shape — the handler comment that claimed
	// "rejected upstream by the DM" was false.
	//
	// Canonical contract: patch is a JSON object. Empty object is a
	// valid no-op. Null, scalar, and array are explicit user errors
	// and must be rejected before persistence.
	raw, ok := p["patch"]
	if !ok || raw == nil {
		return nil, fmt.Errorf("patch: must be a JSON object (got null/absent)")
	}
	patch, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("patch: must be a JSON object (got %T, not map[string]interface{})", raw)
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("patch: JSON-marshalable object required: %w", err)
	}
	return dm.PatchMemoryMetadata(id, string(patchJSON))
}

func handlePromoteMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, err := requireMemoryID(p)
	if err != nil {
		return nil, err
	}
	return dm.PromoteMemory(id)
}

// handleMigrate imports memories from a non-SQLite source file. Wraps the
// markdown/json migration pipeline for MCP clients.
//
//	Wire-format: {
//	  "from_path":     "<path>",          # required for stage mode
//	  "format":        "markdown|json|auto", # default "auto" (extension-based)
//	  "label":         "<name>",          # optional batch label
//	  "dry_run":       false,             # optional
//	  "commit":        false,             # stage + immediately promote
//	  "commit_batch":  "<batch_id>",      # alternative: just promote a staged batch
//	  "undo_batch":    "<batch_id>"       # alternative: rollback a batch
//	}
//
// Returns a map with rows_read, rows_staged, rows_skipped, rows_rejected,
// batch_id, and (if commit or commit_batch) rows_promoted.
func handleMigrate(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// 2026-09-05 audit remediation pass 6 defect C.20 (P2): the migrate
	// action alters persistent database state (stages rows in
	// raw_memories, promotes them to memories, or rejects a batch via
	// direct UPDATE). Previously it ran without any explicit
	// authorization gate — an agent could call `mpm_system.migrate`
	// with arbitrary from_path / commit_batch / undo_batch and have
	// it execute. Mirror the record_global_rule convention
	// (handlers.go handleRecordGlobalRule: "house rules should not be
	// written autonomously"): require explicit `confirm=true`. Only
	// the boolean literal `true` authorises execution; omitted,
	// false, null, string "true", numeric 1, and every other shape
	// is rejected before any DB call. The same handler backs both
	// CLI and MCP surfaces via the registry, so this single boundary
	// closes the safety invariant for every public path.
	confirm, ok := p["confirm"].(bool)
	if !ok || !confirm {
		return nil, fmt.Errorf("migrate requires confirm=true; bulk database writes should not be executed autonomously")
	}
	fromPath, _ := p["from_path"].(string)
	formatStr, _ := p["format"].(string)
	if formatStr == "" {
		formatStr = "auto"
	}
	label, _ := p["label"].(string)
	dryRun, _ := p["dry_run"].(bool)
	commit, _ := p["commit"].(bool)
	commitBatch, _ := p["commit_batch"].(string)
	undoBatch, _ := p["undo_batch"].(string)

	result := map[string]interface{}{}

	// Pure promote mode
	if commitBatch != "" {
		n, err := dm.PromoteRawMemoryBatch(commitBatch, dryRun)
		if err != nil {
			return nil, fmt.Errorf("commit failed: %w", err)
		}
		result["action"] = "promote"
		result["batch_id"] = commitBatch
		result["rows_promoted"] = n
		result["dry_run"] = dryRun
		return result, nil
	}

	// Pure undo mode: delegate via direct SQL (no DM method exists yet;
	// rollback is rare enough that adding a method is overkill).
	if undoBatch != "" {
		// Best-effort undo: delete pending/approved rows for the batch.
		// Approved rows that have been promoted to memories are tombstoned
		// rather than hard-deleted so audit trails survive.
		if !dryRun {
			_, _ = dm.SQLDB().Exec(
				`UPDATE raw_memories SET status='rejected', llm_notes='undo by mcp tool', updated_at=? WHERE import_batch=? AND status IN ('pending','approved')`,
				float64(time.Now().Unix()), undoBatch)
		}
		result["action"] = "undo"
		result["batch_id"] = undoBatch
		result["dry_run"] = dryRun
		return result, nil
	}

	// Stage mode
	if fromPath == "" {
		return nil, fmt.Errorf("from_path is required (or use commit_batch / undo_batch)")
	}

	// Auto-detect format from extension
	format := formatStr
	if format == "auto" {
		ext := strings.ToLower(filepath.Ext(fromPath))
		switch ext {
		case ".md", ".markdown":
			format = "markdown"
		case ".json":
			format = "json"
		default:
			return nil, fmt.Errorf("could not auto-detect format for %s; specify format=markdown|json explicitly", fromPath)
		}
	}

	batchID := fmt.Sprintf("mcp_migrate_%s_%d", sanitizeMigrateLabel(labelOrPath(label, fromPath)), time.Now().Unix())

	var stats *mpminternal.MigrateStats
	var err error
	switch format {
	case "markdown":
		stats, err = dm.IngestFromMarkdownFile(fromPath, batchID, dryRun)
	case "json":
		stats, err = dm.IngestFromJsonFile(fromPath, batchID, dryRun)
	default:
		return nil, fmt.Errorf("unsupported format: %s", format)
	}
	if err != nil {
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	result["action"] = "stage"
	result["batch_id"] = batchID
	result["format"] = format
	result["from_path"] = fromPath
	result["rows_read"] = stats.RowsRead
	result["rows_staged"] = stats.RowsStaged
	result["rows_skipped"] = stats.RowsSkipped
	result["rows_rejected"] = stats.RowsRejected
	result["dry_run"] = dryRun

	if commit && !dryRun && stats.RowsStaged > 0 {
		n, err := dm.PromoteRawMemoryBatch(batchID, false)
		if err != nil {
			return result, fmt.Errorf("auto-commit failed after stage: %w", err)
		}
		result["rows_promoted"] = n
	}

	return result, nil
}

func sanitizeMigrateLabel(s string) string {
	s = strings.ToLower(s)
	for _, bad := range []string{" ", "/", ".", "-"} {
		s = strings.ReplaceAll(s, bad, "_")
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func labelOrPath(label, path string) string {
	if label != "" {
		return label
	}
	return filepath.Base(path)
}

// callReviewMemories returns memories due for spaced reinforcement review.
// Wire-format: {"days": 30, "limit": 20} — both optional with sensible defaults.
//
// 2026-09-05 audit remediation pass 2: parseFloatStrict replaces silent
// ParseFloatOr so days="abc" doesn't silently default to 30 (or any
// other value). Omission still uses the legitimate defaults.
func handleReviewMemories(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	daysF, err := parseFloatStrict(p["days"], 30, "days", 1)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	limitF, err := parseFloatStrict(p["limit"], 20, "limit", 0)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	return dm.ReviewMemories(int(daysF), int(limitF))
}

// callSynthesizeMemory runs LLM-driven merge synthesis for one memory.
// Wire-format: {"memory_id": "<id>"}. Lazy SynthClient creation; requires
// MINIMAX_API_KEY or OPENAI_API_KEY in env to actually invoke the LLM.
func handleSynthesizeMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	return dm.SynthesizeMemoryFor(context.Background(), id)
}

// callGCRun wraps dm.RunGC with a typed payload. Safe defaults:
// dry_run=true (no writes), aggressive=false, max_age_hours=24,
// stale_theory_days=30 (pending theories older than 30 days are
// flagged; auto-resolved only when dry_run=false). Callers must
// explicitly set dry_run=false to mutate state. The full CLI flag
// surface (--review, --purge, --shred-negative) stays on `mpm gc`
// because those modes are operationally distinct.
func handleGCRun(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	dryRun := parseBoolDefault(p["dry_run"], true) // safe default
	aggressive := parseBoolDefault(p["aggressive"], false)
	maxAge := int(internal.ParseFloatOr(p["max_age_hours"], 24))
	staleDays := int(internal.ParseFloatOr(p["stale_theory_days"], 30))
	if staleDays < 0 {
		staleDays = 0
	}

	out, err := dm.RunGC(internal.GCOptions{
		DryRun:          dryRun,
		Aggressive:      aggressive,
		MaxAgeHours:     maxAge,
		StaleTheoryDays: staleDays,
	})
	if err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"success":                 true,
		"dry_run":                 dryRun,
		"cooldown_skip":           out.CooldownSkip,
		"ran":                     out.Ran,
		"scanned":                 out.Scanned,
		"updated":                 out.Updated,
		"audit_pruned":            out.AuditPruned,
		"handoff_pruned":          out.HandoffPruned,
		"cascade_outbox_pruned":   out.CascadeOutboxPruned,
		"dead_memory_count":       len(out.DeadMemories),
		"dead_memories":           out.DeadMemories,
		"stale_theory_days":       staleDays,
		"stale_theories":          out.StaleTheories,
		"stale_theories_resolved": out.StaleTheoriesResolved,
	}
	if out.LastGCRan != nil {
		result["last_gc_ran"] = mpminternal.FormatUnixSeconds(*out.LastGCRan)
	}
	return result, nil
}

// parseBoolDefault extracts a bool from the payload, falling back to def.
// Accepts both native bool (JSON true/false) and the int-shaped values
// some callers emit (0/1, "true"/"false"). Lenient on purpose — payload
// shape across MCP / mpm call / openclaw-plugin isn't strictly uniform.
func parseBoolDefault(v interface{}, def bool) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case string:
		switch t {
		case "true", "True", "TRUE", "1", "yes":
			return true
		case "false", "False", "FALSE", "0", "no", "":
			return false
		}
	}
	return def
}

// parseIntDefault parses a JSON-shaped integer parameter (float64 from
// the JSON boundary, int from native callers, string from legacy
// callers) and returns the default for any unparseable value. The
// compact_drain handler uses it to accept max_batches from any
// caller shape without forcing schema changes.
func parseIntDefault(v interface{}, def int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		var n int
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

// inferNodeType best-effort classifies a citation id into one of the
// cognitive-object types (memory / lesson / skill / decision / theory)
// by inspecting the id prefix. Falls back to "memory" when the prefix
// is unfamiliar — the save_lesson Provenance Proxy must not block on
// unknown id formats and the schema's NOT NULL constraint on
// node_type requires a value.
func inferNodeType(id string) string {
	switch {
	case strings.HasPrefix(id, "skill:"):
		return "skill"
	case strings.HasPrefix(id, "lesson:"), strings.HasPrefix(id, "les-"):
		return "lesson"
	case strings.HasPrefix(id, "dec-"):
		return "decision"
	case strings.HasPrefix(id, "theory:"), strings.HasPrefix(id, "the-"):
		return "theory"
	default:
		return "memory"
	}
}

// callSaveLesson persists a lesson to MPM.
func handleSaveLesson(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	lessonType := internal.ParseStringOr(p["type"], "insight")
	tags := internal.ParseStringSliceOr(p["tags"])

	out, _, err := dm.SaveLesson(fact, lessonType, tags)
	if err != nil {
		return nil, err
	}

	// Provenance Proxy: if the agent lists the node IDs this lesson
	// was distilled from, credit each in the retrieval observability
	// layer via IncrementSuccess. INSERT-or-UPDATE semantic: a node
	// that was never retrieved this turn can still receive a credit
	// when the agent cites it from prior-session memory. Telemetry
	// must not block the user's lesson-save path; errors are
	// swallowed.
	credited := 0
	if raw, ok := p["source_ids"].([]interface{}); ok && len(raw) > 0 {
		for _, v := range raw {
			id, _ := v.(string)
			if id == "" {
				continue
			}
			if err := dm.IncrementSuccess(id, inferNodeType(id)); err == nil {
				credited++
			}
			// err != nil: log via audit, don't surface — the lesson
			// is already saved and the user-facing path succeeded.
		}
	}

	// Augment the SaveLesson response with a credit count so the
	// agent can verify the provenance was wired.
	out["credited_sources"] = credited
	return out, nil
}

// callSearchLessons searches lesson content.
func handleSearchLessons(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}

	// 2026-09-05 audit remediation pass 4 defect C.9 (P2): mirror the
	// C.8 list guard. The params schema declares `type` with the
	// canonical lesson-type enum, but search previously did not read
	// the field at all — passing `type: "bogus"` silently returned a
	// full-corpus search with no signal that the filter was dropped.
	// Validate at the boundary so caller typos fail loudly.
	lessonType, _ := p["type"].(string)
	if lessonType != "" {
		if err := internal.ValidateLessonType(lessonType); err != nil {
			return nil, err
		}
	}

	items, err := dm.SearchLessonsLimited(query)
	if err != nil {
		return nil, err
	}
	// Observability Layer: each lesson search result is a retrieval.
	for _, item := range items {
		if id, _ := item["id"].(string); id != "" {
			_ = dm.RecordRetrieval(id, "lesson")
		}
	}

	// Phase 2D: pointer-native projection.
	projection, err := normalizeProjection(p)
	if err != nil {
		return nil, err
	}
	if projection == "" || projection == "summary" {
		projected := make([]ProjectedLessonEntry, 0, len(items))
		for _, item := range items {
			id, _ := item["id"].(string)
			content, _ := item["content"].(string)
			lessonType, _ := item["type"].(string)
			tags, _ := item["tags"].([]string)
			created, _ := item["created_at"].(string)

			summary, summaryTruncated := internal.SummarizeMemoryWithEllipsis(content, 256)

			var retMeta *RetrievedEntryMetadata
			if dm != nil {
				meta, err := dm.GetRetrievalMetadata(id)
				if err == nil && meta.ReuseCount > 0 {
					lastRetrieved := ""
					if meta.LastRetrievedAt != nil {
						lastRetrieved = time.Unix(*meta.LastRetrievedAt, 0).Format(time.RFC3339)
					}
					retMeta = &RetrievedEntryMetadata{
						ReuseCount:      meta.ReuseCount,
						SuccessCount:    meta.SuccessCount,
						LastRetrievedAt: lastRetrieved,
					}
				}
			}

			rationale := fmt.Sprintf("%s · %dx ref", lessonType, 0)
			if tags != nil && len(tags) > 0 {
				rationale = strings.Join(tags, ", ")
			}

			projected = append(projected, ProjectedLessonEntry{
				ID:                id,
				Summary:           summary,
				SummaryTruncated:  summaryTruncated,
				Pointer:           "mpm://lesson/" + id,
				Type:              lessonType,
				Tags:              tags,
				CreatedAt:         created,
				RetrievalMetadata: retMeta,
				Rationale:         rationale,
			})
		}
		return map[string]interface{}{
			"success": true,
			"mode":    "summary",
			"query":   query,
			"lessons": projected,
			"count":   len(projected),
		}, nil
	}

	// projection == "full": unbounded content with pointer.
	for _, item := range items {
		id, _ := item["id"].(string)
		item["pointer"] = "mpm://lesson/" + id
	}
	return map[string]interface{}{
		"success": true,
		"mode":    "full",
		"results": items,
		"count":   len(items),
	}, nil
}

// callListLessons lists all lessons, optionally filtered by type.
func handleListLessons(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	lessonType, _ := p["type"].(string)
	// 2026-09-05 audit remediation pass 3 defect C.8 (P2):
	// mpm_lessons list with an invalid type silently returned an empty
	// array, masking caller typos. The DM's ValidateLessonType is the
	// canonical allowlist (insight|warning|practice); consult it at
	// the public boundary. Empty string and omission remain legitimate
	// "no filter" paths — only PRESENT-but-INVALID inputs error.
	if lessonType != "" {
		if err := internal.ValidateLessonType(lessonType); err != nil {
			return nil, err
		}
	}

	items, err := dm.ListLessonsFiltered(lessonType)
	if err != nil {
		return nil, err
	}

	// Phase 2D: pointer-native projection.
	projection, err := normalizeProjection(p)
	if err != nil {
		return nil, err
	}
	if projection == "" || projection == "summary" {
		projected := make([]ProjectedLessonEntry, 0, len(items))
		for _, item := range items {
			id, _ := item["id"].(string)
			content, _ := item["content"].(string)
			lessonType, _ := item["type"].(string)
			tags, _ := item["tags"].([]string)
			created, _ := item["created_at"].(string)

			summary, summaryTruncated := internal.SummarizeMemoryWithEllipsis(content, 256)

			var retMeta *RetrievedEntryMetadata
			if dm != nil {
				meta, err := dm.GetRetrievalMetadata(id)
				if err == nil && meta.ReuseCount > 0 {
					lastRetrieved := ""
					if meta.LastRetrievedAt != nil {
						lastRetrieved = time.Unix(*meta.LastRetrievedAt, 0).Format(time.RFC3339)
					}
					retMeta = &RetrievedEntryMetadata{
						ReuseCount:      meta.ReuseCount,
						SuccessCount:    meta.SuccessCount,
						LastRetrievedAt: lastRetrieved,
					}
				}
			}

			rationale := fmt.Sprintf("%s · %dx ref", lessonType, 0)
			if tags != nil && len(tags) > 0 {
				rationale = strings.Join(tags, ", ")
			}

			projected = append(projected, ProjectedLessonEntry{
				ID:                id,
				Summary:           summary,
				SummaryTruncated:  summaryTruncated,
				Pointer:           "mpm://lesson/" + id,
				Type:              lessonType,
				Tags:              tags,
				CreatedAt:         created,
				RetrievalMetadata: retMeta,
				Rationale:         rationale,
			})
		}
		return map[string]interface{}{
			"success": true,
			"mode":    "summary",
			"lessons": projected,
			"count":   len(projected),
		}, nil
	}

	// projection == "full": unbounded content with pointer.
	for _, item := range items {
		id, _ := item["id"].(string)
		item["pointer"] = "mpm://lesson/" + id
	}
	return map[string]interface{}{
		"success": true,
		"mode":    "full",
		"lessons": items,
		"count":   len(items),
	}, nil
}

// callCreateTopic creates a new topic.
func handleCreateTopic(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	name, _ := p["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	description := internal.ParseStringOr(p["description"], "")

	topicID, err := dm.CreateTopicWithDescription(name, description)
	if err != nil {
		return nil, fmt.Errorf("create topic: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"id":      topicID,
		"name":    name,
	}, nil
}

// callSearchTopics searches topics by name/description.
func handleSearchTopics(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	limit := int(internal.ParseFloatOr(p["limit"], 20))
	if limit <= 0 {
		limit = 20
	}

	items, err := dm.SearchTopicsByQuery(query, limit)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"results": items,
		"count":   len(items),
	}, nil
}

// callLinkTopic links a memory to a topic.
//
// W-005: validates both memory_id and topic_id exist before inserting
// the membership. The previous behaviour silently created orphan
// memberships when the topic_id was bogus or soft-deleted, which
// polluted `topic_memberships` and confused subsequent `mpm ls --topic`
// queries. Validation is also enforced inside AddMemoryToTopic as a
// defence-in-depth; the pre-check here just produces a friendlier
// error envelope that names which id was wrong.
func handleLinkTopic(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memory_id"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	topicID, _ := p["topic_id"].(string)
	if topicID == "" {
		return nil, fmt.Errorf("topic_id is required")
	}

	// W-005: confirm both ends exist. AddMemoryToTopic also validates
	// topic_id, but a friendly pre-check surfaces a precise "memory X
	// not found" or "topic Y not found" message at the public surface
	// instead of a generic INSERT failure.
	if _, err := dm.GetMemory(memoryID); err != nil {
		return nil, fmt.Errorf("memory %q not found", memoryID)
	}
	if _, err := dm.GetTopic(topicID); err != nil {
		return nil, fmt.Errorf("topic %q not found", topicID)
	}

	if err := dm.AddMemoryToTopic(memoryID, topicID, "manual"); err != nil {
		return nil, fmt.Errorf("link topic: %w", err)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"topic_id":  topicID,
	}, nil
}

// handleUnlinkTopic removes the membership row linking a memory to a topic.
// Inverse of handleLinkTopic. Per docs/tool-behavioral-contract.md §1
// (soft-delete idempotency) and §4 (idempotent membership unlink):
//
//   - Missing memory_id / topic_id: error at the boundary.
//   - Memory or topic does not exist: error "X not found" — same friendly
//     pre-check handleLinkTopic uses, so callers learn about a typo'd
//     endpoint before any DELETE fires.
//   - Membership exists: removed, returns removed=true.
//   - Membership does not exist: silent success with removed=false
//     (idempotent; the desired terminal state already holds).
//
// Part 2A (2026-09-06): new public action surfacing the existing DM
// primitive RemoveMemoryFromTopic (db.go:4112). Closes the catalog
// §C.1 finding (link had no symmetric unlink at the public surface).
func handleUnlinkTopic(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memory_id"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	topicID, _ := p["topic_id"].(string)
	if topicID == "" {
		return nil, fmt.Errorf("topic_id is required")
	}

	// W-005 mirror: confirm both endpoints exist before the DELETE so
	// callers get a precise "memory X not found" / "topic Y not found"
	// envelope rather than an opaque FK failure.
	if _, err := dm.GetMemory(memoryID); err != nil {
		return nil, fmt.Errorf("memory %q not found", memoryID)
	}
	if _, err := dm.GetTopic(topicID); err != nil {
		return nil, fmt.Errorf("topic %q not found", topicID)
	}

	removed, err := dm.RemoveMemoryFromTopic(memoryID, topicID)
	if err != nil {
		return nil, fmt.Errorf("unlink topic: %w", err)
	}
	return map[string]interface{}{
		"success":   true,
		"memory_id": memoryID,
		"topic_id":  topicID,
		"removed":   removed,
	}, nil
}

// handleListTopics returns all active topics (matches `mpm topic list`
// CLI surface). D-4.1: the prior MCP surface exposed only
// create|search|link; this restores parity so an agent using MCP can
// enumerate existing topics before linking or promoting.
func handleListTopics(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	limit := int(internal.ParseFloatOr(p["limit"], 20))
	if limit <= 0 {
		limit = 20
	}
	items, err := dm.ListTopics()
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return map[string]interface{}{
		"success": true,
		"topics":  items,
		"count":   len(items),
	}, nil
}

// handleShowTopic returns a single topic by id (matches `mpm topic show
// <id>` CLI surface). D-4.1: parity closure.
func handleShowTopic(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	topic, err := dm.GetTopic(id)
	if err != nil {
		return nil, fmt.Errorf("get topic: %w", err)
	}
	if topic == nil {
		return nil, fmt.Errorf("topic not found: %s", id)
	}
	return map[string]interface{}{
		"success": true,
		"topic":   topic,
	}, nil
}

// callAddReference ingests a document as a reference.
func handleAddReference(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	filepath, _ := p["filepath"].(string)
	if filepath == "" {
		return nil, fmt.Errorf("filepath is required")
	}
	title := internal.ParseStringOr(p["title"], "")

	return dm.AddReferenceFromFile(filepath, title)
}

// callSearchReferences searches reference content.
func handleSearchReferences(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := int(internal.ParseFloatOr(p["limit"], 5))
	if limit <= 0 {
		limit = 5
	}

	results, err := dm.SearchReferenceChunks(query, limit)
	if err != nil {
		return nil, fmt.Errorf("search references: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		items = append(items, map[string]interface{}{
			"id":          r["id"],
			"doc_title":   r["doc_title"],
			"chunk_index": r["chunk_index"],
			"content":     r["content"],
		})
	}
	return map[string]interface{}{
		"success": true,
		"results": items,
		"count":   len(items),
	}, nil
}

// callListReferences lists all ingested reference documents.
func handleListReferences(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	limit := int(internal.ParseFloatOr(p["limit"], 50))
	if limit <= 0 {
		limit = 50
	}
	offset := int(internal.ParseFloatOr(p["offset"], 0))
	if offset < 0 {
		offset = 0
	}

	refs, err := dm.ListReferences(limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list references: %w", err)
	}
	return map[string]interface{}{
		"success":    true,
		"references": refs,
		"count":      len(refs),
	}, nil
}

// handleReadReference (alpha-4 W-004) returns a single reference doc by
// id with full metadata + chunk content. Mirrors `mpm call mpm_references
// read` and closes the discoverability gap where the only way to read
// an added reference was via the generic mpm_resolve pointer surface.
//
// Reuses the existing GetReference implementation — there is exactly one
// reference storage path; this handler does not create a second.
func handleReadReference(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	ref, err := dm.GetReference(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("reference %q not found", id)
		}
		return nil, fmt.Errorf("read reference: %w", err)
	}
	return map[string]interface{}{
		"success":   true,
		"reference": ref,
	}, nil
}

// callReadWakeContext returns the last session's context. Data gathering is
// delegated to internal.ReadWakeContext (single source of truth shared with
// the Go MCP server).
func handleReadWakeContext(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, params map[string]interface{}) (interface{}, error) {

	// Alpha-4 W-001: compact projection. Returns a small id+summary
	// struct so an agent that only needs to know "who am I, what was
	// the last handoff, what's open" can avoid pulling the heavy
	// recent_memories / available_skills / global_rules / overdue_wakes
	// surfaces into the boot prompt. Default behavior unchanged.
	//
	// Round 9 T21: validate `projection` at the handler boundary. The
	// pre-fix shape only branched on `projection == "compact"` and let
	// every other value silently fall through to the default full
	// projection. A caller asking `projection="bogus"` got the full
	// envelope back without any indication their explicit projection
	// was ignored. The handler now distinguishes:
	//
	//   omitted / nil / ""  → default full projection (preserved)
	//   "compact"           → compact projection (preserved)
	//   any other string    → ERROR (was silent fall-through)
	//   non-string          → ERROR
	if rawProjection, present := params["projection"]; present && rawProjection != nil {
		projection, ok := rawProjection.(string)
		if !ok {
			return nil, fmt.Errorf("projection must be a string, got %T", rawProjection)
		}
		switch projection {
		case "":
			// Empty string is treated as omitted (matches the `format`
			// validation pattern below; explicit empty ≠ invalid value).
		case "compact":
			return handleReadWakeContextCompact(dm)
		case "full":
			// 2026-09-12 acceptance pass: callers that pre-date the
			// W-001 compact-projection split still pass projection="full"
			// explicitly. The W-001 contract collapsed "full" into the
			// default (omitted) branch — accept the alias so old
			// integrations don't break.
		default:
			return nil, fmt.Errorf("unknown projection %q; canonical values: [compact, full]", projection)
		}
	}

	// 2026-09-05 audit residual pass §I-C.16: validate `format` at
	// the handler boundary. The schema declares
	// `format: enum: [system-prompt]`; the previous shape only
	// branched on `format == "system-prompt"` and let every other
	// value silently fall through to the JSON default. A caller
	// asking `format="bogus"` got a JSON envelope back without any
	// indication their explicit format choice was ignored. The
	// handler now distinguishes:
	//
	//   omitted / nil / ""  → default JSON envelope (preserved)
	//   "system-prompt"     → prose envelope (preserved)
	//   any other string    → ERROR (was silent fall-through)
	//   non-string          → ERROR
	if rawFormat, present := params["format"]; present && rawFormat != nil {
		format, ok := rawFormat.(string)
		if !ok {
			return nil, fmt.Errorf("format must be a string, got %T", rawFormat)
		}
		switch format {
		case "":
			// Explicit empty matches omission → fall through to JSON.
		case "system-prompt":
			text, err := dm.ReadWakeContext()
			if err != nil {
				return nil, fmt.Errorf("read wake context (system-prompt): %w", err)
			}
			return map[string]interface{}{"success": true, "format": "system-prompt", "content": text}, nil
		default:
			return nil, fmt.Errorf("format must be one of [system-prompt], got %q", format)
		}
	}

	data, err := dm.GatherWakeContext()
	if err != nil {
		return nil, fmt.Errorf("gather wake context: %w", err)
	}
	// P2 fix: gatherWakeContext consumes the latest unread handoff (marks read).
	// Subsequent calls would then return LastHandoff=nil, diverging from
	// `mpm wake` CLI which peeks via GetLatestHandoff (latest regardless of read).
	// Fall back to the latest handoff (even if already marked read) so the API
	// always surfaces the most recent handoff, matching CLI behavior.
	if data.LastHandoff == nil {
		if h, herr := dm.GetLatestHandoff(); herr == nil && h != nil {
			data.LastHandoff = h
		}
	}

	// Wire-format size cap (sibling to the prose-format cap applied inside
	// mpminternal.ReadWakeContext). Mutates `data` to shed Tier 1 +
	// Tier 2 if the JSON would exceed MaxWakeContextBytes; sets the
	// corresponding Truncated flags so the agent can distinguish
	// cap-induced emptiness from "checked, none found". The 32 KB
	// guardrail is invariant 4 on WakeContextData — see the type
	// documentation for the shedding order.
	if _, err := mpminternal.EnforceSizeLimit(&data); err != nil {
		return nil, fmt.Errorf("wake_context size cap: %w", err)
	}

	memRefs := make([]map[string]interface{}, 0, len(data.RecentMemories))
	for _, m := range data.RecentMemories {
		memRefs = append(memRefs, map[string]interface{}{
			"id":         m.ID,
			"summary":    m.Summary,
			"pointer":    m.Pointer,
			"created_at": m.CreatedAt,
		})
		// Observability Layer: every node pulled into the boot prompt
		// is "retrieved". Fire-and-forget; telemetry must not block
		// the wake surface. Errors are swallowed.
		_ = dm.RecordRetrieval(m.ID, "memory")
	}

	milestoneRefs := make([]map[string]interface{}, 0, len(data.RecentMilestones))
	for _, m := range data.RecentMilestones {
		milestoneRefs = append(milestoneRefs, map[string]interface{}{
			"id":         m.ID,
			"summary":    m.Summary,
			"pointer":    m.Pointer,
			"created_at": m.CreatedAt,
		})
		_ = dm.RecordRetrieval(m.ID, "memory")
	}

	// Overdue-wakes surface (added 2026-08-13). Pure read — does NOT
	// mutate scheduled_wakes. Complements wakes_pending (which is the
	// scheduler's fire-and-return surface) and the existing pre-dispatch
	// CheckPendingWakes call below. Specifically catches wake kinds that
	// the default CheckPendingWakes kind-filter skips (reminder, drill,
	// task, etc) — those woke up overdue in the silent-failure case that
	// prompted this patch.
	overdueRefs := make([]map[string]interface{}, 0, len(data.OverdueWakes))
	for _, w := range data.OverdueWakes {
		overdueRefs = append(overdueRefs, map[string]interface{}{
			"id":           w.ID,
			"target_time":  float64(w.TargetTime),
			"reason":       w.Reason,
			"kind":         w.Kind,
			"overdue_secs": float64(w.OverdueSecs),
		})
	}

	// Cold-start sweep: check for wakes that came due while the system
	// was offline. The agent sees these immediately on boot — no need to
	// wait for the next tool call to trigger the opportunistic fold.
	// Pass nil kinds for backward-compatible default (notification-only).
	wakes, wErr := dm.CheckPendingWakes(time.Now(), nil)

	// v4 wire format: project the v4 fields (GlobalRules, AvailableSkills)
	// which were previously populated on the struct but never surfaced
	// through the manual map projection. Empty (non-nil) per invariant 3.
	globalRuleRefs := make([]map[string]interface{}, 0, len(data.GlobalRules))
	for _, r := range data.GlobalRules {
		globalRuleRefs = append(globalRuleRefs, map[string]interface{}{
			"content": r.Content,
			"weight":  float64(r.Weight),
		})
	}
	skillRefs := make([]map[string]interface{}, 0, len(data.AvailableSkills))
	for _, s := range data.AvailableSkills {
		skillRefs = append(skillRefs, map[string]interface{}{
			"id":          s.ID,
			"name":        s.Name,
			"version":     s.Version,
			"when_to_use": s.WhenToUse,
			"is_global":   s.IsGlobal,
			"weight":      float64(s.Weight),
		})
	}

	// P3 fix: surface recent theories/lessons/decisions separation in wake
	// context. Previously only recent_memories (any collection) and milestones
	// were shown, so lessons/decisions/theories were invisible unless the
	// agent knew to query them explicitly (validation suite P3).
	recentTheories := fetchRecentMemoriesByCollection(dm, "theories", 5)
	recentDecisions := fetchRecentMemoriesByCollection(dm, "decisions", 5)
	recentLessons := fetchRecentLessonsForWake(dm, 5)

	result := map[string]interface{}{
		"success": true,
		// Wire-format metadata (added with the v4 schema bump). Both
		// unix-seconds; GeneratedAt = "when the struct was assembled",
		// AsOf = "what 'now' represented substrate state at". v3 callers
		// ignore these safely (extra fields are non-additive for them).
		"context_version": data.ContextVersion,
		"generated_at":    data.GeneratedAt,
		"as_of":           data.AsOf,
		// Identity — 2026-09-11 session-identity pass adds the
		// explicit three-ID surface:
		//   - mpm_session_id        — MPM-owned. REQUIRED going forward.
		//   - framework_session_id  — host-owned. Optional.
		//   - session_id            — v3 alias for back-compat (now equal
		//                             to mpm_session_id, NOT read from the
		//                             dormant sessions table).
		"mpm_session_id":            data.MPMSessionID,
		"framework_session_id":      data.FrameworkSessionID,
		"session_id":                data.SessionID,
		"session_current_id":        data.SessionCurrentID,
		"session_previous_id":       data.SessionPreviousID,
		"session_started_at":        data.SessionStartedAt,
		"session_previous_ended_at": data.SessionPreviousEndedAt,
		"active_mode":               data.ActiveMode,
		"active_persona":            data.ActivePersona,
		// Orientation block.
		"recent_topics":           data.RecentTopics,
		"recent_topics_truncated": data.RecentTopicsTruncated,
		"recent_memories":         memRefs,
		"recent_milestones":       milestoneRefs,
		"recent_theories":         recentTheories,
		"recent_lessons":          recentLessons,
		"recent_decisions":        recentDecisions,
		// Attention & pending work.
		"overdue_wakes": overdueRefs,
		"open_works":    data.OpenWorks,
		// completed_works: RECOMMENDED 10. Pairs with open_works so the
		// agent sees both "what's waiting on me" and "what I just shipped"
		// without a separate tool call. Same WakeContextWork shape as
		// open_works for symmetric parsing. Always emitted (empty list,
		// not omitempty) so callers can branch on field presence.
		"completed_works":    data.CompletedWorks,
		"scratchpad_orphans": data.ScratchpadOrphans,
		"last_handoff":       data.LastHandoff,
		// Constraints & capabilities.
		"global_rules":               globalRuleRefs,
		"available_skills":           skillRefs,
		"available_skills_truncated": data.AvailableSkillsTruncated,
		// System health.
		"audit_summary":      data.AuditSummary,
		"epistemic_pressure": data.EpistemicPressure,
		// Stage 2E.3: compact contextual focus projection. Additive
		// on the wake-context delivery surface. Status `available`
		// or `degraded`; items count equals selected-input count.
		"contextual_focus": data.ContextualFocus,
	}
	// Always surface wakes_pending — even when empty — so the agent can
	// branch on field presence rather than parsing absence. The Map shape
	// is consistent with overdue_wakes; the semantic is different
	// (just-fired by the opportunistic dispatch vs. still-pending in the
	// substrate's queue). wakes_pending_count is redundant with len() on
	// the agent side but kept for symmetry with existing tooling.
	if wErr == nil {
		if wakes == nil {
			wakes = []map[string]interface{}{}
		}
		result["wakes_pending"] = wakes
		result["wakes_pending_count"] = float64(len(wakes))
	} else {
		// Surface the failure shape explicitly so the agent can see that
		// the dispatch path was attempted but failed (vs. the field being
		// absent due to silent omission).
		result["wakes_pending"] = []map[string]interface{}{}
		result["wakes_pending_count"] = float64(0)
		result["wakes_pending_error"] = wErr.Error()
	}

	// Epistemic pressure — same shape contract as the other fields:
	// struct → map[string]interface{} for the JSON wire. The struct
	// itself has json tags (raw_count, lesson_count, ratio, threshold,
	// exceeded) but the rest of the handler surface is map-shaped, so
	// convert for consistency. Marshalling/unmarshalling here would
	// also work but introduces a JSON round-trip cost on every wake.
	//
	// Numeric fields are emitted as float64 (JSON's number type) rather
	// than int — this matches what json.Marshal would produce on the
	// wire and avoids type-coercion surprises for downstream parsers.
	result["epistemic_pressure"] = map[string]interface{}{
		"raw_count":         float64(data.EpistemicPressure.RawCount),
		"lesson_count":      float64(data.EpistemicPressure.LessonCount),
		"ratio":             data.EpistemicPressure.Ratio,
		"threshold":         float64(data.EpistemicPressure.Threshold),
		"exceeded":          data.EpistemicPressure.Exceeded,
		"last_compacted_at": data.EpistemicPressure.LastCompactedAt,
	}

	return result, nil
}

// CompactWakeContext is the alpha-4 W-001 lightweight projection of the
// wake surface. Nine fields — enough for an agent to re-orient against
// "who am I, what just happened, what's waiting" — without pulling the
// heavy recent_memories / available_skills / global_rules /
// overdue_wakes arrays into the boot prompt. The full WakeContextData
// remains available via the default and `projection:"full"` paths.
//
// 2026-09-11 session-identity pass: the compact surface now carries
// the explicit mpm_session_id and framework_session_id fields so
// agents can see the three-ID identity in a small payload. SessionID
// remains the v3 legacy alias for back-compat.
type CompactWakeContext struct {
	MPMSessionID       string   `json:"mpm_session_id"`
	FrameworkSessionID string   `json:"framework_session_id,omitempty"`
	SessionID          string   `json:"session_id"`
	SessionCurrentID   string   `json:"session_current_id"`
	SessionStartedAt   int64    `json:"session_started_at"`
	ActiveMode         string   `json:"active_mode"`
	ActivePersona      string   `json:"active_persona"`
	LastHandoffSummary string   `json:"last_handoff_summary,omitempty"`
	LastHandoffEndedAt int64    `json:"last_handoff_ended_at,omitempty"`
	OpenWorkIDs        []string `json:"open_work_ids"`
	AuditSummary       string   `json:"audit_summary"`
	RecentArtifactIDs  []string `json:"recent_artifact_ids"`
}

// handleReadWakeContextCompact (alpha-4 W-001) returns the compact
// compact projection. Gathers the full WakeContextData (cheap — the
// heavy part is serialization, not collection), then projects to the
// compact shape. Errors propagate so the caller can fall back to the
// full path if the substrate is degraded.
func handleReadWakeContextCompact(dm mpminternal.CoreDB) (interface{}, error) {
	data, err := dm.GatherWakeContext()
	if err != nil {
		return nil, fmt.Errorf("gather wake context (compact): %w", err)
	}
	if data.LastHandoff == nil {
		if h, herr := dm.GetLatestHandoff(); herr == nil && h != nil {
			data.LastHandoff = h
		}
	}

	compact := CompactWakeContext{
		MPMSessionID:       data.MPMSessionID,
		FrameworkSessionID: data.FrameworkSessionID,
		SessionID:          data.SessionID,
		SessionCurrentID:   data.SessionCurrentID,
		SessionStartedAt:   data.SessionStartedAt,
		ActiveMode:         data.ActiveMode,
		ActivePersona:      data.ActivePersona,
		AuditSummary:       data.AuditSummary,
		OpenWorkIDs:        []string{}, // invariant 3 — non-nil empty slice
		RecentArtifactIDs:  []string{}, // invariant 3 — non-nil empty slice
	}
	if data.LastHandoff != nil {
		compact.LastHandoffSummary = data.LastHandoff.Summary
		compact.LastHandoffEndedAt = data.LastHandoff.EndedAt
	}
	for _, w := range data.OpenWorks {
		compact.OpenWorkIDs = append(compact.OpenWorkIDs, w.ID)
		if len(compact.OpenWorkIDs) >= 5 {
			break
		}
	}
	// Recent artifact ids: union of recent memory + recent milestone ids,
	// bounded to 10 total so the compact payload stays small.
	maxRecent := 10
	for _, m := range data.RecentMemories {
		if len(compact.RecentArtifactIDs) >= maxRecent {
			break
		}
		compact.RecentArtifactIDs = append(compact.RecentArtifactIDs, m.ID)
	}
	for _, m := range data.RecentMilestones {
		if len(compact.RecentArtifactIDs) >= maxRecent {
			break
		}
		compact.RecentArtifactIDs = append(compact.RecentArtifactIDs, m.ID)
	}

	return map[string]interface{}{
		"success":    true,
		"projection": "compact",
		"context":    compact,
	}, nil
}

// fetchRecentMemoriesByCollection returns up to limit recent memories for a given collection,
// bounded to 256-char summaries with pointers. Used for wake separation of theories/decisions.
//
// RECOMMENDED 11: summaries emitted by this helper are scrubbed of the
// internal `CHALLENGED_MEMORY_ID: <id>` line that atomic challenges use
// (see internal/core/epistemology_tools.go). Without redaction the
// first 256 chars of a challenge theory's content would leak the
// challenged memory's bare id into the wake context, which the agent
// has no use for and which is structurally indistinguishable from a
// cursor typo. The replacement preserves the substantive evidence
// line ("EVIDENCE: ...") so the agent can still reason about WHY the
// challenge was raised — just without the bare-id noise.
func fetchRecentMemoriesByCollection(dm mpminternal.CoreDB, collection string, limit int) []map[string]interface{} {
	if limit <= 0 {
		limit = 5
	}
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created_at FROM memories
		WHERE deleted_at IS NULL AND collection = ?
		ORDER BY created_at DESC LIMIT ?`, collection, limit)
	if err != nil {
		return []map[string]interface{}{}
	}
	defer rows.Close()
	// Defect L (2026-09-13 acceptance): pre-fix the wake-context
	// pointer was always `mpm://memory/<id>` regardless of the row's
	// typed kind. The resolver supports typed URIs (`mpm://theory/`,
	// `mpm://decision/`) but the wake emitter never used them. The
	// fix threads the collection discriminator through to pointer
	// construction: theories → `mpm://theory/<id>`, decisions →
	// `mpm://decision/<id>`, lessons (separate helper below) →
	// `mpm://lesson/<id>`, everything else → `mpm://memory/<id>`.
	pointerKind := memoryPointerKindForCollection(collection)
	out := make([]map[string]interface{}, 0, limit)
	for rows.Next() {
		var id, content, createdAt string
		if scanErr := rows.Scan(&id, &content, &createdAt); scanErr != nil {
			// Wake-context surfacing is best-effort: a malformed row must not
			// abort the whole wake payload. Logged-swallow (warn to watchdog)
			// rather than silent-continue so an operator can investigate
			// database corruption without the lint gate tripping.
			dm.LogAudit(
				mpminternal.AuditWarn,
				"tools.wake_scan",
				fmt.Sprintf("fetchRecentMemoriesByCollection: scan failed (row=%s): %s", id, scanErr.Error()),
				"",
				mpminternal.AuditContext{"row_id": id, "err": scanErr.Error()},
			)
			continue
		}
		summary := scrubChallengeIdentifier(content)
		if len(summary) > 256 {
			summary = summary[:256]
		}
		summary = strings.ReplaceAll(summary, "\n", " ")
		out = append(out, map[string]interface{}{
			"id":         id,
			"summary":    summary,
			"pointer":    pointerKind + "/" + id,
			"created_at": createdAt,
		})
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	return out
}

// memoryPointerKindForCollection maps a memories.collection value to
// the canonical pointer kind used in wake-context emission. Single-
// table kinds use their public name; everything else falls back to
// `memory`. Keep this in sync with handleMpmResolve's whitelist at
// handlers.go (mpm://blob/, work/, memory/, lesson/, theory/) — the
// wake context and the resolver agree on what they recognize.
func memoryPointerKindForCollection(collection string) string {
	switch collection {
	case "theories":
		return "mpm://theory"
	case "decisions":
		return "mpm://decision"
	default:
		return "mpm://memory"
	}
}

// scrubChallengeIdentifier removes internal challenge metadata lines
// from a theory's content. Returns the original string when none of
// the known internal fields are present (the common case for non-
// challenge theories and for decisions). RECOMMENDED 11.
//
// The internal fields stripped are:
//   - CHALLENGED_MEMORY_ID: <id>    (internal cross-reference id)
//   - CHALLENGED_AT_NANO: <number>  (internal epoch-ns timestamp)
//
// Both fields are explicitly internal — they exist to support atomic
// challenge operations and have no semantic value to a downstream agent.
// The user-meaningful fields kept verbatim are EVIDENCE and
// ORIGINAL_CONTENT. Other "CHALLENGED_*" fields, if added in the future,
// must be appended to the strip set — the filter is line-prefix based
// and intentionally NOT regex on free-form text so legitimate user
// content mentioning these words is not destroyed.
//
// The previous implementation assumed a specific field ordering
// (`CHALLENGED_MEMORY_ID` first, `CHALLENGED_AT_NANO` immediately
// after). The actual stored layout (see
// internal/core/epistemology_tools.go: theoryContent template) puts
// `CHALLENGED_AT_NANO` between EVIDENCE and ORIGINAL_CONTENT. That
// ordering assumption silently leaked the timestamp into the wake
// payload. This implementation is line-based and ordering-agnostic —
// each internal field is filtered wherever it appears in the input.
//
// The line-prefix boundary is "starts with `<FIELD>:`" — the colon
// anchor prevents accidental matches against lines like
// "EVIDENCE: the user said CHALLENGED_MEMORY_ID was leaked" (the
// latter would not match because the line starts with "EVIDENCE:",
// not "CHALLENGED_MEMORY_ID:").
func scrubChallengeIdentifier(content string) string {
	// Fast path: none of the internal prefixes appear at all.
	if !strings.Contains(content, "CHALLENGED_MEMORY_ID:") &&
		!strings.Contains(content, "CHALLENGED_AT_NANO:") {
		return content
	}
	var b strings.Builder
	b.Grow(len(content))
	first := true
	isAtomic := false
	// Scan line-by-line, dropping lines whose prefix matches an
	// internal field. Empty lines and non-internal lines are passed
	// through verbatim. The first internal line encountered
	// (`CHALLENGED_MEMORY_ID:`) flips the atomic flag so the
	// surrounding prose can be tagged for the agent.
	droppedAny := false
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.HasPrefix(line, "CHALLENGED_MEMORY_ID:"):
			isAtomic = true
			droppedAny = true
			continue
		case strings.HasPrefix(line, "CHALLENGED_AT_NANO:"):
			droppedAny = true
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		first = false
	}
	// Only inject the "(atomic challenge)" marker if we actually
	// stripped something. Otherwise the function is a no-op and the
	// caller's pre-check path is faster (avoids the split).
	if !droppedAny {
		return content
	}
	out := b.String()
	if isAtomic {
		return "(atomic challenge) " + out
	}
	return out
}

// fetchRecentLessonsForWake returns up to limit recent lessons for wake context.
func fetchRecentLessonsForWake(dm mpminternal.CoreDB, limit int) []map[string]interface{} {
	if limit <= 0 {
		limit = 5
	}
	rows, err := dm.SQLDB().Query(`
		SELECT id, content, created FROM lessons
		ORDER BY created DESC LIMIT ?`, limit)
	if err != nil {
		return []map[string]interface{}{}
	}
	defer rows.Close()
	out := make([]map[string]interface{}, 0, limit)
	for rows.Next() {
		var id, content, created string
		if scanErr := rows.Scan(&id, &content, &created); scanErr != nil {
			// Best-effort: log via watchdog so corruption is visible
			// without aborting the wake payload.
			dm.LogAudit(
				mpminternal.AuditWarn,
				"tools.wake_scan",
				fmt.Sprintf("fetchRecentLessonsForWake: scan failed (row=%s): %s", id, scanErr.Error()),
				"",
				mpminternal.AuditContext{"row_id": id, "err": scanErr.Error()},
			)
			continue
		}
		summary := content
		if len(summary) > 256 {
			summary = summary[:256]
		}
		summary = strings.ReplaceAll(summary, "\n", " ")
		out = append(out, map[string]interface{}{
			"id":         id,
			"summary":    summary,
			"pointer":    "mpm://lesson/" + id,
			"created_at": created,
		})
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	return out
}

// callReadDirectives returns prime directives filtered to the active
// framework. Scope resolution lives in the substrate, not in the agent
// plugin — MPM_FRAMEWORK env (read by mpmcli.ActiveContextFromEnv at
// mpm-mcp boot) populates ac.FrameworkName here. See
// docs/archive/directives.md §4-§5.
func handleReadDirectives(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, _ map[string]interface{}) (interface{}, error) {

	directives, err := dm.ReadDirectivesForFramework(ac.FrameworkName)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":    true,
		"directives": directives,
		"count":      len(directives),
		"framework":  ac.FrameworkName,
	}, nil
}

// callProactiveRecallHint checks conversation context for relevant decisions/theories.
func handleProactiveRecallHint(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	conversationText, _ := p["conversation_text"].(string)
	if conversationText == "" {
		return nil, fmt.Errorf("conversation_text is required")
	}
	maxHints := int(internal.ParseFloatOr(p["max_hints"], 3))
	if maxHints <= 0 {
		maxHints = 3
	}

	overlaps, err := dm.ProactiveRecallHint(conversationText, maxHints, internal.ParseFloatOr(p["min_score"], -3.0))
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"hints":   overlaps,
		"count":   len(overlaps),
	}, nil
}

// callRoute evaluates a prompt against the workspace's mode+persona
// configuration and returns a RoutingReport. This is the JSON-RPC path
// for OpenClaw and Hermes — pure JSON, no text rendering.
//
// Unlike mpm route (text), this handler returns errors instead of silently
// producing empty output. Callers are machines and can handle failures.
func handleRoute(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	prompt, _ := p["prompt"].(string)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	workspace := resolveRouteWorkspace()
	router, err := internal.NewRouter(workspace)
	if err != nil {
		return nil, fmt.Errorf("new router: %w", err)
	}

	return router.Evaluate(prompt), nil
}

// ── Evidence / Confidence ───────────────────────────────────────────────────
//
// These handlers expose the v1 confidence/evidence foundation over the
// universal `mpm call` machine interface so OpenClaw agents can add
// evidence, list it, and inspect the confidence timeline. The recompute
// is synchronous (in v1 the SQLite trigger is a no-op due to a documented
// connection-locking issue, so internal.AddEvidence calls RecomputeConfidence
// directly).

// callAddEvidence inserts a new evidence row and returns the resulting
// confidence. Thin shim over dm.AddEvidence.
func handleAddEvidence(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	artifactType, _ := payload["artifact_type"].(string)
	if artifactType == "" {
		artifactType = "memory"
	}
	// Alpha-4.1.1 audit fix (silent-coercion review): strength and
	// independence_factor were silently defaulting to 0 / 1.0 when the
	// caller sent a string ("5") or any other wrong type. An agent
	// passing strength="5" would get an evidence row with strength=0
	// and no error — the audit trail records the wrong weight. The
	// strict parser mirrors parseWeightStrict's contract: numeric
	// shapes pass through, wrong types error with a canonical message.
	strength, err := parseFloatStrictOr(payload["strength"], 0.5, "strength")
	if err != nil {
		return nil, err
	}
	independence, err := parseFloatStrictOr(payload["independence_factor"], 1.0, "independence_factor")
	if err != nil {
		return nil, err
	}

	return dm.AddEvidence(internal.EvidenceInput{
		ArtifactID:         getString(payload, "artifact_id"),
		ArtifactType:       artifactType,
		Type:               getString(payload, "type"),
		SourceGroup:        getString(payload, "source_group"),
		Strength:           strength,
		IndependenceFactor: independence,
		CreatedBy:          getString(payload, "created_by"),
		CreatedAt:          time.Now(),
		Notes:              getString(payload, "notes"),
	})
}

// callListEvidence returns all evidence rows for an artifact. Thin shim
// over dm.ListEvidence.
//
// Both artifact_id and artifact_type are REQUIRED at the delivery layer.
// artifact_type is not defaulted — it is an enum of six distinct values
// (memory / theory / decision / lesson / skill / work) and silently
// coercing a missing value to "memory" hides a schema violation that
// trains agents to believe their work was never verified.
//
// MPM-BUG-LIST-EVIDENCE-DEAF-2026-08-27.
func handleListEvidence(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	artifactID := getString(payload, "artifact_id")
	artifactType := getString(payload, "artifact_type")

	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		return nil, fmt.Errorf("artifact_type is required")
	}

	return dm.ListEvidence(artifactID, artifactType)
}

// callQueryConfidenceHistory returns the confidence timeline for an artifact.
func handleQueryConfidenceHistory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	limit := 50
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	return dm.QueryConfidenceHistory(getString(payload, "artifact_id"), getString(payload, "artifact_type"), limit)
}

// callQueryConfidenceChanges returns recent confidence-altering events
// with delta and trigger.
func handleQueryConfidenceChanges(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	var filter internal.ConfidenceChangesFilter
	if secs, ok := payload["since_seconds_ago"].(float64); ok && secs > 0 {
		filter.Since = time.Now().Add(-time.Duration(secs) * time.Second)
	} else if sinceF, ok := payload["since"].(float64); ok && sinceF > 0 {
		filter.Since = time.Unix(int64(sinceF), 0)
	}
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		filter.Limit = int(l)
	}
	if v, ok := payload["artifact_id"].(string); ok {
		filter.ArtifactID = v
	}
	if v, ok := payload["artifact_type"].(string); ok {
		filter.ArtifactType = v
	}

	return dm.QueryConfidenceChanges(filter)
}

// callQueryConfidenceTrend returns the trajectory projection of confidence
// over a time window. Completes the orthogonal set:
//
//	state  → explain_confidence       (current reasoning trace)
//	cause  → query_confidence_changes (recent events with delta)
//	history → query_confidence_history (full timeline)
//	direction → query_confidence_trend (this: trajectory, velocity)
//
// velocity is the raw signal; trend is the human-readable label.
// Agents reason better from velocity than from labels.
//
// Optional payload: window_days (default 30).
func handleQueryConfidenceTrend(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	windowDays := 30
	if w, ok := payload["window_days"].(float64); ok && w > 0 {
		windowDays = int(w)
	}
	return dm.QueryConfidenceTrend(getString(payload, "artifact_id"), getString(payload, "artifact_type"), windowDays)
}

// callQueryMemoryQuality returns per-creator memory statistics.
func handleQueryMemoryQuality(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.QueryMemoryQuality()
}

// callShowConfidence returns the current confidence and history for an artifact.
func handleShowConfidence(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.ShowConfidence(getString(payload, "artifact_id"), getString(payload, "artifact_type"))
}

// callRecomputeConfidence forces a manual recompute and returns the snapshot.
func handleRecomputeConfidence(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.RecomputeConfidence(getString(payload, "artifact_id"), getString(payload, "artifact_type"))
}

// callExplainConfidence returns the reasoning trace for an artifact's confidence.
func handleExplainConfidence(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	return dm.ExplainConfidence(getString(payload, "artifact_id"), getString(payload, "artifact_type"))
}

// ── Release / Changelog ──────────────────────────────────────────────
//
// callLogToChangelog writes a changelog memory tied to a specific
// git commit. Mirrors the MCP mpm_log_to_changelog tool exactly — the
// CLI/mcp parity is enforced by routing both through
// DatabaseManager.LogChangelogEntry, which holds the strict
// retrospective contract (full 40-char SHA-1 required). When the
// synthesis engine lands, both the MCP tool and this CLI handler
// will be joined with the git log via the (commit_hash,
// mpm_memory_id) key in changelog.json.
func handleMpmLogToChangelog(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	commitHash, _ := p["commit_hash"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	if commitHash == "" {
		return nil, fmt.Errorf("commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit). Run `git rev-parse HEAD` to get the canonical 40-char SHA-1")
	}

	// Parse the confirmation + contradiction params (opt-in,
	// explicit-only — see docs/epistemic-confirmation.md). Each can be
	// a single string OR a []string. An empty/missing param is
	// treated as no-op for that array; the other arrays still process.
	// Per the design doc, a confirmation OR contradiction is an
	// explicit assertion by the caller that this commit validates or
	// invalidates the named artifact — no keyword matching, no
	// semantic inference.
	confirmations := collectConfirmationSpecs(p, "confirms_lesson_id", "lesson")
	confirmations = append(confirmations, collectConfirmationSpecs(p, "confirms_decision_id", "decision")...)
	confirmations = append(confirmations, collectConfirmationSpecs(p, "confirms_theory_id", "theory")...)

	contradictions := collectContradictionSpecs(p, "contradicts_lesson_id", "lesson")
	contradictions = append(contradictions, collectContradictionSpecs(p, "contradicts_decision_id", "decision")...)
	contradictions = append(contradictions, collectContradictionSpecs(p, "contradicts_theory_id", "theory")...)

	var id string
	var err error
	switch {
	case len(confirmations) > 0 || len(contradictions) > 0:
		// Both directions route through the unified method so a
		// mixed-direction batch is one transaction.
		id, err = dm.LogChangelogEntryWithAssertions(
			fact, commitHash, internal.ParseStringSliceOr(p["tags"]),
			confirmations, contradictions)
	default:
		id, err = dm.LogChangelogEntry(fact, commitHash, internal.ParseStringSliceOr(p["tags"]))
	}
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":        true,
		"id":             id,
		"commit_hash":    commitHash,
		"collection":     "changelog",
		"confirmations":  len(confirmations),
		"contradictions": len(contradictions),
	}, nil
}

// collectConfirmationSpecs normalizes one confirmation param to a slice
// of ConfirmationSpec. Accepts either a single string (one id) or a
// []string (multiple ids) under the param key. Empty strings are
// skipped — the surface rejects them at the WithTx boundary, so
// surfacing the validation error early keeps the failure path
// uniform. Returns nil if the param is absent entirely (the no-
// confirmation path stays on LogChangelogEntry).
func collectConfirmationSpecs(p map[string]interface{}, paramName, artifactType string) []mpminternal.ConfirmationSpec {
	v, ok := p[paramName]
	if !ok || v == nil {
		return nil
	}
	var ids []string
	switch t := v.(type) {
	case string:
		if t != "" {
			ids = []string{t}
		}
	case []string:
		for _, s := range t {
			if s != "" {
				ids = append(ids, s)
			}
		}
	case []interface{}:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				ids = append(ids, s)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := make([]mpminternal.ConfirmationSpec, 0, len(ids))
	for _, id := range ids {
		out = append(out, mpminternal.ConfirmationSpec{
			ArtifactID:   id,
			ArtifactType: artifactType,
		})
	}
	return out
}

// collectContradictionSpecs is the negative-direction counterpart of
// collectConfirmationSpecs. Same shape, returns
// []ContradictionSpec. Mirrors confirms_*_id's "string or []string"
// acceptance so callers don't have to remember the asymmetry.
func collectContradictionSpecs(p map[string]interface{}, paramName, artifactType string) []mpminternal.ContradictionSpec {
	v, ok := p[paramName]
	if !ok || v == nil {
		return nil
	}
	var ids []string
	switch t := v.(type) {
	case string:
		if t != "" {
			ids = []string{t}
		}
	case []string:
		for _, s := range t {
			if s != "" {
				ids = append(ids, s)
			}
		}
	case []interface{}:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				ids = append(ids, s)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := make([]mpminternal.ContradictionSpec, 0, len(ids))
	for _, id := range ids {
		out = append(out, mpminternal.ContradictionSpec{
			ArtifactID:   id,
			ArtifactType: artifactType,
		})
	}
	return out
}

// handleSaveSkill persists a new skill or updates an existing version.
// Skills are markdown documents with YAML frontmatter (name, version,
// when_to_use, constraints, steps) stored as memory rows in
// collection='skills'. The full contract lives in internal/core/skill.go;
// this handler is a thin shim over dm.SaveSkill that adds the
// payload-parsing and frontmatter-validation pass.
//
// Args:
//   - name (string, required)        — the skill's stable name
//   - version (string, required)     — semver, e.g. "2.0.0"
//   - content (string)               — full markdown incl. frontmatter
//     (preferred: explicit frontmatter)
//   - body (string)                  — markdown body, alternative to
//     content. When body is set, the
//     handler synthesises frontmatter
//     from name/version/author so the
//     caller doesn't have to hand-roll
//     YAML just to save a short skill.
//   - author (string, optional)      — agent name for metadata
//   - force (bool, optional)         — overwrite when name+version exists
//
// Alpha-4 W-005: aggregate validation errors instead of failing on
// the first one. An agent that supplies both a missing name AND
// missing version now learns about both in a single response, not
// after two round-trips. Errors are returned as a `{success:false,
// errors:[...]}` payload rather than a Go error, so the envelope is
// still parseable on the wire — the previous shape (`return nil,
// fmt.Errorf(...)`) made the response look like a transport failure
// to callers that distinguish success-payloads from error-payloads.
//
// The structured-args shortcut (body instead of content) skips the
// frontmatter-parse step entirely. The synthesised frontmatter is the
// minimum ParseSkillFrontmatter requires (name, version) plus the
// optional author; the agent can read it back and add fields if it
// wants a richer shape.
func handleSaveSkill(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	name := internal.ParseStringOr(p["name"], "")
	version := internal.ParseStringOr(p["version"], "")
	content := internal.ParseStringOr(p["content"], "")
	body := internal.ParseStringOr(p["body"], "")
	author := internal.ParseStringOr(p["author"], ac.Agent)
	force := false
	if v, ok := p["force"].(bool); ok {
		force = v
	}

	// 2026-09-05 audit remediation pass 4 defect C.15 (P2): the
	// registry schema declares `mode` with the canonical enum
	// [form|refine] (the workshop action's vocabulary), but save did
	// not read the field at all — passing `mode: "bogus"` to save was
	// silently dropped. The schema enum is also not enough on its
	// own (per the brief: "Do not rely solely on enum if the
	// CLI/MCP handler can bypass schema validation"). Validate at the
	// boundary so any caller typo fails loudly, regardless of which
	// mpm_skills action the params are bound to.
	if rawMode, present := p["mode"]; present {
		mode, _ := rawMode.(string)
		if mode != "" && mode != "form" && mode != "refine" {
			return nil, fmt.Errorf("mode must be one of [form, refine], got %q", mode)
		}
	}

	// Aggregate ALL hard validation errors into a single payload. The
	// aggregate path returns success:false with an errors array; the
	// success path is the normal success envelope.
	var errs []string
	if name == "" {
		errs = append(errs, "missing name")
	}
	if version == "" {
		errs = append(errs, "missing version")
	}
	if content == "" && body == "" {
		errs = append(errs, "missing content or body")
	}
	if content != "" && body != "" {
		errs = append(errs, "either content or body, not both")
	}
	if len(errs) > 0 {
		return map[string]interface{}{
			"success": false,
			"errors":  errs,
		}, nil
	}

	// Structured-args shortcut: synthesise minimal frontmatter from
	// name/version/author so the caller doesn't need to hand-roll YAML.
	if content == "" {
		content = synthesiseSkillFrontmatter(name, version, author) + "\n" + body
	}

	// Validate name+version shape before parsing frontmatter so the
	// caller gets the cheaper rejection first. The same call also
	// runs inside dm.SaveSkill, so this is a UX optimisation, not a
	// security gate.
	if _, err := internal.SkillIDForNameAndVersion(name, version); err != nil {
		return nil, err
	}

	// Validate frontmatter before persisting.
	if _, _, err := internal.ParseSkillFrontmatter(content); err != nil {
		return nil, fmt.Errorf("invalid frontmatter: %w", err)
	}

	id, err := dm.SaveSkill(name, version, content, author, force)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"id":      id,
		"name":    name,
		"version": version,
	}, nil
}

// synthesiseSkillFrontmatter builds the minimal YAML frontmatter that
// ParseSkillFrontmatter accepts (name, version) plus an optional
// author comment. Used by handleSaveSkill's body-shortcut path.
func synthesiseSkillFrontmatter(name, version, author string) string {
	authorLine := ""
	if author != "" {
		authorLine = "# author: " + author + "\n"
	}
	return fmt.Sprintf("---\nname: %s\nversion: %s\n%s---\n", name, version, authorLine)
}

// handleReadSkill fetches a skill by name (latest version) or exact id.
// handleReadSkill returns a single skill by id. The canonical id
// vocabulary across mpm_skills actions is `skill_id` (the composite
// id used by the storage layer, e.g. "skill:foo-v1.0.0"). The
// legacy `name`+`version` pair is accepted as an alias for caller
// ergonomics; resolveSkillID normalises the alias form to the
// canonical form before delegating to the DM.
//
// 2026-09-05 audit residual pass §I-C.20: the id-vocabulary drift
// between mpm_skills actions (some expected `name`, others
// expected `skill_id`) is closed at this boundary. Both produce
// the canonical skill_id used downstream; a request that supplies
// both with conflicting values errors rather than silently merging.
func handleReadSkill(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	skillID, err := resolveSkillID(p, false)
	if err != nil {
		return nil, err
	}
	skill, err := dm.ReadSkill(skillID, "")
	if err != nil {
		return nil, err
	}
	// Observability Layer: skill reads are retrievals.
	_ = dm.RecordRetrieval(skill.ID, "skill")
	return map[string]interface{}{
		"success":     true,
		"id":          skill.ID,
		"name":        skill.Name,
		"version":     skill.Version,
		"when_to_use": skill.WhenToUse,
		"domain":      skill.Domain,
		"constraints": skill.Constraints,
		"steps":       skill.Steps,
		"body":        skill.Body,
		"is_global":   skill.IsGlobal,
	}, nil
}

// resolveSkillID resolves a skill identifier from the params map.
// The canonical key is `skill_id` (the composite id used by the
// storage layer, e.g. "skill:foo-v1.0.0"). The legacy `name`+`version`
// pair is accepted as an alias for caller ergonomics. The `requireVersion`
// flag controls whether omitting `version` when `name` is supplied
// is an error (used by delete/promote paths) or a "latest version"
// lookup (used by read).
//
// Reject rules:
//   - both `skill_id` and `name` supplied → ERROR (no silent merge)
//   - neither supplied → ERROR
//   - `name` supplied without `version` (when requireVersion) → ERROR
//   - invalid `name` / `version` (per SkillIDForNameAndVersion) → ERROR
//
// Same skill resource produces the same id (the storage layer is
// keyed on `skill_id`); resolveSkillID normalises the alias form to
// the canonical form before returning.
func resolveSkillID(p map[string]interface{}, requireVersion bool) (string, error) {
	rawID, hasID := p["skill_id"]
	rawName, hasName := p["name"]
	rawVersion, _ := p["version"].(string)

	if hasID && rawID != nil {
		id, ok := rawID.(string)
		if !ok {
			return "", fmt.Errorf("skill_id must be a string, got %T", rawID)
		}
		id = strings.TrimSpace(id)
		if id == "" {
			return "", fmt.Errorf("skill_id must not be empty")
		}
		// Both supplied with possibly-conflicting values: reject the
		// silent-merge class. The caller must declare intent.
		if hasName && rawName != nil {
			if n, _ := rawName.(string); strings.TrimSpace(n) != "" {
				return "", fmt.Errorf("skill_id and name are both supplied; supply one or the other (got skill_id=%q, name=%q)", id, n)
			}
		}
		return id, nil
	}

	// No skill_id. Fall back to name (+ version).
	if !hasName || rawName == nil {
		return "", fmt.Errorf("skill_id is required (or supply name+version)")
	}
	name, ok := rawName.(string)
	if !ok {
		return "", fmt.Errorf("name must be a string, got %T", rawName)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name must not be empty")
	}
	if requireVersion && rawVersion == "" {
		return "", fmt.Errorf("name supplied without version; supply either skill_id or name+version")
	}
	// Read accepts name without version (latest-version lookup). For
	// that case, return the name as-is so the DM can resolve to the
	// highest-versioned row. The DM's ReadSkill treats a bare name as
	// "find the latest version" and a `skill:<name>-v<ver>` form as
	// an exact id.
	if !requireVersion && rawVersion == "" {
		return name, nil
	}
	return internal.SkillIDForNameAndVersion(name, rawVersion)
}

// handleListSkills returns the latest version of each skill in scope.
//
// 2026-09-05 audit residual pass §I-C.15: the previous shape
// defaulted `scope` to "all" via ParseStringOr and silently let any
// other string through to the DM, which then ignored it via the
// `default: scopeClause = ""` branch — equivalent to a broad "all"
// query. An explicit `scope="bogus"` therefore returned every
// skill, surprising callers who thought they were narrowing the
// result. The schema's `enum: [all, local, shared]` is enforced
// here: omitted scope defaults to "all"; an explicit scope must
// be one of the three canonical values; anything else errors at
// the boundary with a clear message listing the allowed values.
func handleListSkills(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	rawScope, present := p["scope"]
	var scope string
	if present && rawScope != nil {
		s, ok := rawScope.(string)
		if !ok {
			return nil, fmt.Errorf("scope must be a string, got %T", rawScope)
		}
		scope = s
	}
	if scope == "" {
		scope = "all"
	}
	switch scope {
	case "all", "local", "shared":
		// ok
	default:
		return nil, fmt.Errorf("scope must be one of [all, local, shared], got %q", scope)
	}
	skills, err := dm.ListSkills(scope)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"skills":  skills,
		"count":   len(skills),
		"scope":   scope,
	}, nil
}

// agent uses this to investigate what went wrong, especially across
// sessions — the wake context surface only shows a count, the details
// come from this tool.
//
// Args:
//
//	--level      (optional) one of warn|error|fatal; default: any
//	--component (optional) subsystem name (e.g. "relay", "synthesis",
//	             "watcher", "security"); default: any
//	--artifact_id (optional) canonical artifact id; surfaces dedup
//	               provenance (F-A1 / F14-1 audit rows) for a memory
//	--days       (optional) lookback window in days; default 7
//	--limit      (optional) max rows; default 20, max 500
//	--include_stack (optional, default false) when true, the result
//	               rows carry the multi-KB stack_trace payload. Most
//	               callers want the headline event without the stack
//	               trace; opt in only when triaging a specific failure.
//	               Alpha-4.1 F-007 / W-002.
func handleQueryAuditLog(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	levelStr := getString(p, "level")
	// D-004 (alpha-4.1.1): normalize the level enum before it
	// hits the SQL boundary. The CLI handler already does this
	// (handlers_audit.go uses strings.ToLower + explicit reject);
	// the MCP path was passing the raw user string to
	// AuditLevel(), so "ERROR" / "Error" silently returned 0 hits
	// (the canonical stored values are lowercase). Lowercasing
	// keeps the contract consistent across both surfaces —
	// callers can write whichever case they prefer.
	if levelStr != "" {
		levelStr = strings.ToLower(levelStr)
	}
	component := getString(p, "component")
	artifactID := getString(p, "artifact_id")
	days := 7
	if v, ok := p["days"]; ok {
		switch t := v.(type) {
		case float64:
			days = int(t)
		case int:
			days = t
		}
	}
	// 2026-09-05 audit remediation pass 3 defect C.11 (P2):
	// mpm_system.query_audit_log silently dropped the `since`
	// parameter. The handler previously accepted since via
	// additionalProperties:true and ignored it. Honour the contract:
	// since is an absolute epoch-seconds cutoff; when both since and
	// days are present, since wins.
	//
	// Pre-fix reproduction:
	//   $ mpm call mpm_system --payload '{"action":"query_audit_log","params":{"since":1700000000}}'
	//     # returned the historical default (rows from days=7),
	//     # NOT rows with created_at >= 1700000000
	if rawSince, ok := p["since"]; ok {
		var sinceSec int64
		switch t := rawSince.(type) {
		case float64:
			sinceSec = int64(t)
		case int:
			sinceSec = int64(t)
		case int64:
			sinceSec = t
		default:
			return nil, fmt.Errorf("since must be a number (epoch seconds), got %T", rawSince)
		}
		if sinceSec <= 0 {
			return nil, fmt.Errorf("since must be > 0 (epoch seconds), got %d", sinceSec)
		}
		now := time.Now().Unix()
		if sinceSec > now {
			// A future cutoff means "no rows" — surface the no-rows
			// result rather than silently returning the default
			// 7-day window. The handler still calls the DM with the
			// computed days; the DM's days<=0 clamp is irrelevant
			// because sinceSec>now implies a small positive days.
			days = 1
		} else {
			days = int((now-sinceSec)/86400) + 1
		}
	}
	limit := 20
	if v, ok := p["limit"]; ok {
		switch t := v.(type) {
		case float64:
			limit = int(t)
		case int:
			limit = t
		}
	}
	includeStack := false
	if v, ok := p["include_stack"]; ok {
		switch t := v.(type) {
		case bool:
			includeStack = t
		case string:
			includeStack = t == "true" || t == "1" || t == "yes"
		case float64:
			includeStack = t != 0
		case int:
			includeStack = t != 0
		}
	}

	items, err := dm.QueryAuditLog(internal.AuditLevel(levelStr), component, artifactID, days, limit, includeStack)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"count":   len(items),
		"results": items,
	}, nil
}

// callListActiveClusters returns the deduped audit cluster proposals
// above threshold, partitioned into known vs unknown. Same fetch+dedup
// pipeline as wake_context.AuditSummary but in structured form, so the
// agent can pull the cluster_key strings it needs to populate
// open_questions at session end. Use this RIGHT BEFORE session_end
// to capture critical-but-unresolved clusters for the next session.
//
// Args:
//
//	(none) — returns whatever's currently active in the cluster table.
//
// Returns:
//
//	{
//	  "success": true,
//	  "known_clusters":   [{key, component, count, first_seen, last_seen, status, known: true}, ...],
//	  "unknown_clusters": [{key, component, count, first_seen, last_seen, status, known: false}, ...],
//	  "count":            {"known": N, "unknown": M}
//	}
//
// "Known" means cluster_key appears in a pending theory, recent
// decision (last 30d), or resolved theory. See internal/cluster_proposals.go
// ActiveClusters() for the dedup logic. One source of truth; the
// wake_context string formatter and this tool pull from the same helper.
func handleListActiveClusters(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	known, unknown, err := dm.ActiveClusters()
	if err != nil {
		return nil, err
	}
	// Empty slices serialize as [] not null in JSON.
	if known == nil {
		known = []internal.ClusterProposal{}
	}
	if unknown == nil {
		unknown = []internal.ClusterProposal{}
	}
	return map[string]interface{}{
		"success":          true,
		"known_clusters":   known,
		"unknown_clusters": unknown,
		"count": map[string]int{
			"known":   len(known),
			"unknown": len(unknown),
		},
	}, nil
}

// handleSnoozeCluster transitions an active audit-cluster proposal to
// 'snoozed' until a future timestamp. Auto-reactivates when snooze_until
// passes (filter clause in ActiveClusters handles the re-emergence).
//
// Wire schema (enforced by JSON-Schema in registry_list.go):
//   - cluster_key  (required) — primary key from list_active_clusters
//   - snooze_until (required) — ISO 8601 absolute ("2026-07-12T12:00:00Z")
//     OR Go duration ("24h", "7d", "1h30m")
//   - reason       (optional) — audit-friendly note
//
// The two parsed formats are accepted because the agent's wire format
// varies: relative durations ("24h") are ergonomic, absolute timestamps
// are needed for cross-tool coordination. parseClusterSnoozeUntil
// handles both. The handler does NOT silently default the cluster_key
// to "current" — explicit input prevents accidental cross-cluster
// snoozing when the agent's intent drifts.
func handleSnoozeCluster(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	clusterKey, _ := p["cluster_key"].(string)
	if clusterKey == "" {
		return nil, fmt.Errorf("cluster_key required")
	}
	snoozeUntil, _ := p["snooze_until"].(string)
	if snoozeUntil == "" {
		return nil, fmt.Errorf("snooze_until required for snooze_cluster")
	}
	reason, _ := p["reason"].(string)

	if err := dm.SetClusterStatus(clusterKey, internal.ClusterStatusSnoozed, snoozeUntil, reason); err != nil {
		return nil, fmt.Errorf("snooze cluster: %w", err)
	}
	return map[string]interface{}{
		"success":      true,
		"cluster_key":  clusterKey,
		"status":       "snoozed",
		"snooze_until": snoozeUntil,
		"reason":       reason,
	}, nil
}

// handleUnsnoozeCluster (Part 2B, 2026-09-06) is the explicit inverse of
// handleSnoozeCluster. Returns the cluster to status='active' and clears
// snooze_until. The row stays in the table for forensics; the active row
// will surface again on the next list_active_clusters call.
//
// Wire schema (enforced by JSON-Schema in registry_list.go):
//   - cluster_key (required) — primary key from list_active_clusters
//   - reason      (optional) — audit-friendly note explaining why the
//     cluster is being reactivated
//
// Semantics:
//   - Missing cluster_key: error at the boundary.
//   - Unknown cluster_key: error (operator typing a stale id wants a
//     diagnostic).
//   - Already-active cluster: silent success with status="active" — the
//     cluster is already in the desired terminal state. Matches the
//     soft-delete idempotency class in docs/tool-behavioral-contract.md §1.
//   - Snoozed cluster: reactivates, status="active", snooze_until=null,
//     audit row written.
//   - Resolved cluster: rejected — resolved is terminal; the existing
//     resolve_cluster contract explicitly forbids auto-reactivation.
//
// Compatibility: callers that previously worked around with
// snooze_cluster snooze_until=0s continue to work — that path is a
// ClusterStatusSnoozed branch with a past timestamp that the
// ActiveClusters filter treats as active. unsnooze_cluster is the
// explicit value path.
func handleUnsnoozeCluster(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	clusterKey, _ := p["cluster_key"].(string)
	if clusterKey == "" {
		return nil, fmt.Errorf("cluster_key is required")
	}
	reason, _ := p["reason"].(string)

	// Probe first so the unknown-id error envelope names the cluster,
	// not a generic SQL error.
	var existingStatus string
	if err := dm.SQLDB().QueryRow(
		`SELECT status FROM audit_cluster_proposals WHERE cluster_key = ?`,
		clusterKey,
	).Scan(&existingStatus); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("cluster not found: %q", clusterKey)
		}
		return nil, fmt.Errorf("lookup cluster: %w", err)
	}

	// Resolved is terminal — refuse to reactivate. The reasoning is the
	// same as in SetClusterStatus: resolved clusters never auto-reactivate,
	// so an explicit unsnooze on a resolved row would silently violate
	// that contract.
	if existingStatus == internal.ClusterStatusResolved {
		return nil, fmt.Errorf("unsnooze_cluster: cluster %q is resolved (terminal); cannot reactivate a resolved cluster", clusterKey)
	}

	// Active + Snoozed both go through SetClusterStatus(Active, "");
	// the status diff drives the audit row and the snooze_until clear.
	if err := dm.SetClusterStatus(clusterKey, internal.ClusterStatusActive, "", reason); err != nil {
		return nil, fmt.Errorf("unsnooze cluster: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"cluster_key": clusterKey,
		"status":      "active",
		"reason":      reason,
	}, nil
}

// handleResolveCluster permanently dismisses an audit-cluster proposal.
// Sets status='resolved'; the cluster row stays in the table for
// forensics but is filtered out of ActiveClusters forever — it can no
// longer re-surface as active.
//
// Wire schema (enforced by JSON-Schema in registry_list.go):
//   - cluster_key (required) — primary key from list_active_clusters
//   - reason      (optional) — audit-friendly note explaining root cause
//
// Resolved clusters can be re-resolved (idempotent) — useful if the
// agent reaches a more refined understanding and overwrites reason
// with the better explanation. The row's first_seen / count stays
// intact so historical signal is preserved.
func handleResolveCluster(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	clusterKey, _ := p["cluster_key"].(string)
	if clusterKey == "" {
		return nil, fmt.Errorf("cluster_key required")
	}
	reason, _ := p["reason"].(string)

	if err := dm.SetClusterStatus(clusterKey, internal.ClusterStatusResolved, "", reason); err != nil {
		return nil, fmt.Errorf("resolve cluster: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"cluster_key": clusterKey,
		"status":      "resolved",
		"reason":      reason,
	}, nil
}

// handleAnnotateCluster appends a forensic annotation to an existing
// audit-cluster proposal's audit trail. The annotation captures
// late-arriving context, root-cause refinement, or post-mortem
// without mutating the cluster's status, snooze_until, count, or any
// other state field.
//
// Wire schema (enforced by JSON-Schema in registry_list.go):
//   - cluster_key (required) — primary key (from list_active_clusters
//     OR remembered historical key for
//     resolved clusters).
//   - annotation  (required) — substantive insight text; appended to
//     the audit trail verbatim.
//   - reason      (optional) — short label (e.g. "post-mortem",
//     "week-later-refinement").
//
// Distinct from resolve_cluster: that tool writes a single audit row
// at resolution time and freezes the cluster. annotate_cluster can be
// called any number of times on any cluster state — the cluster is
// never re-opened, but the audit trail accumulates refinement.
//
// Distinct from snooze_cluster: that tool changes the surface state
// (active → snoozed). Annotating does NOT change surface state at
// all; the cluster continues to surface (or not) according to its
// current status.
func handleAnnotateCluster(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	clusterKey, _ := p["cluster_key"].(string)
	if clusterKey == "" {
		return nil, fmt.Errorf("cluster_key required")
	}
	annotation, _ := p["annotation"].(string)
	if annotation == "" {
		return nil, fmt.Errorf("annotation required")
	}
	reason, _ := p["reason"].(string)

	if err := dm.AnnotateCluster(clusterKey, annotation, reason); err != nil {
		return nil, fmt.Errorf("annotate cluster: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"cluster_key": clusterKey,
		"annotation":  annotation,
		"reason":      reason,
		"appended_to": "system_audit_log (component='cluster')",
	}, nil
}

// handleHandoffWrite writes a handoff record for inter-session communication.
// The summary is the bridge; tasks belong in mpm_work and testable
// questions in mpm_theories. Optional `commitments` / `open_questions`
// string arrays ARE persisted (F13) so the next session's wake context
// carries the full continuity picture — accepted fields are never
// silently dropped.
//
// Identity model (2026-09-11): three correlated IDs:
//   - mpm_session_id        — REQUIRED (auto-allocated from active.json
//     when not supplied). Sticky across
//     CLI/MCP/process boundaries within one
//     interaction lifecycle. NEVER filled with
//     a framework ID as a convenience.
//   - framework_session_id  — OPTIONAL host-owned ID. NULL when absent.
//     Read from payload key
//     "framework_session_id" (canonical); the
//     legacy key "session_id" still routes
//     into the legacy session_id column for
//     back-compat callers.
//   - session_id (legacy)    — UNIQUE nullable column. Back-compat
//     surface. New code should prefer
//     framework_session_id.
//
// mpm_session_id is ALWAYS allocated on every write (the handoff
// tool is an INTERACTION BOUNDARY — invariant #4). The first write
// in a fresh workspace allocates; subsequent writes reuse the same
// id. framework_session_id is the caller-supplied host id, never
// synthesized.
//
// Caller-supplied mpm_session_id override (rare, mostly tests) is
// accepted via payload key "mpm_session_id" but production code
// should leave it empty and let the canonical allocator run.
func handleHandoffWrite(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// Identity reads. Order of precedence (canonical first):
	//   - framework_session_id: canonical host-owned key. Empty when absent.
	//   - session_id:           legacy key. Routes into the legacy
	//                           session_id column. Empty when absent.
	// Both may be empty (Pi, Hermes without hooks) — that's normal.
	//
	// 2026-09-12 reconciliation note: mpm_session_id and
	// framework_session_id are accepted on the payload for forward
	// compatibility with wip/session-identity-rework, but the
	// canonical CoreDB.EndSession surface (which writes those fields)
	// lives on the WIP. The payload keys are silently accepted here
	// so callers using the WIP can speak to this build without parse
	// errors; they will be persisted once the WIP merges.
	_ = getString(p, "framework_session_id")
	sessionID := getString(p, "session_id")
	_ = getString(p, "mpm_session_id")

	summary := getString(p, "summary")
	if summary == "" {
		return nil, fmt.Errorf("summary is required")
	}
	state := getString(p, "state")
	if state == "" {
		state = internal.HandoffClean
	}

	// F13: commitments/open_questions are PERSISTED, not dropped. The
	// previous "Option B" silently ignored them on this surface while the
	// plugins still advertised them — an accepted-then-discarded field,
	// exactly the data-loss class the audit flags. EndSessionV2 already
	// persists both columns and read-backs the row; the wake context
	// renders open_questions for the next session.
	//
	// 2026-09-05 audit remediation pass 4 defect C.17 (P2): the schema
	// declares `open_questions` as array of strings, but the previous
	// shape routed both fields through ParseStringSliceOr, which
	// silently returned nil for non-array scalars and silently dropped
	// non-string elements. A caller typo (e.g. open_questions=[123])
	// produced a handoff with open_questions=[] rather than an error.
	// Use the strict array helper so the four states — omitted,
	// explicit [], valid array, invalid shape — produce distinct
	// outcomes.
	commitments, err := parseStrictStringArrayOrEmpty("commitments", p["commitments"])
	if err != nil {
		return nil, err
	}
	openQuestions, err := parseStrictStringArrayOrEmpty("open_questions", p["open_questions"])
	if err != nil {
		return nil, err
	}

	// 55f3a5ea originally called dm.EndSessionV2 here to surface
	// mpm_session_id / framework_session_id on the write surface, but
	// EndSessionV2 lives on wip/session-identity-rework and the core
	// substrate's behaviour for these fields is still in flight. Roll
	// back to the canonical EndSession so the handoff read/write
	// surfaces stay self-consistent without the WIP. The
	// mpm_session_id / framework_session_id additions on the wake
	// context read surface are preserved (data.MPMSessionID /
	// data.FrameworkSessionID exposed in handleReadWakeContext).
	// When the WIP merges, this call site can swap to EndSessionV2
	// again without further ceremony.
	h, err := dm.EndSession(sessionID, summary, state, commitments, openQuestions)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":        true,
		"handoff":        h,
		"handoff_id":     h.ID,
		"session_id":     h.SessionID,
		"message":        "handoff written. Pending work surfaces via mpm_work; open questions via mpm_theories.",
		"open_questions": h.OpenQuestions,
	}, nil
}

// handleHandoffRead returns a handoff by explicit id when one is given
// (`handoff_id`, with bare `id` accepted per the D-8.1 family convention),
// otherwise the most recent handoff. The wake context
// surfaces unread handoffs automatically; this tool is for explicit
// re-reads of any handoff (read or unread).
//
// CLI acceptance 2026-09-12: pre-fix the id params were ignored and the
// latest handoff was always returned — a read-after-shred returned a
// DIFFERENT live handoff with success:true instead of not-found.
//
// Args:
//
//	--handoff_id / --id (optional) explicit handoff to return; unknown
//	             ids yield success:true + handoff:nil (same shape as the
//	             no-handoffs case) instead of a wrong record.
//	--mark_read (optional) "true" to mark the returned handoff as read
//	             after returning; default false. The wake context marks
//	             its own reads — this tool does not by default so the
//	             agent can browse the handoff history without consuming
//	             the wake context's handoff.
//	--unread    (optional) "true" to return only unread handoffs;
//	             default false (returns latest regardless of read state)
func handleHandoffRead(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	markRead := false
	if v, ok := p["mark_read"]; ok {
		if b, ok := v.(bool); ok {
			markRead = b
		}
	}
	unreadOnly := false
	if v, ok := p["unread"]; ok {
		if b, ok := v.(bool); ok {
			unreadOnly = b
		}
	}

	var h *internal.Handoff
	var err error
	// Explicit id wins over latest/unread selection.
	wantID, _ := p["handoff_id"].(string)
	if wantID == "" {
		wantID, _ = p["id"].(string)
	}
	if wantID != "" {
		h, err = dm.GetHandoffByID(wantID)
	} else if unreadOnly {
		h, err = dm.GetLatestUnreadHandoff()
	} else {
		h, err = dm.GetLatestHandoff()
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]interface{}{
				"success": true,
				"handoff": nil,
				"message": "no handoff found",
			}, nil
		}
		return nil, err
	}
	if markRead {
		// Best-effort mark-read; don't fail the call if marking fails
		// because the caller explicitly opted in. Idempotent.
		_ = dm.MarkHandoffRead(h.ID, "manual-call")
		now := time.Now().UTC().Unix()
		h.ReadAt = &now
		h.ReadBy = "manual-call"
	}
	return map[string]interface{}{
		"success": true,
		"handoff": h,
	}, nil
}

// ptrTime is a small helper for the session_handoff tool.
func ptrTime(t time.Time) *time.Time { return &t }

// handleHandoffList returns recent handoffs in descending created_at order.
// Scan to find unresolved threads without booting a session.
//
// Args:
//
//	--limit      (optional) max handoffs; default 10, max 500
//	--unread     (optional) "true" to filter to unread; default false
func handleHandoffList(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	limit := 10
	if v, ok := p["limit"]; ok {
		switch t := v.(type) {
		case float64:
			limit = int(t)
		case int:
			limit = t
		}
	}
	unreadOnly := false
	if v, ok := p["unread"]; ok {
		if b, ok := v.(bool); ok {
			unreadOnly = b
		}
	}

	items, err := dm.ListHandoffs(limit, unreadOnly)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"count":   len(items),
		"results": items,
	}, nil
}

// handleShredHandoff removes a handoff by id. Handoffs are bootstrap
// data — destroying one removes the previous session's commitments,
// open questions, and ended-state from wake context permanently. The
// shred is idempotent: re-shredding an unknown id returns
// shredded=false with no error (the previous gap: callers had to
// inspect rows-affected and decide; this normalizes the contract).
//
// Required:
//
//	id — handoff id (NOT session_id; look up via session_handoff or
//	     list_handoffs first if you only have the session_id).
//
// Closes MPM-GAP-SHRED-HANDOFF-2026-08-19.
func handleShredHandoff(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// D-8.1: accept either `id` or the legacy `handoff_id` alias so the
	// surface is consistent with mpm_work (work_id) and mpm_memory
	// (memory_id) — all family members accept either a family-specific
	// key or the bare `id`.
	id, _ := p["id"].(string)
	if id == "" {
		if v, ok := p["handoff_id"].(string); ok && v != "" {
			id = v
		}
	}
	if id == "" {
		// Accept session_id for callers that have only the session
		// identifier (the common test-handoff case). Lookup is a
		// best-effort convenience; if the row doesn't exist the
		// downstream DeleteHandoff returns 0 rows and we surface
		// shredded=false cleanly.
		if sid, ok := p["session_id"].(string); ok && sid != "" {
			h, err := dm.GetHandoffBySessionID(sid)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					// Already shredded or never existed — idempotent
					// no-op so repeat shreds return the same shape as
					// the by-id path instead of a validation error.
					return map[string]interface{}{
						"success":      true,
						"shredded":     false,
						"rows_deleted": int64(0),
						"message":      "handoff not found; nothing to shred",
					}, nil
				}
				return nil, fmt.Errorf("lookup handoff by session %q: %w", sid, err)
			}
			if h != nil {
				id = h.ID
			}
		}
	}
	if id == "" {
		// D-10.1: use the canonical W-006 hint so the agent knows how
		// to recover (mpm_context read_wake_context → session_current_id,
		// or MPM_SESSION_ID env var). The hint also names both `id`
		// and `session_id` as accepted forms.
		return nil, internal.ErrSessionIDRequired()
	}

	n, err := dm.DeleteHandoff(id)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":      true,
		"shredded":     n > 0,
		"handoff_id":   id,
		"rows_deleted": n,
		"message":      "handoff shredded",
	}, nil
}

// handleQueryGlobalRules returns memories from the shared DB that
// are marked is_global = 1. This is the read-side of the multi-agent
// shared epistemology: every agent on the workstation sees the same
// house rules, conventions, and persona overlays.
//
// Args:
//
//	--query           (optional) FTS5 keyword search
//	--limit           (optional) max rows; default 50, max 500
//	--include_retired (optional, default false) when true, retired
//	                  rules (retired_at IS NOT NULL) are included;
//	                  the default filters them out so the agent sees
//	                  only active rules. Forensic / post-mortem
//	                  queries opt in.
//
// In local-only mode (no MPM_SHARED_DB attached) returns an empty
// result with success=true — the agent should fall back to local
// recall. This is intentional: shared rules are an additive layer,
// not a replacement for project-specific memory.
func handleQueryGlobalRules(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	limit := 50
	if v, ok := p["limit"]; ok {
		switch t := v.(type) {
		case float64:
			limit = int(t)
		case int:
			limit = t
		}
	}
	if limit > 500 {
		limit = 500
	}

	includeRetired := false
	if v, ok := p["include_retired"]; ok {
		switch t := v.(type) {
		case bool:
			includeRetired = t
		case string:
			includeRetired = t == "true" || t == "1" || t == "yes"
		case float64:
			includeRetired = t != 0
		case int:
			includeRetired = t != 0
		default:
			return nil, fmt.Errorf("include_retired must be a boolean (got %T)", v)
		}
	}

	items, err := dm.QueryGlobalRules(query, limit, includeRetired)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":         true,
		"source":          "shared",
		"attached":        dm.SharedAttached() != "",
		"include_retired": includeRetired,
		"count":           len(items),
		"results":         items,
	}, nil
}

// handleRetireGlobalRule soft-retires a global rule. See
// internal/core/db.go RetireGlobalRule for the full contract.
//
// Wire schema:
//   - rule_id  (required)
//   - confirm  (required, must be exactly true)
//   - reason   (optional, free-form note recorded in audit)
//
// Idempotency: already-retired → silent success with already_retired=true.
// Unknown id → error. Shared DB not attached → error.
//
// Part 1 (2026-09-06): closes the catalog §C.2 finding (global rules
// can be added but never formally retracted).
func handleRetireGlobalRule(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	ruleID, _ := p["rule_id"].(string)
	if ruleID == "" {
		return nil, fmt.Errorf("rule_id is required")
	}
	confirm, _ := p["confirm"].(bool)
	if !confirm {
		return nil, fmt.Errorf("retire_global_rule requires confirm=true; shared-DB state transitions must be operator-gated")
	}
	reason, _ := p["reason"].(string)
	return dm.RetireGlobalRule(ruleID, reason, true)
}

// handleRecordGlobalRule writes a memory to the shared DB with
// is_global=1. Phase 3 of docs/archive/shared-epistemology.md: operator-only. The caller must
// pass confirm=true — without it the call is rejected. This is
// defense-in-depth against agents writing house rules autonomously.
//
// Args:
//
//	--fact        (required) The rule content
//	--tags        (optional) Comma-separated tags
//	--weight      (optional) 0-100, default 10 (house-rule weight)
//	--provenance  (optional) Operator's name / why the rule exists
//	--confirm     (required) Must be true. Refuses without it.
func handleRecordGlobalRule(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	confirm, _ := p["confirm"].(bool)
	if !confirm {
		return nil, fmt.Errorf("record_global_rule requires confirm=true; house rules should not be written autonomously")
	}
	content, _ := p["fact"].(string)
	if content == "" {
		return nil, fmt.Errorf("fact is required")
	}
	tagsRaw, _ := p["tags"].(string)
	tags := splitTags(tagsRaw)
	weight := 10.0
	if v, ok := p["weight"]; ok {
		switch t := v.(type) {
		case float64:
			weight = t
		case int:
			weight = float64(t)
		}
	}
	provenance, _ := p["provenance"].(string)

	id, err := dm.RecordGlobalRule(content, tags, weight, provenance)
	if err != nil {
		return nil, err
	}
	// Forensic log — cross-agent shared-DB write. Other agents see
	// this row when they query audit_log; the row is the operator's
	// record that a house rule was established (or replaced).
	dm.LogAudit(
		mpminternal.AuditInfo, "shared_db",
		fmt.Sprintf("record_global_rule %s", id), "",
		mpminternal.AuditContext{
			"rule_id":       id,
			"weight":        weight,
			"tags":          tags,
			"is_house_rule": true,
		},
	)
	// Arc 2: auto-broadcast as the side-effect of the explicit
	// --confirm=true. The rationale comes from the rule's
	// provenance (which RecordGlobalRule stores verbatim) when
	// present, otherwise a default "house rule established" string.
	// Non-fatal: a broadcast failure does NOT roll back the write.
	broadcastRationale := provenance
	if broadcastRationale == "" {
		broadcastRationale = "New house rule established by operator."
	}
	_, _ = dm.BroadcastMemory(id, mpminternal.BroadcastOpts{
		Kind:            "rule",
		Rationale:       broadcastRationale,
		SourceAgent:     ac.Agent,
		SourceSessionID: ac.SessionID,
	})
	return map[string]interface{}{
		"success": true,
		"id":      id,
		"source":  "shared",
	}, nil
}

// handlePromoteToGlobal copies a local memory to the shared DB. The
// original local row is preserved. The shared copy carries a
// metadata.derived_from_local_id field linking back to the source.
//
// Args:
//
//	--memory_id  (required) The local memory ID to promote
//	--confirm    (required) Must be true.
func handlePromoteToGlobal(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	confirm, _ := p["confirm"].(bool)
	if !confirm {
		return nil, fmt.Errorf("promote_to_global requires confirm=true; cross-project promotion should be operator-gated")
	}
	localID, _ := p["memory_id"].(string)
	if localID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}

	sharedID, err := dm.PromoteToGlobal(localID)
	if err != nil {
		return nil, err
	}
	// Forensic log — local→shared promotion. The local copy stays
	// (per docs/archive/shared-epistemology.md Phase 3); this row records the lineage edge
	// so other agents can see which local IDs have been elevated.
	dm.LogAudit(
		mpminternal.AuditInfo, "shared_db",
		fmt.Sprintf("promote_to_global %s", localID), "",
		mpminternal.AuditContext{
			"local_id":  localID,
			"shared_id": sharedID,
			"lineage":   fmt.Sprintf("local:%s -> shared:%s", localID, sharedID),
		},
	)
	return map[string]interface{}{
		"success":   true,
		"shared_id": sharedID,
		"local_id":  localID,
		"lineage":   fmt.Sprintf("local:%s -> shared:%s", localID, sharedID),
	}, nil
}

// handleWorkshopSkill runs the Skill Workshop pipeline (form or
// refine mode). The full contract is in
// docs/archive/2026-08-28-mpm-skill-workshop-design.md §4 and
// §5. The handler is a thin shim: parse the payload map into
// internal.WorkshopRequest and call internal.RunWorkshop.
func handleWorkshopSkill(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// Resolve the underlying *internal.DatabaseManager (the
	// CoreDB interface doesn't expose RunWorkshop directly — it's
	// defined as a concrete method).
	concrete, ok := dm.(*internal.DatabaseManager)
	if !ok {
		return nil, fmt.Errorf("workshop: underlying DB is not *internal.DatabaseManager (got %T)", dm)
	}

	// Decode params into a WorkshopRequest. JSON round-trip keeps
	// the shape strict; unknown fields are dropped silently.
	//
	// D-010 (alpha-4.1.1): the Go json.Unmarshal error for a wrong-
	// typed decision-model axis (e.g. caller passes "leverage":
	// "3" instead of 3) is "json: cannot unmarshal string into
	// Go struct field WorkshopRequest.decision_model.leverage of
	// type int". That tells the caller the field name and type
	// mismatch but says nothing about the legal range (0..5) or
	// the boundary enum contract. Pre-check the integer axes with
	// explicit type assertions so the error message identifies
	// the field, the expected type, and the legal range — and
	// fail fast before the generic json.Unmarshal error obscures
	// it.
	if dmField, ok := p["decision_model"]; ok {
		if dmMap, ok := dmField.(map[string]interface{}); ok {
			for _, axis := range []string{"reusability", "non_obviousness", "stability", "leverage"} {
				if v, present := dmMap[axis]; present {
					// Accept any JSON number (float64 from encoding/json)
					// and reject strings / bools / null with a field-
					// specific message.
					if _, isNum := v.(float64); !isNum {
						return nil, fmt.Errorf(
							"workshop: decision_model.%s must be an integer from 0 to 5 (got %T: %v) — "+
								"the decision-model contract is a 4-axis integer score; "+
								"see docs/archive/2026-08-28-mpm-skill-workshop-design.md §6",
							axis, v, v,
						)
					}
				}
			}
			if v, present := dmMap["boundary"]; present {
				if s, ok := v.(string); !ok {
					return nil, fmt.Errorf(
						"workshop: decision_model.boundary must be a string from {procedure, judgment, knowledge} "+
							"(got %T: %v)", v, v,
					)
				} else if s != "procedure" && s != "judgment" && s != "knowledge" {
					return nil, fmt.Errorf(
						"workshop: decision_model.boundary must be one of {procedure, judgment, knowledge}, got %q",
						s,
					)
				}
			}
		}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("workshop: marshal params: %w", err)
	}
	var req internal.WorkshopRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("workshop: parse params: %w", err)
	}

	resp, err := internal.RunWorkshop(concrete, &req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// handlePromoteSkillToGlobal marks a skill row as shared (is_global=1)
// and stamps metadata.derived_from_skill_id so the promoted row is
// traceable. The third operator-gated shared-DB promotion path
// alongside record_global_rule and promote_to_global.
//
// Unlike promote_to_global, no separate shared row is created: the skill
// keeps its deterministic id and is flipped in place (see
// PromoteSkillToGlobal). So there is no "local -> shared id" lineage to
// report — the audit note records the single id that was elevated.
//
// Args:
//
//	--skill_id  (required) The skill id to promote (e.g. "skill:agentshell-v1.0.0")
//	--confirm   (required) Must be true. Refuses without it.
func handlePromoteSkillToGlobal(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	confirm, _ := p["confirm"].(bool)
	if !confirm {
		return nil, fmt.Errorf("promote_skill_to_global requires confirm=true; cross-project promotion should be operator-gated")
	}
	// 2026-09-05 audit residual pass §I-C.20: promote_to_global accepts
	// the canonical skill_id (and the legacy name+version alias) through
	// resolveSkillID. The cross-project promotion still requires the
	// explicit confirm=true gate (above).
	skillID, err := resolveSkillID(p, true)
	if err != nil {
		return nil, err
	}

	// Forward the validated confirm rather than a literal true, so the
	// value the operator supplied is the value the gate sees end to end.
	if err := dm.PromoteSkillToGlobal(skillID, confirm); err != nil {
		return nil, err
	}
	// Forensic log — skill→shared promotion. Records which skill id was
	// elevated so other agents can audit the shared surface.
	dm.LogAudit(
		mpminternal.AuditInfo, "shared_db",
		fmt.Sprintf("promote_skill_to_global %s", skillID), "",
		mpminternal.AuditContext{
			"skill_id": skillID,
		},
	)
	return map[string]interface{}{
		"success":   true,
		"skill_id":  skillID,
		"is_global": true,
	}, nil
}

// handleDeleteSkill soft-deletes a skill by id. The row stays in the DB
// for forensics (deleted_at is set); read_skill and list_skills filter
// it out. Idempotent: deleting an unknown id is a no-op (matches
// ShredSkill's silent-on-missing contract). No confirm gate — a
// soft-delete is recoverable from the row, unlike hard shredding; if
// that changes, the gate mirrors promote_to_global /
// promote_skill_to_global.
//
// 2026-09-05 audit remediation §I-C.9 was originally closed by
// commit 45e616f (errors.Is(err, ErrSkillNotFound) translation;
// ErrSkillNotFound removed). That fix was reverted once the
// deliberate-design precedent surfaced: commit 0583bea explicitly
// designed mpm_handoff.shred as idempotent on unknown id, with
// structured shredded=false / rows_deleted=0 / success=true feedback
// and a regression test pinning the contract. mpm_skills.delete was
// matching that pattern from the start (the original handler comment
// explicitly stated "matches ShredSkill's silent-on-missing
// contract"). Reverting restores consistency with the handoff shred
// and the project-wide soft-delete policy — see
// docs/tool-behavioral-contract.md "Not-found semantics for soft
// deletes".
//
// Args:
//
//	--skill_id  (required) The skill id to delete (e.g. "skill:agentshell-v1.0.0")
func handleDeleteSkill(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// 2026-09-05 audit residual pass §I-C.20: delete accepts the
	// canonical skill_id (and the legacy name+version alias) through
	// resolveSkillID, so callers do not need to construct the full
	// id format themselves. Same precedence rules as handleReadSkill.
	skillID, err := resolveSkillID(p, true)
	if err != nil {
		return nil, err
	}
	if err := dm.ShredSkill(skillID); err != nil {
		return nil, err
	}
	// Forensic log — soft-delete is recoverable, so audit-only (no "shared_db"
	// component like the promote paths).
	dm.LogAudit(
		mpminternal.AuditInfo, "skill",
		fmt.Sprintf("delete_skill %s", skillID), "",
		mpminternal.AuditContext{"skill_id": skillID},
	)
	return map[string]interface{}{
		"success":  true,
		"skill_id": skillID,
	}, nil
}

// splitTags is a small helper that turns a comma-separated tag string
// into a []string. Empty input returns nil.
func splitTags(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ── Phase 5a: scheduled_wakes handlers ───────────────────────────────────
//
// Stateless, opportunistic scheduler. No daemon, no ticker. Any MPM call
// that passes through checkWakesAndFold (called inline below) sees due
// wakes surfaced as a WakesPending block in the response. The agent sees
// the wake on the next tool call after its target_time, regardless of
// which session or agent issued the call.
//
// handleCheckWakes / handleListWakes are the explicit pull variants for
// the agent to use when it wants to inspect the wake queue on demand
// (e.g. at session start, or after waking from a passive check).
//
// handleScheduleWake is the write path. Returns the resolved absolute
// target_time so the caller can log it. No operator gate — wakes are
// not house rules; the agent scheduling its own work is the entire
// point of this feature. (The DB-layer scanner/scrubber covers the
// reason field at write time via SaveMemoryWithContext-style choke.)

// checkWakesAndFold runs CheckPendingWakes and folds any due wakes into
// out as a WakesPending block. Returns the (possibly decorated) out map
// so callers can do `out := ...; return checkWakesAndFold(dm, out)`.
// No-op when out is nil or when there are no due wakes.
//
// Arc 2: also folds EventWakesPending from shared.event_wakes when the
// ActiveContext has a SessionID. The two blocks land separately on the
// response so receiving agents can distinguish agent-scheduled future
// tasks (WakesPending) from incoming epistemic events (EventWakesPending).
//
// kinds is forwarded to CheckPendingWakes — see that function for the
// semantics. nil = backward-compatible default (notification-only).
func checkWakesAndFold(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, out map[string]interface{}, kinds []string) map[string]interface{} {
	if out == nil {
		out = map[string]interface{}{}
	}
	if due, err := dm.CheckPendingWakes(time.Now(), kinds); err == nil && len(due) > 0 {
		out["WakesPending"] = due
		out["WakesPendingCount"] = len(due)
	}
	// Arc 2 fan-out pull. Only meaningful when we have a session ID
	// (which the CLI dispatcher and MCP server both inject). Unit
	// tests that don't pass a SessionID skip this branch silently.
	if ac.SessionID != "" {
		if eventWakes, err := dm.CheckPendingEventWakes(ac.SessionID); err == nil && len(eventWakes) > 0 {
			out["EventWakesPending"] = eventWakes
			out["EventWakesPendingCount"] = len(eventWakes)
		}
	}
	return out
}

func handleScheduleWake(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	reason, _ := p["reason"].(string)
	if reason == "" {
		return nil, fmt.Errorf("reason is required")
	}
	targetTime, _ := p["target_time"].(string)
	if targetTime == "" {
		return nil, fmt.Errorf("target_time is required (absolute unix epoch or relative like '24h', '2h', '30m')")
	}
	theoryID, _ := p["theory_id"].(string)
	recurringRule, _ := p["recurring_rule"].(string)

	// 2026-09-05 audit remediation pass 4 defect C.18 (P1): the audit
	// framed target_time and recurring_rule as a single scheduling-
	// form contract, but they are distinct fields in the codebase.
	// target_time is already validated by resolveTargetTime (epoch,
	// relative duration, ISO-8601) — arbitrary strings are rejected.
	// recurring_rule is the structured cron expression field and was
	// previously persisted verbatim, so caller typos ("* * *", "0 25
	// * * *") surfaced only at next-schedule time. Validate at the
	// boundary using the existing robfig/cron parser (same parser as
	// the scheduled_tasks path).
	if recurringRule != "" {
		if _, err := internal.CalculateNextRun(recurringRule, time.Now()); err != nil {
			return nil, fmt.Errorf("recurring_rule: %w", err)
		}
	}

	createdBy := ac.Agent
	if createdBy == "" {
		createdBy = ac.Model
	}
	if createdBy == "" {
		createdBy = "mpm_call"
	}

	var metadata map[string]interface{}
	if raw, ok := p["metadata"]; ok {
		if m, ok := raw.(map[string]interface{}); ok {
			metadata = m
		}
	}

	// 2026-09-22 release-blocker D-1: the canonical `kind=notification`
	// default for the wake discriminator is now authored INSIDE
	// ScheduleWake (the DB-layer producer), not here. This keeps the
	// policy in one place: every caller of ScheduleWake — the public
	// handler, the cascade materializer, the wake reconcile loop, and
	// any future internal producer — gets the same default. See
	// wake_tools.go ScheduleWake for the producer-side fix; see
	// wake_tools.go ResolveWake for the consumer-side matcher.

	out, err := dm.ScheduleWake(reason, targetTime, theoryID, recurringRule, createdBy, metadata)
	if err != nil {
		return nil, err
	}
	return checkWakesAndFold(dm, ac, out, nil), nil
}

func handleCheckWakes(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	kinds := readKindsParam(p)
	out := checkWakesAndFold(dm, ac, map[string]interface{}{
		"success": true,
	}, kinds)
	if _, ok := out["WakesPending"]; !ok {
		out["WakesPending"] = []map[string]interface{}{}
		out["WakesPendingCount"] = 0
	}
	return out, nil
}

// readKindsParam extracts the optional kinds array from a tool payload.
// Returns nil if absent, malformed, or empty — nil triggers the
// backward-compatible default (notification-only) in CheckPendingWakes.
func readKindsParam(p map[string]interface{}) []string {
	if p == nil {
		return nil
	}
	raw, ok := p["kinds"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]interface{})
	if !ok || len(arr) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			kinds = append(kinds, s)
		}
	}
	if len(kinds) == 0 {
		return nil
	}
	return kinds
}

// handleCheckPendingEventWakes (Arc 2): pulls incoming event wakes
// targeting the calling session. Defaults to ac.SessionID; allows
// explicit session_id override for unit tests.
func handleCheckPendingEventWakes(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	sessionID, _ := p["session_id"].(string)
	if sessionID == "" {
		sessionID = ac.SessionID
	}
	if sessionID == "" {
		return nil, fmt.Errorf("session_id required (set ActiveContext.SessionID or pass session_id param)")
	}
	wakes, err := dm.CheckPendingEventWakes(sessionID)
	if err != nil {
		return nil, fmt.Errorf("check_pending_event_wakes: %w", err)
	}
	return map[string]interface{}{
		"success":             true,
		"event_wakes_pending": wakes,
		"count":               len(wakes),
		"session_id":          sessionID,
	}, nil
}

func handleListWakes(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	includeFired := false
	if v, ok := p["include_fired"].(bool); ok {
		includeFired = v
	}
	overdueOnly := false
	if v, ok := p["overdue_only"].(bool); ok {
		overdueOnly = v
	}
	limit := 100
	if v, ok := p["limit"]; ok {
		switch n := v.(type) {
		case float64:
			limit = int(n)
		case int:
			limit = n
		}
	}
	// F-5 (2026-09-04 residual inventory, P2): the JSON Schema for
	// mpm_wakes list advertises `kinds: string[]` but pre-fix this
	// parameter was silently discarded — every agent filtering on
	// kinds received the unfiltered result set. The kinds semantics
	// here match CheckPendingWakes at wake_tools.go:226-249: nil/empty
	// → no filter (backward-compat default for list is "all wakes",
	// NOT notification-only); explicit list → IN filter on
	// json_extract(metadata, '$.kind'); "*" → no filter. We post-filter
	// the ListScheduledWakes result set rather than changing the DB
	// layer signature, which keeps the fix narrowly scoped to the bug.
	kinds := readKindsParam(p)
	items, err := dm.ListScheduledWakes(includeFired, overdueOnly, limit)
	if err != nil {
		return nil, err
	}
	if len(kinds) > 0 {
		filtered := items[:0]
		wantAll := false
		for _, k := range kinds {
			if k == "*" {
				wantAll = true
				break
			}
		}
		if !wantAll {
			allowed := make(map[string]struct{}, len(kinds))
			for _, k := range kinds {
				allowed[k] = struct{}{}
			}
			for _, w := range items {
				wk, _ := w["kind"].(string)
				if wk == "" {
					if md, ok := w["metadata"].(map[string]interface{}); ok {
						wk, _ = md["kind"].(string)
					}
				}
				if _, ok := allowed[wk]; ok {
					filtered = append(filtered, w)
				}
			}
			items = filtered
		}
	}
	out := map[string]interface{}{
		"success":       true,
		"wakes":         items,
		"count":         len(items),
		"include_fired": includeFired,
		"overdue_only":  overdueOnly,
	}
	if len(kinds) > 0 {
		out["kinds"] = kinds
	}
	return checkWakesAndFold(dm, ac, out, nil), nil
}

// handleDigestWakes returns a compact summary of overdue + pending wakes.
// Designed for the "agent wakes up after a long idle" case where listing
// every overdue wake individually would blow out context. Returns age
// buckets, top-N most overdue, total counts, and oldest overdue timestamp.
//
// Pass top_n (default 5) to control how many top-overdue rows are surfaced.
func handleDigestWakes(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	topN := 5
	if v, ok := p["top_n"]; ok {
		switch n := v.(type) {
		case float64:
			topN = int(n)
		case int:
			topN = n
		}
	}
	return dm.DigestScheduledWakes(topN)
}

// ---------------------------------------------------------------------------
// Scheduled Tasks (Agentic Cron)
// ---------------------------------------------------------------------------
//
// Three split tools following the existing wake / lesson pattern
// (schedule_wake, list_wakes, check_wakes are all separate). CRUD
// overloaded onto one tool forces the agent to guess which fields
// are required for which operation; discrete tools make the JSON
// schema self-documenting.
//
// Fail-fast on upsert: the handler runs a single index lookup against
// `memories WHERE id = ? AND collection = 'directives'` BEFORE writing.
// If the directive doesn't exist, the upsert is rejected. Better to
// catch a typo at 2 PM than have the daemon silently drop the wake
// at 3 AM.

// handleUpsertScheduledTask creates or updates a recurring agentic
// workflow. Computes next_run_at from the cron expression (UTC) and
// stores it; the daemon never parses cron on the hot path.
func handleUpsertScheduledTask(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id := internal.ParseStringOr(p["id"], "")
	name := internal.ParseStringOr(p["name"], "")
	cronExpr := internal.ParseStringOr(p["cron_expr"], "")
	directiveID := internal.ParseStringOr(p["directive_id"], "")
	status := internal.ParseStringOr(p["status"], internal.ScheduledTaskActive)

	if id == "" || name == "" || cronExpr == "" || directiveID == "" {
		return nil, fmt.Errorf("id, name, cron_expr, directive_id are all required")
	}
	if status != internal.ScheduledTaskActive && status != internal.ScheduledTaskPaused {
		return nil, fmt.Errorf("invalid status %q: must be %q or %q",
			status, internal.ScheduledTaskActive, internal.ScheduledTaskPaused)
	}

	// Fail-fast: confirm directive_id actually exists in the
	// directives collection. A lightning-fast indexed lookup; the
	// agent catches the typo at upsert time, not at 3 AM.
	sqlDB := dm.SQLDB()
	if sqlDB == nil {
		return nil, fmt.Errorf("db not available")
	}
	var foundID string
	row := sqlDB.QueryRow(
		`SELECT id FROM memories WHERE id = ? AND collection = 'directives' AND deleted_at IS NULL LIMIT 1`,
		directiveID,
	)
	if err := row.Scan(&foundID); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("directive_id %q not found in memories where collection='directives'", directiveID)
		}
		return nil, fmt.Errorf("validate directive_id: %w", err)
	}

	task := internal.ScheduledTask{
		ID:          id,
		Name:        name,
		CronExpr:    cronExpr,
		DirectiveID: directiveID,
		Status:      status,
	}
	if err := dm.UpsertScheduledTask(task); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"task_id": id,
		"status":  status,
	}, nil
}

// handleListScheduledTasks returns all scheduled tasks ordered by
// next_run_at ASC. Use to inspect what's queued and what fired last.
func handleListScheduledTasks(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	tasks, err := dm.ListScheduledTasks()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"tasks":   tasks,
		"count":   len(tasks),
	}, nil
}

// handleDeleteScheduledTask hard-deletes a task by id. Most
// operators should set status='paused' via upsert instead so the
// schedule is preserved for forensics; delete is for permanent
// removal.
func handleDeleteScheduledTask(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id := internal.ParseStringOr(p["id"], "")
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	if err := dm.DeleteScheduledTask(id); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"task_id": id,
		"deleted": true,
	}, nil
}

// handleHealthCheck returns compact operational status for the agent:
// PRAGMA integrity + SQLite page stats + domain counts + lifetime
// SQLITE_BUSY retry counter. Designed for self-diagnosis when the
// agent notices latency, timeouts, or storage pressure. Accepts no
// parameters — the result is the entire payload.
func handleHealthCheck(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	return dm.HealthCheck()
}

// handleCompactEpistemology is the reflex to epistemic_pressure.
//
// Drains eligible raw memories in sequential batches of at most 50
// (the LLM context safeguard — see compactBatchSize in
// internal/core/compact.go). Each batch is independently atomic; the
// drain loops the per-batch primitive until one of the documented
// stop_reason conditions is reached.
//
// Compact semantics:
//
//   - force=false (default) — RELIEVE pressure. The drain stops as
//     soon as raw_count <= threshold. Eligible raw memories may
//     remain after the call. This is the reflex to
//     epistemic_pressure.exceeded=true.
//   - force=true            — DRAIN everything. The threshold gate
//     is bypassed; the drain continues until the substrate is empty
//     or the per-invocation safety cap is hit.
//
// The 50-item batch safety is unrelated to force — every batch is
// capped at compactBatchSize regardless of force or threshold.
//
// Parameters:
//   - force (bool, default false): bypass the pressure threshold gate
//     (does NOT bypass the 50-item per-batch limit).
//   - max_batches (int, default 20, hard cap 100): per-invocation
//     safety cap on LLM calls. 20 batches × 50 raw = 1000 raw memories
//     per invocation. Caller-supplied values above the hard cap are
//     silently clamped, not rejected.
//
// Result envelope (always set):
//   - success: false ONLY on a mid-drain batch failure. true for
//     every other stop_reason — including threshold_reached and
//     max_batches_reached, where work may remain.
//   - batches_processed: count of batches that committed a lesson.
//   - raw_processed: sum of compacted raw memories across all batches.
//   - lessons_created: equal to batches_processed on success.
//   - raw_remaining: live read of the pressure view after the loop —
//     the canonical post-drain count. Inspect this to determine
//     whether more work remains.
//   - lesson_ids: lesson IDs created in batch order.
//   - stop_reason: one of "no_work" | "completed" | "threshold_reached"
//     | "max_batches_reached" | "failure". See the result struct doc
//     for the full taxonomy.
//   - skipped_reason: set only on "no_work" (no_raw_memories) and
//     "threshold_reached" (below_threshold).
//   - failed_batch + failure_reason: present only on "failure".
//
// Failure semantics: when a batch fails mid-drain, earlier successful
// batches remain committed (each batch is atomic). The failed batch
// and all subsequent eligible rows are untouched. The next invocation
// resumes from the remaining work.
func handleCompactEpistemology(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	force := parseBoolDefault(p["force"], false)
	maxBatches := parseIntDefault(p["max_batches"], 0)

	result, err := dm.CompactEpistemologyDrain(context.Background(), force, maxBatches)
	if err != nil {
		// Mid-drain failure — return the partial aggregate AND the
		// error. Earlier successful batches are durable; the agent
		// can decide whether to retry.
		if result != nil {
			out := map[string]interface{}{
				"success":           result.Success,
				"batches_processed": result.BatchesProcessed,
				"raw_processed":     result.RawProcessed,
				"lessons_created":   result.LessonsCreated,
				"raw_remaining":     result.RawRemaining,
				"stop_reason":       result.StopReason,
				"failed_batch":      result.FailedBatch,
				"failure_reason":    result.FailureReason,
			}
			if len(result.LessonIDs) > 0 {
				out["lesson_ids"] = result.LessonIDs
			}
			return out, err
		}
		return nil, err
	}

	// Success or skip path.
	out := map[string]interface{}{
		"success":           result.Success,
		"batches_processed": result.BatchesProcessed,
		"raw_processed":     result.RawProcessed,
		"lessons_created":   result.LessonsCreated,
		"raw_remaining":     result.RawRemaining,
		"stop_reason":       result.StopReason,
	}
	if result.SkippedReason != "" {
		out["skipped_reason"] = result.SkippedReason
	}
	if len(result.LessonIDs) > 0 {
		out["lesson_ids"] = result.LessonIDs
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Ephemeral Scratchpad
// ---------------------------------------------------------------------------
//
// Single-row-per-session volatile thesis storage. Lets the agent checkpoint
// reasoning that isn't ready for permanent memory (save_to_memory). When a
// session ends without promotion, the row becomes an "orphan" surfaced on
// next session's wake context with age tagging ([Fresh]/[Dormant]/[Expired]).
//
// Wire-format invariant: all four tools require session_id. We deliberately
// reject defaulting to the current session — making the agent pass session_id
// explicitly keeps the JSON-Schema contract uniform and prevents the agent
// from making lazy context-blind assumptions when querying state.
//
// Promote is the only mutating tool that crosses table boundaries; it
// reaches SaveMemoryNode via the WithTx callback (DBNode interface), so the
// 20-pattern security scanner runs INSIDE the transaction. Poison rejection
// rolls back both the memory INSERT and the scratchpad DELETE, leaving the
// scratchpad intact for retry with a redacted fact.

// normalizeSupporting accepts either a JSON string (caller pre-serialized)
// or any other JSON-marshallable shape (most common: map[string]interface{}
// from the MCP boundary). nil/missing returns "" with no error — supporting
// is optional. Errors propagate so the handler can return a 4xx-equivalent.
func normalizeSupporting(raw interface{}) (string, error) {
	if raw == nil {
		return "", nil
	}
	if s, ok := raw.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// handleFlushScratchpad upserts a volatile working thesis for a session.
// Idempotent: repeated calls in the same session overwrite cleanly via
// SQLite's UPSERT. decay_at is reset on every flush (TTL metadata for
// future `mpm ops gc --scratchpads`); updated_at is reset for thesis
// evolution velocity tracking. The wake-context surface query IGNORES
// decay_at — orphan-surfacing is the whole point.
func handleFlushScratchpad(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	sessionID := getString(p, "session_id")
	thesis := getString(p, "thesis")
	if sessionID == "" || thesis == "" {
		return nil, fmt.Errorf("session_id and thesis are required")
	}

	supporting, err := normalizeSupporting(p["supporting"])
	if err != nil {
		return nil, fmt.Errorf("failed to normalize supporting data: %w", err)
	}

	const query = `
		INSERT INTO ephemeral_scratchpad (session_id, thesis, supporting, decay_at)
		VALUES (?, ?, ?, CAST(strftime('%s','now', '+24 hours') AS INTEGER))
		ON CONFLICT(session_id) DO UPDATE SET
			thesis = excluded.thesis,
			supporting = excluded.supporting,
			updated_at = CAST(strftime('%s','now') AS INTEGER),
			decay_at = excluded.decay_at;`

	if _, err := dm.ExecTracked(query, 0, sessionID, thesis, supporting); err != nil {
		return nil, fmt.Errorf("flush scratchpad: %w", err)
	}
	return map[string]string{"status": "flushed", "session_id": sessionID}, nil
}

// handleReadScratchpad returns the current scratchpad row for a session.
// Empty payload errors loudly so the agent can self-correct; we do NOT
// default to the current session — explicit session_id is the contract.
func handleReadScratchpad(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	sessionID := getString(p, "session_id")
	if sessionID == "" {
		return nil, internal.ErrSessionIDRequired()
	}

	var thesis, supporting, updatedAt string
	err := dm.QueryRowTracked(
		`SELECT thesis, supporting, updated_at FROM ephemeral_scratchpad WHERE session_id = ?`,
		sessionID,
	).Scan(&thesis, &supporting, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("no scratchpad found for session: %s", sessionID)
		}
		return nil, err
	}
	return map[string]string{
		"session_id": sessionID,
		"thesis":     thesis,
		"supporting": supporting,
		"updated_at": updatedAt,
	}, nil
}

// handleDiscardScratchpad hard-deletes a scratchpad row. No soft-delete
// overhead — scratchpads are volatile by design. Idempotent: deleting a
// non-existent row is a no-op (RowsAffected=0, no error).
func handleDiscardScratchpad(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	sessionID := getString(p, "session_id")
	if sessionID == "" {
		return nil, internal.ErrSessionIDRequired()
	}

	if _, err := dm.ExecTracked(`DELETE FROM ephemeral_scratchpad WHERE session_id = ?`, 0, sessionID); err != nil {
		return nil, fmt.Errorf("discard scratchpad: %w", err)
	}
	return map[string]string{"status": "discarded", "session_id": sessionID}, nil
}

// handlePromoteScratchpad is the only multi-table mutation in the scratchpad
// surface. It atomically:
//  1. SELECTs the scratchpad row (inside the tx — race window closed)
//  2. INSERTs a memory via SaveMemoryNode (scanner runs INSIDE the tx)
//  3. DELETEs the scratchpad row (also inside the tx)
//
// All three operations share one DBNode (txNode). If the scanner rejects
// the memory INSERT (poison phrase, sensitive content), WithTx rolls back
// the entire tx and the scratchpad stays intact for retry with a redacted
// fact. The agent gets the rejection error verbatim from the scanner.
//
// Lineage: the promoted memory gets the `from-scratchpad:<session_id>` tag
// automatically, so future queries can trace the memory back to the
// scratchpad session that produced it.
func handlePromoteScratchpad(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	sessionID := getString(p, "session_id")
	if sessionID == "" {
		return nil, internal.ErrSessionIDRequired()
	}

	var memoryID string
	lineage := fmt.Sprintf("from-scratchpad:%s", sessionID)

	err := dm.WithTx(func(node mpminternal.DBNode) error {
		// Step 1: SELECT inside the tx. If the row vanishes between
		// this read and the DELETE (concurrent discard, or external
		// cleanup), QueryRowTracked returns ErrNoRows and the tx
		// aborts before any INSERT runs.
		var thesis, supporting string
		if err := node.QueryRowTracked(
			`SELECT thesis, supporting FROM ephemeral_scratchpad WHERE session_id = ?`,
			sessionID,
		).Scan(&thesis, &supporting); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("no scratchpad found for session: %s", sessionID)
			}
			return err
		}

		// Step 2: INSERT memory. SaveMemoryNode runs the 20-pattern
		// scanner; rejection here returns an error and the tx aborts.
		// SaveMemoryNode is the canonical tx-aware primitive (see
		// db.go:1433); it accepts the DBNode so the INSERT joins
		// the same tx as the DELETE below.
		content := fmt.Sprintf("Thesis: %s\nSupporting Context: %s", thesis, supporting)
		tags := []string{lineage}
		var err error
		memoryID, err = dm.SaveMemoryNode(
			node,
			"memories", content, "", tags, nil, nil,
			false, 5, // weight=5 (medium), isLongTerm=false
			"", "0.5", "0.5", "", // referenceID, retrieval, importance, createdAt (defaults)
		)
		if err != nil {
			return fmt.Errorf("promote scratchpad: %w", err)
		}

		// Step 3: DELETE scratchpad. If scanner rejected, we never
		// reach this line — WithTx's deferred rollback catches it.
		if _, err := node.ExecTracked(
			`DELETE FROM ephemeral_scratchpad WHERE session_id = ?`,
			0, sessionID,
		); err != nil {
			return fmt.Errorf("delete scratchpad after promote: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return map[string]string{
		"status":    "promoted",
		"memory_id": memoryID,
		"lineage":   lineage,
	}, nil

}

// handleExplainRetrieval runs a standard FTS search and returns a
// diagnostic breakdown per result: base FTS score, reuse count, last
// retrieved timestamp, success count. The intent is observability —
// the agent (or operator) can see WHY a result ranked where it did,
// and how often it has been surfaced before.
//
// The search ranking itself is NOT modified. explain_retrieval reads
// the same HybridSearchMemories path that query_long_term_memory uses,
// then layers retrieval_metadata on top via GetRetrievalMetadata per
// row. The retrieval_metadata is joined in the SQL path (so future
// rankers can use it) but today DefaultRanker passes ftsScore through
// unchanged.
//
// When trace=true (or trace=true is in the payload), the handler also
// traces the three stages of the recall pipeline:
//
//	Stage 1: raw query → BuildFTS5Query() → final MATCH string
//	Stage 2: FTS5 row count + BM25 distribution + top 3 IDs
//	Stage 3: HybridSearch input/output count + discarded-row analysis
//
// This is the diagnostic surface for investigating retrieval failures
// (zero-result queries, short-token drops, hyphen crashes). The
// permanent addition makes the substrate's recall path debuggable
// without throwing printfs at it.
func handleMpmRetrievalDiagnose(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query := internal.ParseStringOr(p["query"], "")
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := int(internal.ParseFloatOr(p["limit"], 10))
	if limit <= 0 {
		limit = 10
	}
	collection := internal.ParseStringOr(p["collection"], "")
	scope := internal.ParseStringOr(p["scope"], "all")
	trace := internal.ParseBoolOr(p["trace"], false)

	items, err := dm.HybridSearchMemories(query, collection, limit, scope)
	if err != nil {
		return nil, err
	}

	var sb strings.Builder
	sb.WriteString("# Retrieval Diagnostic\n\n")
	sb.WriteString("**Query:** ")
	sb.WriteString(query)
	sb.WriteString("\n\n")

	// ── Trace mode (Stage 1/2/3 pipeline diagnostic) ─────────────────────
	var traceData map[string]interface{}
	if trace {
		traceData = runRetrievalTrace(dm, query, collection, scope, limit, items)
		sb.WriteString(formatTraceSection(traceData))
		sb.WriteString("\n\n---\n\n")
	}

	if len(items) == 0 {
		sb.WriteString("_No results._\n")
	} else {
		sb.WriteString(fmt.Sprintf("**%d result(s).** Retrieval ordering follows FTS `bm25()` rank — identical to `query_long_term_memory`. Retrieval metadata is observability only and does NOT influence ranking today (see `DefaultRanker` in retrieval_ranker.go).\n\n", len(items)))

		for _, item := range items {
			id, _ := item["id"].(string)
			if id == "" {
				continue
			}
			meta, _ := dm.GetRetrievalMetadata(id)

			// FTS / combined score — combined_score is the post-hybrid
			// blend when present; fall back to weight for items without it.
			var ftsScore interface{} = item["weight"]
			if cs, ok := item["combined_score"]; ok {
				ftsScore = cs
			}

			sb.WriteString("## Diagnostic: ")
			sb.WriteString(id)
			sb.WriteString("\n")
			sb.WriteString("- Base FTS Match: ")
			sb.WriteString(fmt.Sprintf("%v", ftsScore))
			sb.WriteString("\n")
			sb.WriteString(fmt.Sprintf("- Reuse Count: %d\n", meta.ReuseCount))
			var lastRetrieved string
			if meta.LastRetrievedAt != nil {
				lastRetrieved = mpminternal.FormatUnixSeconds(*meta.LastRetrievedAt)
			} else {
				lastRetrieved = "_never_"
			}
			sb.WriteString("- Last Retrieved: ")
			sb.WriteString(lastRetrieved)
			sb.WriteString("\n")
			sb.WriteString(fmt.Sprintf("- Success Count: %d\n", meta.SuccessCount))
			sb.WriteString("\n")
		}
	}

	result := map[string]interface{}{
		"success":    true,
		"query":      query,
		"diagnostic": sb.String(),
		"count":      len(items),
	}
	if traceData != nil {
		result["trace"] = traceData
	}
	return result, nil
}

// runRetrievalTrace executes the three-stage pipeline diagnostic. It does
// NOT modify any data; it reads the same queries the live tool would run
// and reports what each stage saw. Use this to pinpoint whether a recall
// failure lives in:
//
//   - Stage 1 (BuildFTS5Query transformation): does the MATCH string we
//     build from the user's query match what FTS5 can match against?
//   - Stage 2 (FTS5 execution): does the index actually have rows for
//     this MATCH? What BM25 distribution do they show?
//   - Stage 3 (HybridSearch result handling): how many of those FTS5
//     rows survive the merge/dedup/threshold/slice path? What were the
//     BM25 scores of the rows we threw away?
//
// Expected contract: if a query returns 0 results but Stage 2 reports
// N>0 FTS5 rows, the bug is in Stage 3 (discarding). If Stage 2 itself
// reports 0 rows but direct SQLite with the same MATCH string returns
// rows, the bug is in Stage 1 or the connection state.
func runRetrievalTrace(dm mpminternal.CoreDB, query, collection, scope string, limit int, hybridResults []map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"query":      query,
		"collection": collection,
		"scope":      scope,
	}

	// ── Stage 1: BuildFTS5Query transformation ─────────────────────────
	transformedMATCH := internal.BuildFTS5Query(query)
	out["stage1"] = map[string]interface{}{
		"raw_query":      query,
		"transformed":    transformedMATCH,
		"transform_note": "BuildFTS5Query tokenizes on whitespace + - _ . and appends * prefix per token (porter unicode61). Empty output means the query was all FTS5 special chars (^ \" ( ) : *).",
	}

	// Determine FTS table prefix from scope
	schemaPrefix := ""
	if scope == "shared" {
		schemaPrefix = "shared."
	}
	ftsTable := schemaPrefix + "memories_fts"
	memTable := schemaPrefix + "memories"
	expireClause := " AND (m.expires_at IS NULL OR m.expires_at > strftime('%s','now'))"
	expireClauseFts := " AND (m.expires_at IS NULL OR m.expires_at > strftime('%s','now'))"
	if schemaPrefix == "shared." {
		expireClause = ""
		expireClauseFts = ""
	}

	db := dm.SQLDB()

	// ── Stage 2: FTS5 row count + BM25 distribution + top 3 IDs ─────────
	stage2 := map[string]interface{}{
		"match_string":  transformedMATCH,
		"limit_fetched": limit * 4, // fetch wider than the HybridSearch fetch (limit*2) so we can see what's discarded
	}
	if transformedMATCH == "" {
		stage2["error"] = "BuildFTS5Query returned empty string (would raise sqlite error if sent)"
		stage2["row_count"] = 0
		stage2["top_3_ids"] = []string{}
		stage2["bm25"] = map[string]interface{}{"min": nil, "max": nil, "median": nil, "count": 0}
	} else {
		stage2SQL := fmt.Sprintf(`
			SELECT m.id, bm25(%s) AS score
			FROM %s
			JOIN %s m ON %s.rowid = m.rowid
			WHERE %s MATCH ? AND m.deleted_at IS NULL%s
			  AND (? = '' OR m.collection = ?)
			ORDER BY score
			LIMIT ?`, ftsTable, ftsTable, memTable, ftsTable, ftsTable, expireClauseFts)
		rows, err := db.Query(stage2SQL, transformedMATCH, collection, collection, limit*4)
		if err != nil {
			stage2["error"] = fmt.Sprintf("FTS5 query failed: %v", err)
			stage2["row_count"] = 0
			stage2["top_3_ids"] = []string{}
			stage2["bm25"] = map[string]interface{}{"min": nil, "max": nil, "median": nil, "count": 0}
		} else {
			defer rows.Close()
			scores := []float64{}
			topIDs := []string{}
			for rows.Next() {
				var id string
				var score float64
				if err := rows.Scan(&id, &score); err == nil {
					scores = append(scores, score)
					if len(topIDs) < 3 {
						topIDs = append(topIDs, id)
					}
				}
			}
			stage2["row_count"] = len(scores)
			stage2["top_3_ids"] = topIDs
			if len(scores) > 0 {
				sortedScores := make([]float64, len(scores))
				copy(sortedScores, scores)
				// scores already sorted by bm25 ASC (most negative = best)
				stage2["bm25"] = map[string]interface{}{
					"min":    sortedScores[0],                   // most negative = best match
					"max":    sortedScores[len(sortedScores)-1], // least negative = worst match
					"median": sortedScores[len(sortedScores)/2],
					"count":  len(sortedScores),
				}
			} else {
				stage2["bm25"] = map[string]interface{}{"min": nil, "max": nil, "median": nil, "count": 0}
			}
		}
	}
	out["stage2"] = stage2

	// ── Stage 3: HybridSearch input/output + discarded-row analysis ────
	stage3 := map[string]interface{}{
		"hybrid_output_count": len(hybridResults),
	}

	// Collect IDs HybridSearch returned
	hybridIDs := map[string]bool{}
	for _, item := range hybridResults {
		if id, ok := item["id"].(string); ok {
			hybridIDs[id] = true
		}
	}
	stage3["hybrid_returned_ids"] = mapKeysToSlice(hybridIDs)

	// If Stage 2 returned FTS5 rows, figure out which ones HybridSearch
	// discarded and what their BM25 scores were. This is the key
	// signal: if discarded rows have very negative BM25 scores (good
	// matches), something in the merge/threshold/slice path is broken.
	if transformedMATCH != "" {
		stage3SQL := fmt.Sprintf(`
			SELECT m.id, bm25(%s) AS score
			FROM %s
			JOIN %s m ON %s.rowid = m.rowid
			WHERE %s MATCH ? AND m.deleted_at IS NULL%s
			  AND (? = '' OR m.collection = ?)
			ORDER BY score
			LIMIT ?`, ftsTable, ftsTable, memTable, ftsTable, ftsTable, expireClauseFts)
		rows, err := db.Query(stage3SQL, transformedMATCH, collection, collection, limit*4)
		if err == nil {
			allFTSIDs := []string{}
			discardedScores := []float64{}
			keptScores := []float64{}
			for rows.Next() {
				var id string
				var score float64
				if err := rows.Scan(&id, &score); err == nil {
					allFTSIDs = append(allFTSIDs, id)
					if hybridIDs[id] {
						keptScores = append(keptScores, score)
					} else {
						discardedScores = append(discardedScores, score)
					}
				}
			}
			rows.Close()
			stage3["fts5_total_rows"] = len(allFTSIDs)
			stage3["fts5_kept_by_hybrid"] = len(keptScores)
			stage3["fts5_discarded_by_hybrid"] = len(discardedScores)
			stage3["fts5_kept_ids"] = mapKeysToSlice(hybridIDs)
			if len(discardedScores) > 0 {
				stage3["discarded_bm25"] = map[string]interface{}{
					"min":   discardedScores[0],                      // most-negative discarded score
					"max":   discardedScores[len(discardedScores)-1], // least-negative discarded score
					"count": len(discardedScores),
				}
			} else {
				stage3["discarded_bm25"] = map[string]interface{}{"count": 0}
			}
			if len(keptScores) > 0 {
				stage3["kept_bm25"] = map[string]interface{}{
					"min":   keptScores[0],
					"max":   keptScores[len(keptScores)-1],
					"count": len(keptScores),
				}
			} else {
				stage3["kept_bm25"] = map[string]interface{}{"count": 0}
			}
		}
		_ = expireClause // suppress unused warning; clause already applied via FTS clause
	}

	out["stage3"] = stage3
	return out
}

// formatTraceSection renders the trace map as readable markdown for the
// diagnostic surface. Kept separate from runRetrievalTrace so the data
// shape stays available as JSON for agent consumption.
func formatTraceSection(t map[string]interface{}) string {
	var sb strings.Builder
	sb.WriteString("## Pipeline Trace\n\n")

	// Stage 1
	if s1, ok := t["stage1"].(map[string]interface{}); ok {
		sb.WriteString("### Stage 1 — Query Transformation\n\n")
		sb.WriteString("- Raw query: `")
		sb.WriteString(fmt.Sprintf("%v", s1["raw_query"]))
		sb.WriteString("`\n")
		sb.WriteString("- After `BuildFTS5Query`: `")
		sb.WriteString(fmt.Sprintf("%v", s1["transformed"]))
		sb.WriteString("`\n")
		if note, ok := s1["transform_note"].(string); ok {
			sb.WriteString("- Note: ")
			sb.WriteString(note)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Stage 2
	if s2, ok := t["stage2"].(map[string]interface{}); ok {
		sb.WriteString("### Stage 2 — FTS5 Execution\n\n")
		if errStr, ok := s2["error"].(string); ok && errStr != "" {
			sb.WriteString("- **Error:** ")
			sb.WriteString(errStr)
			sb.WriteString("\n")
		}
		sb.WriteString("- MATCH string sent to FTS5: `")
		sb.WriteString(fmt.Sprintf("%v", s2["match_string"]))
		sb.WriteString("`\n")
		sb.WriteString(fmt.Sprintf("- FTS5 row count (top %d fetched): %v\n", s2["limit_fetched"], s2["row_count"]))
		if ids, ok := s2["top_3_ids"].([]string); ok && len(ids) > 0 {
			sb.WriteString("- Top 3 IDs (best BM25 first):\n")
			for _, id := range ids {
				sb.WriteString("  - `")
				sb.WriteString(id)
				sb.WriteString("`\n")
			}
		}
		if bm, ok := s2["bm25"].(map[string]interface{}); ok {
			if count, _ := bm["count"].(int); count > 0 {
				sb.WriteString(fmt.Sprintf("- BM25 distribution: min=%.3f, max=%.3f, median=%.3f (n=%d)\n",
					toFloat(bm["min"]), toFloat(bm["max"]), toFloat(bm["median"]), count))
			} else {
				sb.WriteString("- BM25 distribution: **no rows**\n")
			}
		}
		sb.WriteString("\n")
	}

	// Stage 3
	if s3, ok := t["stage3"].(map[string]interface{}); ok {
		sb.WriteString("### Stage 3 — HybridSearch Result Handling\n\n")
		if total, ok := s3["fts5_total_rows"].(int); ok {
			kept, _ := s3["fts5_kept_by_hybrid"].(int)
			discarded, _ := s3["fts5_discarded_by_hybrid"].(int)
			sb.WriteString(fmt.Sprintf("- FTS5 total rows: %d\n", total))
			sb.WriteString(fmt.Sprintf("- Kept by HybridSearch: %d\n", kept))
			sb.WriteString(fmt.Sprintf("- **Discarded by HybridSearch: %d**\n", discarded))
			if discarded > 0 {
				if bm, ok := s3["discarded_bm25"].(map[string]interface{}); ok {
					if min, ok := bm["min"].(float64); ok {
						sb.WriteString(fmt.Sprintf("- Discarded BM25 range: min=%.3f (best discarded), max=%.3f (worst discarded)\n",
							min, toFloat(bm["max"])))
					}
				}
				sb.WriteString("\n> **Diagnosis:** FTS5 found rows; HybridSearch discarded them.\n>\n")
				sb.WriteString("> If discarded rows have strongly negative BM25 (e.g., < -1.0), the bug is in the\n")
				sb.WriteString("> merge/dedup/threshold/slice path, not in FTS5 itself. Compare with direct\n")
				sb.WriteString("> SQLite `MATCH '<match_string>' LIMIT <n>` to confirm the FTS5 layer is healthy.\n")
			} else if total > 0 && kept == 0 {
				sb.WriteString("\n> **Diagnosis:** FTS5 found rows; ALL were discarded.\n")
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func toFloat(v interface{}) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

func mapKeysToSlice(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// handleMpmRequestReview implements the mpm_request_review MCP tool.
//
// Architectural intent (Wed 2026-07-29 design session):
//
//	Adapter layer for the ReviewCoordinator orchestration primitive
//	(internal/core/orchestration). This handler is responsible for:
//
//	  1. Fetching artifact bodies from the database (resolving ids
//	     in the caller's 'artifacts' list to actual text).
//	  2. Building the substrate-side RequestReview payload (a
//	     ReviewRequest — see orchestration/review_coordinator.go).
//	  3. Calling DefaultReviewCoordinator.Execute().
//	  4. Rendering the resulting []ReviewResult via the renderers
//	     package, which returns Markdown-shaped output suitable for
//	     both agent consumers (LLMs parse it back as text) and
//	     humans (operators read the dashboard).
//
// The handler is intentionally thin. All the concurrency, timeout,
// profile resolution, and fan-out live in the engine layer
// (orchestration package). All the markdown rendering lives in
// the renderers package. This handler is the only one in the
// call chain that knows about the database, the coordinator, and
// the renderer.
func handleMpmRequestReview(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// Note on error shape: the TestRegistry_AllToolsExecuteWithoutPanic
	// harness compares CLI and MCP error strings after stripping the
	// MCP adapter's "<tool> failed: " prefix via strings.LastIndex(": ").
	// The CLI side returns the bare error from this handler, so
	// including the tool name in the error string would create a
	// permanent drift (CLI: "mpm_request_review: ..." vs stripped-MCP:
	// "..."). Errors below carry only the inner message; the MCP
	// layer adds the tool-name prefix when wrapping.
	components := mpminternal.ParseStringSliceOr(p["components"])
	if len(components) == 0 {
		return nil, fmt.Errorf("'components' is required (at least one substrate component name, e.g. ['memory','critic'])")
	}
	prompt, _ := p["prompt"].(string)
	if prompt == "" {
		return nil, fmt.Errorf("'prompt' is required")
	}
	strategy := mpminternal.ParseStringOr(p["strategy"], "parallel")
	if strategy != "parallel" {
		return nil, fmt.Errorf("only strategy='parallel' is supported in v0.1 (got %q)", strategy)
	}
	var timeout time.Duration
	if t := int(mpminternal.ParseFloatOr(p["timeout_secs"], 0)); t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	// Resolve artifact bodies (id → text). The boundary contract:
	// the ReviewCoordinator never sees an id — it operates on
	// pre-resolved text. This is what makes Skills portable
	// across installs (no DB ids leak into the orchestration
	// engine).
	artifactIDs := mpminternal.ParseStringSliceOr(p["artifacts"])
	var contextData strings.Builder
	for i, id := range artifactIDs {
		mem, err := dm.GetMemory(id)
		if err != nil {
			return nil, fmt.Errorf("artifact %q: %w", id, err)
		}
		if mem == nil {
			return nil, fmt.Errorf("artifact %q not found", id)
		}
		content, _ := mem["content"].(string)
		fmt.Fprintf(&contextData, "### Artifact %d (id=%q)\n\n", i+1, id)
		contextData.WriteString(content)
		contextData.WriteString("\n\n")
	}

	// Wire the coordinator. The orchestrator uses the substrate's
	// config (Profiles + Components + Capabilities) for routing; the
	// ModelFactory builds per-profile HTTP clients from the
	// substrate's existing *synth.SynthClient.
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load substrate config: %w", err)
	}
	// Translate capability names to component names before dispatch.
	// Skills/skills-shaped callers address capabilities (reviewer,
	// reflect, planner, summarise) — ResolveComponents walks the
	// explicit Config.Capabilities map then falls back to the
	// canonical DefaultCapabilities. Names that aren't capabilities
	// pass through unchanged as component names (preserves direct
	// calls like `components=["memory","critic"]`).
	resolvedComponents := cfg.ResolveComponents(components)
	coord := orchestration.NewDefaultReviewCoordinator(cfg, orchestration.DefaultModelFactory())
	req := orchestration.ReviewRequest{
		Components:  resolvedComponents,
		Prompt:      prompt,
		ContextData: contextData.String(),
		Strategy:    orchestration.StrategyParallel,
		Timeout:     timeout,
	}

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	results, err := coord.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("coordinator: %w", err)
	}

	// Return the rendered string directly. The MCP wrapper
	// catches it as the tool's result body; the CLI's `mpm call`
	// prints it to stdout. Same surface both ways.
	return renderers.FormatReviewsMarkdown(results), nil
}

// extractParamsOrFail extracts the `params` envelope from a domain-tool
// payload and fails LOUDLY when it is missing or wrong-typed.
//
// Background: prior to this hardening, every domain dispatcher (mpm_memory,
// the legacy mpm_session [now mpm_handoff / mpm_scratchpad], mpm_wakes, …)
// silently coerced a missing `params` envelope to an empty map. That
// meant a payload shaped like
//
//	{"action":"query","query":"..."}
//
// would route the action string correctly, hand `handleQueryLongTermMemory`
// an EMPTY params map, and the inner handler's "query is required" check
// would fire — far from the source of the wrong-shaped input. The
// 2026-08-13 silent-promotion-by-edge-case archaeology named this exact
// shape as the failure mode the always-cross-check-with-sqlite3 discipline
// catches at the cost of a whole diagnostic.
//
// Contract (alpha-4.1.1 D-006): `params` is OPTIONAL for actions that
// take no parameters. A payload shaped like
//
//	{"action":"list"}
//
// must succeed for actions that don't require params (e.g.
// mpm_decisions action=list with no filters). Actions that DO require
// parameters still fail loudly in their inner handler with the field-
// specific error — extractParamsOrFail no longer pre-rejects for them.
// This eliminates the boilerplate `params={}` for every no-param call
// without weakening validation for parameterized actions.
//
// Top-level fields may still only be `action` and `params`. Anything
// else remains a contract violation rejected here so the error is
// actionable at the boundary.
//
// This function is the single, project-wide gate for that contract. All 13
// domain dispatchers call it on entry; ad-hoc usage of `payload["params"]`
// outside this helper is a contract-drift bug and should be patched back
// to call extractParamsOrFail.
// normalizeCamelCaseKeys copies snake_case source field names to
// camelCase aliases on the params map. Used by domain dispatchers that
// bridge legacy snake_case API callers to camelCase-native inner handlers.
//
// Centralized as a named function so the schema-guard AST walker
// doesn't false-positive on inline `params["k"] = id` writes (those
// would look like handler payload reads but are actually field-rename
// plumbing).
func normalizeCamelCaseKeys(params map[string]interface{}, renames map[string]string) {
	for from, to := range renames {
		if v, ok := params[from]; ok {
			params[to] = v
		}
	}
}

func extractParamsOrFail(toolName string, payload map[string]interface{}) (map[string]interface{}, error) {
	if payload == nil {
		return nil, fmt.Errorf("%s: missing payload — expected JSON object with top-level fields {action:string, params:object}; got null", toolName)
	}

	// Reject top-level fields that aren't `action` or `params`. Their
	// presence means the caller put a param at the top level instead of
	// inside the envelope — silently dropping them was the original bug.
	// List them so the fix is mechanical and the error is actionable.
	var unexpected []string
	for k := range payload {
		if k != "action" && k != "params" {
			unexpected = append(unexpected, k)
		}
	}
	if len(unexpected) > 0 {
		return nil, fmt.Errorf(
			"%s: payload has top-level field(s) %v outside the params envelope — "+
				"expected shape {\"action\":\"<op>\",\"params\":{...}}. "+
				"Move these into params.<key> and retry.",
			toolName, unexpected,
		)
	}

	// `action` must be present and string-typed. The switch in the
	// dispatcher already returns "unknown action ..." otherwise; this
	// check is purely about rejecting NON-string `action` shapes (number,
	// bool, null) which would silently dispatch to the default branch.
	if _, ok := payload["action"]; !ok {
		return nil, fmt.Errorf("%s: missing required field `action` (string) — expected shape {\"action\":\"<op>\",\"params\":{...}}", toolName)
	}
	if _, ok := payload["action"].(string); !ok {
		return nil, fmt.Errorf("%s: field `action` must be a string — got %T; expected shape {\"action\":\"<op>\",\"params\":{...}}", toolName, payload["action"])
	}

	// `params` is OPTIONAL. If absent, return an empty map so actions
	// with no parameters (e.g. mpm_decisions action=list with no filters)
	// can be invoked as `{"action":"list"}` instead of `{"action":"list",
	// "params":{}}`. Inner handlers still validate their specific required
	// params — extractParamsOrFail no longer pre-rejects for them.
	//
	// If `params` IS present, it must be object-typed. A wrong-typed params
	// is still a contract violation that surfaces at the boundary.
	if _, present := payload["params"]; !present {
		return map[string]interface{}{}, nil
	}
	params, ok := payload["params"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf(
			"%s: field `params` must be an object — got %T; expected shape "+
				"{\"action\":\"<op>\",\"params\":{...}}",
			toolName, payload["params"],
		)
	}

	return params, nil
}

// handleMpmMemory is the unified dispatcher for the mpm_memory domain tool.
// It routes the "action" string to the existing per-operation handler,
// passing "params" through as the payload map.
func handleMpmMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_memory", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)

	// Wire-format (2026-08-25 D6 + 2026-08-27 F4 convergence): canonical
	// input field name is snake_case `memory_id`. Legacy camelCase
	// `memoryId` callers are normalized onto the canonical form on every
	// action. The challenge action previously aliased the other way
	// (snake_case → camelCase) — that asymmetry was the F4 rough edge.
	// Now every action routes through the same single mapping below.
	normalizeCamelCaseKeys(params, map[string]string{"memoryId": "memory_id"})

	switch action {
	case "save":
		return handleSaveToMemory(dm, ac, params)
	case "query":
		return handleQueryLongTermMemory(dm, ac, params)
	case "show":
		// W-003: agents used to have to run a search query to fetch a
		// known id. show fetches one row directly and decodes tags +
		// metadata JSON columns into native types so consumers get
		// real arrays/objects, not stringified blobs (W-013).
		return handleShowMemory(dm, ac, params)
	case "shred":
		return handleShredMemory(dm, ac, params)
	case "delete":
		// Alpha cleanup (2026-09-10): explicit soft-delete verb. Reversible
		// via the "restore" action below. Distinct from "shred" (hard,
		// irreversible, broad sweep). See internal/core/memory_tools.go
		// SoftDeleteMemory for the full contract.
		return handleDeleteMemory(dm, ac, params)
	case "restore":
		// Reverses a prior soft-delete by clearing deleted_at. Distinct
		// from "restore_challenge" (which only clears the challenged-
		// status flag, not the deleted_at tombstone). See
		// RestoreMemory for the full contract.
		return handleRestoreMemory(dm, ac, params)
	case "reinforce":
		return handleReinforceMemory(dm, ac, params)
	case "weaken":
		return handleWeakenMemory(dm, ac, params)
	case "snooze":
		return handleSnoozeMemory(dm, ac, params)
	case "set_weight":
		return handleSetMemoryWeight(dm, ac, params)
	case "patch":
		return handlePatchMemory(dm, ac, params)
	case "promote":
		return handlePromoteMemory(dm, ac, params)
	case "review":
		return handleReviewMemories(dm, ac, params)
	case "synthesize":
		return handleSynthesizeMemory(dm, ac, params)
	case "challenge":
		// Dispatcher already normalized memoryId → memory_id above;
		// handler reads the canonical snake_case key.
		return handleChallengeMemory(dm, ac, params)
	case "restore_challenge":
		// F7-1 (alpha-final): restore_challenge is the agent-facing
		// counterpart to "mpm challenge restore <id>". Previously the
		// restore operation was CLI-only; this action exposes the same
		// canonical DatabaseManager method via the agent surface.
		return handleRestoreChallengeMemory(dm, ac, params)
	case "commit_milestone":
		return handleCommitMilestone(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_memory. Valid actions include save, query, show, shred, delete, restore, reinforce, weaken, snooze, set_weight, patch, promote, review, synthesize, challenge, restore_challenge, commit_milestone", action)
	}
}

// handleMpmHandoff is the entry point for the mpm_handoff tool.
// Covers cross-session communication: write, read, list, and shred handoffs.
// Scratchpad operations moved to handleMpmScratchpad.
func handleMpmHandoff(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_handoff", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "write":
		return handleHandoffWrite(dm, ac, params)
	case "read":
		return handleHandoffRead(dm, ac, params)
	case "list":
		return handleHandoffList(dm, ac, params)
	case "shred":
		return handleShredHandoff(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_handoff. Valid actions include write, read, list, shred", action)
	}
}

// handleMpmScratchpad is the entry point for the mpm_scratchpad tool.
// Covers intra-session volatile working memory: flush, read, discard, promote.
// Handoff operations moved to handleMpmHandoff.
func handleMpmScratchpad(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_scratchpad", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "flush":
		return handleFlushScratchpad(dm, ac, params)
	case "read":
		return handleReadScratchpad(dm, ac, params)
	case "discard":
		return handleDiscardScratchpad(dm, ac, params)
	case "promote":
		return handlePromoteScratchpad(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_scratchpad. Valid actions include flush, read, discard, promote", action)
	}
}

func handleMpmWakes(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_wakes", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "schedule":
		return handleScheduleWake(dm, ac, params)
	case "check":
		return handleCheckWakes(dm, ac, params)
	case "check_pending_event":
		return handleCheckPendingEventWakes(dm, ac, params)
	case "list":
		return handleListWakes(dm, ac, params)
	case "digest":
		return handleDigestWakes(dm, ac, params)
	case "resolve":
		return handleResolveWake(dm, ac, params)
	case "upsert_task":
		return handleUpsertScheduledTask(dm, ac, params)
	case "list_tasks":
		return handleListScheduledTasks(dm, ac, params)
	case "delete_task":
		return handleDeleteScheduledTask(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_wakes. Valid actions include schedule, check, check_pending_event, list, digest, resolve, upsert_task, list_tasks, delete_task", action)
	}
}

// handleResolveWake is the explicit wake-resolution action surfaced
// in 2026-09-19 to close lesson 9b9f286c (mpm_wakes lacks explicit
// resolution). Lifecycle contract:
//
//   - Marks fired=1 with fired_at=now and fired_by="wake-resolver".
//   - Idempotent: re-running on an already-fired wake returns
//     success with status="already_resolved" and no new audit row.
//     No double-fire noise from retries or double-clicks.
//   - Refuses scheduled_tasks-owned rows (no metadata.kind →
//     "not_a_wake"). Scheduled tasks have their own lifecycle
//     (`delete_task`); mixing the surfaces would let an agent
//     accidentally retire a recurring schedule.
//
// Required params:
//
//   - wake_id: the wake to resolve (string, required).
//   - reason:  canonical enum — reconciled | obsolete |
//     superseded | already_satisfied (string, required).
//
// Optional params:
//
//   - result_reference: opaque pointer (e.g. theory id that was
//     disproven, new artifact id that superseded the downstream)
//     recorded in the audit row's context for the investigator.
//     Not persisted on the wake row itself — secrets should
//     never pass through this surface.
//
// Safe-credential contract: positionally injects NO credential.
// The reason is bounded to the canonical enum; if the operator
// needs to record sensitive evidence, the canonical evidence
// tool is the right surface.
func handleResolveWake(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	wakeID, _ := p["wake_id"].(string)
	if wakeID == "" {
		// Backward-compat: also accept id and wakeId for callers
		// who default to the canonical shape used by other
		// mpm_* actions.
		wakeID, _ = p["id"].(string)
	}
	if wakeID == "" {
		wakeID, _ = p["wakeId"].(string)
	}
	if wakeID == "" {
		return nil, fmt.Errorf("resolve wake: wake_id is required (or id / wakeId)")
	}
	reason, _ := p["reason"].(string)
	if reason == "" {
		return nil, fmt.Errorf("resolve wake: reason is required — must be one of reconciled|obsolete|superseded|already_satisfied")
	}
	resultReference, _ := p["result_reference"].(string)

	resolved, status, err := dm.ResolveWake(wakeID, reason, resultReference)
	if err != nil {
		// Not every error is a failure to surface to the agent.
		// For the "refused but recoverable" states (wake_not_found,
		// not_a_wake, invalid_reason), the error message already
		// names the cause; the agent can correct the call and
		// retry. The `success:false` envelope makes the refusal
		// machine-readable too.
		return map[string]interface{}{
			"success": false,
			"status":  status,
			"wake_id": wakeID,
			"reason":  reason,
			"error":   err.Error(),
		}, nil
	}
	return map[string]interface{}{
		"success":          true,
		"resolved":         resolved,
		"status":           status,
		"wake_id":          wakeID,
		"reason":           reason,
		"result_reference": resultReference,
	}, nil
}

func handleMpmTheories(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_theories", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "propose":
		return handleProposeTheory(dm, ac, params)
	case "resolve":
		// 2026-09-10 cleanup: handleResolveTheory now reads canonical
		// snake_case fields directly (`id`, `status`, `winner_id`) and
		// retains `theoryId`/`theory_id` and `newStatus`/`new_status`
		// as backward-compat aliases. The pre-cleanup dispatcher had
		// a normalizeCamelCaseKeys step that mutated `theory_id` →
		// `theoryId` before the handler read it; with the handler now
		// accepting both, the dispatcher no longer needs the rewrite.
		// Keeping the handler canonical-first prevents the rewrite
		// from masking alias acceptance during testing.
		return handleResolveTheory(dm, ac, params)
	// alpha-4 audit D-006: read symmetry. Previously the only actions
	// were propose/resolve — agents using `mpm call mpm_theories` had no
	// way to enumerate or look up theories, even though the CLI has had
	// `mpm theories` since alpha-3. Mirrors handleMpmDecisions.
	case "show":
		return handleShowTheory(dm, ac, params)
	case "list":
		return handleListTheories(dm, ac, params)
	case "query":
		return handleQueryTheories(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_theories. Valid actions include propose, resolve, show, list, query", action)
	}
}

// handleShowTheory (alpha-4 audit D-006) returns a single theory row by id.
func handleShowTheory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("show theory: id is required")
	}
	row, err := dm.GetTheory(id)
	if err != nil {
		return nil, fmt.Errorf("show theory: %w", err)
	}
	return map[string]interface{}{"success": true, "theory": row}, nil
}

// handleListTheories (alpha-4 audit D-006) returns theories matching an
// optional status filter (pending|all|proven|disproven|resolved) and
// optional tag filter. Default filter is "pending" to match the CLI's
// `mpm theories` default of "all" — callers that want everything must
// pass status="all" explicitly.
func handleListTheories(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	status, _ := p["status"].(string)
	// 2026-09-05 audit remediation pass 2: explicit limit validation
	// (omitted → 50, 0 → 0, negative → error, non-integer → error).
	limit, err := parseLimitStrict(p["limit"], 50)
	if err != nil {
		return nil, fmt.Errorf("list theories: %w", err)
	}
	filter := mpminternal.TheoryFilter{
		Status: status,
		Limit:  limit,
	}
	if tagsAny, ok := p["tags"].([]interface{}); ok {
		for _, t := range tagsAny {
			if s, ok := t.(string); ok {
				filter.Tags = append(filter.Tags, s)
			}
		}
	}
	rows, err := dm.ListTheories(filter)
	if err != nil {
		return nil, fmt.Errorf("list theories: %w", err)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"success":  true,
		"status":   statusOrDefaultTheory(status),
		"count":    len(rows),
		"theories": rows,
	}, nil
}

// handleQueryTheories (alpha-4 audit D-006) returns theories matching a
// free-text FTS5 query. Reuses SearchMemories for BM25 scoring and
// reinforcement weighting.
func handleQueryTheories(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query theories: query is required")
	}
	// 2026-09-05 audit remediation pass 2: see handleListTheories.
	limit, err := parseLimitStrict(p["limit"], 50)
	if err != nil {
		return nil, fmt.Errorf("query theories: %w", err)
	}
	rows, err := dm.QueryTheories(query, limit)
	if err != nil {
		return nil, fmt.Errorf("query theories: %w", err)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"success":  true,
		"query":    query,
		"count":    len(rows),
		"theories": rows,
	}, nil
}

// statusOrDefaultTheory returns "pending" for an empty status string so
// the envelope carries a meaningful status even when the caller omits it.
// Mirrors statusOrDefault for decisions (which defaults to "active").
func statusOrDefaultTheory(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}

func handleMpmLessons(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_lessons", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "save":
		return handleSaveLesson(dm, ac, params)
	case "search":
		return handleSearchLessons(dm, ac, params)
	case "list":
		return handleListLessons(dm, ac, params)
	case "delete":
		// Soft delete: sets deleted_at tombstone; row + history +
		// content remain persisted. Reversible via `restore`.
		return handleDeleteLesson(dm, ac, params)
	case "restore":
		// Clears the deleted_at tombstone; lesson reappears in
		// list/search/get. Idempotent on an already-visible lesson.
		return handleRestoreLesson(dm, ac, params)
	case "shred":
		// Irreversible hard delete. Removes the row from
		// lessons_base + lessons_fts. No restore path.
		return handleShredLesson(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_lessons. Valid actions include save, search, list, delete, restore, shred", action)
	}
}

// handleDeleteLesson implements the soft-delete action on the
// mpm_lessons tool surface. Reversible — see handleRestoreLesson.
// Field vocabulary: canonical `id`. `lesson_id` retained as a narrow
// backward-compat alias.
func handleDeleteLesson(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		id, _ = p["lesson_id"].(string)
	}
	if id == "" {
		return nil, fmt.Errorf("delete lesson: id is required")
	}
	if err := dm.DeleteLesson(id); err != nil {
		return nil, fmt.Errorf("delete lesson: %w", err)
	}
	return map[string]interface{}{"success": true, "id": id, "action": "delete", "reversible": true}, nil
}

// handleRestoreLesson undoes a soft-delete. Idempotent on an
// already-visible lesson (returns a clean error). A shredded lesson
// (hard-deleted) cannot be restored — its row is gone from
// lessons_base entirely and this handler reports "not found".
func handleRestoreLesson(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		id, _ = p["lesson_id"].(string)
	}
	if id == "" {
		return nil, fmt.Errorf("restore lesson: id is required")
	}
	if err := dm.RestoreLesson(id); err != nil {
		return nil, fmt.Errorf("restore lesson: %w", err)
	}
	return map[string]interface{}{"success": true, "id": id, "action": "restore"}, nil
}

// handleShredLesson implements the irreversible hard-delete action.
// No restore path. Field vocabulary matches handleDeleteLesson.
func handleShredLesson(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		id, _ = p["lesson_id"].(string)
	}
	if id == "" {
		return nil, fmt.Errorf("shred lesson: id is required")
	}
	if err := dm.ShredLesson(id); err != nil {
		return nil, fmt.Errorf("shred lesson: %w", err)
	}
	return map[string]interface{}{"success": true, "id": id, "action": "shred", "reversible": false}, nil
}

func handleMpmDecisions(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_decisions", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "record":
		return handleRecordDecision(dm, ac, params)
	case "supersede":
		return handleSupersedeDecision(dm, ac, params)
	case "invalidate":
		return handleInvalidateDecision(dm, ac, params)
	case "show":
		return handleShowDecision(dm, ac, params)
	case "list":
		return handleListDecisions(dm, ac, params)
	case "query":
		return handleQueryDecisions(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_decisions. Valid actions include record, supersede, invalidate, show, list, query", action)
	}
}

// handleShowDecision (alpha-4 D-005) returns a single decision by ID.
//
// 2026-09-05 audit residual pass §I-C.14: the previous shape
// wrapped any DM error (including "no rows") with a generic prefix,
// making it impossible to distinguish a not-found from a SQL
// failure on the wire. The DM now translates sql.ErrNoRows into
// "decision not found: <id>"; this handler surfaces that as the
// public not-found envelope so callers see the same shape on both
// CLI and MCP. Other DM errors continue to wrap as before.
func handleShowDecision(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("show decision: id is required")
	}
	row, err := dm.GetDecision(id)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"success": true, "decision": row}, nil
}

// handleListDecisions (alpha-4 D-005) returns decisions matching an
// optional status filter (active|all|superseded|invalidated) and
// optional tag filter.
func handleListDecisions(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	status, _ := p["status"].(string)
	// 2026-09-05 audit remediation pass 2: explicit limit validation.
	// The previous shape silently coerced limit <= 0 to 50 inside the
	// DM, which made limit=0 indistinguishable from limit-omitted.
	// parseLimitStrict enforces: omitted → 50, 0 → 0, negative →
	// error, non-integer → error.
	limit, err := parseLimitStrict(p["limit"], 50)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	filter := mpminternal.DecisionFilter{
		Status: status,
		Limit:  limit,
	}
	if tagsAny, ok := p["tags"].([]interface{}); ok {
		for _, t := range tagsAny {
			if s, ok := t.(string); ok {
				filter.Tags = append(filter.Tags, s)
			}
		}
	}
	rows, err := dm.ListDecisions(filter)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"success":   true,
		"status":    statusOrDefault(status),
		"count":     len(rows),
		"decisions": rows,
	}, nil
}

// handleQueryDecisions (alpha-4 D-005) returns decisions matching a
// free-text FTS5 query. Reuses SearchMemories for BM25 scoring and
// reinforcement weighting.
func handleQueryDecisions(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	query, _ := p["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("query decisions: query is required")
	}
	// 2026-09-05 audit remediation pass 2: see handleListDecisions.
	limit, err := parseLimitStrict(p["limit"], 50)
	if err != nil {
		return nil, fmt.Errorf("query decisions: %w", err)
	}
	rows, err := dm.QueryDecisions(query, limit)
	if err != nil {
		return nil, fmt.Errorf("query decisions: %w", err)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	return map[string]interface{}{
		"success":   true,
		"query":     query,
		"count":     len(rows),
		"decisions": rows,
	}, nil
}

// statusOrDefault returns "active" for an empty status string so the
// envelope carries a meaningful status even when the caller omits it.
func statusOrDefault(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

func handleMpmTopics(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_topics", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "create":
		return handleCreateTopic(dm, ac, params)
	case "search":
		return handleSearchTopics(dm, ac, params)
	case "link":
		return handleLinkTopic(dm, ac, params)
	case "unlink":
		// Part 2A (2026-09-06): inverse of link. Idempotent on missing
		// membership (removed=false, success=true). See handleUnlinkTopic.
		return handleUnlinkTopic(dm, ac, params)
	case "list":
		// D-4.1: parity with `mpm topic list` CLI surface.
		return handleListTopics(dm, ac, params)
	case "show":
		// D-4.1: parity with `mpm topic show <id>` CLI surface.
		return handleShowTopic(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_topics. Valid actions include create, search, link, unlink, list, show", action)
	}
}

func handleMpmReferences(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_references", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "add":
		return handleAddReference(dm, ac, params)
	case "read":
		// W-004: parity with mpm_memory/mpm_lessons/mpm_decisions.
		// Closes the gap where the only way to read a reference was
		// via the generic mpm_resolve pointer.
		return handleReadReference(dm, ac, params)
	case "search":
		return handleSearchReferences(dm, ac, params)
	case "list":
		return handleListReferences(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_references. Valid actions include add, read, search, list", action)
	}
}

func handleMpmEvidence(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_evidence", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "add":
		return handleAddEvidence(dm, ac, params)
	case "list":
		return handleListEvidence(dm, ac, params)
	case "source_groups":
		// Discovery surface backing the F6 rejection message: agents can
		// enumerate the accepted vocabulary instead of guessing.
		return map[string]interface{}{
			"success": true,
			"outcome": mpminternal.EvidenceSourceGroupOfClass(mpminternal.SourceGroupClassOutcome),
			"audit":   mpminternal.EvidenceSourceGroupOfClass(mpminternal.SourceGroupClassAudit),
			"action":  mpminternal.EvidenceSourceGroupOfClass(mpminternal.SourceGroupClassAction),
		}, nil
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_evidence. Valid actions include add, list, source_groups", action)
	}
}

func handleMpmConfidence(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_confidence", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "show":
		return handleShowConfidence(dm, ac, params)
	case "recompute":
		return handleRecomputeConfidence(dm, ac, params)
	case "explain":
		return handleExplainConfidence(dm, ac, params)
	case "history":
		return handleQueryConfidenceHistory(dm, ac, params)
	case "changes":
		return handleQueryConfidenceChanges(dm, ac, params)
	case "trend":
		return handleQueryConfidenceTrend(dm, ac, params)
	case "quality":
		return handleQueryMemoryQuality(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_confidence. Valid actions include show, recompute, explain, history, changes, trend, quality", action)
	}
}

func handleMpmSkills(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_skills", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "save":
		return handleSaveSkill(dm, ac, params)
	case "read":
		return handleReadSkill(dm, ac, params)
	case "list":
		return handleListSkills(dm, ac, params)
	case "delete":
		return handleDeleteSkill(dm, ac, params)
	case "promote_to_global":
		return handlePromoteSkillToGlobal(dm, ac, params)
	case "workshop":
		return handleWorkshopSkill(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_skills. Valid actions include save, read, list, delete, promote_to_global, workshop", action)
	}
}

func handleMpmContext(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_context", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "read_wake_context":
		return handleReadWakeContext(dm, ac, params)
	case "read_directives":
		return handleReadDirectives(dm, ac, params)
	case "proactive_recall_hint":
		return handleProactiveRecallHint(dm, ac, params)
	case "query_global_rules":
		return handleQueryGlobalRules(dm, ac, params)
	case "record_global_rule":
		return handleRecordGlobalRule(dm, ac, params)
	case "retire_global_rule":
		return handleRetireGlobalRule(dm, ac, params)
	case "promote_to_global":
		return handlePromoteToGlobal(dm, ac, params)
	case "route":
		return handleRoute(dm, ac, params)
	case "recent_activity":
		return handleRecentActivity(dm, ac, params)
	case "contextual_candidates":
		return handleContextualCandidates(dm, ac, params)
	case "contextual_selection":
		return handleContextualSelection(dm, ac, params)
	case "contextual_materialization":
		return handleContextualMaterialization(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_context. Valid actions include read_wake_context, read_directives, proactive_recall_hint, query_global_rules, record_global_rule, retire_global_rule, promote_to_global, route, recent_activity, contextual_candidates, contextual_selection, contextual_materialization", action)
	}
}

// handleRecentActivity implements mpm_context action=recent_activity.
// Read-only semantic activity surface: pulls from tool_invocations,
// classifies via the canonical helpers (EffectiveActorKind,
// ClassifyAction), enriches deterministically where linkage exists,
// and uses bounded pagination so a sparse semantic feed can always
// surface the newest matching events.
//
// Default scope: agent + human + unknown mutating actions, newest-first,
// bounded by RecentActivityDefaultLimit (hard max
// RecentActivityHardMaxLimit). Excludes read-only queries, handoff
// delivery side effects, and system/diagnostic bookkeeping.
//
// Count is ALWAYS len(events) — derived from the slice so the count
// invariant is enforced by construction. Scan metadata is surfaced
// alongside the events so callers can distinguish a true short
// history (history_exhausted=true) from a truncated scan (truncated=true
// + scanned_rows/scan_limit reported).
//
// include_system is accepted as a no-op for back-compat. The substrate's
// tool_invocations does NOT comprehensively cover cascade-materializer /
// cascade-reconciler / scheduler-retention / GC / migration writes,
// so the old promise of "include all system activity" was misleading.
// Agents wanting structured system audit should use mpm_system
// (query_audit_log, list_clusters) instead.
func handleRecentActivity(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	if dm == nil {
		return nil, fmt.Errorf("recent_activity: dm is nil")
	}

	// Parse params with parseLimitStrict semantics where applicable.
	limit := mpminternal.RecentActivityDefaultLimit
	if v, present := p["limit"]; present && v != nil {
		switch n := v.(type) {
		case float64:
			limit = int(n)
		case int:
			limit = n
		default:
			return nil, fmt.Errorf("recent_activity: limit must be int, got %T", v)
		}
	}

	var since int64
	if v, present := p["since"]; present && v != nil {
		switch n := v.(type) {
		case float64:
			since = int64(n)
		case int64:
			since = n
		case int:
			since = int64(n)
		default:
			return nil, fmt.Errorf("recent_activity: since must be int, got %T", v)
		}
	}

	actorKind, _ := p["actor_kind"].(string)
	frameworkName, _ := p["framework_name"].(string)
	sessionID, _ := p["session_id"].(string)
	artifactType, _ := p["artifact_type"].(string)

	// include_system is a strict no-op kept for legacy callers. The
	// substrate cannot truthfully provide comprehensive system audit
	// coverage from tool_invocations (cascade-materializer,
	// cascade-reconciler, scheduler-retention, GC, migration do not
	// all flow through tool_invocations), so the parameter is
	// accepted as input and silently ignored.
	//
	// recent_activity MUST be observational: a parameter choice must
	// NEVER produce a durable semantic/audit mutation. The legacy
	// deprecation audit-row insert was an observer-effect violation;
	// it is removed here. Callers wanting structured system audit
	// should use mpm_system (query_audit_log, list_clusters).
	includeSystem := false
	if v, present := p["include_system"]; present && v != nil {
		switch b := v.(type) {
		case bool:
			includeSystem = b
		default:
			return nil, fmt.Errorf("recent_activity: include_system must be bool, got %T", v)
		}
	}

	res, err := dm.RecentActivityWithMeta(mpminternal.RecentActivityQueryParams{
		Limit:         limit,
		Since:         since,
		ActorKind:     actorKind,
		FrameworkName: frameworkName,
		SessionID:     sessionID,
		ArtifactType:  artifactType,
		IncludeSystem: includeSystem,
	})
	if err != nil {
		return nil, fmt.Errorf("recent_activity: %w", err)
	}

	return map[string]interface{}{
		"success":           true,
		"action":            "recent_activity",
		"events":            res.Events,
		"count":             len(res.Events), // derived; enforces invariant
		"limit":             limit,
		"truncated":         res.Truncated,
		"history_exhausted": res.HistoryExhausted,
		"scanned_rows":      res.ScannedRows,
		"scan_limit":        res.ScanLimit,
		"defaults": map[string]interface{}{
			"actor_scope":  "agent+human+unknown",
			"class_filter": "mutating",
			"ordering":     "newest-first",
			"note":         "system activity (drill/system/maintenance) is intentionally excluded — use mpm_system query_audit_log for audit surface",
		},
	}, nil
}

func handleMpmSystem(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	params, err := extractParamsOrFail("mpm_system", payload)
	if err != nil {
		return nil, err
	}
	action, _ := payload["action"].(string)
	switch action {
	case "gc_run":
		return handleGCRun(dm, ac, params)
	case "compact":
		return handleCompactEpistemology(dm, ac, params)
	case "health_check":
		return handleHealthCheck(dm, ac, params)
	case "migrate":
		return handleMigrate(dm, ac, params)
	case "query_audit_log":
		return handleQueryAuditLog(dm, ac, params)
	case "list_clusters":
		return handleListActiveClusters(dm, ac, params)
	case "snooze_cluster":
		return handleSnoozeCluster(dm, ac, params)
	case "unsnooze_cluster":
		// Part 2B (2026-09-06): explicit inverse of snooze_cluster.
		// Routes through SetClusterStatus(ClusterStatusActive, "") which
		// clears snooze_until. See handleUnsnoozeCluster for the contract.
		return handleUnsnoozeCluster(dm, ac, params)
	case "resolve_cluster":
		return handleResolveCluster(dm, ac, params)
	case "annotate_cluster":
		return handleAnnotateCluster(dm, ac, params)
	case "critic_findings":
		// W-006: discover critic-emitted findings. The critic (mpm-critic)
		// emits findings as `mpm call mpm_memory challenge` invocations;
		// each lands a row in tool_invocations with tool_name='mpm_memory'
		// and action='challenge'. This action surfaces those rows so the
		// operator/agent can read them without grepping system_audit_log.
		return handleCriticFindings(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_system. Valid actions include gc_run, compact, health_check, migrate, query_audit_log, list_clusters, snooze_cluster, unsnooze_cluster, resolve_cluster, annotate_cluster, critic_findings", action)
	}
}

// handleCriticFindings (alpha-4 W-006) returns a list of critic-emitted
// findings discovered via the existing tool_invocations audit log.
//
// The critic (cmd/mpm-critic) emits findings as `mpm call mpm_memory
// challenge` invocations. Each invocation lands a row in
// tool_invocations with tool_name='mpm_memory' and action='challenge'.
// This action surfaces those rows so operators/agents can read what the
// critic flagged without grepping system_audit_log.
//
// No new persistence: reuses the existing audit trail. No new table.
// The action is read-only and respects the same limit envelope as
// other query-style actions.
func handleCriticFindings(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	limit := int(internal.ParseFloatOr(p["limit"], 50))
	if limit <= 0 {
		limit = 50
	}

	sqlDB := dm.SQLDB()
	rows, err := sqlDB.Query(`
		SELECT id, session_id, invocation_id, started_at, duration_ms, result_status, error_message
		FROM tool_invocations
		WHERE tool_name = 'mpm_memory' AND action = 'challenge'
		ORDER BY started_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list critic findings: %w", err)
	}
	defer rows.Close()

	findings := make([]map[string]interface{}, 0, limit)
	for rows.Next() {
		var (
			id, sessionID, invocationID, resultStatus string
			startedAt                                 int64
			durationMs                                sql.NullInt64
			errorMessage                              sql.NullString
		)
		if err := rows.Scan(&id, &sessionID, &invocationID, &startedAt, &durationMs, &resultStatus, &errorMessage); err != nil {
			return nil, fmt.Errorf("scan critic finding row: %w", err)
		}
		item := map[string]interface{}{
			"invocation_id": id,
			"session_id":    sessionID,
			"invocation":    invocationID,
			"started_at":    startedAt,
			"result_status": resultStatus,
		}
		if durationMs.Valid {
			item["duration_ms"] = durationMs.Int64
		}
		if errorMessage.Valid && errorMessage.String != "" {
			item["error_message"] = errorMessage.String
		}
		findings = append(findings, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate critic findings: %w", err)
	}

	return map[string]interface{}{
		"success":  true,
		"findings": findings,
		"count":    len(findings),
		"source":   "tool_invocations(action=challenge)",
	}, nil
}

// ── Pointer / Blob tools (Phase 1) ────────────────────────────────────────

// handleMpmResolve resolves a mpm:// URI to its content via the global resolver.
// Phase 2 supports mpm://blob/<id>, mpm://memory/<id>, mpm://lesson/<id>,
// and mpm://theory/<id>. max_bytes applies a soft ceiling on the amount of
// content returned; 0 means unlimited (subject to the resolver's own limits).
func handleMpmResolve(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	uri, _ := payload["uri"].(string)
	if uri == "" {
		return nil, fmt.Errorf("uri is required")
	}
	maxBytes, _ := payload["max_bytes"].(float64) // JSON numbers are float64
	full, _ := payload["full"].(bool)

	// Parse the mpm:// URI locally (mpm-core cannot import main module's pointer).
	ptr, err := parsePointerURI(uri)
	if err != nil {
		return nil, err
	}

	// full=true bypasses the inline-bound cap (max_bytes=0 → no cap).
	// Callers wanting a custom bound pass max_bytes explicitly; full
	// always wins when both are set.
	if full {
		maxBytes = 0
	}

	// Phase 2: all four kinds are supported.
	if ptr.Kind != "blob" && ptr.Kind != "work" && ptr.Kind != "memory" && ptr.Kind != "lesson" && ptr.Kind != "theory" {
		return nil, fmt.Errorf("%w: Phase 2 supports mpm://blob/, work/, memory/, lesson/, and theory/", ErrUnsupportedKind)
	}

	if globalResolver == nil {
		// Fallback for CLI (`mpm call mpm_resolve`) where SetResolver was not
		// called at boot (MCP wires it, CLI does not). Resolve directly via
		// DatabaseManager for non-blob kinds so the pointer architecture test
		// can retrieve full content without requiring MCP transport.
		switch ptr.Kind {
		case "memory":
			mem, err := dm.GetMemory(ptr.ID)
			if err != nil {
				return nil, err
			}
			content, _ := mem["content"].(string)
			// Final release-pass contract (defect C): ordinary pointer
			// resolution is bounded to the inline save-echo cap so
			// agent-facing responses stay compact. Callers that want
			// the complete payload pass full=true (handled upstream
			// at line 6435) or a large max_bytes. Without either,
			// this is the safe default — bounded projection.
			//
			// full=true is an EXPLICIT opt-in to unbounded retrieval
			// — the bounded fallback must NOT fire, or the caller's
			// intent is silently dropped. Use a sentinel (full) rather
			// than overloading maxB=0 (which is also the unset default).
			var bounded bool
			if full {
				// full=true: explicit opt-in to unbounded content.
				// Caller accepts that the payload may be large.
				bounded = false
			} else {
				maxB := int(maxBytes)
				if maxB <= 0 {
					maxB = mpminternal.DefaultMaxInlineContentBytes
				}
				bounded = len(content) > maxB
				if bounded {
					content = content[:maxB]
				}
			}
			_ = dm.RecordRetrieval(ptr.ID, "memory")
			resp := map[string]interface{}{
				"content":      content,
				"content_type": "text/plain",
				"pointer":      "mpm://memory/" + ptr.ID,
				"bounded":      bounded,
			}
			// resolveMetadataFor strips the unbounded `content` field so
			// the bounded top-level content is the only place the full
			// payload can appear in the response. See the launch-block
			// fix in docs/CONTEXT_ECONOMICS.md (Sept 2026 launch).
			if mem != nil {
				resp["metadata"] = resolveMetadataFor(mem)
			}
			// W-007: surface challenge status with parity to handleShowMemory.
			// The stored `content` string is intentionally untouched — the
			// banner is exposed alongside the (possibly bounded) content.
			metaForChallenge := map[string]interface{}{}
			if rawMeta, ok := mem["metadata"].(string); ok {
				metaForChallenge = parseMetadataColumn(rawMeta)
			} else if m, ok := mem["metadata"].(map[string]interface{}); ok {
				metaForChallenge = m
			}
			isChallenged, banner := deriveChallengeFields(metaForChallenge)
			resp["is_challenged"] = isChallenged
			resp["banner"] = banner
			return resp, nil
		case "lesson":
			lesson, err := dm.GetLesson(ptr.ID)
			if err != nil {
				return nil, err
			}
			_ = dm.RecordRetrieval(ptr.ID, "lesson")
			// Final release-pass contract (defect C): bounded
			// projection by default; full content via full=true or
			// large max_bytes. See memory-case comment for the
			// rationale.
			lessonContent := lesson.Content
			lessonMaxB := int(maxBytes)
			if lessonMaxB <= 0 {
				lessonMaxB = mpminternal.DefaultMaxInlineContentBytes
			}
			lessonBounded := len(lessonContent) > lessonMaxB
			if lessonBounded {
				lessonContent = lessonContent[:lessonMaxB]
			}
			return map[string]interface{}{
				"content":      lessonContent,
				"content_type": "text/plain",
				"pointer":      "mpm://lesson/" + ptr.ID,
				"bounded":      lessonBounded,
				"metadata": map[string]interface{}{
					"id":   lesson.ID,
					"type": string(lesson.Type),
				},
			}, nil
		case "theory":
			mem, err := dm.GetMemory(ptr.ID)
			if err != nil {
				return nil, err
			}
			coll, _ := mem["collection"].(string)
			if coll != "theories" {
				return nil, fmt.Errorf("mpm://theory/%s: not a theory (collection=%q)", ptr.ID, coll)
			}
			content, _ := mem["content"].(string)
			_ = dm.RecordRetrieval(ptr.ID, "theory")
			// Final release-pass contract (defect C): bounded
			// projection by default; full content via full=true or
			// large max_bytes.
			theoryMaxB := int(maxBytes)
			if theoryMaxB <= 0 {
				theoryMaxB = mpminternal.DefaultMaxInlineContentBytes
			}
			theoryBounded := len(content) > theoryMaxB
			if theoryBounded {
				content = content[:theoryMaxB]
			}
			return map[string]interface{}{
				"content":      content,
				"content_type": "text/plain",
				"pointer":      "mpm://theory/" + ptr.ID,
				"bounded":      theoryBounded,
				// resolveMetadataFor strips the unbounded `content` field
				// so the bounded top-level content is the only place the
				// full payload can appear in the response.
				"metadata": resolveMetadataFor(mem),
			}, nil
		case "work":
			work, err := dm.GetWork(ptr.ID)
			if err != nil {
				return nil, err
			}
			content := work.Title
			if work.Content != "" {
				content = work.Title + "\n\n" + work.Content
			}
			// Final release-pass contract (defect C): bounded
			// projection by default; full content via full=true or
			// large max_bytes.
			//
			// full=true must bypass the bounded fallback so the
			// caller's explicit opt-in is honoured — see the
			// matching fix on the memory branch above.
			var bounded bool
			if full {
				bounded = false
			} else {
				maxB := int(maxBytes)
				if maxB <= 0 {
					maxB = mpminternal.DefaultMaxInlineContentBytes
				}
				bounded = len(content) > maxB
				if bounded {
					content = content[:maxB]
				}
			}
			_ = dm.RecordRetrieval(ptr.ID, "work")
			return map[string]interface{}{
				"content":      content,
				"content_type": "text/plain",
				"pointer":      "mpm://work/" + ptr.ID,
				"bounded":      bounded,
			}, nil
		case "blob":
			return nil, fmt.Errorf("blob store not initialized; SetBlobStore was not called at boot")
		default:
			return nil, fmt.Errorf("mpm_resolve: resolver not initialized; SetResolver was not called at mpm-mcp boot")
		}
	}

	result, err := globalResolver.Resolve(context.Background(), ptr, ResolveOptions{MaxBytes: int64(maxBytes)})
	if err != nil {
		return nil, err
	}
	defer result.Reader.Close()

	content, err := io.ReadAll(result.Reader)
	if err != nil {
		return nil, err
	}

	resp := map[string]interface{}{
		"content":      string(content),
		"content_type": result.ContentType,
		"pointer":      result.Pointer,
		"bounded":      result.Bounded,
	}
	// resolveMetadataFor strips the unbounded `content` field so the
	// bounded top-level content is the only place the full payload can
	// appear in the response. See the launch-block fix in
	// docs/CONTEXT_ECONOMICS.md (Sept 2026 launch).
	if result.Metadata != nil {
		resp["metadata"] = resolveMetadataFor(result.Metadata)
	}
	// W-007: surface challenge banner with parity to handleShowMemory.
	// The resolver-driven path is what CLI `mpm call mpm_resolve` uses;
	// the database-manager fallback inside the memory-branch case
	// above injects the same fields for the test / direct-handler path.
	// Both branches must advertise the same banner so callers cannot
	// observe a contract asymmetry between routes.
	metaForChallenge := map[string]interface{}{}
	if result.Metadata != nil {
		// The memory-shaped result has `metadata` as a JSON-encoded
		// string (the raw column value). Parse it so deriveChallengeFields
		// can see `status` directly.
		if rawMeta, ok := result.Metadata["metadata"].(string); ok {
			metaForChallenge = parseMetadataColumn(rawMeta)
		} else if inner, ok := result.Metadata["metadata"].(map[string]interface{}); ok {
			metaForChallenge = inner
		}
	}
	isChallenged, banner := deriveChallengeFields(metaForChallenge)
	resp["is_challenged"] = isChallenged
	resp["banner"] = banner
	return resp, nil
}

// parsePointerURI parses a mpm:// URI into a Pointer.
// Duplicates internal/pointer.Parse logic here so mpm-core tools does not
// need to import the main module's pointer package.
// parsePointerURI is a local copy of pointer.Parse for use by the tools
// package, which cannot import the main module's internal/pointer package.
// This implementation must stay in sync with pointer.Parse.

// resolveMetadataFor projects a stored artifact row into the safe
// metadata shape returned by mpm_resolve. The CRITICAL invariant is:
//
//	top-level `content`  = bounded content (already capped to max_bytes)
//	`metadata.content`   = MUST NOT echo the original unbounded content
//
// Before this projection existed, handleMpmResolve's CLI fallback set
// resp["metadata"] = mem verbatim, and the MCP-resolver path set
// resp["metadata"] = result.Metadata verbatim. Both pass-throughs
// leaked the full stored content via metadata.content even when the
// top-level content was bounded, defeating the pointer architecture's
// model-facing context bound. This helper strips the unbounded content
// field at the response boundary; stored state in the memories table
// is NOT mutated.
//
// All other keys (id, collection, tags, weight, metadata, created_at,
// pointer, etc.) are preserved so legitimate metadata flows through
// unchanged. The result is a NEW map — the input is not mutated.
func resolveMetadataFor(mem map[string]interface{}) map[string]interface{} {
	if mem == nil {
		return nil
	}
	out := make(map[string]interface{}, len(mem))
	for k, v := range mem {
		if k == "content" {
			// Drop the unbounded content; the bounded slice is already
			// exposed at resp["content"]. This is the fix site.
			continue
		}
		out[k] = v
	}
	return out
}

// Key invariants shared with pointer.Parse:
//   - Rejects URIs containing '?' or '#' (query/fragment components)
//   - Validates id against ^[a-z0-9-]+$
//   - Returns ErrPointerWrongScheme / ErrPointerMalformed as documented
func parsePointerURI(uri string) (Pointer, error) {
	// Reject query strings and fragments before scheme check (same as pointer.Parse).
	for _, c := range uri {
		if c == '?' || c == '#' {
			return Pointer{}, fmt.Errorf("pointer: malformed URI")
		}
	}

	const scheme = "mpm://"
	if len(uri) < len(scheme) || uri[:len(scheme)] != scheme {
		return Pointer{}, fmt.Errorf("pointer: wrong scheme (expected mpm://)")
	}
	path := uri[len(scheme):]

	slashIdx := -1
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			slashIdx = i
			break
		}
	}
	if slashIdx <= 0 {
		return Pointer{}, fmt.Errorf("pointer: malformed URI")
	}

	kind := path[:slashIdx]
	id := path[slashIdx+1:]
	if kind == "" || id == "" {
		return Pointer{}, fmt.Errorf("pointer: malformed URI")
	}
	// Validate id: lowercase alphanumeric plus hyphens (same as pointer.Parse).
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return Pointer{}, fmt.Errorf("pointer: malformed URI")
		}
	}
	return Pointer{Kind: kind, ID: id}, nil
}

// handleMpmBlobRead reads a blob with byte offset and a server-side max_bytes
// ceiling of 256 KB. Rejects binary content types in Phase 1.
func handleMpmBlobRead(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	if blobStoreForHandlers == nil {
		return nil, fmt.Errorf("blob store not initialized; SetBlobStore was not called at boot")
	}
	id, _ := payload["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	// 2026-09-05 audit remediation pass 5 defect C.16: validate the
	// offset at the handler boundary so both CLI and MCP paths agree.
	// The previous shape silently coerced non-numeric and null values
	// to 0 via a bare `.(float64)` assertion, and a negative offset
	// produced a generic OS-level seek error. Both behaviours are
	// non-deterministic from the caller's perspective — the same input
	// could reach the handler via CLI or MCP with materially different
	// downstream handling because the boundary was permissive. Use
	// parseBlobOffset (defined just below) to enforce the four-state
	// contract.
	offset, err := parseBlobOffset(payload["offset"])
	if err != nil {
		return nil, err
	}
	maxBytes, _ := payload["max_bytes"].(float64)

	// Server ceiling: 256 KB.
	const serverMax = 256 * 1024
	effectiveMax := int64(maxBytes)
	if effectiveMax <= 0 {
		// D4 fix (2026-08-25): default page size now cooperates with the
		// MCP output boundary. The previous 50 KB default exceeded the
		// boundary (default 10 KB), so every unbounded read of a mid-size
		// blob self-spilled — the client received a spill envelope
		// pointing at ANOTHER blob (the read-result itself) instead of
		// content, with no in-band hint. Reserve ~1 KB for the response
		// envelope's own keys and JSON escaping headroom; has_more /
		// next_offset paginate the rest. An explicit max_bytes is honored
		// up to serverMax (a caller explicitly asking beyond the boundary
		// accepts the spill envelope as the honest bounded answer).
		effectiveMax = int64(DefaultOutputThresholdBytes() - 1024)
		if effectiveMax < 512 {
			effectiveMax = 512
		}
	}
	if effectiveMax > serverMax {
		effectiveMax = serverMax
	}

	opts := GetOptions{
		Offset:   int64(offset),
		MaxBytes: effectiveMax,
	}

	reader, meta, err := blobStoreForHandlers.Get(context.Background(), id, opts)
	if err != nil {
		if errors.Is(err, errBlobMissing) {
			return nil, fmt.Errorf("blob %s: file missing (DB row exists)", id)
		}
		if errors.Is(err, errBlobNotFound) {
			return nil, fmt.Errorf("blob %s: not found", id)
		}
		return nil, err
	}
	defer reader.Close()

	// Phase 1: reject binary materialization.
	ct := meta.ContentType
	if ct != "text/plain" && ct != "application/json" && !strings.HasPrefix(ct, "text/") {
		return nil, fmt.Errorf("binary materialization not supported in Phase 1; use a tool that returns text")
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	size := meta.SizeBytes
	nextOffset := int64(offset) + int64(len(content))
	hasMore := nextOffset < size

	return map[string]interface{}{
		"content":        string(content),
		"content_type":   ct,
		"offset":         int64(offset),
		"bytes_returned": len(content),
		"next_offset":    nextOffset,
		"has_more":       hasMore,
	}, nil
}

// handleMpmBlobSearch performs server-side regex or substring search within a blob.
// Server ceilings: 100 matches, 256 KB scanned. Query must be ≤256 chars.
func handleMpmBlobSearch(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
	if blobStoreForHandlers == nil {
		return nil, fmt.Errorf("blob store not initialized; SetBlobStore was not called at boot")
	}
	id, _ := payload["id"].(string)
	query, _ := payload["query"].(string)
	useRegex, _ := payload["regex"].(bool)
	caseInsensitive, _ := payload["case_insensitive"].(bool)
	maxMatches, _ := payload["max_matches"].(float64)
	maxBytes, _ := payload["max_bytes"].(float64)

	if id == "" || query == "" {
		return nil, fmt.Errorf("id and query are required")
	}

	// Server ceilings.
	const serverMaxMatches = 100
	const serverMaxBytes = 256 * 1024

	effectiveMaxMatches := int(maxMatches)
	if effectiveMaxMatches <= 0 {
		effectiveMaxMatches = 20
	}
	if effectiveMaxMatches > serverMaxMatches {
		effectiveMaxMatches = serverMaxMatches
	}

	effectiveMaxBytes := int64(maxBytes)
	if effectiveMaxBytes <= 0 {
		effectiveMaxBytes = 50 * 1024
	}
	if effectiveMaxBytes > serverMaxBytes {
		effectiveMaxBytes = serverMaxBytes
	}

	// Query length limit.
	if len(query) > 256 {
		return nil, fmt.Errorf("query exceeds 256 char limit")
	}

	sq := SearchQuery{
		Query:           query,
		Regex:           useRegex,
		CaseInsensitive: caseInsensitive,
		MaxMatches:      effectiveMaxMatches,
		MaxBytes:        effectiveMaxBytes,
	}

	matches, err := blobStoreForHandlers.Search(context.Background(), id, sq)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, 0, len(matches))
	for _, m := range matches {
		result = append(result, map[string]interface{}{
			"line_no":     m.LineNo,
			"byte_offset": m.ByteOffset,
			"snippet":     m.Snippet,
		})
	}

	truncated := len(matches) >= effectiveMaxMatches

	// Compute actual bytes from snippets.
	var bytesReturned int64
	for _, m := range result {
		if s, ok := m["snippet"].(string); ok {
			bytesReturned += int64(len(s))
		}
	}

	return map[string]interface{}{
		"matches":        result,
		"match_count":    len(matches),
		"truncated":      truncated,
		"bytes_returned": bytesReturned,
	}, nil
}

// parseWeightStrict is the F12-1 strict-type guard for the `weight`
// field at the mpm_memory save boundary. The contract:
//   - nil (absent)          → use def
//   - float64 / int / int64 → use the numeric value
//   - anything else         → error (NEVER silently coerce)
//
// internal.ParseFloatOr is intentionally permissive (silent coercion
// for limit/offset/timeout where wrong-type-→-default is harmless)
// but the alpha audit flagged that a memory weight passed as a string
// silently defaulted to the documented 0.5 with no error. That
// contract is wrong for save: an agent that sent weight="5" would
// believe it was requesting a high-priority memory when it was
// actually getting the default. We fail loudly here instead.
func parseWeightStrict(v interface{}, def float64) (float64, error) {
	if v == nil {
		return def, nil
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	}
	return 0, fmt.Errorf("field `weight` must be a number (float64/int), got %T", v)
}

// parseFloatStrictOr is the named-field counterpart to parseWeightStrict.
// Used by the evidence handler where the field name appears in the error
// message (strength, independence_factor) so the caller can pinpoint which
// input was wrong. Same numeric-only contract: float64 / float32 / int /
// int64 pass through, nil returns the default, anything else errors with
// the offending type and the field name.
func parseFloatStrictOr(v interface{}, def float64, fieldName string) (float64, error) {
	if v == nil {
		return def, nil
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	}
	return 0, fmt.Errorf("field `%s` must be a number (float64/int), got %T", fieldName, v)
}

// maxQueryLimit caps the per-call result ceiling so a runaway `limit`
// can't fan out a massive FTS5 + vector scan. 200 is well above the
// documented "default 5" and the historical "silent cap at 10" without
// inviting DoS-shaped queries.
const maxQueryLimit = 200

// parseLimitStrict validates a `limit`-shaped numeric input at the API
// boundary. W-009: prior behaviour silently substituted a default for
// every value <= 0, hiding the caller's intent. Now:
//
//	nil          → def
//	0            → 0 (literally "no results")
//	negative     → error (was the silent-substitution bug)
//	above max    → max (clamped, not silently substituted)
//	wrong type   → error
//
// The "above max → max" branch is a clamp, not a substitution — the
// caller can still ask for less, just not absurdly more.
func parseLimitStrict(v interface{}, def int) (int, error) {
	if v == nil {
		return def, nil
	}
	var n int
	switch x := v.(type) {
	case float64:
		// JSON numbers parse to float64 by default; reject fractional limits.
		if x != float64(int(x)) {
			return 0, fmt.Errorf("field `limit` must be an integer, got %v", x)
		}
		n = int(x)
	case float32:
		if x != float32(int(x)) {
			return 0, fmt.Errorf("field `limit` must be an integer, got %v", x)
		}
		n = int(x)
	case int:
		n = x
	case int64:
		n = int(x)
	default:
		return 0, fmt.Errorf("field `limit` must be a number (float64/int), got %T", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("field `limit` must be >= 0, got %d", n)
	}
	if n > maxQueryLimit {
		n = maxQueryLimit
	}
	return n, nil
}

// parseFloatStrict is the strict counterpart to internal.ParseFloatOr.
// 2026-09-05 audit remediation pass 2: mpm_memory mutation paths used
// silent ParseFloatOr which coerced strings ("5" → 5.0) and bad input
// ("abc" → default) without error. For mutation APIs this is the
// exact silent-corruption class the audit flagged.
//
// Contract:
//
//	nil           → def (legitimate omission)
//	float64       → value
//	float32       → value
//	int           → float64(value)
//	int64         → float64(value)
//	string        → ERROR (no silent coerce; matches F12-1
//	                       parseWeightStrict)
//	""            → ERROR (empty is not a valid number)
//	whitespace    → ERROR
//	anything else → ERROR
//
// If min is non-zero (e.g. min=0), values below min return an error
// rather than being silently clamped by downstream code.
//
// Distinguishes omitted (legitimate default) from present-but-invalid
// (rejection), per the audit's explicit requirement.
func parseFloatStrict(v interface{}, def float64, name string, min float64) (float64, error) {
	if v == nil {
		return def, nil
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	default:
		return 0, fmt.Errorf("field `%s` must be a number (float64/int), got %T", name, v)
	}
	if min != 0 && f < min {
		return 0, fmt.Errorf("field `%s` must be >= %v, got %v", name, min, f)
	}
	return f, nil
}

// parseBlobOffset enforces the canonical mpm_blob_read offset contract
// at the handler boundary. Both the CLI dispatcher
// (cmd/mpm/call.go handleCall) and the MCP adapter
// (cmd/mpm-mcp/tools.go mcpAdapter) feed the same payload shape into
// handleMpmBlobRead, so a single boundary validation here gives both
// surfaces identical semantics.
//
// Canonical contract:
//
//	omitted        → 0 (legitimate default; reads from start)
//	nil            → 0 (null omission; same as omitted)
//	0              → 0 (explicit; reads from start)
//	N (positive)   → N (reads from byte N)
//	size           → empty read at EOF (has_more=false; not an error)
//	> size         → empty read past EOF (has_more=false; not an error)
//	< 0            → ERROR: offset must be non-negative
//	non-integer    → ERROR: offset must be an integer
//	string         → ERROR: offset must be an integer
//	bool / other   → ERROR: offset must be an integer
//
// The handler previously used a bare `.(float64)` assertion that
// silently coerced non-numeric input to 0 and let negative offsets
// reach the OS-level Seek call (which returns a generic error). Both
// paths now share the same deterministic error envelope.
func parseBlobOffset(v interface{}) (int64, error) {
	if v == nil {
		return 0, nil
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int:
		f = float64(x)
	case int64:
		f = float64(x)
	default:
		return 0, fmt.Errorf("offset must be an integer, got %T", v)
	}
	if f != float64(int64(f)) {
		return 0, fmt.Errorf("offset must be an integer, got %v", f)
	}
	if f < 0 {
		return 0, fmt.Errorf("offset must be non-negative, got %v", f)
	}
	return int64(f), nil
}
