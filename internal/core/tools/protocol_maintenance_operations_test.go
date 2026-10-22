// protocol_maintenance_operations_test.go — the agent protocol must
// document only operations MPM actually supports.
//
// The agent protocol (agent_installation/mpm-agent-protocol.md) is the
// document an agent reads when it needs to know HOW to fulfil the
// managed behavioural contract. It therefore names concrete MPM
// operations: `mpm call mpm_decisions --payload '{"action":"supersede",
// ...}'`, and so on.
//
// Two failure modes follow from that, and both are silent:
//
//  1. The protocol names an operation that does not exist. The agent
//     reads a plausible-looking invocation, tries it, and gets an
//     "unknown action" or "unknown tool" error at the exact moment it
//     was trying to preserve something. The document is not wrong in
//     the way a broken doc is wrong — it is wrong in the way a stale
//     doc is wrong, which is worse, because the agent trusted it.
//
//  2. The protocol names a real operation with a payload the transport
//     cannot carry. The `mpm call` CLI takes its payload from
//     --payload, --payload-file, or stdin. A POSITIONAL JSON argument
//     is silently ignored by the payload parser, so
//
//     mpm call mpm_memory '{"action":"query",...}'
//
//     does not fail loudly at the shell — it fails inside the handler
//     with "missing required field `action`". This exact form was in
//     the protocol before this test existed.
//
// The registry in registry_list.go is the single source of truth for
// which tools exist and which actions each one accepts. This test
// resolves the protocol's documented invocations against that
// registry, so a documented operation that is not supported fails the
// build instead of failing an agent mid-task.
package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// protocolPath is the agent protocol, resolved from this test file's
// location so the test works from any package directory.
func protocolPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must resolve this test file")
	// internal/core/tools -> internal/core -> internal -> <repo root>
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	require.NoError(t, err, "computing repo root from this test file location")
	return filepath.Join(root, "agent_installation", "mpm-agent-protocol.md")
}

func readProtocol(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(protocolPath(t))
	require.NoError(t, err, "reading the agent protocol")
	return string(data)
}

// documentedCallRe matches a fully-formed protocol invocation:
//
//	mpm call mpm_decisions --payload '{"action":"supersede", ...}'
//
// The action group deliberately excludes the placeholder spellings used
// by the protocol's generic examples (`<tool>`, `<op>`), so a generic
// illustration is not mistaken for a concrete invocation.
var documentedCallRe = regexp.MustCompile(
	`mpm call (mpm_[a-z0-9_]+) --payload '\{"action":"([a-z0-9_]+)"`,
)

// positionalCallRe matches the BROKEN invocation form: a positional
// JSON payload argument, which `mpm call` silently ignores.
var positionalCallRe = regexp.MustCompile(
	`mpm call mpm_[a-z0-9_]+ '\{`,
)

// allowedActions returns the action names a registered tool accepts.
//
// Two schema shapes are in use: a flat
// `properties.action.enum` (most tools) and a `oneOf` list of
// per-action branches whose `properties.action.const` names the action
// (mpm_work). Both are read here so a tool with either shape can be
// checked.
func allowedActions(t *testing.T, tool Tool) []string {
	t.Helper()

	var schema map[string]interface{}
	require.NoError(
		t, json.Unmarshal(tool.Schema, &schema),
		"tool %s must carry a JSON schema", tool.Name,
	)

	var out []string
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		if action, ok := props["action"].(map[string]interface{}); ok {
			if enum, ok := action["enum"].([]interface{}); ok {
				for _, v := range enum {
					s, ok := v.(string)
					require.True(t, ok, "tool %s has a non-string action enum entry", tool.Name)
					out = append(out, s)
				}
			}
		}
	}
	if oneOf, ok := schema["oneOf"].([]interface{}); ok {
		for _, branch := range oneOf {
			b, ok := branch.(map[string]interface{})
			require.True(t, ok, "tool %s has a malformed oneOf branch", tool.Name)
			props, ok := b["properties"].(map[string]interface{})
			if !ok {
				continue
			}
			action, ok := props["action"].(map[string]interface{})
			if !ok {
				continue
			}
			if c, ok := action["const"].(string); ok {
				out = append(out, c)
			}
		}
	}

	require.NotEmpty(
		t, out,
		"tool %s must declare its accepted actions in a way this test "+
			"can read; extend allowedActions if the schema shape changed",
		tool.Name,
	)
	return out
}

// TestProtocol_DocumentedCallsAreSupportedOperations resolves every
// `mpm call <tool> --payload '{"action":"X"'` invocation in the agent
// protocol against the live tool registry.
func TestProtocol_DocumentedCallsAreSupportedOperations(t *testing.T) {
	protocol := readProtocol(t)

	matches := documentedCallRe.FindAllStringSubmatch(protocol, -1)
	require.NotEmpty(
		t, matches,
		"the protocol must contain concrete `mpm call` invocations; if "+
			"they were reworded, this test is no longer checking anything",
	)

	seen := map[string]bool{}
	for _, m := range matches {
		toolName, action := m[1], m[2]

		tool, ok := ByName(toolName)
		require.True(
			t, ok,
			"the protocol documents `%s` but no such tool is registered; "+
				"document only operations MPM actually supports",
			toolName,
		)

		allowed := allowedActions(t, tool)
		require.Contains(
			t, allowed, action,
			"the protocol documents `%s` action %q, which the registry "+
				"does not accept; accepted actions are %v",
			toolName, action, allowed,
		)

		seen[toolName+"/"+action] = true
	}

	// The maintenance section is the point of this file: it must
	// actually name the operations it claims an agent should use. If
	// the section were emptied, the generic invocations elsewhere in
	// the protocol would still satisfy the loop above, so this
	// assertion is what keeps the test honest.
	for _, required := range []string{
		"mpm_memory/query",
		"mpm_memory/patch",
		"mpm_decisions/query",
		"mpm_decisions/supersede",
		"mpm_theories/query",
		"mpm_theories/resolve",
		"mpm_evidence/add",
		"mpm_work/create",
	} {
		require.True(
			t, seen[required],
			"the protocol must document %s as a supported maintenance "+
				"operation; documented so far: %v",
			required, sortedKeys(seen),
		)
	}
}

// TestProtocol_NoPositionalPayloadInvocations pins the transport
// contract for the documented fallback form.
//
// `mpm call` reads its payload from --payload, --payload-file, or
// stdin. A positional JSON argument is not a payload source: the
// parser ignores it and the handler then rejects an empty envelope
// with "missing required field `action`". A protocol example in that
// form teaches an agent a command that cannot work.
func TestProtocol_NoPositionalPayloadInvocations(t *testing.T) {
	protocol := readProtocol(t)

	matches := positionalCallRe.FindAllString(protocol, -1)
	require.Empty(
		t, matches,
		"the protocol must not show a positional JSON payload to "+
			"`mpm call`; use --payload, --payload-file, or stdin. "+
			"Offending occurrences: %v",
		matches,
	)
}

// TestProtocol_MaintenanceSectionIsSubstantive guards against the
// maintenance section being replaced by a heading and a sentence.
//
// The point of the section is that an agent can act on it. A section
// that merely names the topic would satisfy every structural check
// above while teaching nothing, so this asserts the specific
// commitments the section is required to carry.
func TestProtocol_MaintenanceSectionIsSubstantive(t *testing.T) {
	protocol := readProtocol(t)

	section := extractSection(t, protocol, "## 9. EVENT-DRIVEN MAINTENANCE OF INHERITED KNOWLEDGE")

	// The causal question that makes maintenance event-driven rather
	// than periodic.
	require.Contains(
		t, section, "act on incorrectly",
		"the maintenance section must pose the causal question that "+
			"triggers maintenance",
	)

	// The three kinds of stale knowledge, which are different and
	// must not be collapsed.
	for _, kind := range []string{
		"Previously correct, now outdated",
		"Incorrect",
		"Unresolved",
	} {
		require.Contains(
			t, section, kind,
			"the maintenance section must distinguish %q from the "+
				"other kinds of stale knowledge",
			kind,
		)
	}

	// Flagging without inventing corrections.
	require.Contains(
		t, section, "flag it instead",
		"the maintenance section must teach flagging suspected stale "+
			"knowledge rather than inventing a correction",
	)
	require.Contains(
		t, section, "metadata column only",
		"the maintenance section must state that the memory patch "+
			"path touches metadata, not the stored fact",
	)

	// Provenance preservation.
	require.Contains(
		t, section, "Do not launder history",
		"the maintenance section must forbid erasing the history a "+
			"correction is built on",
	)

	// Not ceremonial housekeeping.
	require.Contains(
		t, section, "Store-wide scans",
		"the maintenance section must rule out store-wide scans",
	)
	require.Contains(
		t, section, "Ceremonial housekeeping",
		"the maintenance section must rule out housekeeping performed "+
			"to demonstrate diligence",
	)

	// Continuation state, not titles.
	require.Contains(
		t, section, "A title is not continuation state",
		"the maintenance section must require actionable continuation "+
			"state rather than task titles",
	)
}

// extractSection returns the body of a `## ` section, from the given
// heading up to (but excluding) the next `## ` heading.
func extractSection(t *testing.T, document, heading string) string {
	t.Helper()

	start := strings.Index(document, heading)
	require.NotEqual(
		t, -1, start,
		"the protocol must contain the heading %q", heading,
	)
	rest := document[start+len(heading):]

	if idx := strings.Index(rest, "\n## "); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
