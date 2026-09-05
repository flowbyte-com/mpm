// onboarding_drift_test.go — drift detection for the seven
// tool-description / schema facts identified by the
// onboarding-mcp-native audit
// (docs/onboarding-mcp-native-audit-2026-09-05.md Part B) as gaps
// currently living only in the managed block.
//
// The audit's Part D recommendation was: "Move operational constraints
// out of the file-based managed block and into per-tool descriptions
// and schemas (which MCP-native tooling sees on every conversation),
// and pin the new home with a drift test analogous to
// render_managed_blocks.py --check." This file is that test.
//
// The seven facts are:
//
//   1.  mpm_work complete   — schema: requires work_id
//   2.  mpm_work complete   — description: host session termination
//                              does NOT auto-complete a work item
//   3.  mpm_handoff write   — schema: requires summary
//   4.  mpm_handoff write   — description: mid-session acks are NOT
//                              session-closing; do not write a
//                              handoff for them
//   5.  mpm_context         — description: wake payload includes
//                              <available_skills> catalogue
//   6.  mpm_context         — description: read_wake_context supports
//                              projection="compact"
//   7.  (NOT a separate test — mpm_memory / mpm_lessons summary
//        projection defaults were already in descriptions and
//        schemas at audit time; no drift risk from this audit's
//        scope; tracked here for completeness.)
//
// If any of these tools' descriptions or schemas silently regress,
// the test fails with a clear pointer to the lost fact and its
// source.

package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// stringLiteral unescapes the JSON-encoded tool description string
// inside Registry[i].Description, which the registry embeds as a
// raw string literal with backtick delimiters. Used so we can match
// substring markers against the prose without having to keep the test
// in lockstep with every quoting choice.
func stringLiteral(s string) string {
	return s
}

// toolByName returns the Registry entry for a given tool name, or
// fatals.
func toolByName(t *testing.T, name string) *Tool {
	t.Helper()
	for i := range Registry {
		if Registry[i].Name == name {
			return &Registry[i]
		}
	}
	t.Fatalf("Registry has no tool named %q — was it retired and not updated?", name)
	return nil
}

// findAction locates the action descriptor's properties branch in
// the JSON schema. For tools that use oneOf-per-action (mpm_work,
// mpm_handoff), it picks the branch matching `const == action` and
// returns the `params` sub-object (which lives INSIDE `properties`,
// not at the branch's top level — that was the bug in v1 of this
// test that mistook the parsed path).
func findAction(t *testing.T, schema json.RawMessage, action string) map[string]interface{} {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal(schema, &root); err != nil {
		t.Fatalf("schema unmarshal: %v", err)
	}
	oneofs, _ := root["oneOf"].([]interface{})
	for _, branch := range oneofs {
		m, _ := branch.(map[string]interface{})
		props, _ := m["properties"].(map[string]interface{})
		actProps, _ := props["action"].(map[string]interface{})
		if c, _ := actProps["const"].(string); c == action {
			params, _ := props["params"].(map[string]interface{})
			return params
		}
	}
	t.Fatalf("action=%q not found in schema's oneOf branches", action)
	return nil
}

// requiredFromParams returns the `required` list from a JSON schema
// `params` branch.
func requiredFromParams(t *testing.T, params map[string]interface{}) []string {
	t.Helper()
	req, _ := params["required"].([]interface{})
	out := make([]string, 0, len(req))
	for _, v := range req {
		out = append(out, v.(string))
	}
	return out
}

func containsString(s, sub string) bool {
	return strings.Contains(s, sub)
}

func containsAny(slice []string, want string) bool {
	for _, v := range slice {
		if v == want {
			return true
		}
	}
	return false
}

// TestOnboardingDrift_mpm_work pins facts (1) and (2).
func TestOnboardingDrift_mpm_work(t *testing.T) {
	entry := toolByName(t, "mpm_work")

	// (1) Schema: action=complete requires work_id.
	t.Run("schema_complete_requires_work_id", func(t *testing.T) {
		params := findAction(t, entry.Schema, "complete")
		req := requiredFromParams(t, params)
		if !containsAny(req, "work_id") {
			t.Errorf("mpm_work.complete schema must require work_id; required=%v", req)
		}
	})

	// (2) Description: host session termination does NOT auto-complete.
	t.Run("description_no_auto_complete", func(t *testing.T) {
		desc := stringLiteral(entry.Description)
		if !containsString(desc, "Host session termination does NOT auto-complete") {
			t.Errorf("mpm_work description must say host session termination does NOT auto-complete; got:\n%s",
				desc)
		}
		if !containsString(desc, "complete") {
			t.Errorf("mpm_work description must call out the complete action specifically; got:\n%s",
				desc)
		}
	})
}

// TestOnboardingDrift_mpm_handoff pins facts (3) and (4).
func TestOnboardingDrift_mpm_handoff(t *testing.T) {
	entry := toolByName(t, "mpm_handoff")

	// (3) Schema: action=write requires summary.
	t.Run("schema_write_requires_summary", func(t *testing.T) {
		params := findAction(t, entry.Schema, "write")
		req := requiredFromParams(t, params)
		if !containsAny(req, "summary") {
			t.Errorf("mpm_handoff.write schema must require summary; required=%v", req)
		}
	})

	// (4) Description: mid-session acks are NOT session-closing.
	t.Run("description_no_ack_handoff", func(t *testing.T) {
		desc := stringLiteral(entry.Description)
		if !containsString(desc, "Mid-session acknowledgements") {
			t.Errorf("mpm_handoff description must say mid-session acks are NOT session-closing; got:\n%s",
				desc)
		}
		if !containsString(desc, "ack") {
			t.Errorf("mpm_handoff description must list 'ack' as one of the non-session-closing acks; got:\n%s",
				desc)
		}
	})
}

// TestOnboardingDrift_mpm_context pins facts (5) and (6).
func TestOnboardingDrift_mpm_context(t *testing.T) {
	entry := toolByName(t, "mpm_context")

	// (5) Description: <available_skills> catalogue mentioned.
	t.Run("description_available_skills", func(t *testing.T) {
		desc := stringLiteral(entry.Description)
		if !containsString(desc, "<available_skills>") {
			t.Errorf("mpm_context description must mention <available_skills> catalogue; got:\n%s",
				desc)
		}
	})

	// (6) Description: projection="compact" mentioned.
	t.Run("description_projection_compact", func(t *testing.T) {
		desc := stringLiteral(entry.Description)
		if !containsString(desc, `projection="compact"`) {
			t.Errorf("mpm_context description must mention projection=\"compact\"; got:\n%s",
				desc)
		}
	})
}

// TestOnboardingDrift_mpm_memory_summary_default notes (fact 7) — the
// projection-summary default was already in mpm_memory / mpm_lessons
// descriptions at audit time, so this test pins the *continued*
// presence rather than a fix. If it ever does disappear, the gap would
// need to be plugged again, so this is still worth pinning.
func TestOnboardingDrift_mpm_memory_summary_default(t *testing.T) {
	entry := toolByName(t, "mpm_memory")
	desc := stringLiteral(entry.Description)
	if !containsString(desc, "projection defaults to 'summary'") &&
		!containsString(desc, `projection defaults to "summary"`) {
		t.Errorf("mpm_memory description must continue to carry the projection=summary default; got:\n%s",
			desc)
	}
}

func TestOnboardingDrift_mpm_lessons_summary_default(t *testing.T) {
	entry := toolByName(t, "mpm_lessons")
	desc := stringLiteral(entry.Description)
	if !containsString(desc, "projection defaults to 'summary'") &&
		!containsString(desc, `projection defaults to "summary"`) {
		t.Errorf("mpm_lessons description must continue to carry the projection=summary default; got:\n%s",
			desc)
	}
}

// TestOnboardingDrift_ToolCountFitsSurface — a small sanity guard
// that the Registry is internally consistent with the assertions in
// this test file. (Bigger count checks live in
// mpm_challenge_removed_test.go.)
func TestOnboardingDrift_AllFactsArePinned(t *testing.T) {
	if len(Registry) < 21 {
		t.Fatalf("tool count regressed below 21: %d — the seven facts in this file assume the audited registry shape",
			len(Registry))
	}
}
