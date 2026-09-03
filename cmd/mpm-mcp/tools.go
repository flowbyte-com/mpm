// cmd/mpm-mcp — Native Go MCP server for MPM.
//
// Exposes MPM reasoning primitives to MCP clients (Claude Code, etc.)
// over stdio. Replaces the Python wrapper at .claude/mpm-mcp/server.py.
//
// Tools are no longer defined here — they live in internal/tools. This
// file is the thin MCP adapter: it iterates tools.Registry, builds the
// MCP Tool list (description + JSON schema), and dispatches incoming
// MCP requests to the unified handlers via a single adapter closure.
//
// Adding a new tool is now: (1) write handleFoo in internal/tools, (2)
// add a Tool entry to internal/tools/registry_list.go. The MCP server
// picks it up automatically — no edits here.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/flowbyte-com/mpm/internal/blobstore"
	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// blobStoreAdapter wraps the concrete *blobstore.FilesystemBackend so it
// satisfies the tools.blobStoreInterface expected by the tools package.
// Defined here (in the main module) because mpm-core cannot import the main
// module's internal/blobstore package.
type blobStoreAdapter struct {
	bs *blobstore.FilesystemBackend
}

func (a *blobStoreAdapter) Get(ctx context.Context, id string, opts tools.GetOptions) (io.ReadCloser, tools.Metadata, error) {
	reader, meta, err := a.bs.Get(ctx, id, blobstore.GetOptions{
		Offset:   opts.Offset,
		MaxBytes: opts.MaxBytes,
	})
	if err != nil {
		return nil, tools.Metadata{}, err
	}
	return reader, tools.Metadata{
		ContentType: meta.ContentType,
		SizeBytes:   meta.SizeBytes,
	}, nil
}

func (a *blobStoreAdapter) Search(ctx context.Context, id string, query tools.SearchQuery) ([]tools.Match, error) {
	matches, err := a.bs.Search(ctx, id, blobstore.SearchQuery{
		Query:           query.Query,
		Regex:           query.Regex,
		CaseInsensitive: query.CaseInsensitive,
		MaxMatches:      query.MaxMatches,
		MaxBytes:        query.MaxBytes,
	})
	if err != nil {
		return nil, err
	}
	result := make([]tools.Match, 0, len(matches))
	for _, m := range matches {
		result = append(result, tools.Match{
			LineNo:     m.LineNo,
			ByteOffset: m.ByteOffset,
			Snippet:    m.Snippet,
		})
	}
	return result, nil
}

// artifactResolverAdapter handles all Phase 2 pointer kinds. It satisfies
// tools.pointerResolverInterface. Blob resolution delegates to the
// blobStoreAdapter (Phase 1 behavior, bit-for-bit preserved). Memory,
// lesson, and theory resolutions use the DatabaseManager directly.
// Retrieval telemetry is recorded on every attempt (fire-and-forget).
type artifactResolverAdapter struct {
	blobBS *blobStoreAdapter
	dm     *core.DatabaseManager
}

func (a *artifactResolverAdapter) Resolve(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	switch p.Kind {
	case "blob":
		return a.resolveBlob(ctx, p, opts)
	case "work":
		return a.resolveWork(ctx, p, opts)
	case "memory":
		return a.resolveMemory(ctx, p, opts)
	case "lesson":
		return a.resolveLesson(ctx, p, opts)
	case "theory":
		return a.resolveTheory(ctx, p, opts)
	default:
		return tools.Resolution{}, fmt.Errorf("%w: Phase 2 supports blob/work/memory/lesson/theory", tools.ErrUnsupportedKind)
	}
}

// resolveBlob is Phase 1 blob resolution — bit-for-bit identical to the
// previous pointerResolverAdapter behavior.
func (a *artifactResolverAdapter) resolveBlob(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	reader, meta, err := a.blobBS.Get(ctx, p.ID, tools.GetOptions{})
	if err != nil {
		return tools.Resolution{}, err
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return tools.Resolution{}, err
	}

	return tools.Resolution{
		Pointer:     "mpm://blob/" + p.ID,
		ContentType: meta.ContentType,
		Reader:     io.NopCloser(bytes.NewReader(content)),
		Metadata:   nil,
		Bounded:    false,
	}, nil
}

func (a *artifactResolverAdapter) resolveWork(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	work, err := a.dm.GetWork(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}

	content := work.Title
	if work.Content != "" {
		content = work.Title + "\n\n" + work.Content
	}

	maxBytes := int(opts.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = 512
	}
	bounded := len(content) > maxBytes
	if bounded {
		content = core.SummarizeWork(content, maxBytes)
	}

	_ = a.dm.RecordRetrieval(p.ID, "work")

	return tools.Resolution{
		Pointer:     "mpm://work/" + p.ID,
		ContentType: "text/plain",
		Reader:      io.NopCloser(strings.NewReader(content)),
		Metadata:    nil,
		Bounded:     bounded,
	}, nil
}

func (a *artifactResolverAdapter) resolveMemory(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	mem, err := a.dm.GetMemory(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	content, _ := mem["content"].(string)

	// Bounded materialization: truncate to max_bytes, default 512 bytes.
	maxBytes := int(opts.MaxBytes)
	if maxBytes <= 0 {
		maxBytes = 512
	}
	bounded := len(content) > maxBytes
	if bounded {
		content = core.SummarizeBounded(content, maxBytes)
	}

	// Fire-and-forget retrieval telemetry.
	_ = a.dm.RecordRetrieval(p.ID, "memory")

	return tools.Resolution{
		Pointer:     "mpm://memory/" + p.ID,
		ContentType: "text/plain",
		Reader:     io.NopCloser(strings.NewReader(content)),
		Metadata:   mem,
		Bounded:    bounded,
	}, nil
}

func (a *artifactResolverAdapter) resolveLesson(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	lesson, err := a.dm.GetLesson(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}

	// Lessons are compact — return in full, no bounding needed.
	_ = a.dm.RecordRetrieval(p.ID, "lesson")

	return tools.Resolution{
		Pointer:     "mpm://lesson/" + p.ID,
		ContentType: "text/plain",
		Reader:     io.NopCloser(strings.NewReader(lesson.Content)),
		Metadata: map[string]interface{}{
			"id":                 lesson.ID,
			"type":               string(lesson.Type),
			"tags":               lesson.Tags,
			"reinforcement_count": lesson.ReinforcementCount,
			"created":            lesson.Created,
		},
		Bounded: false,
	}, nil
}

func (a *artifactResolverAdapter) resolveTheory(ctx context.Context, p tools.Pointer, opts tools.ResolveOptions) (tools.Resolution, error) {
	mem, err := a.dm.GetMemory(p.ID)
	if err != nil {
		return tools.Resolution{}, err
	}
	collection, _ := mem["collection"].(string)
	if collection != "theories" {
		return tools.Resolution{}, fmt.Errorf("mpm://theory/%s: not a theory (collection=%q)", p.ID, collection)
	}
	content, _ := mem["content"].(string)
	_ = a.dm.RecordRetrieval(p.ID, "theory")

	return tools.Resolution{
		Pointer:     "mpm://theory/" + p.ID,
		ContentType: "text/plain",
		Reader:     io.NopCloser(strings.NewReader(content)),
		Metadata: map[string]interface{}{
			"id":         mem["id"],
			"collection": collection,
			"weight":     mem["weight"],
			"tags":       mem["tags"],
		},
		Bounded: false,
	}, nil
}

const emptyWakeContext = "Wake context is empty. Ready for context."

// blobStore and outputPolicy_ are initialised once at server boot and closed
// over by mcpAdapter so every tool invocation can apply the output policy.
var (
	blobStore     blobstore.BlobStore // interface; concrete *FilesystemBackend set at boot
	outputPolicy_ tools.OutputPolicy
)

// RegisterAllTools registers every entry in tools.Registry on the given
// MCP server. The "route" tool is special: its handler closes over the
// router, which is constructed at server boot. We build it last with
// the live router instance instead of using the registry stub.
//
// This replaces the previous 35-line s.AddTool(...) block plus 30
// handle*() adapter functions — both have been moved to the registry
// or the single mcpAdapter closure below.
func RegisterAllTools(s *server.MCPServer, dm *core.DatabaseManager, ac core.ActiveContext, router *core.Router, bs *blobstore.FilesystemBackend, op tools.OutputPolicy) {
	blobStore = bs
	blobAdapter := &blobStoreAdapter{bs: bs}
	tools.SetBlobStore(blobAdapter)                                // wire Phase 1 blob tools
	tools.SetResolver(&artifactResolverAdapter{blobBS: blobAdapter, dm: dm}) // wire Phase 2 mpm_resolve
	outputPolicy_ = op
	for _, tool := range tools.Registry {
		if tool.Name == "route" {
			continue // registered below with the live router closure
		}
		s.AddTool(
			mcp.NewToolWithRawSchema(tool.Name, tool.Description, tool.Schema),
			mcpAdapter(dm, ac, tool.Handler),
		)
	}

	// route: needs the live *Router instance, not the registry stub.
	// The MCP server constructs the router once at boot from mode/*.md
	// and persona/*.md files; routing per-request is pure string
	// matching with zero parsing overhead.
	s.AddTool(
		mcp.NewTool("route",
			mcp.WithDescription(
				"Evaluate a user prompt and auto-select the best-matching MPM mode(s) "+
					"and persona. Modes use threshold filtering (multiple can activate); "+
					"personas use max-pooling (only the highest scorer wins, if any beats threshold 1). "+
					"Patterns are pre-compiled at server boot. Anti-patterns penalize false positives. "+
					"Returns a full diagnostic report with scores and triggers per component."),
			mcp.WithString("prompt",
				mcp.Required(),
				mcp.Description("The user prompt or message to route."),
			),
		),
		makeRouteHandler(router),
	)
}

// mcpAdapter wraps a registry HandlerFunc as an MCP server.ToolHandlerFunc.
//
// MCP requests arrive as mcp.CallToolRequest with arguments extracted via
// req.GetArguments() (a map[string]interface{}). The registry Handler
// already takes that shape directly — we just need to:
//   - convert errors to mcp.NewToolResultErrorFromErr
//   - apply the OutputPolicy (DecisionPass or DecisionSpill)
//   - convert the result to a JSON text result
//   - opportunistically fold any due scheduled_wakes into the response
//
// No arg-rewriting, no type assertions, no per-tool boilerplate. The
// 30+ previous handle*() functions collapsed to this single closure.
func mcpAdapter(dm *core.DatabaseManager, ac core.ActiveContext, handler tools.HandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload := req.GetArguments()
		if payload == nil {
			payload = map[string]interface{}{}
		}
		// Per-call invocation correlation. Handler and audit share the same ID
		// so work_events.invocation_id == tool_invocations.invocation_id for
		// this turn. Honors MPM_FRAMEWORK/MPM_PROVENANCE_FRAMEWORK already on ac.
		callAC := ac
		if callAC.InvocationID == "" {
			callAC.InvocationID = uuid.NewString()
		}
		if callAC.FrameworkName == "" {
			callAC.FrameworkName = "mcp"
		} else if callAC.FrameworkName == "mcp" {
			// keep mcp
		}
		startedAt := time.Now()
		result, err := handler(dm, callAC, payload)
		completedAt := time.Now()
		// Audit insert is best-effort; audit failures must not propagate
		// to the MCP client (see audit_hook.go for isolation contract).
		auditAC := callAC
		auditStatus := "success"
		if err != nil {
			auditStatus = "error"
		}
		recordToolInvocation(dm, auditAC, req.Params.Name, payload,
			startedAt, completedAt, extractAction(payload), auditStatus, err)
		if err != nil {
			return mcp.NewToolResultErrorFromErr(req.Params.Name+" failed", err), nil
		}

		// Marshal once — used for both output policy decision and response.
		jsonBytes, jErr := json.Marshal(result)
		if jErr != nil {
			jsonBytes = []byte(fmt.Sprintf("%q", fmt.Sprintf("%v", result)))
		}

		// Apply the output policy using the already-serialized bytes.
		decision, threshold, err := outputPolicy_.Apply(ctx, jsonBytes)
		if err != nil {
			// Policy check failed (e.g. context cancelled); return error.
			return mcp.NewToolResultError("output policy check failed: " + err.Error()), nil
		}

		// Phase 1 blob telemetry: log output policy decision.
		// Alpha-4 D-004: demoted to Debug — these are operational
		// telemetry, not operational faults. With mpm-mcp discarding
		// logs to io.Discard by default (see main.go), they don't
		// pollute the stdio protocol unless MPM_VERBOSE=1 is set.
		decisionStr := map[tools.Decision]string{tools.DecisionPass: "pass", tools.DecisionSpill: "spill"}[decision]
		slog.Debug("mcp_output_policy",
			"decision", decisionStr,
			"serialized_bytes", len(jsonBytes),
			"threshold_bytes", threshold,
			"tool", req.Params.Name,
		)

		var content []mcp.Content

		if decision == tools.DecisionSpill {
			// Spill: store result in blob store and return an envelope.
			ttl := blobStore.TTL()
			meta := blobstore.Metadata{
				SourceTool:  req.Params.Name,
				SizeBytes:   int64(len(jsonBytes)),
				ContentType: "application/json",
				CreatedAt:   time.Now(),
				ExpiresAt:   time.Now().Add(ttl),
			}
			ptr, putErr := blobStore.Put(ctx, bytes.NewReader(jsonBytes), meta)
			if putErr != nil {
				slog.Error("mcp_spill_failed",
					"error", putErr.Error(),
					"serialized_bytes", len(jsonBytes),
					"tool", req.Params.Name,
				)
				return mcp.NewToolResultError("internal: spill failed; result suppressed"), nil
			}

			// Phase 1 blob telemetry: log successful spill.
			// Alpha-4 D-004: demoted to Debug — see sibling mcp_output_policy
			// note for rationale.
			slog.Debug("mcp_spill",
				"blob_id", ptr.ID,
				"size_bytes", meta.SizeBytes,
				"content_type", meta.ContentType,
				"expires_at_unix", meta.ExpiresAt.Unix(),
				"tool", req.Params.Name,
				"call_id", "", // not available in this context
			)

			// Build preview from first keys of the result object.
			preview := buildSpillPreview(jsonBytes)

			envelope := map[string]interface{}{
				"status":       "spilled",
				"pointer":      fmt.Sprintf("mpm://blob/%s", ptr.ID),
				"size_bytes":   int64(len(jsonBytes)),
				"content_type": "application/json",
				"source_tool":  req.Params.Name,
				"preview":      preview,
				"expires_at":   meta.ExpiresAt.Format(time.RFC3339),
			}
			envBytes, _ := json.Marshal(envelope)
			content = []mcp.Content{
				mcp.TextContent{Type: mcp.ContentTypeText, Text: string(envBytes)},
			}
		} else {
			// Pass: return the JSON result directly.
			content = []mcp.Content{
				mcp.TextContent{Type: mcp.ContentTypeText, Text: string(jsonBytes)},
			}
		}

		// Opportunistic wake fold — compatibility safety net for the
		// deadline-driven mpm-scheduler daemon. As of the deadline-driven
		// work, mpm-scheduler is the authoritative dispatcher for
		// notification-kind scheduled_wakes at their target_time. This
		// fold remains so that MCP tool responses surface due wakes to
		// the agent immediately, even when the daemon is unavailable,
		// and without waiting for the bounded-sleep floor in Run().
		//
		// Atomic safety: dm.CheckPendingWakes performs an UPDATE WHERE
		// fired=0 in a single transaction. The daemon's dispatchClaim
		// uses the same shape. Exactly one wins on a race.
		//
		// check for due wakes and prepend a visually distinct XML
		// notification block. Uses the raw JSON text for the primary
		// content so any client that doesn't understand the wake block
		// still gets clean machine-readable output.
		if dm != nil {
			if due, dErr := dm.CheckPendingWakes(time.Now(), nil); dErr == nil && len(due) > 0 {
				notification := core.FormatWakeNotification(due)
				prepended := make([]mcp.Content, 0, len(content)+1)
				prepended = append(prepended, mcp.TextContent{
					Type: mcp.ContentTypeText,
					Text: notification,
				})
				prepended = append(prepended, content...)
				content = prepended
			}
		}

		return &mcp.CallToolResult{Content: content}, nil
	}
}

// makeRouteHandler is the only per-tool MCP handler that survives —
// route closes over the *Router (constructed once at server boot),
// which is not in the registry because the registry has no router
// reference. Every other tool uses the generic mcpAdapter.
func makeRouteHandler(router *core.Router) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		prompt, _ := req.GetArguments()["prompt"].(string)
		if prompt == "" {
			return mcp.NewToolResultError("prompt is required"), nil
		}
		return jsonResult(router.Evaluate(prompt)), nil
	}
}

// jsonResult marshals v to JSON and wraps it in an mcp text result.
// Errors during marshalling fall back to a quoted string so the
// handler still returns a useful response.
func jsonResult(v interface{}) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("%q", fmt.Sprintf("%v", v)))
	}
	return mcp.NewToolResultText(string(b))
}

// mcpAdapterForTest wires test doubles into the global policy/blob vars
// and delegates to mcpAdapter. The defer restores the originals so tests
// are fully isolated from each other. Callers must call RestoreGlobals() after
// all adapter calls complete.
func mcpAdapterForTest(dm *core.DatabaseManager, ac core.ActiveContext, handler tools.HandlerFunc, bs blobstore.BlobStore, op tools.OutputPolicy) (func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error), func()) {
	origBS := blobStore
	origOP := outputPolicy_
	blobStore = bs
	outputPolicy_ = op
	restore := func() {
		blobStore = origBS
		outputPolicy_ = origOP
	}
	return mcpAdapter(dm, ac, handler), restore
}

// ── Arg helpers ────────────────────────────────────────────────────────────

// parseNum extracts a numeric value from a JSON-decoded arg.
// Defaults to def when the value is missing or the wrong type.
// Accepts float64, int, int64 (covers every JSON number encoding
// the MCP server is likely to see).
func parseNum(v interface{}, def float64) float64 {
	if v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return def
}

// stringArg returns the string value of v (empty string if not a string).
func stringArg(v interface{}) string {
	s, _ := v.(string)
	return s
}

// defaultString returns s (if non-string) or defaultStr.
func defaultString(v interface{}, defaultStr string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return defaultStr
}

// parseStringSliceArg accepts a string (comma-separated) or []interface{}
// (each must be a string). Returns nil for any other shape or empty
// input. The opencode plugin emits arrays; many clients flatten to CSV
// when the tool schema is a string — accept both shapes.
func parseStringSliceArg(v interface{}) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		parts := strings.Split(t, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	}
	return nil
}

// buildSpillPreview extracts a preview from a spilled JSON result.
// Returns approximate item count and first few keys for the spill envelope.
func buildSpillPreview(jsonBytes []byte) map[string]interface{} {
	var v interface{}
	if err := json.Unmarshal(jsonBytes, &v); err != nil {
		return map[string]interface{}{"kind": "unknown", "approx_items": 0, "first_keys": []string{}}
	}

	result := map[string]interface{}{"kind": "unknown", "approx_items": 0, "first_keys": []string{}}

	switch val := v.(type) {
	case []interface{}:
		result["kind"] = "array"
		result["approx_items"] = len(val)
	case map[string]interface{}:
		result["kind"] = "json"
		result["approx_items"] = len(val)
		keys := make([]string, 0, 5)
		for k := range val {
			if len(keys) >= 5 {
				break
			}
			keys = append(keys, k)
		}
		result["first_keys"] = keys
	default:
		result["kind"] = fmt.Sprintf("%T", v)
	}

	return result
}