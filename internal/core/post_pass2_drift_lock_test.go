// post_pass2_drift_lock_test.go — Post-Pass-2 Debt Reconciliation
// (2026-09-04) regression lock. Pins the doc/code correspondence
// for drifts found during the pass-2 audit:
//
//   1. Canonical protocol §3.1 must state the 0..5 axis range and
//      the boundary enum {procedure, judgment, knowledge} — matching
//      what skill_workshop.go's validateInput enforces.
//   2. Canonical protocol §7.2 and §8.2 use `mpm_references read`,
//      never `mpm_references show` — matching the registry enum
//      and the handler switch.
//   3. The "9-field" claim for the compact projection must not
//      survive anywhere — the struct has 8 always-on fields plus
//      0/2 LastHandoff fields; the canonical description is
//      semantic ("id+summary envelope"), not a count.
//   4. The mpm_decisions registry schema enum must list every
//      action the handler accepts, not just the original three —
//      the handler dispatches on `show | list | query` in
//      addition to `record | supersede | invalidate`.
//   5. The canonical snippets file's mpm_lessons row must not
//      claim `type` is required on save — handleSaveLesson
//      defaults to "insight" when missing.
//
// Each test reads the source file and asserts the drift has not
// reappeared. The test fails if a future edit reintroduces the
// drift; it passes once the source is corrected.
package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot locates the repository root from this test file's location.
// This file lives at internal/core/post_pass2_drift_lock_test.go, so
// the repo root is two parents up.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	require.NoError(t, err)
	return root
}

func readFile(t *testing.T, rel string) string {
	t.Helper()
	root := repoRoot(t)
	p := filepath.Join(root, rel)
	b, err := os.ReadFile(p)
	require.NoError(t, err, "reading %s", rel)
	return string(b)
}

// TestPass2_Protocol_Section3_1_WorkshopScoringMatchesCode locks the
// protocol §3.1 wording to the actual validator (0..5 range, boundary
// enum {procedure, judgment, knowledge}).
func TestPass2_Protocol_Section3_1_WorkshopScoringMatchesCode(t *testing.T) {
	root := repoRoot(t)
	proto := readFile(t, filepath.Join("agent_installation", "mpm-agent-protocol.md"))

	// Section §3.1 should describe the 0..5 axis range.
	assert.Regexp(t, `Reusability[^)]*0[–-]5`, proto,
		"protocol §3.1 must advertise the 0..5 axis range (validator enforces this)")
	assert.Regexp(t, `Non[- ]obviousness[^)]*0[–-]5`, proto,
		"protocol §3.1 must advertise the 0..5 axis range for non_obviousness")
	assert.Regexp(t, `Stability[^)]*0[–-]5`, proto,
		"protocol §3.1 must advertise the 0..5 axis range for stability")
	assert.Regexp(t, `Leverage[^)]*0[–-]5`, proto,
		"protocol §3.1 must advertise the 0..5 axis range for leverage")

	// Section §3.1 must enumerate the documented boundary values.
	assert.Contains(t, proto, "procedure | judgment | knowledge",
		"protocol §3.1 boundary enum must match validator")

	// The old (pre-F15-1) drift wording must be gone.
	assert.NotRegexp(t, `Reusability[^)]*0[–-]2`, proto,
		"protocol §3.1 must not retain the old 0..2 wording")
	assert.NotRegexp(t, `procedure\s*\|\s*fact\s*\|\s*preference\s*\|\s*one_off`, proto,
		"protocol §3.1 must not retain the old boundary enum")

	_ = root // silence unused
}

// TestPass2_Protocol_ReferencesReadNotShow locks the protocol's
// reference-tool action name. mpm_references has no `show` action —
// the registry enum is add/read/search/list and the handler switch
// uses `read`.
func TestPass2_Protocol_ReferencesReadNotShow(t *testing.T) {
	proto := readFile(t, filepath.Join("agent_installation", "mpm-agent-protocol.md"))

	// The action is `read`, never `show`.
	assert.NotContains(t, proto, "mpm_references show",
		"protocol must not reference the stale mpm_references show action")
	assert.NotRegexp(t, `mpm_references[^\n]*\bshow\b`, proto,
		"protocol must not reference mpm_references show in any context")

	// The action listed alongside search must be `read`.
	assert.Contains(t, proto, "mpm_references list",
		"protocol §7.2 / §8.2 must list mpm_references list (still canonical)")
}

// TestPass2_RegistryCompactProjectionOmitsFieldCount locks the
// canonical description of the compact projection to its semantic
// meaning. The actual struct has 8 always-on fields plus 0 or 2
// LastHandoff fields; the count is variable.
func TestPass2_RegistryCompactProjectionOmitsFieldCount(t *testing.T) {
	registry := readFile(t, filepath.Join("internal", "core", "tools", "registry_list.go"))
	handlers := readFile(t, filepath.Join("internal", "core", "tools", "handlers.go"))
	cliHandlers := readFile(t, filepath.Join("cmd", "mpm", "handlers_session.go"))
	claudeMD := readFile(t, "CLAUDE.md")

	// The "9-field" framing must be gone — the count is wrong and
	// variable. Use a semantic description instead.
	for _, src := range []struct {
		name, body string
	}{
		{"registry_list.go", registry},
		{"handlers.go", handlers},
		{"cmd/mpm/handlers_session.go", cliHandlers},
		{"CLAUDE.md", claudeMD},
	} {
		assert.NotRegexp(t, `\b9[- ]field\b`, src.body,
			"%s must not describe the compact projection with a field count", src.name)
	}
}

// TestPass2_RegistryMpmDecisionsEnumMatchesHandler locks the
// mpm_decisions action enum in the registry schema to the full set
// the handler accepts. The handler dispatches on six actions:
// record, supersede, invalidate, show, list, query.
func TestPass2_RegistryMpmDecisionsEnumMatchesHandler(t *testing.T) {
	registry := readFile(t, filepath.Join("internal", "core", "tools", "registry_list.go"))
	handlers := readFile(t, filepath.Join("internal", "core", "tools", "handlers.go"))

	// Locate the mpm_decisions block in the registry.
	idx := strings.Index(registry, `"mpm_decisions"`)
	require.GreaterOrEqual(t, idx, 0, "registry must define mpm_decisions")
	block := registry[idx:idx+2000]

	// The action enum must list all six values.
	for _, action := range []string{`"record"`, `"supersede"`, `"invalidate"`, `"show"`, `"list"`, `"query"`} {
		assert.Contains(t, block, action,
			"mpm_decisions schema action enum must include %s (handler dispatches it)", action)
	}

	// Cross-check: the handler error message names the same six.
	require.Contains(t, handlers, "Valid actions include record, supersede, invalidate, show, list, query",
		"handler error message must enumerate all six mpm_decisions actions")
}

// TestPass2_SnippetsLessonsTypeOptional locks the canonical
// snippets file to the actual handler behavior: handleSaveLesson
// defaults `type` to "insight" when missing, so it is optional.
func TestPass2_SnippetsLessonsTypeOptional(t *testing.T) {
	snippets := readFile(t, filepath.Join("agent_installation", "MPM_AGENT_INTEGRATION_SNIPPETS.md"))
	handlers := readFile(t, filepath.Join("internal", "core", "tools", "handlers.go"))

	// Find the mpm_lessons row in the tool-reference stability
	// contract table. The row format is:
	//   `| mpm_lessons | ... | ... |`
	lines := strings.Split(snippets, "\n")
	var row string
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "|") && strings.Contains(line, "mpm_lessons") && strings.Contains(line, "save") && strings.Contains(line, "search") && strings.Contains(line, "list") {
			row = lines[i]
			break
		}
	}
	require.NotEmpty(t, row, "snippets file must have an mpm_lessons row in the tool-reference table")
	assert.NotRegexp(t, `requires[^a-z]+type`, row,
		"snippets mpm_lessons row must not claim type is required on save")

	// Cross-check: handler defaults missing type to "insight".
	assert.Contains(t, handlers, `ParseStringOr(p["type"], "insight")`,
		"handler must default missing type to insight (the documented behavior)")
}

// TestPass2_RegistryMpmLessonsSchemaOmitsRequiredType confirms the
// JSON schema for mpm_lessons does not mark `type` as required. The
// handler defaults to insight; the schema must reflect that.
func TestPass2_RegistryMpmLessonsSchemaOmitsRequiredType(t *testing.T) {
	registry := readFile(t, filepath.Join("internal", "core", "tools", "registry_list.go"))

	// Extract the mpm_lessons Schema JSON. The Go source uses
	// `Schema: json.RawMessage(\`{...}\`)` so the JSON is delimited
	// by a backtick pair inside the RawMessage call.
	idx := strings.Index(registry, `"mpm_lessons"`)
	require.GreaterOrEqual(t, idx, 0, "registry must define mpm_lessons")
	block := registry[idx : idx+3000]

	openIdx := strings.Index(block, "json.RawMessage(`")
	require.GreaterOrEqual(t, openIdx, 0, "mpm_lessons must use json.RawMessage literal")
	open := openIdx + len("json.RawMessage(`")
	closeIdx := strings.Index(block[open:], "`)")
	require.Greater(t, closeIdx, 0, "mpm_lessons schema must close with backtick-paren")
	schemaJSON := block[open : open+closeIdx]

	var schema struct {
		Properties struct {
			Params struct {
				Properties map[string]map[string]interface{} `json:"properties"`
				Required   []string                          `json:"required"`
			} `json:"params"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal([]byte(schemaJSON), &schema))

	// `type` is optional — must not appear in params.required.
	for _, req := range schema.Properties.Params.Required {
		assert.NotEqual(t, "type", req,
			"mpm_lessons.params.required must not list `type` (handler defaults it to insight)")
	}
}
