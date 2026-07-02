package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	mpminternal "mpm/internal"
	"mpm/internal/tools"
)

// openCallDM returns a freshly-opened workspace DatabaseManager and a
// close function the caller must defer. Tests that need to inject a
// temp DB should open one directly and pass it to the registry handler
// — the registry's HandlerFunc signature takes dm explicitly, so there's
// no need for a global override.
func openCallDM() (*mpminternal.DatabaseManager, func(), error) {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		return nil, nil, fmt.Errorf("db: %w", err)
	}
	return dm, func() { dm.Close() }, nil
}

// handleCall is the main entry point for `mpm call <tool>`.
//
// Payload is read from one of three sources (in priority order):
//  1. --payload <json>     inline JSON arg (avoid with apostrophes/quotes)
//  2. --payload-file <p>   JSON file path (no shell escaping)
//  3. stdin                pipe or redirect (`echo ... | mpm call ...` or `< file`)
//
// If none are present, returns an empty payload (some tools need no input).
func handleCall(args []string) int {
	if len(args) < 1 {
		printError("usage: mpm call <tool_name> [--payload <json> | --payload-file <path>] | (stdin)")
		return 1
	}

	toolName := args[0]
	tool, ok := tools.ByName(toolName)
	if !ok {
		printError("unknown tool: %s. available: %s", toolName, strings.Join(tools.Names(), ", "))
		return 1
	}

	payload, err := parsePayload(args[1:])
	if err != nil {
		printError("failed to parse payload: %v", err)
		return 1
	}

	// Open DB for this call. The CLI dispatcher owns the lifetime;
	// handlers in the registry receive dm as their first arg.
	dm, closeDM, err := openCallDM()
	if err != nil {
		printError("failed to open db: %v", err)
		return 1
	}
	defer closeDM()

	// Build the ActiveContext from CLI-detected mode/persona.
	// The MCP server constructs this once at boot and passes it directly;
	// the CLI derives it from the active.json file on disk.
	injectActiveContext()
	defer clearActiveContext()
	ac := mpminternal.ActiveContext{Mode: activeMode, Persona: activePersona}

	result, err := tool.Handler(dm, ac, payload)
	if err != nil {
		// Errors are JSON to stderr so the agent can parse them
		// Direct fmt.Fprintf to stderr: JSON error envelope for `mpm call` — must be raw JSON, not user-formatted.
	fmt.Fprintf(os.Stderr, "%s\n", must(json.Marshal(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})))
		return 1
	}

	// Opportunistic wake fold (Phase 5a): any `mpm call` surfaces due
	// wakes in its response. The dispatcher is the single chokepoint so we
	// add the fold here rather than in every handler. Cost: one indexed
	// SELECT (sub-millisecond on WAL with the scheduled_wakes_due index).
	// The wake handlers themselves also fold (defense-in-depth + unit-test
	// visibility when the dispatcher is bypassed).
	if resultMap, ok := result.(map[string]interface{}); ok {
		if due, dErr := dm.CheckPendingWakes(time.Now()); dErr == nil && len(due) > 0 {
			resultMap["WakesPending"] = due
			resultMap["WakesPendingCount"] = len(due)
		}
		result = resultMap
	}

	fmt.Println(string(must(json.Marshal(result))))
	return 0
}

// parsePayload extracts JSON from --payload flag, --payload-file flag, or stdin.
// If none are present, returns an empty map (some tools need no input).
//
// Stdin is detected via hasStdinData() (ModeCharDevice check), which correctly
// detects pipes and file redirection. The previous implementation used
// stat.Size() > 0, which silently failed for pipes — Stat returns Size=0 for
// pipes even when data is available, so the input was never read.
func parsePayload(args []string) (map[string]interface{}, error) {
	// Last-flag-wins for --payload and --payload-file, matching standard CLI
	// convention (git, kubectl, etc.). Iterate fully, remember the most
	// recent hit, return after the loop. If both are present, the later one
	// in argv order wins.
	var (
		inlineJSON string
		inlineSet  bool
		filePath   string
		fileSet    bool
	)
	for i := 0; i < len(args); i++ {
		if args[i] == "--payload" && i+1 < len(args) {
			inlineJSON = args[i+1]
			inlineSet = true
			fileSet = false // --payload overrides any earlier --payload-file
			i++
			continue
		}
		if args[i] == "--payload-file" && i+1 < len(args) {
			filePath = args[i+1]
			fileSet = true
			inlineSet = false // --payload-file overrides any earlier --payload
			i++
			continue
		}
	}
	if inlineSet {
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(inlineJSON), &p); err != nil {
			return nil, fmt.Errorf("--payload: %w", err)
		}
		return p, nil
	}
	if fileSet {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("--payload-file: %w", err)
		}
		// Trim BOM if present (some editors write UTF-8 BOM)
		s := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
		var p map[string]interface{}
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return nil, fmt.Errorf("--payload-file: %w", err)
		}
		return p, nil
	}
	// No flag; try stdin if data is piped or redirected.
	// hasStdinData uses ModeCharDevice — correct for both pipes (Size=0 but
	// has data) and file redirection (Size>0).
	if hasStdinData() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		// Trim BOM if present (some editors write UTF-8 BOM)
		s := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
		if len(s) > 0 {
			var p map[string]interface{}
			if err := json.Unmarshal([]byte(s), &p); err != nil {
				return nil, fmt.Errorf("stdin: %w", err)
			}
			return p, nil
		}
	}
	return map[string]interface{}{}, nil
}

// must is a tiny helper for JSON marshalling where marshalling errors are
// programming bugs (we always pass concrete map[string]interface{} or
// []byte payloads, both of which marshal cleanly). The trailing-error arg
// matches the (data, err) return shape of json.Marshal so call sites read
// naturally: `must(json.Marshal(x))`.
func must(data []byte, _ error) []byte { return data }

// runHandler invokes a registry handler with the test DM and an empty
// ActiveContext. Tests that previously called callFoo(payload) directly
// should use this instead — it routes through the same code path
// production uses, so CLI/MCP parity is verified by construction.
//
// The handler name must match a tool in tools.Registry. Tests that want
// to assert on a specific tool look up the entry via tools.ByName.
func runHandler(dm *mpminternal.DatabaseManager, name string, payload map[string]interface{}) (interface{}, error) {
	tool, ok := tools.ByName(name)
	if !ok {
		return nil, fmt.Errorf("test: unknown tool %q", name)
	}
	return tool.Handler(dm, mpminternal.ActiveContext{}, payload)
}
