// registry_dispatcher_parity_test.go — bidirectional parity lock
// between Tool.Schema (the registry-advertised public action set)
// and the dispatcher's case list (the actually-routed set).
//
// Defect class: dispatcher supports an action that the registry
// schema does not advertise. Pre-fix, this surfaced twice in close
// succession — D-006 (mpm_theories: show/list/query) and D-4.1
// (mpm_topics: list/show) — without the existing per-tool runtime
// tests catching it because they only pinned the dispatcher, not
// the schema enum.
//
// The fix:
//   1. extractSchemaActionEnum reads the registry's declared enum
//      via ByName + JSON parse (the same path MCP / `mpm call`
//      exercise at runtime).
//   2. extractDispatcherActionList triggers an unknown action and
//      parses the canonical "Valid actions include ..." list from
//      the dispatcher's error — that list IS the dispatcher's
//      claimed public surface.
//   3. AssertSetEqual is set-equality on the two lists. Order is
//      irrelevant; both must round-trip the same set.
//
// This is the smallest reusable helper that makes this class of
// drift impossible for any tool whose action contract is encoded
// in a JSON-Schema enum. Adding a new tool to the parity check is
// one line: assertParityForTool(t, dm, ac, "mpm_newtool").
package tools

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// extractSchemaActionEnum returns the action enum declared by the
// tool's JSON-Schema, in declaration order. This is what the
// registry advertises to consumers (MCP server, future --help, etc).
//
// Tools with no action enum (e.g. mpm_resolve, mpm_retrieval_diagnose)
// return an empty slice; assertParityForTool skips them.
func extractSchemaActionEnum(t *testing.T, toolName string) []string {
	t.Helper()
	tool, ok := ByName(toolName)
	if !ok {
		t.Fatalf("registry must contain %q", toolName)
	}

	var schema struct {
		Properties struct {
			Action struct {
				Enum []string `json:"enum"`
			} `json:"action"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("%s schema must unmarshal as JSON: %v", toolName, err)
	}
	return schema.Properties.Action.Enum
}

// extractDispatcherActionList returns the dispatcher's public action
// set by triggering an unknown action and parsing the canonical
// "Valid actions include X, Y, Z" suffix from the returned error.
//
// The error message IS the canonical source of truth for what the
// dispatcher routes — every handleMpm* emits one in its default
// branch. This avoids source-file grep fragility and exercises the
// real dispatcher path (the same one MCP / `mpm call` use).
func extractDispatcherActionList(t *testing.T, toolName string, dm mpminternal.CoreDB, ac mpminternal.ActiveContext) []string {
	t.Helper()
	tool, ok := ByName(toolName)
	if !ok {
		t.Fatalf("registry must contain %q", toolName)
	}

	const probe = "parity-probe-bogus-action-XYZ"
	_, err := tool.Handler(dm, ac, map[string]interface{}{"action": probe})
	if err == nil {
		t.Fatalf("%s must reject unknown action %q with an error", toolName, probe)
	}
	msg := err.Error()
	const marker = "Valid actions include "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		t.Fatalf("%s error must contain %q (got: %s)", toolName, marker, msg)
	}
	rest := msg[idx+len(marker):]
	// Trim a trailing quote (handlers use %q on the offending action)
	// and any trailing punctuation beyond the list.
	if i := strings.IndexAny(rest, `"`); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.Split(rest, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// assertParityForTool runs both extractors and asserts they return
// the same set of public actions. This is the bidirectional parity
// invariant the brief calls for:
//
//   registry → dispatcher
//     every advertised action is accepted (covered transitively by
//     the runtime parity: an advertised action rejected at dispatch
//     would surface in the dispatcher's "unknown action" path and
//     cause the two sets to diverge).
//
//   dispatcher → registry
//     every public action is advertised (this is what the drift
//     class missed — schema enum shorter than dispatcher case list).
//
// Tools whose action contract is NOT represented by an enum (e.g.
// mpm_resolve takes pointer URIs, not action verbs) have an empty
// schema enum and are skipped: an empty enum means "no advertised
// action surface", which the dispatcher honours by not enumerating
// cases either.
func assertParityForTool(t *testing.T, toolName string, dm mpminternal.CoreDB, ac mpminternal.ActiveContext) {
	t.Helper()
	schema := extractSchemaActionEnum(t, toolName)
	dispatcher := extractDispatcherActionList(t, toolName, dm, ac)

	if len(schema) == 0 {
		// Tool without an enum contract — skip; the test framework
		// catches tools that DO have an enum but don't honour it.
		t.Skipf("%s has no action enum (design choice — parity not applicable)", toolName)
	}

	schemaSorted := append([]string(nil), schema...)
	dispatcherSorted := append([]string(nil), dispatcher...)
	sort.Strings(schemaSorted)
	sort.Strings(dispatcherSorted)

	if !stringSlicesEqual(schemaSorted, dispatcherSorted) {
		t.Errorf(
			"%s registry/dispatcher parity violated (bidirectional):\n"+
				"  schema enum advertises:   %v\n"+
				"  dispatcher case list:     %v\n"+
				"  in schema only:           %v\n"+
				"  in dispatcher only:       %v",
			toolName,
			schema, dispatcher,
			setDifference(schema, dispatcher),
			setDifference(dispatcher, schema),
		)
	}
}

// stringSlicesEqual is set-equality on already-sorted string slices.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// setDifference returns elements in `a` that are not in `b`.
func setDifference(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, s := range b {
		inB[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := inB[s]; !ok {
			out = append(out, s)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// TestParity_MpmTheories_RegistryMatchesDispatcher locks D-006:
// mpm_theories schema enum must equal the dispatcher's case list.
// Pre-fix the schema advertised {propose, resolve} but the dispatcher
// also routes {show, list, query} (added in alpha-4 audit D-006 to
// give the agent runtime parity with the CLI's `mpm theories`).
// Schema was never widened; consumers introspecting the registry saw
// only 2 of 5 actions.
func TestParity_MpmTheories_RegistryMatchesDispatcher(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}
	assertParityForTool(t, "mpm_theories", dm, ac)
}

// TestParity_MpmTopics_RegistryMatchesDispatcher locks the D-4.1
// follow-on: mpm_topics schema enum must equal the dispatcher's case
// list. Pre-fix the schema advertised {create, search, link} but the
// dispatcher also routes {list, show} (added in alpha-5 audit D-4.1
// to give the agent runtime parity with the CLI's `mpm topic list`
// and `mpm topic show`). Schema was never widened.
func TestParity_MpmTopics_RegistryMatchesDispatcher(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}
	assertParityForTool(t, "mpm_topics", dm, ac)
}

// TestParity_AllActionTools_LockEverySurface — 2026-09-05 audit
// remediation pass 2 (C.19 / P3). Closes the parity coverage gap
// for every remaining public tool with an action enum.
//
// Pre-fix (alpha-4 era) only mpm_theories and mpm_topics were
// locked via the per-tool tests above. The audit identified 13
// others — without this broad lock, future schema drift (schema enum
// shorter than dispatcher case list) is not caught at `make test`
// time. The exact class of drift surfaced in alpha-4 D-006
// (mpm_theories missing show/list/query) and alpha-5 D-4.1
// (mpm_topics missing list/show).
//
// assertParityForTool skips tools without an enum (e.g. mpm_resolve,
// mpm_retrieval_diagnose, mpm_blob_read, mpm_blob_search,
// log_to_changelog, request_review) so the iteration is safe for
// the mixed enum/non-enum registry.
func TestParity_AllActionTools_LockEverySurface(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	toolsWithActionEnums := []string{
		"mpm_memory",
		"mpm_lessons",
		"mpm_decisions",
		"mpm_theories",  // already locked above; iterated for completeness
		"mpm_skills",
		"mpm_topics",    // already locked above; iterated for completeness
		"mpm_references",
		"mpm_evidence",
		"mpm_confidence",
		"mpm_context",
		"mpm_wakes",
		"mpm_handoff",
		"mpm_scratchpad",
		"mpm_system",
		"mpm_work",
	}
	for _, name := range toolsWithActionEnums {
		assertParityForTool(t, name, dm, ac)
	}
}
