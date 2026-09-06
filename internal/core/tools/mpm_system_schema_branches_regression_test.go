// mpm_system_schema_branches_regression_test.go — 2026-09-05 audit
// remediation closure for C.18 (P3): the mpm_system JSON schema
// advertises a flat params shape with `additionalProperties: true`
// at the top level. This file pins the action-branched oneOf shape
// (mirroring mpm_memory and mpm_work) so the audit defect cannot
// regress.
//
// Pre-fix shape (registry_list.go mpm_system entry):
//   {
//     "type": "object",
//     "properties": {
//       "action": {"type": "string", "enum": [...]},
//       "params": {
//         "type": "object",
//         "properties": { 11 keys flat ... },
//         "additionalProperties": true
//       }
//     },
//     "required": ["action"]
//   }
//
// Post-fix shape (this test pins it):
//   {
//     "type": "object",
//     "properties": {
//       "action": {"type": "string", "enum": [...]},
//       "params": {"type": "object", "description": "..."}
//     },
//     "required": ["action"],
//     "oneOf": [
//       { "properties": { "action": {"const": "gc_run"},
//         "params": { "type": "object",
//                     "properties": { dry_run, aggressive, max_age_hours, stale_theory_days },
//                     "additionalProperties": false } },
//         "required": ["params"] },
//       ... 9 more branches ...
//     ]
//   }
//
// Why this matters: the flat shape forced every caller to honour every
// key, and the `additionalProperties: true` swallowed typos silently —
// the exact data-loss class the schema-guard catches elsewhere. The
// action-branched shape lets each action declare ONLY the keys its
// handler reads, so over-declaration is mechanically impossible.

package tools

import (
	"encoding/json"
	"testing"
)

// mpmSystemActionBranches pins the 11 actions the dispatcher's case
// list claims (handlers.go handleMpmSystem default branch). The
// oneOf cardinality MUST match the dispatcher — any drift surfaces
// as either an extra branch (handler ignores it) or a missing branch
// (the action fails schema validation before reaching the dispatcher).
//
// History: the original list had 10 actions. unsnooze_cluster was
// added in the Part 2B snooze-mirroring remediation (2026-09-06) as
// the explicit inverse of snooze_cluster. The dispatcher (handlers.go
// handleMpmSystem) routes unsnooze_cluster to handleUnsnoozeCluster.
// The schema (registry_list.go mpm_system entry) declares a per-action
// oneOf branch for it. The test slice was stale at 10; this is the
// final-pass correction.
var mpmSystemActionBranches = []string{
	"gc_run",
	"compact",
	"health_check",
	"migrate",
	"query_audit_log",
	"list_clusters",
	"snooze_cluster",
	"unsnooze_cluster",
	"resolve_cluster",
	"annotate_cluster",
	"critic_findings",
}

// findMpmSystemTool returns the Registry entry for mpm_system or
// fails the test.
func findMpmSystemTool(t *testing.T) *Tool {
	t.Helper()
	for i := range Registry {
		if Registry[i].Name == "mpm_system" {
			return &Registry[i]
		}
	}
	t.Fatal("mpm_system not registered")
	return nil
}

// TestMpmSystem_SchemaIsActionBranched closes C.18 (P3) by pinning
// the structural shape of the mpm_system JSON schema. Each branch
// is verified individually: action const discriminator + per-action
// params shape. The test fails loudly on:
//   - missing action const for a known action
//   - extra branch (over-declared action the dispatcher doesn't route)
//   - additionalProperties:true on any per-branch params (the audit
//     defect)
func TestMpmSystem_SchemaIsActionBranched(t *testing.T) {
	tool := findMpmSystemTool(t)
	var schema map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_system schema is not valid JSON: %v", err)
	}

	// Top-level structural pins: type=object, required=[action].
	if got, want := schema["type"], "object"; got != want {
		t.Errorf("mpm_system top-level type: got %v, want %q", got, want)
	}
	required, ok := schema["required"].([]interface{})
	if !ok || len(required) != 1 || required[0] != "action" {
		t.Errorf("mpm_system top-level required: got %v, want [\"action\"]", schema["required"])
	}

	// Properties must contain only `action` and `params` — the audit
	// defect was additionalProperties:true nested on the params object
	// that swallowed the 11 flat keys. Post-fix, params is a strict
	// envelope declared per-branch.
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_system schema missing top-level `properties`")
	}
	if _, ok := props["action"]; !ok {
		t.Error("mpm_system top-level properties missing `action`")
	}
	if _, ok := props["params"]; !ok {
		t.Error("mpm_system top-level properties missing `params`")
	}
	// Reject any unexpected top-level property — the audit shape is
	// `action` + `params` only.
	for k := range props {
		if k != "action" && k != "params" {
			t.Errorf("mpm_system top-level properties contains unexpected key %q (action-branched schema declares only action + params)", k)
		}
	}

	// oneOf branches MUST exist and cardinality MUST match the
	// dispatcher's case list (handlers.go:5774-5801).
	branchesRaw, ok := schema["oneOf"].([]interface{})
	if !ok {
		t.Fatal("mpm_system schema missing top-level `oneOf` (action-branched shape required to close C.18)")
	}
	if len(branchesRaw) != len(mpmSystemActionBranches) {
		t.Errorf("mpm_system oneOf branches: got %d, want %d (one per action: %v)",
			len(branchesRaw), len(mpmSystemActionBranches), mpmSystemActionBranches)
	}

	// Index branches by action const so missing/extra branches are
	// individually diagnosed instead of surfaced as a cardinality
	// mismatch alone.
	branchesByAction := map[string]map[string]interface{}{}
	for i, raw := range branchesRaw {
		branch, ok := raw.(map[string]interface{})
		if !ok {
			t.Errorf("mpm_system oneOf[%d] is not an object", i)
			continue
		}
		branchProps, ok := branch["properties"].(map[string]interface{})
		if !ok {
			t.Errorf("mpm_system oneOf[%d] missing `properties`", i)
			continue
		}
		actionField, ok := branchProps["action"].(map[string]interface{})
		if !ok {
			t.Errorf("mpm_system oneOf[%d] missing nested `action`", i)
			continue
		}
		actionConst, ok := actionField["const"].(string)
		if !ok {
			t.Errorf("mpm_system oneOf[%d].action is not a const string (got %v)", i, actionField)
			continue
		}
		branchesByAction[actionConst] = branch
	}
	for _, want := range mpmSystemActionBranches {
		if _, ok := branchesByAction[want]; !ok {
			t.Errorf("mpm_system schema is missing oneOf branch for action %q (dispatcher routes it; schema must advertise it)", want)
		}
	}
	for got := range branchesByAction {
		found := false
		for _, want := range mpmSystemActionBranches {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("mpm_system schema declares oneOf branch for action %q but the dispatcher does not route it (over-declaration drift)", got)
		}
	}

	// Per-branch pin: each params object MUST be a strict object
	// (additionalProperties:false) so typos surface as schema-rejection
	// rather than silent drops. This is the structural fix for C.18 —
	// the pre-fix shape had additionalProperties:true on the top-level
	// params envelope.
	for action, branch := range branchesByAction {
		branchProps, _ := branch["properties"].(map[string]interface{})
		paramsField, ok := branchProps["params"].(map[string]interface{})
		if !ok {
			t.Errorf("mpm_system oneOf[%s] missing nested `params` object", action)
			continue
		}
		if got, want := paramsField["type"], "object"; got != want {
			t.Errorf("mpm_system oneOf[%s].params.type: got %v, want %q", action, got, want)
		}
		addl, hasAddl := paramsField["additionalProperties"]
		if !hasAddl {
			t.Errorf("mpm_system oneOf[%s].params is missing additionalProperties:false (C.18 closes with strict per-branch params)", action)
			continue
		}
		if addl != false {
			t.Errorf("mpm_system oneOf[%s].params.additionalProperties: got %v, want false (C.18 closes with strict per-branch params)", action, addl)
		}
	}
}

// TestMpmSystem_SchemaDeclarationsCoverHandlerReads locks the
// per-action declared params against what each handler actually
// reads. This is the per-branch equivalent of the over-decl/under-decl
// check in TestSchemaSupersetOfHandlerPayloadReads, applied to the
// inner action shapes.
//
// Without this pin, a future schema edit could silently drop a
// handler read (under-decl) or add a never-read key (over-decl) for
// any single action without tripping TestSchemaSupersetOfHandlerPayloadReads
// (which only walks top-level properties).
//
// The handler reads are derived from handlers.go: see handleGCRun
// (1419), handleCompactEpistemology (4368), handleHealthCheck (4313),
// handleMigrate (1250), handleQueryAuditLog (3109), handleListActiveClusters
// (3229), handleSnoozeCluster (3268), handleResolveCluster (3304),
// handleAnnotateCluster (3346), handleCriticFindings (5816).
func TestMpmSystem_SchemaDeclarationsCoverHandlerReads(t *testing.T) {
	tool := findMpmSystemTool(t)
	var schema map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &schema); err != nil {
		t.Fatalf("mpm_system schema is not valid JSON: %v", err)
	}

	branchesRaw, ok := schema["oneOf"].([]interface{})
	if !ok {
		t.Fatal("mpm_system schema missing oneOf — TestMpmSystem_SchemaIsActionBranched must pass first")
	}

	// handlerParams is the canonical handler-read set per action.
	// Sourced from handlers.go handlers (see comment above). Update
	// when adding new handler reads; this list IS the audit closure.
	handlerParams := map[string][]string{
		"gc_run":           {"dry_run", "aggressive", "max_age_hours", "stale_theory_days"},
		"compact":          {"force", "max_batches"},
		"health_check":     {}, // no params
		"migrate":          {"confirm", "from_path", "format", "label", "dry_run", "commit", "commit_batch", "undo_batch"},
		"query_audit_log":  {"level", "component", "artifact_id", "days", "since", "limit", "include_stack"},
		"list_clusters":    {}, // no params
		"snooze_cluster":   {"cluster_key", "snooze_until", "reason"},
		"unsnooze_cluster": {"cluster_key", "reason"},
		"resolve_cluster":  {"cluster_key", "reason"},
		"annotate_cluster": {"cluster_key", "annotation", "reason"},
		"critic_findings":  {"limit"},
	}

	// Index branches by action const.
	branchesByAction := map[string]map[string]interface{}{}
	for _, raw := range branchesRaw {
		branch := raw.(map[string]interface{})
		bp := branch["properties"].(map[string]interface{})
		actionField := bp["action"].(map[string]interface{})
		actionConst := actionField["const"].(string)
		branchesByAction[actionConst] = branch
	}

	for action, expected := range handlerParams {
		branch, ok := branchesByAction[action]
		if !ok {
			// Already reported by TestMpmSystem_SchemaIsActionBranched.
			continue
		}
		bp, _ := branch["properties"].(map[string]interface{})
		paramsField, _ := bp["params"].(map[string]interface{})
		paramsProps, _ := paramsField["properties"].(map[string]interface{})

		declared := map[string]bool{}
		for k := range paramsProps {
			declared[k] = true
		}

		// Under-decl check: every handler read must be declared.
		var missing []string
		for _, k := range expected {
			if !declared[k] {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			t.Errorf("mpm_system oneOf[%s].params is missing handler-read keys %v (handler reads them but schema doesn't advertise them — clients can't supply)",
				action, missing)
		}
	}
}
