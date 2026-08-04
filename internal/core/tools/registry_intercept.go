// registry_intercept.go — wraps every tool in Registry with an
// interceptor that pushes ToolCallRecords into the global buffer.
//
// v spec 2026-08-04: registry-level interception is the right hook
// point because (a) every tool that mcp-mcp or the CLI surfaces must
// pass through Registry, and (b) wrapping at init time means the
// instrumentation is invisible to handler authors — they write the
// tool, the registry wraps it.
//
// Cognitive-verb exclusion (per v's structural improvement): the
// substrate's own save/write verbs (save_to_memory, record_decision,
// propose_theory, etc.) MUST NOT overwrite the RecentTool buffer
// during their own execution. If an agent calls read_file then calls
// save_to_memory, the save_to_memory invocation passes through the
// interceptor without disturbing the buffer head — the read_file
// observation is what we want to capture as provenance.
//
// Why an init() and not explicit wiring at every call site:
//
//   The Registry is built once at package init from registry_list.go.
//   An init() in this file runs after Registry is populated, so the
//   wrap is guaranteed to fire before any tool is invoked. Explicit
//   wiring at every call site would be N=50+ duplicates of the same
//   four-line pattern; init() is the substrate's existing convention
//   for "transform every entry in a registry on startup".
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	mpminternal "github.com/flowbyte-com/mpm-core"
)

// Intercept wraps a single HandlerFunc with the buffer-recording
// interceptor. Cognitive verbs pass through untouched. Observation
// and action tools (read_file, web_search, exec, etc.) get a
// ToolCallRecord pushed to the global buffer after fn returns.
//
// The interceptor records BOTH successful and failed calls. A failed
// tool is still an observation ("the file didn't exist" is a fact
// the agent may save later); we just can't hash a result that
// didn't materialize, so ResultHash stays empty on error.
func Intercept(toolName string, fn HandlerFunc) HandlerFunc {
	return func(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, payload map[string]interface{}) (interface{}, error) {
		// Skip the buffer entirely for cognitive verbs. The agent's
		// epistemic state is captured by the save_to_memory snapshot
		// itself, not by overwriting the buffer with "the agent just
		// saved a memory" — that's circular.
		if mpminternal.IsCognitiveVerb(toolName) {
			return fn(dm, ac, payload)
		}

		// Build the record up-front so CalledAt reflects the actual
		// start of the tool execution, not the post-result time.
		rec := &mpminternal.ToolCallRecord{
			ToolName: toolName,
			CalledAt: time.Now(),
			CallID:   uuid.New().String(),
		}

		result, err := fn(dm, ac, payload)

		// Hash the result if we got one. Failed calls get an empty
		// ResultHash — the record still gets pushed, with CallID +
		// CalledAt + ToolName being the audit-relevant fields.
		if err == nil && result != nil {
			if h := hashResult(result); h != "" {
				rec.ResultHash = h
			}
		}

		// Push to buffer. Skip silently if ac.SessionID is empty —
		// CLI invocations and certain unit tests don't carry a
		// session, so there's nothing to attribute the call to.
		if ac.SessionID != "" {
			mpminternal.GlobalToolBuffer().Record(ac.SessionID, rec)
		}

		// Surface interceptor errors as warnings rather than failing
		// the underlying tool call. Buffer instrumentation is
		// best-effort; a hash failure or full buffer must never
		// break the user's save.
		if err != nil {
			slog.Debug("tool_interceptor: tool returned error (recorded anyway)",
				"tool", toolName, "session_id", ac.SessionID, "error", err.Error())
		}

		return result, err
	}
}

// hashResult computes a sha256 hex digest of the JSON-marshaled
// result. Returns empty string if the result cannot be marshaled —
// callers treat empty as "no hash available" rather than failing.
func hashResult(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Warn("tool_interceptor: result marshal failed (hash skipped)",
			"error", err.Error())
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// init wraps every entry in Registry with the buffer interceptor.
// Runs once at process start, after registry_list.go has populated
// the slice. Subsequent tool calls (MCP or CLI) flow through the
// wrapped HandlerFunc transparently.
func init() {
	for i := range Registry {
		// Capture by index + value so the loop variable doesn't
		// alias across iterations.
		entry := Registry[i]
		entry.Handler = Intercept(entry.Name, entry.Handler)
		Registry[i] = entry
	}
}