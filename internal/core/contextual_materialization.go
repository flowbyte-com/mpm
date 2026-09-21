// contextual_materialization.go — Stage 2E.2 bounded contextual
// materialization. Turns Stage 2E.1 selected pointer metadata into
// small, authoritative, useful pieces of context. NO re-ranking.
// NO LLM. NO persistence. NO wake firing. NO handoff read mutation.
//
// Architectural placement
// ────────────────────────
//
//	discovery  (Stage 2D  — GenerateContextualCandidates)
//	selection  (Stage 2E.1 — SelectContextualCandidates)
//	materialization  (Stage 2E.2 — this file)
//	delivery   (Stage 2E.3 — wake-context integration)
//
// Materialization owns bounded detail, NOT relevance. Selection already
// decided what is relevant. A failed materialization is an observable
// fact about the projection, NOT permission to substitute unselected
// candidates.
//
// Observational guarantee
// ───────────────────────
//   - no INSERT/UPDATE/DELETE on any persistent table
//   - no read markers advanced on handoffs (no GetLatestUnreadHandoff
//     side-effect variants; only GetHandoffByID)
//   - no wake fired / no scheduled_wakes mutated
//   - no scratchpad writes
//   - no audit log writes
//   - no persisted projection state
//
// Deterministic
// ─────────────
// Identical (SelectionResult, limits) → byte-equivalent semantic output.
// Output order matches selection order (Stage 2E.1's deterministic
// policy order is preserved).

package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ── Public types ─────────────────────────────────────────────

// MaterializationStatus is the closed vocabulary for the per-item
// materialization outcome. statuses are mutually exclusive per item.
// `truncated` is an ORTHOGONAL boolean (see MaterializedContextItem);
// it never appears as a status. The exact invariant:
//
//	materialized
//	+ pointer_only
//	+ missing
//	+ unsupported
//	+ error
//	    == selected_input_count
//
// Every selected item is represented exactly once; grouping is not
// performed by Stage 2E.2 (selected_input_count == output_item_count).
type MaterializationStatus string

const (
	StatusMaterialized MaterializationStatus = "materialized"
	StatusPointerOnly  MaterializationStatus = "pointer_only"
	StatusMissing      MaterializationStatus = "missing"
	StatusUnsupported  MaterializationStatus = "unsupported"
	StatusError        MaterializationStatus = "error"
)

// MaterializationLimits bounds the bounded detail budget.
// All sizes are UTF-8 BYTE counts of the JSON-serialized Detail
// strings (deterministic, no tokenizer dependency, no fake token
// accounting). Truncation occurs only at valid UTF-8 codepoint
// boundaries.
//
// The DETAIL budget governs only the materialized Detail field.
// Selected metadata envelopes (kind/id/band/rationale/etc.) are NOT
// budget-counted and are always retained. The total serialized
// response size is reported separately for diagnostics.
type MaterializationLimits struct {
	// DetailBudgetBytes is the total budget for the byte-serialized
	// Detail strings across the whole result. Per-item selection of
	// pointer/band/rationale metadata is NOT counted against this
	// budget — metadata always fits.
	DetailBudgetBytes int `json:"detail_budget_bytes,omitempty"`
	// PerItemByteCap caps any single item's Detail length.
	PerItemByteCap int `json:"per_item_byte_cap,omitempty"`
}

// DefaultMaterializationLimits returns the canonical Stage 2E.2
// defaults. Tuned against current read_wake_context payload (~8 KB
// total) and the production-selected count (10 items).
//
//   - DetailBudgetBytes: 6000   (one Stage-2E.1 selection × compact per-kind detail)
//   - PerItemByteCap:    800    (single artifact cannot monopolize)
//
// Per-item cap is well below the global default so the global budget
// IS the gating constraint, not the per-item cap.
func DefaultMaterializationLimits() MaterializationLimits {
	return MaterializationLimits{
		DetailBudgetBytes: 6000,
		PerItemByteCap:    800,
	}
}

// HardMaterializationLimits are the safety ceilings. Not overridable
// from the public handler. These are the maximum bounds the
// materializer will accept; callers asking for larger bounds are
// silently clamped.
const (
	HardMaterializationDetailBudget = 20000
	HardMaterializationPerItemCap   = 4000
)

// MaterializedContextItem is one materialized output. Every item
// retains the Stage-2E.1 selected metadata envelope so an agent can
// always follow the pointer. Detail is bounded and may be empty
// when status is pointer_only / missing / unsupported / error.
// Truncated is an orthogonal boolean describing whether the
// Detail was shortened at the per-item cap.
type MaterializedContextItem struct {
	// ── Compact selection-reference envelope (always populated) ──
	CandidateID          string                   `json:"candidate_id"` // kind:artifact_id
	Kind                 string                   `json:"kind"`
	ArtifactID           string                   `json:"artifact_id"`
	Pointer              string                   `json:"pointer,omitempty"`
	Timestamp            int64                    `json:"timestamp,omitempty"`
	Band                 string                   `json:"band"`
	Rationale            string                   `json:"rationale"`
	WhyNow               string                   `json:"why_now"`
	LifecycleState       string                   `json:"lifecycle_state,omitempty"`
	CombinationMatches   []string                 `json:"combination_matches,omitempty"`
	CompressedRelatedIDs []string                 `json:"compressed_related_ids,omitempty"`
	CompressionTriggers  []MaterializationTrigger `json:"compression_triggers,omitempty"`
	ActorKind            string                   `json:"actor_kind,omitempty"`
	FrameworkName        string                   `json:"framework_name,omitempty"`

	// ── Materialization outcome ──
	Status      MaterializationStatus `json:"status"`
	Detail      string                `json:"detail,omitempty"`    // bounded safe representation
	DetailBytes int                   `json:"detail_bytes"`        // len([]byte(Detail))
	Truncated   bool                  `json:"truncated,omitempty"` // orthogonal to Status
	Note        string                `json:"note,omitempty"`      // short status hint
}

// MaterializationTrigger is the bounded wire form of SelectionTrigger
// for the materialization envelope. Lives here so the Stage-2E.2
// surface is self-contained.
type MaterializationTrigger struct {
	SourceID        string `json:"source_id"`
	Band            string `json:"band"`
	Rationale       string `json:"rationale"`
	CombinationName string `json:"combination_name,omitempty"`
}

// MaterializationResult is the bounded output of the materializer.
type MaterializationResult struct {
	Items       []MaterializedContextItem  `json:"items"`
	Diagnostics MaterializationDiagnostics `json:"diagnostics"`
}

// MaterializationDiagnostics captures the run metadata. All canonical
// statuses / kinds pre-zero-registered.
type MaterializationDiagnostics struct {
	MaterializedAt       int64          `json:"materialized_at"`
	SelectedInputCount   int            `json:"selected_input_count"`
	OutputItemCount      int            `json:"output_item_count"`
	DetailBytesUsed      int            `json:"detail_bytes_used"`
	DetailBudgetBytes    int            `json:"detail_budget_bytes"`
	TruncatedItemCount   int            `json:"truncated_item_count"`
	ReadsAttempted       int            `json:"reads_attempted"`
	ReadsByKind          map[string]int `json:"reads_by_kind"`
	ByStatus             map[string]int `json:"by_status"`
	ByKind               map[string]int `json:"by_kind"`
	SerializedBytesTotal int            `json:"serialized_bytes_total"`
}

// ── Materializer entry ────────────────────────────────────────

// preItem is the internal per-item record used by
// MaterializeContextualSelection across PASS 1 (compact safe detail)
// and PASS 2 (per-item cap + budget allocation). Kept package-local.
type preItem struct {
	item  MaterializedContextItem
	bytes int
}

// MaterializeContextualSelection turns a Stage-2E.1 SelectionResult
// into bounded MaterializedContextItems. Selection order is preserved.
// Failed materializations become pointer-only / missing / error
// items — never replacements with unselected candidates.
//
// Pure: does not advance handoff read markers, does not fire wakes,
// does not write to any table.
func MaterializeContextualSelection(
	selection SelectionResult,
	dm *DatabaseManager,
	limits MaterializationLimits,
	now int64,
) MaterializationResult {
	if dm == nil {
		return MaterializationResult{
			Diagnostics: emptyDiag(now, limits, len(selection.Items)),
		}
	}
	if limits.DetailBudgetBytes <= 0 {
		limits.DetailBudgetBytes = DefaultMaterializationLimits().DetailBudgetBytes
	}
	if limits.PerItemByteCap <= 0 {
		limits.PerItemByteCap = DefaultMaterializationLimits().PerItemByteCap
	}
	if limits.DetailBudgetBytes > HardMaterializationDetailBudget {
		limits.DetailBudgetBytes = HardMaterializationDetailBudget
	}
	if limits.PerItemByteCap > HardMaterializationPerItemCap {
		limits.PerItemByteCap = HardMaterializationPerItemCap
	}

	// Pre-zero diagnostics.
	byStatus := map[string]int{}
	for _, s := range []MaterializationStatus{
		StatusMaterialized, StatusPointerOnly, StatusMissing,
		StatusUnsupported, StatusError,
	} {
		byStatus[string(s)] = 0
	}
	byKind := map[string]int{}
	for _, k := range CanonicalKinds {
		byKind[k] = 0
	}
	readsByKind := map[string]int{}
	for _, k := range CanonicalKinds {
		readsByKind[k] = 0
	}

	// PASS 1: build compact safe detail for every item. Each item
	// produces exactly one output (1 selected == 1 output). No
	// grouping is performed in Stage 2E.2.
	pre := make([]preItem, 0, len(selection.Items))
	readsAttempted := 0
	for _, sel := range selection.Items {
		mi, reads := materializeOne(sel, dm, limits, now)
		readsAttempted += reads
		pre = append(pre, preItem{item: mi, bytes: len([]byte(mi.Detail))})
		byStatus[string(mi.Status)]++
		byKind[mi.Kind]++
		readsByKind[mi.Kind] += reads
	}

	// PASS 2: apply per-item cap, then budget-capped detail allocation.
	// Items that fit the per-item cap are kept verbatim; items that
	// exceed it are truncated to the cap. Truncated items count
	// toward the budget.
	detailBytes := 0
	truncatedCount := 0
	output := make([]MaterializedContextItem, 0, len(pre))
	for _, p := range pre {
		// Per-item cap (deterministic rune-safe truncation).
		item := p.item
		b := p.bytes
		if b > limits.PerItemByteCap {
			item.Detail = truncateUTF8(item.Detail, limits.PerItemByteCap)
			item.DetailBytes = len([]byte(item.Detail))
			item.Truncated = true
			truncatedCount++
			b = item.DetailBytes
		}
		// Budget enforcement: if remaining budget is exhausted, fall
		// back to pointer-only. The selected metadata envelope is
		// retained; only Detail is dropped.
		if detailBytes+b > limits.DetailBudgetBytes {
			if b > 0 {
				item.Detail = ""
				item.DetailBytes = 0
				if item.Status == StatusMaterialized {
					item.Status = StatusPointerOnly
					item.Note = "budget_exhausted"
					byStatus[string(StatusMaterialized)]--
					byStatus[string(StatusPointerOnly)]++
				}
			}
			b = 0
		}
		detailBytes += b
		output = append(output, item)
	}

	return MaterializationResult{
		Items: output,
		Diagnostics: MaterializationDiagnostics{
			MaterializedAt:       now,
			SelectedInputCount:   len(selection.Items),
			OutputItemCount:      len(output),
			DetailBytesUsed:      detailBytes,
			DetailBudgetBytes:    limits.DetailBudgetBytes,
			TruncatedItemCount:   truncatedCount,
			ReadsAttempted:       readsAttempted,
			ReadsByKind:          readsByKind,
			ByStatus:             byStatus,
			ByKind:               byKind,
			SerializedBytesTotal: 0,
		},
	}
}

func emptyDiag(now int64, limits MaterializationLimits, selected int) MaterializationDiagnostics {
	byStatus := map[string]int{}
	for _, s := range []MaterializationStatus{
		StatusMaterialized, StatusPointerOnly, StatusMissing,
		StatusUnsupported, StatusError,
	} {
		byStatus[string(s)] = 0
	}
	byKind := map[string]int{}
	for _, k := range CanonicalKinds {
		byKind[k] = 0
	}
	readsByKind := map[string]int{}
	for _, k := range CanonicalKinds {
		readsByKind[k] = 0
	}
	return MaterializationDiagnostics{
		MaterializedAt:       now,
		SelectedInputCount:   selected,
		OutputItemCount:      0,
		DetailBudgetBytes:    limits.DetailBudgetBytes,
		ReadsByKind:          readsByKind,
		ByStatus:             byStatus,
		ByKind:               byKind,
		SerializedBytesTotal: 0,
	}
}

// materializeOne turns one SelectedItem into one MaterializedContextItem.
// Returns (item, reads_attempted). One selected item is always one
// materialized item; no grouping is performed.
func materializeOne(
	sel SelectedItem,
	dm *DatabaseManager,
	limits MaterializationLimits,
	now int64,
) (MaterializedContextItem, int) {
	// Compact selection-reference envelope (always populated).
	mi := MaterializedContextItem{
		CandidateID:          sel.Candidate.ID,
		Kind:                 sel.Candidate.Kind,
		ArtifactID:           sel.Candidate.ArtifactID,
		Pointer:              sel.Candidate.Pointer,
		Timestamp:            sel.Candidate.Timestamp,
		Band:                 sel.Band,
		Rationale:            string(sel.Rationale),
		WhyNow:               sel.WhyNow,
		LifecycleState:       sel.Candidate.LifecycleState,
		CombinationMatches:   append([]string(nil), sel.CombinationMatches...),
		CompressedRelatedIDs: append([]string(nil), sel.CompressedRelatedIDs...),
		ActorKind:            sel.Candidate.ActorKind,
		FrameworkName:        sel.Candidate.FrameworkName,
	}
	for _, t := range sel.CompressionTriggers {
		mi.CompressionTriggers = append(mi.CompressionTriggers, MaterializationTrigger{
			SourceID:        t.SourceID,
			Band:            t.Band.String(),
			Rationale:       string(t.Rationale),
			CombinationName: t.CombinationName,
		})
	}
	if mi.CombinationMatches == nil {
		mi.CombinationMatches = []string{}
	}
	if mi.CompressedRelatedIDs == nil {
		mi.CompressedRelatedIDs = []string{}
	}
	if mi.CompressionTriggers == nil {
		mi.CompressionTriggers = []MaterializationTrigger{}
	}

	reads := 0
	switch sel.Candidate.Kind {
	case "work":
		mi.Status, mi.Detail, mi.Note = matWork(dm, sel.Candidate.ArtifactID, &reads)
	case "handoff":
		mi.Status, mi.Detail, mi.Note = matHandoff(dm, sel.Candidate.ArtifactID, &reads)
	case "memory":
		mi.Status, mi.Detail, mi.Note = matMemory(dm, sel.Candidate.ArtifactID, &reads)
	case "lesson":
		mi.Status, mi.Detail, mi.Note = matLesson(dm, sel.Candidate.ArtifactID, &reads)
	case "theory":
		mi.Status, mi.Detail, mi.Note = matTheory(dm, sel.Candidate.ArtifactID, &reads)
	case "decision":
		mi.Status, mi.Detail, mi.Note = matDecision(dm, sel.Candidate.ArtifactID, &reads)
	case "evidence":
		mi.Status, mi.Detail, mi.Note = matEvidence(dm, sel.Candidate.ArtifactID, &reads)
	case "wake":
		mi.Status, mi.Detail, mi.Note = matWake(dm, sel.Candidate.ArtifactID, now, &reads)
	case "scratchpad":
		mi.Status, mi.Detail, mi.Note = matScratchpad(dm, sel.Candidate.ArtifactID, &reads)
	case "activity":
		mi.Status, mi.Detail, mi.Note = matActivity(&sel.Candidate, &reads)
	default:
		mi.Status = StatusUnsupported
		mi.Note = "kind_not_in_canonical_kinds"
	}
	mi.DetailBytes = len([]byte(mi.Detail))
	_ = limits
	return mi, reads
}

// ── Kind-specific materializers ─────────────────────────────
//
// Each mat* function:
//   - calls a read-only getter (no side effects)
//   - builds a compact safe representation
//   - returns (status, detail, note)
//   - reads counter is incremented via the *reads pointer
//
// All detail strings are sized to fit roughly within the per-item
// cap before budget enforcement; PASS 2 in MaterializeContextualSelection
// applies the hard per-item cap.

func matWork(dm *DatabaseManager, id string, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	w, err := dm.GetWork(id)
	if err != nil || w == nil {
		return StatusMissing, "", "work_not_found"
	}
	status := string(w.Status)
	title := truncate(w.Title, 200)
	verification := string(w.Verification)
	updatedAt := w.UpdatedAt
	detail := fmt.Sprintf("status=%s; verification=%s; updated_at=%d; title=%s",
		status, verification, updatedAt, title)
	return StatusMaterialized, detail, ""
}

func matHandoff(dm *DatabaseManager, id string, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	// Read-only: GetHandoffByID does NOT mutate read_at.
	h, err := dm.GetHandoffByID(id)
	if err != nil || h == nil {
		return StatusMissing, "", "handoff_not_found"
	}
	summary := truncate(h.Summary, 400)
	var commits, questions string
	if len(h.Commitments) > 0 {
		commits = strings.Join(h.Commitments, " | ")
		commits = truncate(commits, 200)
	}
	if len(h.OpenQuestions) > 0 {
		questions = strings.Join(h.OpenQuestions, " | ")
		questions = truncate(questions, 200)
	}
	detail := fmt.Sprintf("ended_at=%d; ended_state=%s; summary=%s; commitments=%s; open_questions=%s",
		h.EndedAt, h.EndedState, summary, commits, questions)
	return StatusMaterialized, detail, ""
}

func matMemory(dm *DatabaseManager, id string, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	// GetMemory returns map; safe fields are content, collection,
	// weight, updated_at. Metadata may carry arbitrary payload —
	// we deliberately exclude it.
	m, err := dm.GetMemory(id)
	if err != nil || m == nil {
		return StatusMissing, "", "memory_not_found"
	}
	content := truncate(stringOr(m, "content"), 400)
	collection := stringOr(m, "collection")
	weight := float64Or(m, "weight", 0)
	updatedAt := int64Or(m, "updated_at", 0)
	detail := fmt.Sprintf("collection=%s; weight=%.2f; updated_at=%d; content=%s",
		collection, weight, updatedAt, content)
	return StatusMaterialized, detail, ""
}

func matLesson(dm *DatabaseManager, id string, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	l, err := dm.GetLesson(id)
	if err != nil || l == nil {
		return StatusMissing, "", "lesson_not_found"
	}
	content := truncate(l.Content, 400)
	typ := string(l.Type)
	detail := fmt.Sprintf("type=%s; content=%s", typ, content)
	return StatusMaterialized, detail, ""
}

func matTheory(dm *DatabaseManager, id string, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	t, err := dm.GetTheory(id)
	if err != nil || t == nil {
		return StatusMissing, "", "theory_not_found"
	}
	content := truncate(stringOr(t, "content"), 400)
	status := stringOr(t, "status")
	tags := stringOr(t, "tags")
	detail := fmt.Sprintf("status=%s; tags=%s; content=%s", status, tags, content)
	return StatusMaterialized, detail, ""
}

func matDecision(dm *DatabaseManager, id string, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	d, err := dm.GetDecision(id)
	if err != nil || d == nil {
		return StatusMissing, "", "decision_not_found"
	}
	content := truncate(stringOr(d, "content"), 400)
	detail := fmt.Sprintf("content=%s", content)
	return StatusMaterialized, detail, ""
}

func matEvidence(dm *DatabaseManager, id string, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	// ShowConfidence returns the evidence row + confidence history;
	// we only project the bounded current-confidence summary.
	res, err := dm.ShowConfidence(id, "evidence")
	if err != nil || res == nil {
		return StatusMissing, "", "evidence_not_found"
	}
	b, _ := json.Marshal(res)
	detail := truncate(string(b), 400)
	return StatusMaterialized, detail, ""
}

func matWake(dm *DatabaseManager, id string, now int64, reads *int) (MaterializationStatus, string, string) {
	if id == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	// Read-only scalar query — never fire / mutate.
	var targetTime, createdAt int64
	var reason, createdBy sql.NullString
	var fired int
	var theoryID sql.NullString
	err := dm.db.QueryRow(`
		SELECT target_time, reason, fired, theory_id, created_by, created_at
		FROM scheduled_wakes WHERE id = ?`, id,
	).Scan(&targetTime, &reason, &fired, &theoryID, &createdBy, &createdAt)
	if err == sql.ErrNoRows {
		return StatusMissing, "", "wake_not_found"
	}
	if err != nil {
		return StatusError, "", "wake_read_error"
	}
	var due string
	if targetTime <= now {
		due = "overdue"
	} else {
		due = fmt.Sprintf("due_in+%d", targetTime-now)
	}
	state := "pending"
	if fired == 1 {
		state = "fired"
	}
	r := truncate(reason.String, 200)
	detail := fmt.Sprintf("state=%s; due=%s; target_time=%d; reason=%s", state, due, targetTime, r)
	return StatusMaterialized, detail, ""
}

func matScratchpad(dm *DatabaseManager, sessionID string, reads *int) (MaterializationStatus, string, string) {
	if sessionID == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	*reads++
	// Read-only scalar query. PK is session_id.
	var thesis sql.NullString
	var supporting sql.NullString
	var updatedAt int64
	var decayAt sql.NullInt64
	err := dm.db.QueryRow(`
		SELECT thesis, supporting, updated_at, decay_at
		FROM ephemeral_scratchpad WHERE session_id = ?`, sessionID,
	).Scan(&thesis, &supporting, &updatedAt, &decayAt)
	if err == sql.ErrNoRows {
		return StatusMissing, "", "scratchpad_not_found"
	}
	if err != nil {
		return StatusError, "", "scratchpad_read_error"
	}
	// Conservative bounded excerpt. Scratchpad may carry arbitrary
	// working content; we deliberately cap and never expand.
	excerpt := truncate(thesis.String, 200)
	detail := fmt.Sprintf("session_id=%s; updated_at=%d; thesis=%s",
		sessionID, updatedAt, excerpt)
	_ = supporting
	_ = decayAt
	return StatusMaterialized, detail, ""
}

func matActivity(c *Candidate, reads *int) (MaterializationStatus, string, string) {
	if c == nil || c.ArtifactID == "" {
		return StatusPointerOnly, "", "missing_id"
	}
	// RecentActivityEvent is already bounded and secret-safe; we
	// do NOT issue a fresh read here. The candidate's Summary is
	// the bounded event summary built by the Stage-2D activity
	// source. This counts as 0 reads — the bounded facts are
	// already on the candidate. Each selected activity item is
	// emitted as its own output (1 selected == 1 output); no grouping.
	_ = reads
	actorKind := c.ActorKind
	framework := c.FrameworkName
	summary := truncateUTF8(c.Summary, 200)
	detail := fmt.Sprintf("actor=%s; framework=%s; summary=%s",
		actorKind, framework, summary)
	if actorKind == "" && framework == "" {
		detail = fmt.Sprintf("summary=%s", summary)
	}
	return StatusMaterialized, detail, ""
}

// ── Helpers ──────────────────────────────────────────────────

func stringOr(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func int64Or(m map[string]interface{}, key string, fallback int64) int64 {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return fallback
}

func float64Or(m map[string]interface{}, key string, fallback float64) float64 {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return fallback
}

// truncateUTF8 truncates s to at most maxBytes UTF-8 bytes, never
// splitting a codepoint. Returns s unchanged if it already fits.
// Pure rune-safe; returns valid UTF-8.
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// Walk back from the cut to a UTF-8 rune boundary.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// applyActivityGrouping is REMOVED in Stage 2E.2 (final clarifications):
// grouping is not performed at this layer. Each selected activity item
// remains its own output row with a compact per-event representation.
// Future context packaging can compress downstream without changing
// 1 selected == 1 output cardinality here.

// stableJoin is a deterministic string joiner. Used for predictable
// downstream byte budgets.
func stableJoin(parts []string, sep string) string {
	cp := append([]string(nil), parts...)
	sort.Strings(cp)
	return strings.Join(cp, sep)
}

// ── Time stub for tests ─────────────────────────────────────

// matNow is a package-local stub used to allow tests to pin time.
// Production code uses the `now` argument passed to MaterializeContextualSelection.
var matNow = func() int64 { return time.Now().Unix() }
