// tool_buffer.go — process-local record of the most recent tool
// invocation per session, consumed by the _epistemic_snapshot resolver
// to stamp provenance on memory writes.
//
// v spec 2026-08-04: a single in-memory ring buffer (N=1 per session)
// holds the latest ToolCallRecord for each session_id. mcp-mcp wraps
// every observation/action tool with an interceptor that pushes to
// this buffer; the resolver pops the head at save_to_memory time.
//
// Why a singleton:
//
//   The buffer is process-local state, scoped to a single mpm-mcp
//   process lifetime. There's exactly one buffer per binary. This
//   matches the existing substrate convention (LoadActiveJSON,
//   getDB) where shared runtime state lives at package level rather
//   than being dependency-injected. Test code calls ResetForTest()
//   to start clean.
//
// Why N=1 per session:
//
//   The agent execution loop writes tools serially. Only the most
//   recent call can plausibly be the "observation" that inspired a
//   follow-up save — anything older has been overwritten by an
//   intervening call (or has aged out of the 60s observation
//   window enforced in snapshot.go). Holding more than one record
//   per session is dead weight.
//
// Why no eviction:
//
//   Worst case: ~1000 active sessions × ~200 bytes ≈ 200KB. Alpha
//   trade-off — add TTL eviction in v0.2 if long-running mpm-mcp
//   processes on busy multi-agent hosts show memory growth.
package internal

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// ─────────────────────────────────────────────────────────────────────
// Cognitive-verb exclusion
// ─────────────────────────────────────────────────────────────────────

// cognitiveVerbs lists the substrate-internal save/write tools that
// MUST NOT overwrite the RecentTool buffer. If an agent calls read_file
// and then calls save_to_memory, the save_to_memory invocation should
// pass through to the substrate without disturbing the buffer head —
// the read_file observation is what we want to capture as provenance.
//
// Adding new write tools to the registry? Append here too. The list
// is exhaustive on purpose: a missing entry silently breaks provenance
// for that tool's downstream saves.
var cognitiveVerbs = map[string]bool{
	"save_to_memory":    true,
	"record_decision":   true,
	"propose_theory":    true,
	"resolve_theory":    true,
	"save_lesson":       true,
	"save_skill":        true,
	"save_reference":    true,
	"add_reference":     true,
	"commit_milestone":  true,
	"create_topic":      true,
	"link_topic":        true,
	"add_evidence":      true,
	"challenge_memory":  true,
	"shred_memory":      true,
	"reinforce_memory":  true,
	"weaken_memory":     true,
	"promote_memory":    true,
	"promote_skill_to_global": true,
	"promote_to_global": true,
	"record_global_rule": true,
	"delete_skill":      true,
	"synthesize_memory": true,
	"compact_epistemology": true,
	"flush_scratchpad":   true,
	"discard_scratchpad": true,
	"promote_scratchpad": true,
	"patch_memory_metadata": true,
	"schedule_wake":      true,
	"upsert_scheduled_task": true,
	"session_end":        true,
	"session_handoff":    true,
	"mpm_log_to_changelog": true,
}

// IsCognitiveVerb returns true iff the named tool is a substrate-internal
// write that should not pollute the RecentTool buffer. Observation and
// action tools (read_file, web_search, exec, etc.) return false.
//
// Exported because the registry interceptor (in internal/core/tools)
// needs to gate its push-to-buffer logic on this predicate at tool
// invocation time.
func IsCognitiveVerb(name string) bool {
	return cognitiveVerbs[name]
}

// ─────────────────────────────────────────────────────────────────────
// Buffer
// ─────────────────────────────────────────────────────────────────────

// ToolBuffer is a process-local, session-keyed map of the most recent
// ToolCallRecord per session. Safe for concurrent use; reads take a
// read-lock, writes take a write-lock (saves outnumber records ~10x, so
// the bias toward fast reads matters).
type ToolBuffer struct {
	mu      sync.RWMutex
	entries map[string]*ToolCallRecord
}

// NewToolBuffer returns an empty buffer. Production code should use
// GlobalToolBuffer(); this constructor is for tests.
func NewToolBuffer() *ToolBuffer {
	return &ToolBuffer{
		entries: make(map[string]*ToolCallRecord),
	}
}

// Record stores rec as the most recent tool call for sessionID. Any
// previous record for that session is overwritten (N=1 invariant).
// rec must not be nil — the resolver assumes a non-nil RecentTool.
func (b *ToolBuffer) Record(sessionID string, rec *ToolCallRecord) {
	if rec == nil || sessionID == "" {
		return
	}
	if rec.CallID == "" {
		rec.CallID = uuid.New().String()
	}
	if rec.CalledAt.IsZero() {
		rec.CalledAt = time.Now()
	}
	b.mu.Lock()
	b.entries[sessionID] = rec
	b.mu.Unlock()
}

// Head returns the most recent record for sessionID, or nil if none
// has been recorded (or it has been Forgotten). Read-locked — safe to
// call concurrently with Record from other sessions.
func (b *ToolBuffer) Head(sessionID string) *ToolCallRecord {
	if sessionID == "" {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.entries[sessionID]
}

// Forget drops the entry for sessionID. Call from session_end handlers
// to release buffer memory for sessions that will never save again.
// Idempotent — forgetting an unknown session is a no-op.
func (b *ToolBuffer) Forget(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	delete(b.entries, sessionID)
	b.mu.Unlock()
}

// Len returns the number of tracked sessions. Diagnostics only —
// not part of any hot path.
func (b *ToolBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries)
}

// ─────────────────────────────────────────────────────────────────────
// Singleton
// ─────────────────────────────────────────────────────────────────────

// globalToolBuffer is the package-level singleton. Initialized at
// process start; tests reset via ResetForTest().
var globalToolBuffer = NewToolBuffer()

// GlobalToolBuffer returns the process-local singleton. This is the
// canonical accessor for production code; the MCP interceptor and the
// snapshot resolver both call into this.
//
// If the substrate ever needs multiple buffers (e.g. per-agent in a
// multi-tenant deploy), the right move is to make ToolBuffer a
// dependency on DatabaseManager — not to add a second singleton.
func GlobalToolBuffer() *ToolBuffer {
	return globalToolBuffer
}

// ResetForTest wipes the singleton. Test-only — production code must
// never call this. Exposed (not unexported) because tests live in
// other packages and need to reach across the package boundary.
func ResetForTest() {
	globalToolBuffer = NewToolBuffer()
}