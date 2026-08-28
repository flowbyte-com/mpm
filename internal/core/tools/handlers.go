package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// handleSaveToMemory persists a memory row.
//
// Mirror contract (read this before debugging "why isn't my fact in
// mirror.jsonl"): the JSONL mirror at src/db/mirror.jsonl is appended to
// ONLY for these collections: changelog, memories, theories, decisions,
// knowledge, directives, mpm-projects, world-cup-2026. Collections NOT
// mirrored include:
//
//   - lessons  — lessons are cognitive-process trace, not source-of-truth
//                memory; the lesson's own index table is the source.
//   - scratchpad_orphans — ephemeral by definition; lives in
//                          ephemeral_scratchpad, not memories.
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
		internal.ParseFloatOr(p["weight"], 0.5),
		internal.ParseStringOr(p["ttl"], ""),
		ac,
		wc,
	)
	if err != nil {
		return nil, err
	}
	return out, nil
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
	ID                 string                  `json:"id"`
	Summary            string                  `json:"summary"`          // first 256 chars via SummarizeMemory
	Pointer            string                  `json:"pointer"`          // "mpm://memory/<id>"
	Type               string                  `json:"type"`             // always "memory"
	Tags               []string                `json:"tags,omitempty"`
	Collection         string                  `json:"collection"`
	CreatedAt          int64                   `json:"created_at"`
	ReinforcementCount int                     `json:"reinforcement_count"`
	Weight             int                     `json:"weight"`
	// RetrievalMetadata key always present; null when never retrieved.
	RetrievalMetadata *RetrievedEntryMetadata `json:"retrieval_metadata"`
	Score             float64                 `json:"score"`
	Rationale         string                  `json:"rationale"`
	IsStale           bool                    `json:"is_stale"`
}

// ProjectedLessonEntry is the Phase 2D pointer-native lesson output shape.
// Compact lessons are returned in full; the pointer enables re-retrieval.
type ProjectedLessonEntry struct {
	ID                 string                  `json:"id"`
	Summary            string                  `json:"summary"`    // full (lessons are compact)
	Pointer            string                  `json:"pointer"`   // "mpm://lesson/<id>"
	Type               string                  `json:"type"`      // warning/practice/insight
	Tags               []string                `json:"tags,omitempty"`
	CreatedAt          string                  `json:"created_at"`
	ReinforcementCount int                     `json:"reinforcement_count"`
	// RetrievalMetadata key always present; null when never retrieved.
	RetrievalMetadata *RetrievedEntryMetadata `json:"retrieval_metadata"`
	Rationale         string                  `json:"rationale"`
}

// computeScore derives a scalar from a memory map for projected output.
// Mirrors the scoring logic used in the FTS retrieval path.
func computeScore(mem map[string]interface{}) float64 {
	score, _ := mem["combined_score"].(float64)
	if score == 0 {
		weight, _ := mem["weight"].(int)
		reinf, _ := mem["reinforcement_count"].(int)
		score = float64(reinf*2) + float64(weight)*1.5
	}
	return score
}

// formatRationaleForMemory produces a human-readable provenance string for
// a projected memory entry.
func formatRationaleForMemory(mem map[string]interface{}) string {
	parts := make([]string, 0, 3)
	if coll, ok := mem["collection"].(string); ok && coll != "" {
		parts = append(parts, coll)
	}
	if w, ok := mem["weight"].(int); ok && w > 0 {
		parts = append(parts, fmt.Sprintf("weight %d", w))
	}
	if r, ok := mem["reinforcement_count"].(int); ok && r > 0 {
		parts = append(parts, fmt.Sprintf("%dx ref", r))
	}
	if len(parts) == 0 {
		return "memory"
	}
	return strings.Join(parts, " · ")
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
	limit := int(internal.ParseFloatOr(p["limit"], 5))
	if limit <= 0 {
		limit = 5
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
	projection, _ := p["projection"].(string)
	if projection == "" || projection == "summary" {
		projected := make([]ProjectedMemoryEntry, 0, len(items))
		for _, mem := range items {
			id, _ := mem["id"].(string)
			content, _ := mem["content"].(string)
			tags, _ := mem["tags"].([]string)
			coll, _ := mem["collection"].(string)
			createdAt, _ := mem["created_at"].(float64)
			reinf, _ := mem["reinforcement_count"].(int)
			weight, _ := mem["weight"].(int)

			summary, _ := internal.SummarizeMemoryWithEllipsis(content, 256)

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
				ID:                  id,
				Summary:             summary,
				Pointer:             "mpm://memory/" + id,
				Type:                "memory",
				Tags:                tags,
				Collection:          coll,
				CreatedAt:           int64(createdAt),
				ReinforcementCount:  reinf,
				Weight:              weight,
				RetrievalMetadata:   retMeta,
				Score:               computeScore(mem),
				Rationale:           rationale,
				IsStale:             isStale,
			})
		}
		return map[string]interface{}{
			"success":  true,
			"mode":     "summary",
			"query":    query,
			"memories": projected,
			"count":    len(projected),
			"scope":    defaultScope(scope),
		}, nil
	}

	// projection == "full": unbounded content, opt-in only.
	for _, mem := range items {
		content, _ := mem["content"].(string)
		id, _ := mem["id"].(string)
		mem["pointer"] = "mpm://memory/" + id
		if len(content) > 0 {
			mem["content"] = content
		}
	}
	return map[string]interface{}{
		"success":  true,
		"mode":     "full",
		"memories": items,
		"count":    len(items),
		"scope":    defaultScope(scope),
	}, nil
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
	dependencies := internal.ParseStringSliceOr(p["dependencies"])
	if dependencies == nil {
		dependencies = []string{}
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
// Arc 1 closure: if winnerId is provided in the params, the theory
// is treated as an arbitration theory (created by the close-call
// path of `mpm ops resolve-contradictions`). The system routes to
// ResolveArbitrationTheory which atomically slashes the loser,
// writes a resolution memory, attaches evidence to the winner, and
// marks the queue row resolved. If winnerId is absent, the legacy
// path runs (just mark the theory resolved; no slash).
func handleResolveTheory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	theoryID, _ := p["theoryId"].(string)
	if theoryID == "" {
		return nil, fmt.Errorf("theoryId is required")
	}
	conclusion, _ := p["conclusion"].(string)
	if conclusion == "" {
		return nil, fmt.Errorf("conclusion is required")
	}
	winnerID, _ := p["winnerId"].(string)

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

	newStatus, _ := p["newStatus"].(string)
	if newStatus != "proven" && newStatus != "disproven" {
		return nil, fmt.Errorf("newStatus must be 'proven' or 'disproven'")
	}

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
			"theory_id": theoryID,
			"status":    newStatus,
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
	tags := internal.ParseStringSliceOr(p["tags"])
	if tags == nil {
		tags = []string{}
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

// handleSupersedeDecision implements the F9 invalidation path: a corrected
// or superseding decision that is distinguishable from stale knowledge.
func handleSupersedeDecision(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	originalID, _ := p["original_id"].(string)
	if originalID == "" {
		return nil, fmt.Errorf("original_id is required")
	}
	choice, _ := p["choice"].(string)
	if choice == "" {
		return nil, fmt.Errorf("choice is required (the replacement decision)")
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
func handleInvalidateDecision(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["decision_id"].(string)
	if id == "" {
		return nil, fmt.Errorf("decision_id is required")
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
	id, _ := p["memory_id"].(string)

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

func handleReinforceMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	delta := int(internal.ParseFloatOr(p["delta"], 1))
	return dm.ReinforceMemoryTool(id, delta)
}

func handleWeakenMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	delta := int(internal.ParseFloatOr(p["delta"], 1))
	return dm.WeakenMemoryTool(id, delta)
}

func handleSnoozeMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	days := int(internal.ParseFloatOr(p["days"], 1))
	return dm.SnoozeMemory(id, days)
}

func handleSetMemoryWeight(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	weight := int(internal.ParseFloatOr(p["weight"], 0))
	return dm.SetMemoryWeight(id, weight)
}

func handlePatchMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	patch, _ := p["patch"]
	// patch must be a JSON object (map). The DM layer takes a string,
	// so marshal here. A nil/primitive patch is rejected upstream by
	// the DM (UpdateMemoryMetadata validates the prefix).
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("patch must be JSON-marshalable: %w", err)
	}
	return dm.PatchMemoryMetadata(id, string(patchJSON))
}

func handlePromoteMemory(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	id, _ := p["memory_id"].(string)
	return dm.PromoteMemory(id)
}

// handleMigrate imports memories from a non-SQLite source file. Wraps the
// markdown/json migration pipeline for MCP clients.
//
// Wire-format: {
//   "from_path":     "<path>",          # required for stage mode
//   "format":        "markdown|json|auto", # default "auto" (extension-based)
//   "label":         "<name>",          # optional batch label
//   "dry_run":       false,             # optional
//   "commit":        false,             # stage + immediately promote
//   "commit_batch":  "<batch_id>",      # alternative: just promote a staged batch
//   "undo_batch":    "<batch_id>"       # alternative: rollback a batch
// }
//
// Returns a map with rows_read, rows_staged, rows_skipped, rows_rejected,
// batch_id, and (if commit or commit_batch) rows_promoted.
func handleMigrate(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
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
func handleReviewMemories(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	days := int(internal.ParseFloatOr(p["days"], 30))
	limit := int(internal.ParseFloatOr(p["limit"], 20))
	return dm.ReviewMemories(days, limit)
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
	projection, _ := p["projection"].(string)
	if projection == "" || projection == "summary" {
		projected := make([]ProjectedLessonEntry, 0, len(items))
		for _, item := range items {
			id, _ := item["id"].(string)
			content, _ := item["content"].(string)
			lessonType, _ := item["type"].(string)
			tags, _ := item["tags"].([]string)
			created, _ := item["created_at"].(string)

			summary, _ := internal.SummarizeMemoryWithEllipsis(content, 256)

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
				ID:                 id,
				Summary:            summary,
				Pointer:            "mpm://lesson/" + id,
				Type:               lessonType,
				Tags:               tags,
				CreatedAt:          created,
				RetrievalMetadata:   retMeta,
				Rationale:          rationale,
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
	lessonType := internal.ParseStringOr(p["type"], "")

	items, err := dm.ListLessonsFiltered(lessonType)
	if err != nil {
		return nil, err
	}

	// Phase 2D: pointer-native projection.
	projection, _ := p["projection"].(string)
	if projection == "" || projection == "summary" {
		projected := make([]ProjectedLessonEntry, 0, len(items))
		for _, item := range items {
			id, _ := item["id"].(string)
			content, _ := item["content"].(string)
			lessonType, _ := item["type"].(string)
			tags, _ := item["tags"].([]string)
			created, _ := item["created_at"].(string)

			summary, _ := internal.SummarizeMemoryWithEllipsis(content, 256)

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
				ID:                 id,
				Summary:            summary,
				Pointer:            "mpm://lesson/" + id,
				Type:               lessonType,
				Tags:               tags,
				CreatedAt:          created,
				RetrievalMetadata:   retMeta,
				Rationale:          rationale,
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
func handleLinkTopic(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	memoryID, _ := p["memory_id"].(string)
	if memoryID == "" {
		return nil, fmt.Errorf("memory_id is required")
	}
	topicID, _ := p["topic_id"].(string)
	if topicID == "" {
		return nil, fmt.Errorf("topic_id is required")
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

// callReadWakeContext returns the last session's context. Data gathering is
// delegated to internal.ReadWakeContext (single source of truth shared with
// the Go MCP server).
func handleReadWakeContext(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, params map[string]interface{}) (interface{}, error) {

	// format=system-prompt returns the human-readable projection instead of JSON.
	// Same WakeContextData, different presentation — per the Projection Principle.
	if format, _ := params["format"].(string); format == "system-prompt" {
		text, err := dm.ReadWakeContext()
		if err != nil {
			return nil, fmt.Errorf("read wake context (system-prompt): %w", err)
		}
		return map[string]interface{}{"success": true, "format": "system-prompt", "content": text}, nil
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
		"success":             true,
		// Wire-format metadata (added with the v4 schema bump). Both
		// unix-seconds; GeneratedAt = "when the struct was assembled",
		// AsOf = "what 'now' represented substrate state at". v3 callers
		// ignore these safely (extra fields are non-additive for them).
		"context_version":     data.ContextVersion,
		"generated_at":        data.GeneratedAt,
		"as_of":               data.AsOf,
		// Identity — session_id retained as the v3 alias for
		// SessionCurrentID. New v4 callers should prefer the split
		// fields below; older callers can keep using session_id.
		"session_id":                 data.SessionID,
		"session_current_id":         data.SessionCurrentID,
		"session_previous_id":       data.SessionPreviousID,
		"session_started_at":        data.SessionStartedAt,
		"session_previous_ended_at": data.SessionPreviousEndedAt,
		"active_mode":         data.ActiveMode,
		"active_persona":       data.ActivePersona,
		// Orientation block.
		"recent_topics":       data.RecentTopics,
		"recent_topics_truncated": data.RecentTopicsTruncated,
		"recent_memories":     memRefs,
		"recent_milestones":   milestoneRefs,
		"recent_theories":     recentTheories,
		"recent_lessons":      recentLessons,
		"recent_decisions":    recentDecisions,
		// Attention & pending work.
		"overdue_wakes":       overdueRefs,
		"open_works":          data.OpenWorks,
		// completed_works: RECOMMENDED 10. Pairs with open_works so the
		// agent sees both "what's waiting on me" and "what I just shipped"
		// without a separate tool call. Same WakeContextWork shape as
		// open_works for symmetric parsing. Always emitted (empty list,
		// not omitempty) so callers can branch on field presence.
		"completed_works":     data.CompletedWorks,
		"scratchpad_orphans":  data.ScratchpadOrphans,
		"last_handoff":        data.LastHandoff,
		// Constraints & capabilities.
		"global_rules":        globalRuleRefs,
		"available_skills":    skillRefs,
		"available_skills_truncated": data.AvailableSkillsTruncated,
		// System health.
		"audit_summary":       data.AuditSummary,
		"epistemic_pressure":  data.EpistemicPressure,
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
			"pointer":    "mpm://memory/" + id,
			"created_at": createdAt,
		})
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	return out
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
	var strength float64
	if s, ok := payload["strength"].(float64); ok {
		strength = s
	}
	var independence float64 = 1.0
	if i, ok := payload["independence_factor"].(float64); ok {
		independence = i
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
// git commit. Mirrors the MCP log_to_changelog tool exactly — the
// CLI/mcp parity is enforced by routing both through
// DatabaseManager.LogChangelogEntry, which holds the strict
// retrospective contract (full 40-char SHA-1 required). When the
// synthesis engine lands, both the MCP tool and this CLI handler
// will be joined with the git log via the (commit_hash,
// mpm_memory_id) key in changelog.json.
func handleLogToChangelog(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	fact, _ := p["fact"].(string)
	commitHash, _ := p["commit_hash"].(string)
	if fact == "" {
		return nil, fmt.Errorf("fact is required")
	}
	if commitHash == "" {
		return nil, fmt.Errorf("commit_hash is required (strict retrospective contract: every changelog memory must reference an existing commit). Run `git rev-parse HEAD` to get the canonical 40-char SHA-1")
	}

	id, err := dm.LogChangelogEntry(fact, commitHash, internal.ParseStringSliceOr(p["tags"]))
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":     true,
		"id":          id,
		"commit_hash": commitHash,
		"collection":  "changelog",
	}, nil
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
//   - content (string, required)     — full markdown incl. frontmatter
//   - author (string, optional)      — agent name for metadata
//   - force (bool, optional)         — overwrite when name+version exists
//
// Validation order is deliberate: SkillIDForNameAndVersion first (cheap
// arg-shape check) so the caller gets a precise error message before
// the more expensive frontmatter parse. ParseSkillFrontmatter runs
// next so the DM never sees malformed YAML — keeping rejection at the
// boundary rather than mid-transaction. dm.SaveSkill re-runs both for
// defence-in-depth, but a front-end rejection here saves a DB round
// trip and produces a tighter error string.
func handleSaveSkill(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	name := internal.ParseStringOr(p["name"], "")
	version := internal.ParseStringOr(p["version"], "")
	content := internal.ParseStringOr(p["content"], "")
	author := internal.ParseStringOr(p["author"], ac.Agent)
	force := false
	if v, ok := p["force"].(bool); ok {
		force = v
	}
	if name == "" || version == "" || content == "" {
		return nil, fmt.Errorf("name, version, and content are required")
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

// handleReadSkill fetches a skill by name (latest version) or exact id.
func handleReadSkill(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	name := internal.ParseStringOr(p["name"], "")
	version := internal.ParseStringOr(p["version"], "")
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	skill, err := dm.ReadSkill(name, version)
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

// handleListSkills returns the latest version of each skill in scope.
func handleListSkills(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	scope := internal.ParseStringOr(p["scope"], "all")
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
//	--days       (optional) lookback window in days; default 7
//	--limit      (optional) max rows; default 20, max 500
func handleQueryAuditLog(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	levelStr := getString(p, "level")
	component := getString(p, "component")
	days := 7
	if v, ok := p["days"]; ok {
		switch t := v.(type) {
		case float64:
			days = int(t)
		case int:
			days = t
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

	items, err := dm.QueryAuditLog(internal.AuditLevel(levelStr), component, days, limit)
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
		"success":         true,
		"known_clusters":  known,
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
//                              OR Go duration ("24h", "7d", "1h30m")
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
//                              OR remembered historical key for
//                              resolved clusters).
//   - annotation  (required) — substantive insight text; appended to
//                              the audit trail verbatim.
//   - reason      (optional) — short label (e.g. "post-mortem",
//                              "week-later-refinement").
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
func handleHandoffWrite(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	sessionID := getString(p, "session_id")
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}
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
	// exactly the data-loss class the audit flags. EndSession already
	// persists both columns and read-backs the row; the wake context
	// renders open_questions for the next session.
	commitments := internal.ParseStringSliceOr(p["commitments"])
	openQuestions := internal.ParseStringSliceOr(p["open_questions"])

	h, err := dm.EndSession(sessionID, summary, state, commitments, openQuestions)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":        true,
		"handoff":        h,
		"handoff_id":     h.ID,
		"message":        "handoff written. Pending work surfaces via mpm_work; open questions via mpm_theories.",
		"open_questions": h.OpenQuestions,
	}, nil
}

// handleHandoffRead returns the most recent handoff. The wake context
// surfaces unread handoffs automatically; this tool is for explicit
// re-reads of any handoff (read or unread).
//
// Args:
//
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
	if unreadOnly {
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
	id, _ := p["id"].(string)
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
		return nil, fmt.Errorf("shred_handoff: 'id' (or 'session_id') is required")
	}

	n, err := dm.DeleteHandoff(id)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":  true,
		"shredded": n > 0,
		"handoff_id": id,
		"rows_deleted": n,
		"message": "handoff shredded",
	}, nil
}

// handleQueryGlobalRules returns memories from the shared DB that
// are marked is_global = 1. This is the read-side of the multi-agent
// shared epistemology: every agent on the workstation sees the same
// house rules, conventions, and persona overlays.
//
// Args:
//
//	--query  (optional) FTS5 keyword search
//	--limit  (optional) max rows; default 50, max 500
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

	items, err := dm.QueryGlobalRules(query, limit)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":  true,
		"source":   "shared",
		"attached": dm.SharedAttached() != "",
		"count":    len(items),
		"results":  items,
	}, nil
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
	weight := 10
	if v, ok := p["weight"]; ok {
		switch t := v.(type) {
		case float64:
			weight = int(t)
		case int:
			weight = t
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
			"rule_id":  id,
			"weight":   weight,
			"tags":     tags,
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
		Kind:        "rule",
		Rationale:   broadcastRationale,
		SourceAgent: ac.Agent,
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
	skillID := internal.ParseStringOr(p["skill_id"], "")
	if skillID == "" {
		return nil, fmt.Errorf("skill_id is required")
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
// Args:
//
//	--skill_id  (required) The skill id to delete (e.g. "skill:agentshell-v1.0.0")
func handleDeleteSkill(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	skillID := internal.ParseStringOr(p["skill_id"], "")
	if skillID == "" {
		return nil, fmt.Errorf("skill_id is required")
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
	items, err := dm.ListScheduledWakes(includeFired, overdueOnly, limit)
	if err != nil {
		return nil, err
	}
	return checkWakesAndFold(dm, ac, map[string]interface{}{
		"success":       true,
		"wakes":         items,
		"count":         len(items),
		"include_fired": includeFired,
		"overdue_only":  overdueOnly,
	}, nil), nil
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
// Reads its own state (raw_count, threshold) from the pressure view,
// batches the oldest 50 raw memories, calls the LLM for a strict-JSON
// lesson, validates, and commits atomically. See internal/core.CompactEpistemology
// for the orchestration details.
//
// Optional `force` flag bypasses the pressure threshold — used by
// agents who want to compact proactively even when the gauge reads
// below_threshold (rare; mostly for tests and one-off cleanups).
func handleCompactEpistemology(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	force := parseBoolDefault(p["force"], false)

	result, err := dm.CompactEpistemology(context.Background(), force)
	if err != nil {
		return nil, err
	}

	// No-op path (skipped_reason set): return as-is.
	if result.SkippedReason != "" {
		return map[string]interface{}{
			"success":        true,
			"compacted":       0,
			"lessons_created": 0,
			"raw_marked":      0,
			"skipped_reason":  result.SkippedReason,
		}, nil
	}

	// Commit path: lesson inserted, raw memories marked.
	return map[string]interface{}{
		"success":         true,
		"compacted":        result.Compacted,
		"lessons_created":  result.LessonsCreated,
		"raw_marked":      result.RawMarked,
		"lesson_id":        result.LessonID,
	}, nil
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
		return nil, fmt.Errorf("session_id is required")
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
		"session_id":  sessionID,
		"thesis":      thesis,
		"supporting":  supporting,
		"updated_at":  updatedAt,
	}, nil
}

// handleDiscardScratchpad hard-deletes a scratchpad row. No soft-delete
// overhead — scratchpads are volatile by design. Idempotent: deleting a
// non-existent row is a no-op (RowsAffected=0, no error).
func handleDiscardScratchpad(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	sessionID := getString(p, "session_id")
	if sessionID == "" {
		return nil, fmt.Errorf("session_id is required")
	}

	if _, err := dm.ExecTracked(`DELETE FROM ephemeral_scratchpad WHERE session_id = ?`, 0, sessionID); err != nil {
		return nil, fmt.Errorf("discard scratchpad: %w", err)
	}
	return map[string]string{"status": "discarded", "session_id": sessionID}, nil
}

// handlePromoteScratchpad is the only multi-table mutation in the scratchpad
// surface. It atomically:
//   1. SELECTs the scratchpad row (inside the tx — race window closed)
//   2. INSERTs a memory via SaveMemoryNode (scanner runs INSIDE the tx)
//   3. DELETEs the scratchpad row (also inside the tx)
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
		return nil, fmt.Errorf("session_id is required")
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
//   Stage 1: raw query → BuildFTS5Query() → final MATCH string
//   Stage 2: FTS5 row count + BM25 distribution + top 3 IDs
//   Stage 3: HybridSearch input/output count + discarded-row analysis
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
		"match_string":     transformedMATCH,
		"limit_fetched":    limit * 4, // fetch wider than the HybridSearch fetch (limit*2) so we can see what's discarded
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
					"min":    sortedScores[0],                                  // most negative = best match
					"max":    sortedScores[len(sortedScores)-1],               // least negative = worst match
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
					"min":    discardedScores[0],                            // most-negative discarded score
					"max":    discardedScores[len(discardedScores)-1],      // least-negative discarded score
					"count":  len(discardedScores),
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

// handleRequestReview implements the request_review MCP tool.
//
// Architectural intent (Wed 2026-07-29 design session):
//
//   Adapter layer for the ReviewCoordinator orchestration primitive
//   (internal/core/orchestration). This handler is responsible for:
//
//     1. Fetching artifact bodies from the database (resolving ids
//        in the caller's 'artifacts' list to actual text).
//     2. Building the substrate-side RequestReview payload (a
//        ReviewRequest — see orchestration/review_coordinator.go).
//     3. Calling DefaultReviewCoordinator.Execute().
//     4. Rendering the resulting []ReviewResult via the renderers
//        package, which returns Markdown-shaped output suitable for
//        both agent consumers (LLMs parse it back as text) and
//        humans (operators read the dashboard).
//
// The handler is intentionally thin. All the concurrency, timeout,
// profile resolution, and fan-out live in the engine layer
// (orchestration package). All the markdown rendering lives in
// the renderers package. This handler is the only one in the
// call chain that knows about the database, the coordinator, and
// the renderer.
func handleRequestReview(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	// Note on error shape: the TestRegistry_AllToolsExecuteWithoutPanic
	// harness compares CLI and MCP error strings after stripping the
	// MCP adapter's "<tool> failed: " prefix via strings.LastIndex(": ").
	// The CLI side returns the bare error from this handler, so
	// including the tool name in the error string would create a
	// permanent drift (CLI: "request_review: ..." vs stripped-MCP:
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
	coord := orchestration.NewDefaultReviewCoordinator(cfg, orchestration.DefaultModelFactory())
	req := orchestration.ReviewRequest{
		Components:  components,
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
// mpm_session, mpm_wakes, …) silently coerced a missing `params` envelope
// to an empty map. That meant a payload shaped like
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
// Contract: every domain tool (mpm_memory, mpm_session, …) takes a payload
// whose TOP-LEVEL fields may ONLY be `action` (string) and `params`
// (object). Anything else is a contract violation and is rejected with a
// schema-error message that names the tool, the missing/extra fields, and
// the correct envelope shape — so the caller knows exactly what to fix.
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

	// `params` must be present AND object-typed. Missing params is a
	// contract violation; an empty object is fine (and meaningful for
	// actions with no required params).
	params, ok := payload["params"].(map[string]interface{})
	if !ok {
		// Distinguish "missing" from "wrong type" so the error message
		// tells the caller which edit they need to make.
		if _, present := payload["params"]; !present {
			return nil, fmt.Errorf(
				"%s: missing required field `params` (object) — expected shape "+
					"{\"action\":\"<op>\",\"params\":{...}}. "+
					"Action `%v` requires an explicit params envelope, even if empty.",
				toolName, payload["action"],
			)
		}
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
	case "shred":
		return handleShredMemory(dm, ac, params)
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
	case "commit_milestone":
		return handleCommitMilestone(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_memory. Valid actions include save, query, shred, reinforce, weaken, snooze, set_weight, patch, promote, review, synthesize, challenge, commit_milestone", action)
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
	case "upsert_task":
		return handleUpsertScheduledTask(dm, ac, params)
	case "list_tasks":
		return handleListScheduledTasks(dm, ac, params)
	case "delete_task":
		return handleDeleteScheduledTask(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_wakes. Valid actions include schedule, check, check_pending_event, list, digest, upsert_task, list_tasks, delete_task", action)
	}
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
		// Normalize snake_case to camelCase for the underlying handler.
		// Routed through the named helper so the schema-guard AST walker
		// doesn't false-positive on dispatcher body as "unread payload keys".
		normalizeCamelCaseKeys(params, map[string]string{
			"theory_id":  "theoryId",
			"new_status": "newStatus",
			"winner_id":  "winnerId",
		})
		return handleResolveTheory(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_theories. Valid actions include propose, resolve", action)
	}
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
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_lessons. Valid actions include save, search, list", action)
	}
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
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_decisions. Valid actions include record, supersede, invalidate", action)
	}
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
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_topics. Valid actions include create, search, link", action)
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
	case "search":
		return handleSearchReferences(dm, ac, params)
	case "list":
		return handleListReferences(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_references. Valid actions include add, search, list", action)
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
	case "promote_to_global":
		return handlePromoteToGlobal(dm, ac, params)
	case "route":
		return handleRoute(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_context. Valid actions include read_wake_context, read_directives, proactive_recall_hint, query_global_rules, record_global_rule, promote_to_global, route", action)
	}
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
	case "resolve_cluster":
		return handleResolveCluster(dm, ac, params)
	case "annotate_cluster":
		return handleAnnotateCluster(dm, ac, params)
	default:
		return nil, fmt.Errorf("unknown action %q for mpm_system. Valid actions include gc_run, compact, health_check, migrate, query_audit_log, list_clusters, snooze_cluster, resolve_cluster, annotate_cluster", action)
	}
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

	// Parse the mpm:// URI locally (mpm-core cannot import main module's pointer).
	ptr, err := parsePointerURI(uri)
	if err != nil {
		return nil, err
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
			maxB := int(maxBytes)
			if maxB <= 0 {
				maxB = 512
			}
			bounded := len(content) > maxB
			if bounded {
				// Use same bounding helper as query path (first 256 truncation is for wake;
				// here we bound to maxB).
				if len(content) > maxB {
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
			if mem != nil {
				resp["metadata"] = mem
			}
			return resp, nil
		case "lesson":
			lesson, err := dm.GetLesson(ptr.ID)
			if err != nil {
				return nil, err
			}
			_ = dm.RecordRetrieval(ptr.ID, "lesson")
			return map[string]interface{}{
				"content":      lesson.Content,
				"content_type": "text/plain",
				"pointer":      "mpm://lesson/" + ptr.ID,
				"bounded":      false,
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
			return map[string]interface{}{
				"content":      content,
				"content_type": "text/plain",
				"pointer":      "mpm://theory/" + ptr.ID,
				"bounded":      false,
				"metadata":     mem,
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
			maxB := int(maxBytes)
			if maxB <= 0 {
				maxB = 512
			}
			bounded := len(content) > maxB
			if bounded && len(content) > maxB {
				content = content[:maxB]
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
	if result.Metadata != nil {
		resp["metadata"] = result.Metadata
	}
	return resp, nil
}

// parsePointerURI parses a mpm:// URI into a Pointer.
// Duplicates internal/pointer.Parse logic here so mpm-core tools does not
// need to import the main module's pointer package.
// parsePointerURI is a local copy of pointer.Parse for use by the tools
// package, which cannot import the main module's internal/pointer package.
// This implementation must stay in sync with pointer.Parse.
//
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
	offset, _ := payload["offset"].(float64)
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
