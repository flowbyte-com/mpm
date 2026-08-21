// snapshot.go — _epistemic_snapshot auto-injection for memory writes.
//
// v spec 2026-08-04: capture the epistemic environment at the exact
// moment a memory is saved. Five blocks under metadata._epistemic_snapshot:
//
//   execution    — active mode + persona + agent self-assessment
//   provenance   — what observation (if any) preceded this save
//   context      — goal text snapshot + pointers
//   creator      — who/what wrote this memory
//   validation   — current evidence-based epistemic state
//
// Why this lives in its own file (vs. being folded into memory.go):
//
//   memory.go is already 1800+ lines and the responsibility split is clean:
//   memory.go owns the row (Memory struct + MemoryStore), snapshot.go owns
//   the metadata envelope (EpistemicSnapshot + the resolver that produces
//   one). The two files share package internal and use each other's types.
//
//   Pattern 3 guard rail: the EpistemicSnapshot struct is the rigid Go
//   shape that AST-level tools (vet, struct field reflection) can validate
//   against. Wrappers cannot shove a loose map[string]interface{} into
//   this block — they must call ResolveSnapshot, which returns a fully
//   validated pointer. Any non-conforming field fails compilation or
//   runtime validation.
//
// Why we do NOT use ActiveContext.provenanceMeta() output:
//
//   The existing ActiveContext provenance block at metadata.provenance
//   ({source, model, compute, agent}) is consumed by DecayWeights via a
//   raw json_extract() SQL path. Changing that schema would silently
//   break decay multipliers on every existing memory. The new
//   _epistemic_snapshot block is an additive, parallel envelope — both
//   blocks coexist; the legacy one keeps decay, the new one carries
//   audit-grade context.
package internal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// Schema constants
// ─────────────────────────────────────────────────────────────────────

// SnapshotSchemaVersion is stamped on every EpistemicSnapshot by the
// resolver. Readers must be forward-compatible: skip unknown fields,
// refuse to deserialize a future major version. Bump MAJOR on shape
// changes, MINOR on additive fields, PATCH on cosmetic.
//
// Phase 1 blob table (blobs table + FilesystemBackend + mpm:// URI
// scheme) was added in 2026-08. The schema addition is fully
// additive and does not require a version bump here.
const SnapshotSchemaVersion = "1.0.0"

// DefaultMaxObservationWindowMs is the default ceiling for
// tool-call → save proximity. 60 seconds was chosen on 2026-08-04
// because LLM chain-of-thought between tool observation and the
// save_to_memory invocation can easily exceed 30 seconds for deep
// reasoning; clamping tighter would silently drop provenance on the
// most valuable insights. Tunable per-install via system_config.
const DefaultMaxObservationWindowMs = 60000

// GoalSnapshotMaxChars is the cap on goal_snapshot before overflow
// rejection. 200 chars is enough for a meaningful goal sentence without
// leaking large sensitive context.
const GoalSnapshotMaxChars = 200

// URIMaxChars is the cap on provenance.uri before overflow rejection.
const URIMaxChars = 2048

// Enum tables. Frozen for the alpha — widening these is a schema version
// bump, not a runtime concern. Validators below enforce these exactly.
var (
	validConfidenceBands = map[string]bool{
		"low": true, "medium": true, "high": true,
	}
	validReasoningDepths = map[string]bool{
		"shallow": true, "medium": true, "deep": true,
	}
	validValidationStatuses = map[string]bool{
		"unvalidated":   true,
		"corroborated":  true,
		"contradicted":  true,
	}
	validSourceToolClasses = map[string]bool{
		// free-form string for the actual tool name; these are coarse classes
		// for query-time grouping
		"tool":          true, // read, exec, search, fetch, etc.
		"user_message":  true, // human-typed input
		"system_event":  true, // cron / wake / handoff
		"tool_synthesis": true, // LLM-synthesized observation
		"imported":      true, // migrate / batch ingest
		"manual":        true, // operator typed into CLI
		"unknown":       true,
	}
)

// ─────────────────────────────────────────────────────────────────────
// Errors
// ─────────────────────────────────────────────────────────────────────

// SnapshotError is the base type for all _epistemic_snapshot validation
// and resolution failures. Wrapped errors preserve the operation that
// failed (Validate / Resolve / Build) and the offending field path.
type SnapshotError struct {
	Op    string // "resolve" | "validate" | "build"
	Field string // JSON-path-style location, e.g. "provenance.uri"
	Err   error
}

func (e *SnapshotError) Error() string {
	return fmt.Sprintf("epistemic_snapshot %s: field=%s: %v", e.Op, e.Field, e.Err)
}

func (e *SnapshotError) Unwrap() error { return e.Err }

var (
	ErrGoalSnapshotTooLong = errors.New("goal_snapshot exceeds max chars (200)")
	ErrURITooLong          = errors.New("provenance.uri exceeds max chars (2048)")
	ErrInvalidConfidenceBand = errors.New("confidence_band not in {low, medium, high}")
	ErrInvalidReasoningDepth = errors.New("reasoning_depth not in {shallow, medium, deep}")
	ErrInvalidValidationStatus = errors.New("validation.status not in {unvalidated, corroborated, contradicted}")
	ErrMissingCreatorAgent = errors.New("creator.agent_id is required")
	ErrMissingCreatorSession = errors.New("creator.session_id is required")
	ErrStaleToolObservation = errors.New("recent_tool.called_at outside observation window")
)

// ─────────────────────────────────────────────────────────────────────
// Types — _epistemic_snapshot envelope
// ─────────────────────────────────────────────────────────────────────

// EpistemicSnapshot is the system-stamped telemetry block injected into
// every memory's metadata at save time. Wrappers MUST go through
// ResolveSnapshot — never instantiate this directly except in tests.
//
// JSON layout: stored as metadata._epistemic_snapshot. The leading
// underscore on the wrapper key is a convention: signals "system-owned,
// do not modify from agent/user metadata paths". SchemaVersion is the
// only field without omitempty — readers can rely on its presence to
// distinguish "snapshot present" from "snapshot absent".
type EpistemicSnapshot struct {
	SchemaVersion string             `json:"schema_version"`
	Execution     *ExecutionContext  `json:"execution,omitempty"`
	Provenance    *ProvenanceContext `json:"provenance,omitempty"`
	Context       *GoalContext       `json:"context,omitempty"`
	Creator       *CreatorContext    `json:"creator"`
	Validation    *ValidationState   `json:"validation,omitempty"`
}

// ExecutionContext captures the agent's behavioral + epistemic state at
// the moment of save. Mode and Persona are pulled from active.json by
// the resolver; ConfidenceBand and ReasoningDepth are supplied by the
// wrapper as the agent's self-assessment (cannot be inferred from
// substrate state).
type ExecutionContext struct {
	Mode           string `json:"mode"`
	Persona        string `json:"persona"`
	ConfidenceBand string `json:"confidence_band,omitempty"` // enum
	ReasoningDepth string `json:"reasoning_depth,omitempty"` // enum
	CapturedAt     string `json:"captured_at"`               // RFC3339, required
}

// ProvenanceContext records what observation (if any) preceded this save.
// The resolver enforces a strict observation window: if RecentTool is
// older than the configured ceiling, the resolver drops the entire
// provenance block rather than stamp a stale claim. source_tool is a
// free-form string (wrapper normalizes: lowercase, spaces→underscores);
// source_tool_class is one of the validSourceToolClasses for grouping.
type ProvenanceContext struct {
	SourceTool          string `json:"source_tool,omitempty"`           // free-form
	SourceToolClass     string `json:"source_tool_class,omitempty"`    // enum
	URI                 string `json:"uri,omitempty"`                  // ≤2048
	ToolCallID          string `json:"tool_call_id,omitempty"`
	ObservationWindowMS int    `json:"observation_window_ms,omitempty"` // ≥0
	ContentHash         string `json:"content_hash,omitempty"`         // sha256 hex
}

// GoalContext is the snapshotted scratchpad state at save time.
// goal_snapshot is the literal text (truncated-then-rejected at 200
// chars); active_goal_id is a stable short hash of the snapshot text so
// operators can correlate memories saved under the same goal across
// sessions; scratchpad_session_id is the FK back to the ephemeral
// scratchpad row that produced this snapshot.
//
// IMPORTANT: this block is omitted entirely if no scratchpad exists
// for the session — never stamped with empty strings.
type GoalContext struct {
	GoalSnapshot       string `json:"goal_snapshot,omitempty"`        // ≤200
	ActiveGoalID       string `json:"active_goal_id,omitempty"`       // 16-char hex
	ScratchpadSessionID string `json:"scratchpad_session_id,omitempty"`
}

// CreatorContext identifies who/what produced this memory. agent_id
// and session_id are REQUIRED — there is no "anonymous" memory in this
// substrate. model is the runtime model identifier (e.g.
// "minimax-portal/MiniMax-M3"); empty if the wrapper doesn't know.
type CreatorContext struct {
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id"`
	Model     string `json:"model,omitempty"`
}

// ValidationState is a point-in-time statement of the artifact's
// evidence-based epistemic state. Computed at save time from the
// evidence table; NOT kept in sync thereafter (a query that needs live
// status must call ComputeValidationState again).
//
// For a brand-new memory the artifact_id is empty at save time, so this
// block is omitted entirely — there's no evidence to evaluate yet.
//
// trigger_evidence_id is set ONLY when status is corroborated or
// contradicted; it points at the evidence row that most recently
// flipped the state. For unvalidated memories the field is absent.
type ValidationState struct {
	Status            string `json:"status"`                       // enum
	EvidenceCount     int    `json:"evidence_count"`               // ≥0
	TriggerEvidenceID string `json:"trigger_evidence_id,omitempty"` // set iff status ≠ unvalidated
	LastValidatedAt   string `json:"last_validated_at,omitempty"`  // RFC3339
}

// ─────────────────────────────────────────────────────────────────────
// Types — wrapper-side inputs
// ─────────────────────────────────────────────────────────────────────

// WrapperContext is the runtime context the wrapper (mcp-mcp / mpm CLI)
// MUST supply at save time. Only fields the host environment actually
// knows about live here — substrate state (active.json, scratchpad,
// evidence) is fetched by the resolver, never duplicated into the
// wrapper payload (single-source-of-truth discipline).
//
// RecentTool is optional: nil means "no preceding tool observation"
// (the memory came from pure reasoning, user message, or system event).
// ConfidenceBand and ReasoningDepth are the agent's self-assessment,
// not wrapper inferences — the substrate cannot derive these.
type WrapperContext struct {
	AgentID         string         // e.g. "main"
	SessionID       string         // e.g. "973c26d2-..."
	Model           string         // e.g. "minimax-portal/MiniMax-M3"; empty if unknown
	RecentTool      *ToolCallRecord // most recent tool call; nil if none / stale
	ConfidenceBand  string         // "low" | "medium" | "high"
	ReasoningDepth  string         // "shallow" | "medium" | "deep"
}

// ToolCallRecord is the wrapper-side record of a single tool invocation.
// The wrapper maintains an in-memory ring buffer (default N=1) and
// pushes a record on every tool execution; the resolver reads the head
// at save time and either stamps it as provenance (within window) or
// drops it (stale).
type ToolCallRecord struct {
	ToolName   string    // "read_file" | "web_search" | "exec" | ...
	CallID     string    // unique per invocation
	URI        string    // filepath / URL / query / sanitized command
	ResultHash string    // sha256 hex of result body; empty if N/A
	CalledAt   time.Time // for observation-window enforcement
}

// ─────────────────────────────────────────────────────────────────────
// Resolver — the single function both wrappers call
// ─────────────────────────────────────────────────────────────────────

// ResolveSnapshot builds a fully-validated EpistemicSnapshot from the
// wrapper-supplied runtime context plus substrate-resident state. This
// is the single resolver; both mpm-mcp and the mpm CLI call into this
// function. Drift between wrappers is impossible because there is only
// one implementation.
//
// artifactID and artifactType are optional (pass "", "" for a brand-new
// memory). When supplied, the resolver queries the evidence table to
// compute ValidationState. When empty, the Validation block is omitted.
//
// On any validation failure, ResolveSnapshot returns a *SnapshotError
// with Field pointing at the offending JSON path; callers should fail
// the save with the wrapped error rather than dropping the snapshot
// silently.
func ResolveSnapshot(
	ctx context.Context,
	dm *DatabaseManager,
	wc WrapperContext,
	artifactID string,
	artifactType string,
) (*EpistemicSnapshot, error) {
	if wc.AgentID == "" {
		return nil, &SnapshotError{Op: "resolve", Field: "wrapper.agent_id", Err: ErrMissingCreatorAgent}
	}
	if wc.SessionID == "" {
		return nil, &SnapshotError{Op: "resolve", Field: "wrapper.session_id", Err: ErrMissingCreatorSession}
	}

	maxWindow := DefaultMaxObservationWindowMs
	if dm != nil {
		maxWindow = dm.GetConfigInt("epistemic_snapshot.max_observation_window_ms", DefaultMaxObservationWindowMs)
	}

	snap := &EpistemicSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Creator: &CreatorContext{
			AgentID:   wc.AgentID,
			SessionID: wc.SessionID,
			Model:     wc.Model,
		},
	}

	// execution — substrate pulls active mode/persona; wrapper provides
	// confidence/depth.
	exec, err := loadExecutionContext(ctx, wc)
	if err != nil {
		return nil, err
	}
	snap.Execution = exec

	// provenance — strict observation-window enforcement; drop, don't weaken.
	if wc.RecentTool != nil {
		prov, ok := buildProvenanceContext(wc.RecentTool, maxWindow)
		if ok {
			snap.Provenance = prov
		}
		// ok == false means the tool call is stale → provenance omitted entirely
	}

	// context — pull current scratchpad; omit block if no scratchpad exists.
	goal, err := loadGoalContext(ctx, dm, wc.SessionID)
	if err != nil {
		return nil, err
	}
	snap.Context = goal

	// validation — only for existing artifacts (updates / refreshes).
	// New memories have no artifact_id yet; the block is omitted.
	if artifactID != "" && artifactType != "" && dm != nil {
		validation, err := computeValidationState(ctx, dm, artifactID, artifactType)
		if err != nil {
			return nil, err
		}
		snap.Validation = validation
	}

	// Final validation pass — AST guard rail.
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return snap, nil
}

// loadExecutionContext builds the execution block. Mode and persona
// come from active.json (the same source wake_context reads); confidence
// and depth come from the wrapper.
func loadExecutionContext(_ context.Context, wc WrapperContext) (*ExecutionContext, error) {
	state, err := LoadActiveJSON()
	if err != nil {
		return nil, &SnapshotError{Op: "resolve", Field: "execution.mode", Err: fmt.Errorf("load active.json: %w", err)}
	}

	// modes is []string; if "auto" is in the list, surface "auto" as the
	// mode. Otherwise join the list with comma — matches active.json's
	// multi-mode semantics.
	mode := ""
	if len(state.Modes) > 0 {
		mode = strings.Join(state.Modes, ",")
	}

	return &ExecutionContext{
		Mode:           mode,
		Persona:        state.Persona,
		ConfidenceBand: wc.ConfidenceBand,
		ReasoningDepth: wc.ReasoningDepth,
		CapturedAt:     time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// buildProvenanceContext enforces the observation-window gate. Returns
// (nil, false) when the tool call is too old; the caller omits the
// provenance block entirely rather than stamp a stale claim.
func buildProvenanceContext(rt *ToolCallRecord, maxWindowMs int) (*ProvenanceContext, bool) {
	windowMs := time.Since(rt.CalledAt).Milliseconds()
	if windowMs < 0 {
		windowMs = 0 // clock skew safety
	}
	if windowMs > int64(maxWindowMs) {
		return nil, false
	}

	uri := rt.URI
	if len(uri) > URIMaxChars {
		// Reject, don't truncate. Same principle as goal_snapshot.
		// Caller treats this as a typed error rather than stamping a
		// truncated URI that misrepresents the source.
		// (Note: buildProvenanceContext is best-effort; the actual
		// rejection happens in Validate() after the struct is built.
		// We pre-check here so the resolver can fail before SQL.)
		return nil, false
	}

	return &ProvenanceContext{
		SourceTool:          rt.ToolName,
		SourceToolClass:     classifySourceTool(rt.ToolName),
		URI:                 uri,
		ToolCallID:          rt.CallID,
		ObservationWindowMS: int(windowMs),
		ContentHash:         rt.ResultHash,
	}, true
}

// classifySourceTool groups free-form tool names into a small enum
// for query-time aggregation. Wrappers normalize tool names
// (lowercase, spaces→underscores) before populating ToolCallRecord;
// this function does coarse class detection based on name prefix.
//
// Conservative — defaults to "tool" if no pattern matches. We never
// guess "user_message" or "system_event" from a tool name; those
// classes are set by the wrapper at ToolCallRecord construction time.
func classifySourceTool(name string) string {
	switch {
	case strings.HasPrefix(name, "read_"),
		strings.HasPrefix(name, "web_"),
		strings.HasPrefix(name, "exec"),
		strings.HasPrefix(name, "playwright_"),
		strings.HasPrefix(name, "image_"),
		strings.HasPrefix(name, "music_"),
		strings.HasPrefix(name, "video_"),
		strings.HasPrefix(name, "mpm_"):
		return "tool"
	default:
		return "tool"
	}
}

// loadGoalContext reads the current session's scratchpad (if any) and
// produces a GoalContext block. Returns (nil, nil) when no scratchpad
// row exists — caller treats this as "no goal to stamp" rather than
// failing the save.
//
// goal_snapshot overflow (>200 chars) is a hard error: rejecting
// forces the wrapper to handle the reality of the data rather than
// hiding it in a truncated snapshot.
func loadGoalContext(_ context.Context, dm *DatabaseManager, sessionID string) (*GoalContext, error) {
	if dm == nil || sessionID == "" {
		return nil, nil
	}

	var thesis string
	err := dm.QueryRowTracked(
		`SELECT thesis FROM ephemeral_scratchpad WHERE session_id = ?`, sessionID,
	).Scan(&thesis)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, &SnapshotError{Op: "resolve", Field: "context.goal_snapshot", Err: fmt.Errorf("read scratchpad: %w", err)}
	}

	if len(thesis) > GoalSnapshotMaxChars {
		return nil, &SnapshotError{Op: "resolve", Field: "context.goal_snapshot", Err: ErrGoalSnapshotTooLong}
	}

	return &GoalContext{
		GoalSnapshot:        thesis,
		ActiveGoalID:        shortHash(thesis),
		ScratchpadSessionID: sessionID,
	}, nil
}

// computeValidationState queries the evidence table for an artifact
// and produces a ValidationState block. Used for existing artifacts
// (updates, refreshes); not called for new memories.
//
// Status rules:
//   - contradicted: any evidence row with type = 'challenge'
//   - corroborated: any non-challenge evidence with strength > 0
//   - unvalidated:  otherwise
//
// trigger_evidence_id is the most recent (by created_at) evidence row
// matching the current status — gives operators a direct pointer to
// the observation that flipped the state.
func computeValidationState(_ context.Context, dm *DatabaseManager, artifactID, artifactType string) (*ValidationState, error) {
	rows, err := ListEvidenceForArtifact(dm, artifactID, artifactType)
	if err != nil {
		return nil, &SnapshotError{Op: "resolve", Field: "validation.status", Err: fmt.Errorf("list evidence: %w", err)}
	}

	state := &ValidationState{
		Status:        "unvalidated",
		EvidenceCount: len(rows),
	}

	if len(rows) == 0 {
		return state, nil
	}

	// Walk most-recent-first to find both status and trigger ID in one pass.
	// We sort in Go rather than re-querying because rows is typically small
	// (<10 rows per artifact) and the artifact_id+artifact_type index makes
	// the single ordered query cheap enough — but pulling all rows then
	// sorting is simpler than two queries.
	for i := len(rows) - 1; i >= 0; i-- {
		e := rows[i]
		if e.Type == "challenge" {
			state.Status = "contradicted"
			state.TriggerEvidenceID = e.ID
			state.LastValidatedAt = e.CreatedAt.UTC().Format(time.RFC3339)
			return state, nil
		}
	}
	// No challenges → check for any positive-strength corroboration.
	for i := len(rows) - 1; i >= 0; i-- {
		e := rows[i]
		if e.Strength > 0 {
			state.Status = "corroborated"
			state.TriggerEvidenceID = e.ID
			state.LastValidatedAt = e.CreatedAt.UTC().Format(time.RFC3339)
			return state, nil
		}
	}
	// Evidence exists but all are zero-strength non-challenges — still
	// treated as unvalidated (no positive signal has landed).
	return state, nil
}

// ─────────────────────────────────────────────────────────────────────
// Validation — Pattern 3 AST guard rail
// ─────────────────────────────────────────────────────────────────────

// Validate enforces the schema contract. Called by ResolveSnapshot
// before returning; safe to call again by external tooling (e.g.
// `mpm doctor` introspection or test fixtures).
//
// All enum fields are checked; all required fields are checked; length
// bounds are checked; uri and goal_snapshot overflow become typed
// errors rather than silent truncation.
func (s *EpistemicSnapshot) Validate() error {
	if s.SchemaVersion == "" {
		return &SnapshotError{Op: "validate", Field: "schema_version", Err: errors.New("required")}
	}
	if s.Creator == nil {
		return &SnapshotError{Op: "validate", Field: "creator", Err: errors.New("required")}
	}
	if s.Creator.AgentID == "" {
		return &SnapshotError{Op: "validate", Field: "creator.agent_id", Err: ErrMissingCreatorAgent}
	}
	if s.Creator.SessionID == "" {
		return &SnapshotError{Op: "validate", Field: "creator.session_id", Err: ErrMissingCreatorSession}
	}

	if s.Execution != nil {
		if s.Execution.CapturedAt == "" {
			return &SnapshotError{Op: "validate", Field: "execution.captured_at", Err: errors.New("required")}
		}
		if s.Execution.ConfidenceBand != "" && !validConfidenceBands[s.Execution.ConfidenceBand] {
			return &SnapshotError{Op: "validate", Field: "execution.confidence_band", Err: ErrInvalidConfidenceBand}
		}
		if s.Execution.ReasoningDepth != "" && !validReasoningDepths[s.Execution.ReasoningDepth] {
			return &SnapshotError{Op: "validate", Field: "execution.reasoning_depth", Err: ErrInvalidReasoningDepth}
		}
	}

	if s.Provenance != nil {
		if len(s.Provenance.URI) > URIMaxChars {
			return &SnapshotError{Op: "validate", Field: "provenance.uri", Err: ErrURITooLong}
		}
		if s.Provenance.ObservationWindowMS < 0 {
			return &SnapshotError{Op: "validate", Field: "provenance.observation_window_ms", Err: errors.New("must be ≥ 0")}
		}
		if s.Provenance.SourceToolClass != "" && !validSourceToolClasses[s.Provenance.SourceToolClass] {
			return &SnapshotError{Op: "validate", Field: "provenance.source_tool_class", Err: fmt.Errorf("not in %v", enumKeys(validSourceToolClasses))}
		}
	}

	if s.Context != nil {
		if len(s.Context.GoalSnapshot) > GoalSnapshotMaxChars {
			return &SnapshotError{Op: "validate", Field: "context.goal_snapshot", Err: ErrGoalSnapshotTooLong}
		}
	}

	if s.Validation != nil {
		if !validValidationStatuses[s.Validation.Status] {
			return &SnapshotError{Op: "validate", Field: "validation.status", Err: ErrInvalidValidationStatus}
		}
		if s.Validation.EvidenceCount < 0 {
			return &SnapshotError{Op: "validate", Field: "validation.evidence_count", Err: errors.New("must be ≥ 0")}
		}
		// trigger_evidence_id must be present iff status ≠ unvalidated
		if s.Validation.Status != "unvalidated" && s.Validation.TriggerEvidenceID == "" {
			return &SnapshotError{Op: "validate", Field: "validation.trigger_evidence_id", Err: errors.New("required when status is corroborated or contradicted")}
		}
		if s.Validation.Status == "unvalidated" && s.Validation.TriggerEvidenceID != "" {
			return &SnapshotError{Op: "validate", Field: "validation.trigger_evidence_id", Err: errors.New("must be empty when status is unvalidated")}
		}
	}

	return nil
}

// MergeInto stamps this snapshot into an existing metadata map under
// the _epistemic_snapshot key. Preserves all existing top-level keys
// (agent-invented tags, the legacy metadata.provenance block used by
// DecayWeights, etc.). Idempotent — calling twice with the same
// snapshot produces the same result.
func (s *EpistemicSnapshot) MergeInto(metadata map[string]interface{}) map[string]interface{} {
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	snapMap, err := structToMap(s)
	if err != nil {
		// Validate() should have caught this already; if we get here,
		// fall back to a minimal stamping rather than failing the save.
		snapMap = map[string]interface{}{
			"schema_version": s.SchemaVersion,
		}
	}
	metadata["_epistemic_snapshot"] = snapMap
	return metadata
}

// structToMap is a small helper for MergeInto. We avoid pulling in a
// reflection library for this single use; explicit marshaling keeps the
// snapshot shape under our control.
func structToMap(s *EpistemicSnapshot) (map[string]interface{}, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

// shortHash returns a 16-char hex prefix of sha256(input). Used for
// active_goal_id — stable enough to correlate memories saved under
// the same goal, short enough to be readable in queries.
func shortHash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

// enumKeys is a small helper for error messages — produces a sorted
// []string from a set-like map.
func enumKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Stable ordering for readable errors.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}