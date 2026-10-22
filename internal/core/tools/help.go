// help.go — mpm_help's discovery catalogue, shared by both transports.
//
// mpm_help answers one question: what can MPM do, and how do I call it?
// It is registered as a native MCP closure rather than a registry entry
// (a registry entry would double-register under MPM_EXPOSE_ALL_TOOLS=1,
// where the full surface is exposed alongside the compact one).
//
// The consequence of being a closure is that `mpm call` — which
// dispatches registry-backed tools only — could not reach it, leaving
// shell-only agents with no way to discover the substrate. The fix is
// not a second catalogue: both transports serve the implementation
// below, so an agent cannot discover a conflicting schema depending on
// which transport it used.
//
// The returned map is the transport-neutral payload; both the MCP
// handler and the `mpm call` fallback add their own envelope around it.

package tools

import (
	"errors"
	"fmt"
	"strings"
)

// cliReachTemplate is the canonical `mpm call` form for a tool. It is
// the same string in every transport and in every row of the list
// action, so an agent can copy it verbatim.
const cliReachTemplate = `mpm call %s --payload '{"action":"<op>","params":{...}}'`

// cliReach renders the canonical `mpm call` invocation for a tool name.
func cliReach(name string) string {
	return fmt.Sprintf(cliReachTemplate, name)
}

// helpActions are the actions the discovery catalogue accepts.
//
// It is the machine-readable form of the contract Help implements.
// HelpActions exposes it to callers that need to know the action
// vocabulary without calling Help — the agent protocol guard uses it
// to check a documented invocation against the actions that exist.
var helpActions = []string{"list", "show"}

// HelpActions returns the actions the discovery catalogue accepts.
//
// The returned slice is a copy; callers cannot mutate the contract.
func HelpActions() []string {
	out := make([]string, len(helpActions))
	copy(out, helpActions)
	return out
}

// Help answers mpm_help's discovery contract.
//
// action "list" enumerates every registry tool with a one-line summary
// and the `mpm call` form that reaches it. action "show" returns one
// tool's full description.
//
// The returned map carries "success": true. A failed request returns a
// nil map and an error whose message is the operator-facing reason;
// both transports report that error the way they report every other
// validation failure.
//
// Help reads only the in-memory Registry. It never touches the
// database, so an agent can discover what MPM can do without creating
// or mutating a store.
func Help(action, toolName string) (map[string]interface{}, error) {
	switch action {
	case "list":
		rows := make([]map[string]interface{}, 0, len(Registry))
		for _, t := range Registry {
			short := t.Description
			if i := strings.Index(short, "\n"); i >= 0 {
				short = short[:i]
			}
			rows = append(rows, map[string]interface{}{
				"name":          t.Name,
				"one_liner":     short,
				"reach_via_cli": cliReach(t.Name),
			})
		}
		return map[string]interface{}{
			"success":            true,
			"tool_count":         len(rows),
			"tools":              rows,
			"discovery_hint":     "Specialist tools not initially exposed via tools/list are still registered and reachable via the host shell using `mpm call <tool>` (see reach_via_cli per row).",
			"compatibility_mode": "Set MPM_EXPOSE_ALL_TOOLS=1 to revert to the full surface for hosts that need it.",
		}, nil
	case "show":
		if toolName == "" {
			return nil, errors.New("mpm_help show: 'tool' param required")
		}
		t, ok := ByName(toolName)
		if !ok {
			return nil, fmt.Errorf("mpm_help: unknown tool %q", toolName)
		}
		return map[string]interface{}{
			"success":       true,
			"name":          t.Name,
			"description":   t.Description,
			"reach_via_cli": cliReach(t.Name),
		}, nil
	default:
		return nil, fmt.Errorf("mpm_help: unknown action %q (valid: %s)", action, strings.Join(helpActions, ", "))
	}
}
