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
	"sort"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// extractSchemaActionEnum returns the action enum declared by the
// tool's JSON-Schema, in declaration order. This is what the
// registry advertises to consumers (MCP server, future --help, etc).
//
// A tool with no action enum returns an empty slice. That is a
// legitimate registry state, not an error — the caller decides whether
// an empty enum makes the tool in scope for parity. The pre-2026-09-29
// version answered that question with t.Skipf inside the caller's loop,
// which aborted the entire sweep; see assertParityForTool.
func extractSchemaActionEnum(t *testing.T, toolName string) []string {
	t.Helper()
	enum, _, err := actionEnumOf(toolName)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return enum
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
// Scope: this function is STRICT. It is called for a specific named
// tool, so a missing action enum is drift in that tool's contract, not
// an inapplicable test, and is reported as a failure.
//
// The pre-2026-09-29 version called t.Skipf here, and its only caller
// other than the two per-tool tests was a LOOP. A t.Skipf inside a loop
// aborts the whole test function on the first enum-less tool, so the
// entire sweep reported as skipped and checked nothing past that
// point — while still reading as a passing test. In-scope filtering
// now happens in the sweep, before this function is called, so it can
// never abort a sweep.
func assertParityForTool(t *testing.T, toolName string, dm mpminternal.CoreDB, ac mpminternal.ActiveContext) {
	t.Helper()

	schema, declaresEnum, err := actionEnumOf(toolName)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !declaresEnum {
		t.Errorf("%s declares no action enum, so this parity assertion is being asked to lock a "+
			"contract the tool does not have. Either the tool's action surface is encoded some other "+
			"way (in which case extend assertParityForTool to read it), or the enum was lost and "+
			"the registry now under-advertises what the dispatcher routes", toolName)
		return
	}

	dispatcher := extractDispatcherActionList(t, toolName, dm, ac)

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
// Population: every registered tool whose schema declares an action
// enum, discovered from the registry itself (toolsDeclaringActionEnum).
//
// The pre-2026-09-29 version iterated a hand-written list of 15 names
// and called assertParityForTool, which called t.Skipf for any tool
// without an enum. That combination is a guard that cannot report
// anything: t.Skipf inside a loop aborts the entire test function, so
// one enum-less tool in the middle of the list would have silenced
// every tool after it while the suite stayed green. Two changes close
// that:
//
//   1. The population is derived from the registry, not restated in
//      this file. The hardcoded list happened to match the registry
//      exactly on the day it was written, but a tool added later with
//      an action enum was never added to the list and so was never
//      checked — the guard silently fell behind the thing it guards.
//   2. Out-of-scope tools are filtered BEFORE the loop body runs, so
//      no per-tool call can skip the sweep. checkParityPopulation
//      then requires a non-zero population, so a discovery failure
//      fails the test rather than yielding a vacuous pass.
func TestParity_AllActionTools_LockEverySurface(t *testing.T) {
	dm := newTestDMForTools(t)
	ac := mpminternal.ActiveContext{}

	inScope, allTools, err := toolsDeclaringActionEnum()
	if err != nil {
		t.Fatalf("cannot establish the parity sweep population: %v", err)
	}
	if err := checkParityPopulation("registry/dispatcher parity sweep", len(inScope), allTools); err != nil {
		t.Fatalf("%v", err)
	}

	for _, name := range inScope {
		// NOTE: deliberately no t.Skip in this loop. Out-of-scope tools
		// were removed by the filter above; a t.Skip here would abort
		// the sweep for every remaining tool.
		assertParityForTool(t, name, dm, ac)
	}

	t.Logf("parity locked on %d of %d registered tools (%d advertise no action enum and are out of scope: %v)",
		len(inScope), len(allTools), len(allTools)-len(inScope), outOfScopeTools(allTools, inScope))
}

// outOfScopeTools reports which registered tools the parity sweep did
// not check, so the log line above names them rather than leaving the
// coverage gap implicit.
func outOfScopeTools(all, inScope []string) []string {
	in := make(map[string]bool, len(inScope))
	for _, n := range inScope {
		in[n] = true
	}
	var out []string
	for _, n := range all {
		if !in[n] {
			out = append(out, n)
		}
	}
	return out
}
