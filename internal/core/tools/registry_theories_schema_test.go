// registry_theories_schema_test.go — the published mpm_theories schema
// must accept EXACTLY the payloads the resolve handler accepts.
//
// WHY THIS EXISTS
// ---------------
// `mpm_theories` is the only tool whose published schema puts its
// per-action `oneOf` INSIDE `params` — that is, the branch is chosen
// by the payload's shape rather than by the action.
//
// The other tools that publish per-action required params (`mpm_work`,
// `mpm_handoff`, `mpm_system`) key their `oneOf` on
// `action: {"const": ...}`, so a branch always matches the action the
// caller asked for. `mpm_theories` does not: its two branches are
// distinguished only by the presence of `hypothesis` or `conclusion`,
// so an action with no branch — `show`, `list`, `query` — matches
// NEITHER branch and its payload is schema-invalid.
//
// That made this one tool's schema a statement about required params —
// and the statement drifted. The alpha-4.1.2 D-008/W-006 cleanup moved
// the canonical vocabulary to `id` and `status` (with `theoryId` /
// `newStatus` retained as backward-compat aliases) and updated the
// handler; the published schema was not updated.
//
// Two independent defects followed, in opposite directions:
//
//   - The schema required only `id` and `conclusion`, so a payload the
//     handler REJECTS — no status, no arbitration winner — passed the
//     published schema.
//   - Requiring `id` rejected the compatibility aliases the handler
//     supports, so a documented call form failed validation.
//
// The schema is the agent-facing contract: `mpm_help show mpm_theories`
// returns it, and the MCP server publishes it. A schema that accepts a
// call the handler refuses sends the agent in a circle; a schema that
// refuses a call the handler accepts locks it out.
//
// WHAT THIS PINS
// --------------
// For every RESOLVE payload shape the table below enumerates, the
// schema's verdict and the handler's verdict are the SAME:
//
//   - canonical resolution          id + conclusion + status
//   - legacy resolution             theoryId + conclusion + newStatus
//   - snake-case legacy resolution  theory_id + conclusion + new_status
//   - arbitration without status    id + conclusion + winner_id
//   - arbitration alias             id + conclusion + winnerId
//   - missing outcome               id + conclusion            (rejected)
//   - missing identity              conclusion + status        (rejected)
//   - invalid outcome value         id + conclusion + status=confirmed
//                                                                  (rejected)
//   - invalid outcome value, alias  id + conclusion + newStatus=confirmed
//                                                                  (rejected)
//   - empty conclusion / identity   (rejected)
//   - two outcome keys, one invalid (rejected)
//   - empty canonical id, valid alias                     (accepted)
//   - empty status, valid alias                           (accepted)
//   - status + newStatus supplied, new_status ignored    (accepted)
//
// The handler reads the outcome from `status`, then `new_status`, then
// `newStatus` — the first non-empty one wins — and refuses a payload
// whose second status key disagrees with the first. It never reads
// `new_status` at all when BOTH `status` and `newStatus` are supplied,
// because its second conflict guard is skipped in that case. The
// schema mirrors that exactly: `new_status` is value-checked unless
// `status` and `newStatus` are both present and non-empty.
//
// The schema is validated with a real JSON Schema validator
// (santhosh-tekuri/jsonschema/v6) against real payloads — not by
// inspecting the `required` array, which cannot see an `anyOf`.
//
// The comparison is between the schema's verdict and the handler's
// PARAMETER acceptance. A data failure — a theory that does not exist,
// a shared DB that is not attached — means the parameters were
// ACCEPTED, and is classified as such.
//
// THE DOCUMENTED DIVERGENCE
// -------------------------
// The handler rejects a payload that supplies `status` AND a status
// alias with DIFFERENT values. JSON Schema cannot express "these two
// property values must be equal" — no draft has a keyword for
// comparing two sibling values — so the schema accepts that payload and
// the property description states the rule instead. That case is
// asserted explicitly below, so the limitation is visible rather than
// assumed.
//
// KNOWN PRE-EXISTING DIVERGENCES (outside this schema's scope)
// ----------------------------------------------------------
// Three divergences predate this work and are NOT fixed here; they
// are recorded so this test's claim is not read as universal:
//
//   - the `show`, `list` and `query` actions have no branch in
//     `params.oneOf`, so their payloads are schema-invalid while the
//     handler accepts them;
//   - a non-string value for ANY string-typed resolve param (`id`,
//     `theoryId`, `theory_id`, `status`, `newStatus`, `new_status`,
//     `winner_id`, `winnerId`) is schema-rejected, while the handler's
//     type assertion falls through to the next key - so the handler
//     accepts the payload only if another key supplies the value, and
//     refuses it otherwise;
//   - a resolve payload that also carries `hypothesis` matches BOTH
//     branches of `params.oneOf`, so it is schema-invalid, while the
//     handler dispatches on the action and accepts it.
//
// All three need a decision about the published schema's SHAPE (an
// action-keyed branch selection, and a relaxed value type) and belong
// in their own change.
package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// theoriesSchemaValidator compiles the published mpm_theories schema
// under the compiler's default draft.
func theoriesSchemaValidator(t *testing.T) *jsonschema.Schema {
	t.Helper()
	return theoriesSchemaValidatorDraft(t, nil)
}

// theoriesSchemaValidatorDraft compiles the published mpm_theories
// schema under a named draft. An empty name means the default.
//
// The published schema carries no `$schema` keyword, so a client may
// pick any draft it supports. The constructs this schema uses are
// draft-07 core, and the verdicts must not depend on the draft.
func theoriesSchemaValidatorDraft(t *testing.T, draft *jsonschema.Draft) *jsonschema.Schema {
	t.Helper()
	tool, ok := ByName("mpm_theories")
	require.True(t, ok, "mpm_theories must be registered")
	var doc any
	require.NoError(t, json.Unmarshal(tool.Schema, &doc),
		"the mpm_theories schema must be valid JSON")
	c := jsonschema.NewCompiler()
	if draft != nil {
		c.DefaultDraft(draft)
	}
	require.NoError(t, c.AddResource("mpm_theories.json", doc),
		"the mpm_theories schema must be addable to the compiler")
	sch, err := c.Compile("mpm_theories.json")
	require.NoError(t, err,
		"the mpm_theories schema must compile as a JSON Schema")
	return sch
}

// theoriesProposeUnique proposes a pending theory with a UNIQUE
// hypothesis, so each test case that resolves gets its own theory.
func theoriesProposeUnique(t *testing.T, dm *mpminternal.DatabaseManager, hypothesis string) string {
	t.Helper()
	res, err := dm.ProposeTheory(hypothesis, "validation criteria", nil, nil, []string{"schema-contract"})
	require.NoError(t, err)
	id, _ := res["id"].(string)
	require.NotEmpty(t, id)
	return id
}

// theoriesParamsRejected reports whether the handler rejected the
// PAYLOAD as malformed, as opposed to failing on the DATA.
//
// An out-of-range value produces a TYPED payload-shape error, which is
// classified by type. The handler's other parameter-validation errors
// all begin with "resolve theory:" and carry one of the markers below.
// A data failure — a theory that does not exist, a theory that is
// already resolved, a shared DB that is not attached — is neither, and
// means the parameters were ACCEPTED.
func theoriesParamsRejected(err error) bool {
	if err == nil {
		return false
	}
	// A typed payload-shape error IS a parameter rejection: the
	// handler returns one when a value is out of range (an invalid
	// status). Classifying it by TYPE rather than by message text
	// means a rewording of the error cannot silently flip a case.
	var shape *mpminternal.PayloadShapeError
	if errors.As(err, &shape) {
		return true
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "resolve theory:") {
		return false
	}
	return strings.Contains(msg, "is required") ||
		strings.Contains(msg, "is invalid") ||
		strings.Contains(msg, "conflicting")
}

// theoriesResolveCase is one payload shape, with the verdict each side
// must reach.
type theoriesResolveCase struct {
	name   string
	params map[string]interface{}

	// wantSchemaValid is whether the PUBLISHED SCHEMA accepts the
	// payload.
	wantSchemaValid bool
	// wantAccepted is whether the HANDLER accepts the parameters (it
	// may still fail on the data).
	wantAccepted bool
	// divergence, when set, records a payload the two sides judge
	// differently and why.
	divergence string
}

// theoriesResolveCases builds the case table. It takes the ids of
// theories that must be genuinely resolved, so each of them gets its
// OWN theory — a theory cannot be resolved twice.
func theoriesResolveCases(t *testing.T, dm *mpminternal.DatabaseManager) []theoriesResolveCase {
	t.Helper()
	proven := theoriesProposeUnique(t, dm, "canonical resolution")
	disproven := theoriesProposeUnique(t, dm, "canonical resolution disproven")
	camel := theoriesProposeUnique(t, dm, "legacy camel resolution")
	snake := theoriesProposeUnique(t, dm, "legacy snake resolution")
	arbitration := theoriesProposeUnique(t, dm, "arbitration resolution")
	arbitrationAlias := theoriesProposeUnique(t, dm, "arbitration alias resolution")

	return []theoriesResolveCase{
		{
			name:   "canonical resolution",
			params: map[string]interface{}{"id": proven, "conclusion": "Evidence", "status": "proven"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			name:   "canonical resolution, disproven",
			params: map[string]interface{}{"id": disproven, "conclusion": "Evidence", "status": "disproven"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			name:   "legacy resolution, camel alias",
			params: map[string]interface{}{"theoryId": camel, "conclusion": "Evidence", "newStatus": "proven"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			name:   "legacy resolution, snake alias",
			params: map[string]interface{}{"theory_id": snake, "conclusion": "Evidence", "new_status": "proven"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			// The arbitration path returns BEFORE the handler reads
			// `status`, so no status is required — and a status
			// supplied alongside is ignored, exactly as the handler
			// ignores it.
			name:   "arbitration without status, canonical winner",
			params: map[string]interface{}{"id": arbitration, "conclusion": "Evidence", "winner_id": "W-1"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			name:   "arbitration without status, alias winner",
			params: map[string]interface{}{"id": arbitrationAlias, "conclusion": "Evidence", "winnerId": "W-2"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			// The arbitration path ignores the status, so an
			// out-of-range one is accepted — by BOTH sides.
			name:   "arbitration ignores an out-of-range status",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "confirmed", "winner_id": "W-3"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			name:   "missing outcome",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "missing identity",
			params: map[string]interface{}{"conclusion": "Evidence", "status": "proven"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "invalid outcome value",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "confirmed"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "invalid outcome value, alias",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "newStatus": "confirmed"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			// The handler reads `status` FIRST, so an out-of-range
			// `status` wins over a valid alias — and the schema must
			// reject it too, even though the alias would satisfy the
			// outcome requirement on its own.
			name:   "two outcome keys, the canonical one invalid",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "confirmed", "newStatus": "proven"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "two outcome keys, the alias invalid",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "proven", "newStatus": "confirmed"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "two outcome aliases, one invalid",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "newStatus": "bogus", "new_status": "proven"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			// An empty status reads as absent — the handler falls
			// through to the alias.
			name:   "empty status, valid alias",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "", "newStatus": "proven"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			// An empty canonical id reads as absent — the handler
			// falls through to the alias.
			name:   "empty canonical id, valid alias",
			params: map[string]interface{}{"id": "", "theoryId": "T", "conclusion": "Evidence", "status": "proven"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			// The handler never reads `new_status` when BOTH
			// `status` and `newStatus` are supplied: its second
			// conflict guard is skipped, so the value is not
			// consulted at all. The schema mirrors that.
			name:   "status and newStatus supplied, new_status ignored",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "proven", "newStatus": "proven", "new_status": "bogus"},
			wantSchemaValid: true,
			wantAccepted:    true,
		},
		{
			// The same payload WITHOUT the third key: `new_status`
			// is the effective outcome (the handler reads
			// status, then new_status, then newStatus), so it is
			// value-checked.
			name:   "new_status is the effective outcome, and it is invalid",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "new_status": "bogus"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "empty conclusion",
			params: map[string]interface{}{"id": "T", "conclusion": "", "status": "proven"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "empty identity",
			params: map[string]interface{}{"id": "", "conclusion": "Evidence", "status": "proven"},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			// An empty winner reads as absent, so the status IS
			// required and IS validated.
			name:   "empty winner, invalid status",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "confirmed", "winner_id": ""},
			wantSchemaValid: false,
			wantAccepted:    false,
		},
		{
			name:   "conflicting outcome and alias",
			params: map[string]interface{}{"id": "T", "conclusion": "Evidence", "status": "proven", "newStatus": "disproven"},
			wantSchemaValid: true,
			wantAccepted:    false,
			divergence: "JSON Schema cannot compare two sibling property values, so the " +
				"schema accepts a payload whose status and status alias disagree; the " +
				"property description states the rule and the handler enforces it",
		},
	}
}

// theoriesCheckCase runs one case against one compiled schema and
// returns the schema's and the handler's verdicts.
func theoriesCheckCase(t *testing.T, dm *mpminternal.DatabaseManager, sch *jsonschema.Schema, tc theoriesResolveCase) (bool, bool) {
	t.Helper()

	payload := map[string]interface{}{
		"action": "resolve",
		"params": tc.params,
	}
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	var instance any
	require.NoError(t, json.Unmarshal(encoded, &instance))
	schemaErr := sch.Validate(instance)
	schemaValid := schemaErr == nil

	_, handlerErr := handleResolveTheory(dm, mpminternal.ActiveContext{}, tc.params)
	handlerAccepted := !theoriesParamsRejected(handlerErr)

	return schemaValid, handlerAccepted
}

// TestTheoriesResolveSchemaAgreesWithTheHandler is the contract: the
// published schema and the handler judge every resolve payload the
// same way.
func TestTheoriesResolveSchemaAgreesWithTheHandler(t *testing.T) {
	sch := theoriesSchemaValidator(t)
	dm := f3_3NewDM(t)

	for _, tc := range theoriesResolveCases(t, dm) {
		t.Run(tc.name, func(t *testing.T) {
			schemaValid, handlerAccepted := theoriesCheckCase(t, dm, sch, tc)

			if schemaValid != tc.wantSchemaValid {
				t.Errorf("the published schema %s the payload, but the test expects it to %s\n  payload: %v",
					map[bool]string{true: "accepts", false: "rejects"}[schemaValid],
					map[bool]string{true: "accept", false: "reject"}[tc.wantSchemaValid],
					tc.params)
			}
			if handlerAccepted != tc.wantAccepted {
				t.Errorf("the handler %s the payload, but the test expects it to %s\n  payload: %v",
					map[bool]string{true: "accepts", false: "rejects"}[handlerAccepted],
					map[bool]string{true: "accept", false: "reject"}[tc.wantAccepted],
					tc.params)
			}
			if tc.divergence == "" && schemaValid != handlerAccepted {
				t.Errorf("the published schema and the handler disagree about the payload\n  payload: %v\n  schema valid: %v\n  handler accepted: %v",
					tc.params, schemaValid, handlerAccepted)
			}
			if tc.divergence != "" && schemaValid == handlerAccepted {
				t.Errorf("this case is documented as a divergence but the two sides agreed\n  payload: %v", tc.params)
			}
		})
	}
}

// TestTheoriesResolveSchemaAgreesWithTheHandlerOverTheWholeStatusSpace
// sweeps the ENTIRE status-key space — every combination of present /
// absent / empty / valid / invalid across `status`, `newStatus` and
// `new_status`, with and without an arbitration winner under EITHER
// winner key — and asserts that the published schema and the handler
// agree on every payload.
//
// The point of a sweep rather than a table of hand-picked payloads: a
// hand-picked table only proves what somebody thought to write down.
// The regression this test exists for — an enum constraint that stopped
// applying as soon as a second status key was present — hid in a
// payload shape no table had.
//
// The ONE permitted divergence is the conflict class: the handler
// refuses two status keys that disagree, and no JSON Schema draft can
// compare two sibling values. That class is recognised by the
// handler's own message and is asserted here rather than tolerated
// silently — a divergence that is NOT a conflict fails the test.
func TestTheoriesResolveSchemaAgreesWithTheHandlerOverTheWholeStatusSpace(t *testing.T) {
	sch := theoriesSchemaValidator(t)
	dm := f3_3NewDM(t)

	values := []struct {
		name string
		v    interface{}
	}{
		{"absent", nil},
		{"empty", ""},
		{"proven", "proven"},
		{"disproven", "disproven"},
		{"invalid", "confirmed"},
	}
	// Every winner STATE is swept: neither key, the canonical key, the
	// alias, and both together. The schema treats the two keys
	// symmetrically, but the handler reads `winner_id` first and falls
	// back to `winnerId`, so each state must reach the arbitration
	// path — and each state is a distinct payload, so no payload is
	// evaluated twice.
	winners := []struct {
		name string
		keys []string
	}{
		{"no winner", nil},
		{"canonical winner", []string{"winner_id"}},
		{"alias winner", []string{"winnerId"}},
		{"both winner keys", []string{"winner_id", "winnerId"}},
	}

	checked := 0
	// Distinct payloads, not iterations: two different rows of the
	// winner dimension can build the SAME payload (a row that sets no
	// key is the same as any other such row), and a sweep that
	// evaluates the same payload twice proves less than its iteration
	// count suggests.
	distinct := map[string]bool{}
	for _, st := range values {
		for _, ns := range values {
			for _, nsn := range values {
				for _, w := range winners {
					params := map[string]interface{}{"id": "T-1", "conclusion": "Evidence"}
					if st.v != nil {
						params["status"] = st.v
					}
					if ns.v != nil {
						params["newStatus"] = ns.v
					}
					if nsn.v != nil {
						params["new_status"] = nsn.v
					}
					for _, key := range w.keys {
						params[key] = "W-1"
					}

					payload := map[string]interface{}{"action": "resolve", "params": params}
					encoded, err := json.Marshal(payload)
					require.NoError(t, err)
					var instance any
					require.NoError(t, json.Unmarshal(encoded, &instance))
					schemaValid := sch.Validate(instance) == nil

					_, handlerErr := handleResolveTheory(dm, mpminternal.ActiveContext{}, params)
					handlerAccepted := !theoriesParamsRejected(handlerErr)

					checked++
					distinct[string(encoded)] = true
					if schemaValid == handlerAccepted {
						continue
					}
					// The conflict class: the handler refuses two
					// status keys that disagree. The schema cannot
					// compare two sibling values, so it accepts.
					if handlerErr != nil && strings.Contains(handlerErr.Error(), "conflicting") {
						continue
					}
					t.Errorf("the published schema and the handler disagree, and it is NOT the conflict class\n  payload: %s\n  schema valid: %v\n  handler accepted: %v (%v)",
						encoded, schemaValid, handlerAccepted, handlerErr)
				}
			}
		}
	}
	require.Equal(t, 500, checked,
		"the sweep must cover every status-key combination and every "+
			"winner state")
	require.Equal(t, 500, len(distinct),
		"the sweep must evaluate 500 DISTINCT payloads; it evaluated "+
			"%d distinct payloads out of %d iterations, so two winner "+
			"states are building the same payload", len(distinct), checked)
}

// TestTheoriesResolveSchemaProseMatchesTheImplementation pins that the
// published schema's own PROSE says what the schema implements.
//
// The schema's descriptions are what an agent reads at the point of
// call — `mpm_help show mpm_theories` returns them, and the MCP server
// publishes them. A description that says "exactly one" while the
// implementation accepts several is the same class of defect as a
// `required` array that disagrees with the handler: the agent is told
// a rule the code does not enforce.
//
// The implementation is `anyOf` — AT LEAST one of the identity fields,
// and AT LEAST one of the outcomes — so the prose must say so.
func TestTheoriesResolveSchemaProseMatchesTheImplementation(t *testing.T) {
	tool, ok := ByName("mpm_theories")
	require.True(t, ok, "mpm_theories must be registered")
	var schema map[string]interface{}
	require.NoError(t, json.Unmarshal(tool.Schema, &schema))

	params, ok := schema["properties"].(map[string]interface{})["params"].(map[string]interface{})
	require.True(t, ok, "mpm_theories schema has no params object")
	branches, ok := params["oneOf"].([]interface{})
	require.True(t, ok && len(branches) > 0,
		"mpm_theories params schema has no oneOf branches")

	var resolve map[string]interface{}
	for _, b := range branches {
		branch, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		bp, ok := branch["properties"].(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := bp["conclusion"]; has {
			resolve = branch
		}
	}
	require.NotNil(t, resolve, "mpm_theories schema has no resolve branch")

	props, ok := resolve["properties"].(map[string]interface{})
	require.True(t, ok, "the resolve branch has no properties")

	for field, want := range map[string]string{
		"id":     "At least one identity field is required",
		"status": "At least one outcome is required",
	} {
		p, ok := props[field].(map[string]interface{})
		require.True(t, ok, "the resolve branch has no %q property", field)
		desc, _ := p["description"].(string)
		require.Contains(t, desc, want,
			"the published mpm_theories schema's %q description does not say %q. "+
				"The schema implements `anyOf` - at least one - so the prose an "+
				"agent reads must say the same thing.", field, want)
	}

	// The description must also name the ONE corner in which the
	// handler does not read `new_status` at all, so an agent that
	// reads it is not told a rule the code does not enforce. This
	// sentence is the reason the schema's `new_status` constraint is
	// suspended on that path; without it, the schema and its prose
	// could drift apart again.
	statusDesc, _ := props["status"].(map[string]interface{})["description"].(string)
	require.Contains(t, statusDesc,
		"it never reads new_status when both status and newStatus are supplied",
		"the published mpm_theories schema's \"status\" description does not "+
			"name the corner in which the handler ignores new_status. The "+
			"schema suspends the new_status value constraint on exactly that "+
			"path, so the prose an agent reads must say so.")

	// And nothing may still claim "exactly one", which the
	// implementation has never enforced. The check is
	// case-insensitive: a lowercase "exactly one" says the same thing.
	for field := range props {
		p, ok := props[field].(map[string]interface{})
		if !ok {
			continue
		}
		desc, _ := p["description"].(string)
		require.NotContains(t, strings.ToLower(desc), "exactly one",
			"the published mpm_theories schema's %q description says "+
				"\"exactly one\"; the schema implements `anyOf` - at least one", field)
	}
}

// TestTheoriesResolveSchemaVerdictsDoNotDependOnTheDraft pins that the
// published schema means the same thing under every draft a client may
// pick. The schema carries no `$schema` keyword, so a client chooses
// the draft — and a verdict that changed with it would be a schema that
// only one kind of client can rely on.
func TestTheoriesResolveSchemaVerdictsDoNotDependOnTheDraft(t *testing.T) {
	dm := f3_3NewDM(t)
	cases := theoriesResolveCases(t, dm)

	drafts := []*jsonschema.Draft{nil, jsonschema.Draft7, jsonschema.Draft2019, jsonschema.Draft2020}
	for i, draft := range drafts {
		name := "default"
		if draft != nil {
			name = draft.String()
		}
		sch := theoriesSchemaValidatorDraft(t, draft)
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				schemaValid, handlerAccepted := theoriesCheckCase(t, dm, sch, tc)
				if schemaValid != tc.wantSchemaValid {
					t.Errorf("under draft %s the published schema %s the payload, but the test expects it to %s\n  payload: %v",
						name,
						map[bool]string{true: "accepts", false: "rejects"}[schemaValid],
						map[bool]string{true: "accept", false: "reject"}[tc.wantSchemaValid],
						tc.params)
				}
				if handlerAccepted != tc.wantAccepted {
					t.Errorf("under draft %s the handler %s the payload, but the test expects it to %s\n  payload: %v",
						name,
						map[bool]string{true: "accepts", false: "rejects"}[handlerAccepted],
						map[bool]string{true: "accept", false: "reject"}[tc.wantAccepted],
						tc.params)
				}
			})
		}
		_ = i
	}
}

// TestTheoriesResolveSchemaStillGuardsTheProposeAction makes sure the
// correction to the resolve branch did not loosen the propose branch:
// a propose payload must still validate, and must still be the ONLY
// branch it matches.
func TestTheoriesResolveSchemaStillGuardsTheProposeAction(t *testing.T) {
	sch := theoriesSchemaValidator(t)

	valid := []string{
		`{"action":"propose","params":{"hypothesis":"the tokenizer differs on this OS"}}`,
		`{"action":"propose","params":{"hypothesis":"h","validation_criteria":"c"}}`,
	}
	for _, p := range valid {
		var instance any
		require.NoError(t, json.Unmarshal([]byte(p), &instance))
		require.NoError(t, sch.Validate(instance),
			"a valid propose payload must pass the published schema: %s", p)
	}

	invalid := []string{
		`{"action":"propose","params":{}}`,
		`{"action":"propose","params":{"hypothesis":""}}`,
	}
	for _, p := range invalid {
		var instance any
		require.NoError(t, json.Unmarshal([]byte(p), &instance))
		require.Error(t, sch.Validate(instance),
			"a propose payload with no hypothesis must fail the published schema: %s", p)
	}
}

// TestTheoriesResolveBranchDeclaresTheCanonicalVocabulary pins that the
// resolve branch DECLARES every form the handler reads — the canonical
// names and the retained aliases alike — so a caller that uses a
// supported alias is never told it does not exist.
func TestTheoriesResolveBranchDeclaresTheCanonicalVocabulary(t *testing.T) {
	tool, ok := ByName("mpm_theories")
	require.True(t, ok, "mpm_theories must be registered")
	var schema map[string]interface{}
	require.NoError(t, json.Unmarshal(tool.Schema, &schema))

	params, ok := schema["properties"].(map[string]interface{})["params"].(map[string]interface{})
	require.True(t, ok, "mpm_theories schema has no params object")
	branches, ok := params["oneOf"].([]interface{})
	require.True(t, ok && len(branches) > 0,
		"mpm_theories params schema has no oneOf branches")

	var resolve map[string]interface{}
	for _, b := range branches {
		branch, ok := b.(map[string]interface{})
		if !ok {
			continue
		}
		bp, ok := branch["properties"].(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := bp["conclusion"]; has {
			resolve = branch
		}
	}
	require.NotNil(t, resolve, "mpm_theories schema has no resolve branch")

	props, ok := resolve["properties"].(map[string]interface{})
	require.True(t, ok, "the resolve branch has no properties")
	for _, want := range []string{
		"id",         // canonical identity
		"theoryId",   // retained alias
		"theory_id",  // retained alias
		"conclusion", // required, free text
		"status",     // canonical outcome
		"newStatus",  // retained alias
		"new_status", // retained alias
		"winner_id",  // canonical arbitration selector
		"winnerId",   // retained alias
	} {
		require.Contains(t, props, want,
			"the published mpm_theories resolve schema does not declare %q; "+
				"the handler reads it, so an agent that does not know about "+
				"it cannot make the call", want)
	}
}
