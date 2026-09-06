// cli_args_invoke.go — direct tool invocation helper for CLI handlers.
//
// Stage S5 of the approved CLI refactor (see
// docs/archive/mpm-cli-overhaul-investigation-2026-09-06.md §9).
// Lets a top-level CLI command invoke a registered tool directly
// (bypassing the JSON envelope that `mpm call <tool>` emits) so
// the CLI can render its own human or `--json` output from the
// tool's structured result.
//
// Why a separate helper rather than calling `mpm call` via
// subprocess:
//
//   - subprocess startup is slow;
//   - `mpm call` opens its own DatabaseManager (`openCallDM`),
//     producing two open DB connections per invocation;
//   - `mpm call` always emits a JSON envelope on stdout, which
//     the human-mode CLI must NOT do.
//
// invokeTool reuses the CLI singleton DatabaseManager and the
// ActiveContext built from CLI globals + env vars, then calls the
// tool's registered Handler directly. It records the same audit
// row `mpm call` records, so tool_invocations provenance is
// preserved.
//
// S1-S4 helper invariants are NOT touched by this file:
//   - splitOnDashDash, ExtractJSONFlag, parseBoundedInt, parseEnum
//     remain at their established signatures.
//   - This file is the *thin adapter layer* that delegates to the
//     canonical tool path. It does not parse CLI argv directly
//     (callers do); it does not render CLI output (callers do).
package main

import (
	"fmt"
	"os"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// invokeTool directly invokes a tool's registered Handler and returns
// the raw result map. Records an audit row in tool_invocations
// (same instrumentation as `mpm call`).
//
// Returns:
//   - (map, nil)            on success; the map is the tool's result.
//   - (nil-or-partial, err) on failure; err is deterministic.
//
// The caller is responsible for rendering the result. The CLI
// wrapper around invokeTool should:
//
//   - render human output from the result map;
//   - render `--json` output from the result map;
//   - emit error messages via usererror.Error or respond(...).
//
// This helper does NOT emit JSON envelopes. That is the difference
// from `mpm call`. CLI handlers invoke the canonical tool path
// for the semantics but own the output rendering.
func invokeTool(name string, payload map[string]interface{}) (map[string]interface{}, error) {
	dm := getDBConcrete()
	if dm == nil {
		return nil, fmt.Errorf("database unavailable")
	}
	tool, ok := tools.ByName(name)
	if !ok {
		// Internal failure — name should have been verified at
		// handler design time. Surface a precise error rather than
		// a silent no-op.
		return nil, fmt.Errorf("internal: tool %q is not registered", name)
	}
	ac := mpminternal.ActiveContext{
		SessionID:  getOrMakeSessionID(),
		Agent:      resolveAgentID(),
		Hostname:   resolveHostname(),
		Model:      os.Getenv("MPM_PROVENANCE_MODEL"),
		FrameworkName: "mpm-cli",
	}
	startedAt := time.Now()
	result, err := tool.Handler(dm, ac, payload)
	completedAt := time.Now()
	if db := dm.SQLDB(); db != nil {
		recordToolInvocation(db, ac, name, payload,
			startedAt, completedAt, extractAction(payload), auditStatus(err), err)
	}
	if err != nil {
		// Tool returned an error. result may still carry a partial
		// map (e.g. F12-1 embedding_status="unavailable" still
		// reports memory_id); return both so the caller can render
		// the partial info.
		m, _ := result.(map[string]interface{})
		return m, err
	}
	m, _ := result.(map[string]interface{})
	if m == nil {
		return map[string]interface{}{}, nil
	}
	return m, nil
}