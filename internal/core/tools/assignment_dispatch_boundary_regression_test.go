// assignment_dispatch_boundary_regression_test.go — pin the authority
// boundary of the DISPATCH surface.
//
// # The boundary this file pins
//
// Continuity 001 PR 4 adds an autonomous dispatcher: a component that
// CLAIMS work and starts a contained runtime process on its own
// initiative. It spends real model budget and it writes to the
// assignment ledger.
//
// That is an OPERATOR capability, for the same reason creating a root
// is: the dispatcher decides what runs, and an agent that could reach it
// could start inference on its own.
//
// # The three properties
//
//   - NO registry action advertises a dispatch action;
//   - NO payload field unlocks one;
//   - the operator's own verbs still work, so the capability is
//     SEPARATED rather than deleted.
//
// # The third property is the one that is easy to get wrong
//
// A boundary that only has a "no" is a deletion. The operator must
// still be able to run one dispatch pass, through their own shell, and
// the agent-facing surface must remain able to do the work it is
// supposed to do.
//
// The operator's path is proved in cmd/mpm
// (assignment_dispatch_cmds_test.go), which drives the CLI end to end.
package tools

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// dispatchOnlyActions are the operations the agent-facing surface must
// not be able to reach, under ANY action name.
var dispatchOnlyActions = []string{
	"dispatch",
	"dispatch-status",
	"dispatch_status",
	"dispatchstatus",
	"run-dispatcher",
	"auto-dispatch",
	"autodispatch",
}

// dispatchOnlyPayloads are the payload shapes an agent might use to try
// to start a dispatch. Every one of them must fail closed.
var dispatchOnlyPayloads = []struct {
	name  string
	extra map[string]interface{}
}{
	{"bare", map[string]interface{}{}},
	{"enabled_true", map[string]interface{}{"enabled": true}},
	{"force_true", map[string]interface{}{"force": true}},
	{"operator_true", map[string]interface{}{"operator": true}},
	{"authority_true", map[string]interface{}{"authority": true}},
	{"auto_true", map[string]interface{}{"auto": true}},
	{"confirm_true", map[string]interface{}{"confirm": true}},
}

// TestAssignment_NoRegistryActionDispatches proves that no action of
// the agent-facing tool can start a dispatch.
//
// The check is over the WHOLE registry, not just the assignment tool: a
// dispatch could be started by any other tool's action, and the
// boundary has to hold wherever the surface is.
func TestAssignment_NoRegistryActionDispatches(t *testing.T) {
	for _, tool := range Registry {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			for _, action := range dispatchOnlyActions {
				_, err := tool.Handler(newTestDMForTools(t), mpminternal.ActiveContext{},
					map[string]interface{}{
						"action": action,
						"params": map[string]interface{}{
							"id":       "at-whatever",
							"agent":    "mpm-worker",
							"owner":    "mpm-dispatcher",
							"lease":    900,
							"sequence": 1,
						},
					})
				if err == nil {
					t.Errorf("the %s tool accepted the operator-only action %q; "+
						"starting a dispatch must require the operator's own verb",
						tool.Name, action)
				}
			}
		})
	}
}

// TestAssignment_NoPayloadFieldUnlocksADispatch proves the refusal is
// not a flag in disguise.
//
// A boolean the agent itself sets cannot distinguish "the operator asked
// for this" from "the agent decided this", so no payload field may
// unlock the dispatch.
func TestAssignment_NoPayloadFieldUnlocksADispatch(t *testing.T) {
	for _, tc := range dispatchOnlyPayloads {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for _, action := range dispatchOnlyActions {
				params := map[string]interface{}{
					"id":    "at-whatever",
					"agent": "mpm-worker",
					"owner": "mpm-dispatcher",
				}
				for k, v := range tc.extra {
					params[k] = v
				}
				_, err := handleMpmAssignment(newTestDMForTools(t), mpminternal.ActiveContext{},
					map[string]interface{}{
						"action": action,
						"params": params,
					})
				if err == nil {
					t.Errorf("the assignment tool accepted %q with %v; "+
						"no payload field may unlock the dispatcher",
						action, tc.extra)
				}
			}
		})
	}
}

// TestAssignment_DispatchActionsAreNotAdvertised proves discovery
// honesty.
//
// An action that is always refused must not appear in the advertised
// vocabulary, because a strict MCP host validates the action against
// the schema and would offer an action that cannot work.
func TestAssignment_DispatchActionsAreNotAdvertised(t *testing.T) {
	enum := schemaEnum(t, "mpm_assignment")
	if enum == nil {
		t.Fatal("mpm_assignment has no action enum; cannot verify the advertised vocabulary")
	}
	for _, action := range enum {
		for _, forbidden := range dispatchOnlyActions {
			if action == forbidden {
				t.Errorf("mpm_assignment advertises the operator-only action %q", action)
			}
		}
	}
}

// TestAssignment_NoToolSchemaMentionsTheDispatchActions proves the
// advertised vocabulary in the WHOLE registry is clean of the dispatch
// names — not just the assignment tool's.
func TestAssignment_NoToolSchemaMentionsTheDispatchActions(t *testing.T) {
	for _, tool := range Registry {
		tool := tool
		t.Run(tool.Name, func(t *testing.T) {
			enum := schemaEnum(t, tool.Name)
			if enum == nil {
				return
			}
			for _, action := range enum {
				for _, forbidden := range dispatchOnlyActions {
					if action == forbidden {
						t.Errorf("%s advertises the operator-only action %q",
							tool.Name, action)
					}
				}
			}
		})
	}
}
