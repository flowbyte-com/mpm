// help.go — mpm_help's discovery catalogue, shared by both transports.
//
// mpm_help answers two questions: what can MPM do and how do I call it,
// and — for a specific tool — what payload shape will it accept? The
// second question needs the tool's registered JSON input schema, not its
// description: a description says what a tool is for, while the schema
// says which actions and fields are actually legal. The approved
// behavioural contract tells agents to obtain the appropriate schema
// before invoking an unfamiliar operation, so discovery returns it.
//
// mpm_help is registered as a native MCP closure rather than a registry
// entry (a registry entry would double-register under
// MPM_EXPOSE_ALL_TOOLS=1, where the full surface is exposed alongside
// the compact one).
//
// The consequence of being a closure is that `mpm call` — which
// dispatches registry-backed tools only — could not reach it, leaving
// shell-only agents with no way to discover the substrate. The fix is
// not a second catalogue: both DISCOVERY transports serve the
// implementation below, so an agent cannot discover a conflicting schema
// depending on which discovery transport it used.
//
// That parity is between the two ways of asking for discovery. It is
// NOT a claim that every tool's registered schema is the schema its
// native compact MCP wrapper publishes — those wrappers publish
// simplified schemas of their own and may accept transport-specific
// actions (see inputSchemaField).
//
// The returned map is the transport-neutral payload; both the MCP
// handler and the `mpm call` fallback add their own envelope around it.

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// inputSchemaField is the response field that carries a tool's registered
// JSON input schema.
//
// The name follows this repository's JSON convention: MPM's own envelopes
// use snake_case keys (tool_count, discovery_hint, compatibility_mode,
// reach_via_cli, schema_version), so the discovery catalogue does too. It
// is the snake_case spelling of the MCP wire name "inputSchema".
//
// WHAT SCHEMA THIS CARRIES: the tool's REGISTERED schema — the registry
// entry's own `Tool.Schema`, which is the schema the `mpm call` path and
// the runtime dispatcher validate an incoming payload against.
//
// WHAT SCHEMA THIS DOES NOT CLAIM: the schema the native COMPACT MCP
// wrapper for the same tool publishes. Those wrappers deliberately
// publish a simplified input schema, and a compact wrapper may
// additionally accept transport-specific actions the registry entry does
// not list. `mpm_context` is the concrete case: its compact wrapper
// accepts `write_handoff` and `read_handoff` — the session-continuity
// actions, which the compact adapter routes to the mpm_handoff handler —
// while the registry entry's action enum does not contain them, and the
// registry handler rejects them as unknown actions.
//
// So describing the registered schema as "the schema the MCP surface
// publishes" overstates the parity: the compact MCP surface and the
// registered CLI / substrate surface are genuinely different contracts.
// The discovery catalogue reports the registered one, and says so.
// See docs/SPEC.md, "MCP/CLI parity: what is exposed via both surfaces".
const inputSchemaField = "input_schema"

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

// toolInputSchema decodes a tool's registered JSON input schema into a
// JSON object.
//
// The registry's schema is the tool's PUBLISHED CONTRACT: it is what an MCP
// client receives as the tool's input schema, and it is what this discovery
// surface returns. Discovery therefore returns that object verbatim rather
// than a second, hand-written summary of it — a summary would drift from
// the contract the client is shown.
//
// A correction to an earlier version of this comment, which claimed the
// schema is "the same object the runtime dispatcher validates an incoming
// payload against". That was wrong, and it is worth being precise here
// because the distinction decides where a rule belongs:
//
//   - There is NO JSON-schema validation in MPM's Go code. Neither the
//     `mpm call` dispatcher nor the runtime dispatcher validates a payload
//     against the registered schema, and the MCP server does not either —
//     mcp-go's input-schema validation is opt-in via
//     server.WithInputSchemaValidation, which the server does not set, and
//     mcp.NewToolWithRawSchema (the form this server registers with)
//     bypasses the structured-schema helpers entirely.
//   - So the schema is DISCOVERY, not enforcement. A rule that must hold
//     belongs in the handler or the core, where it is transport-independent.
//
// A caller that trusts the published schema to be enforced would be wrong.
// The registry schema tells an agent what the parameters ARE; the handler
// and the core decide what is ACCEPTED.
//
// It is NOT the native compact MCP wrapper's schema. The compact
// wrappers publish deliberately simplified input schemas and may expose
// transport-specific actions (see inputSchemaField). Discovery reports
// the registered contract; the compact surface is described by its own
// published schema.
//
// An empty schema is not an error: it means the tool declares no input
// constraints, and the honest answer is an empty JSON object. A schema
// that is present but unreadable IS an error — it is a registry defect,
// and reporting it is better than handing an agent a schema that does
// not describe the tool.
func toolInputSchema(t Tool) (map[string]interface{}, error) {
	if len(t.Schema) == 0 {
		return map[string]interface{}{}, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(t.Schema, &out); err != nil {
		return nil, fmt.Errorf("mpm_help: tool %q has an unreadable registered schema: %w", t.Name, err)
	}
	if out == nil {
		return map[string]interface{}{}, nil
	}
	return out, nil
}

// Help answers mpm_help's discovery contract.
//
// action "list" enumerates every registry tool with a one-line summary
// and the `mpm call` form that reaches it. action "show" returns one
// tool's full description together with its registered JSON input
// schema under "input_schema". The schema is the tool's REGISTERED
// (CLI / substrate) schema — see inputSchemaField for why that is a
// different statement from "the schema the MCP surface publishes".
//
// The schema is returned as a JSON object, not as an escaped JSON
// string, so an agent can read the action vocabulary, the required
// fields and the nested parameter properties directly out of the
// response.
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
		schema, err := toolInputSchema(t)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"success":        true,
			"name":           t.Name,
			"description":    t.Description,
			inputSchemaField: schema,
			"reach_via_cli":  cliReach(t.Name),
		}, nil
	default:
		return nil, fmt.Errorf("mpm_help: unknown action %q (valid: %s)", action, strings.Join(helpActions, ", "))
	}
}
