// recent_activity_result_status_regression_test.go — handler-level
// contract coverage for the result_status selector on the
// recent_activity tool, plus the unaffected-neighbour proof for
// mpm_system critic_findings.
//
// The narrow layer (internal/core/recent_activity_result_status_test.go)
// covers the query. This file covers the parts only the tool surface
// owns: the parameter is accepted, defaulted, type-checked, rejected on
// a bad value, and echoed back — and that the surface an operator was
// already using for failures did not move.
//
// All fixtures are hermetic: mpminternal.NewTestDM roots the database
// in t.TempDir() and redirects MPM_WORKSPACE, so no assertion here can
// reach the live MPM database.
package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// callRecentActivity invokes the handler directly, which is the same
// entry point the MCP and CLI dispatchers reach.
func callRecentActivity(t *testing.T, dm mpminternal.CoreDB, params map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	if params == nil {
		params = map[string]interface{}{}
	}
	res, err := handleRecentActivity(dm, mpminternal.ActiveContext{}, params)
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("handleRecentActivity returned %T, want map[string]interface{}", res)
	}
	return m, nil
}

// eventIDs pulls the event IDs out of a recent_activity response.
func eventIDs(t *testing.T, res map[string]interface{}) []string {
	t.Helper()
	raw, ok := res["events"]
	if !ok {
		t.Fatal("response has no events key")
	}
	list, ok := raw.([]mpminternal.RecentActivityEvent)
	if !ok {
		t.Fatalf("events is %T, want []internal.RecentActivityEvent", raw)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.ID)
	}
	return out
}

func eventListHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// seedOutcome inserts one tool_invocations row for a mutating action
// with the given persisted result_status. Direct SQL, because the point
// is to control the persisted column rather than to exercise the
// dispatcher that would normally write it.
func seedOutcome(t *testing.T, dm mpminternal.CoreDB, id, tool, action, status string, startedAt int64) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms, error_message)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "rs-session", tool, action, "rs-inv-"+id,
		"human", "openclaw", "", status,
		startedAt, startedAt+1, 1, nil,
	)
	if err != nil {
		t.Fatalf("seedOutcome(%s): %v", id, err)
	}
}

// TestHandleRecentActivity_ResultStatus_DefaultIsSuccess pins the
// compatibility guarantee at the surface an agent actually calls: a
// params map with no result_status key — which is what every existing
// agent, prompt, and script sends — returns successes and not failures.
func TestHandleRecentActivity_ResultStatus_DefaultIsSuccess(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()
	seedOutcome(t, dm, "h-ok", "mpm_memory", "save", "success", now)
	seedOutcome(t, dm, "h-err", "mpm_memory", "save", "error", now+1)

	res, err := callRecentActivity(t, dm, map[string]interface{}{"limit": 50})
	if err != nil {
		t.Fatalf("handleRecentActivity: %v", err)
	}
	got := eventIDs(t, res)

	if !eventListHas(got, "h-ok") {
		t.Errorf("omitted the successful row; got %v", got)
	}
	if eventListHas(got, "h-err") {
		t.Errorf("returned the failed row %q; got %v. "+
			"Callers that never set result_status must keep the success-only view", "h-err", got)
	}
}

// TestHandleRecentActivity_ResultStatus_EchoesAppliedSelector pins that
// the response reports the selector that was APPLIED, not the one that
// was sent. A caller that omitted the parameter can therefore see it
// was defaulted rather than silently widened.
func TestHandleRecentActivity_ResultStatus_EchoesAppliedSelector(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()
	seedOutcome(t, dm, "h-ok", "mpm_memory", "save", "success", now)
	seedOutcome(t, dm, "h-err", "mpm_memory", "save", "error", now+1)

	for _, tc := range []struct {
		name   string
		params map[string]interface{}
		want   string
	}{
		{"unset", nil, mpminternal.RecentActivityStatusSuccess},
		{"explicit success", map[string]interface{}{"result_status": "success"}, mpminternal.RecentActivityStatusSuccess},
		{"explicit error", map[string]interface{}{"result_status": "error"}, mpminternal.RecentActivityStatusError},
		{"explicit all", map[string]interface{}{"result_status": "all"}, mpminternal.RecentActivityStatusAll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := callRecentActivity(t, dm, tc.params)
			if err != nil {
				t.Fatalf("handleRecentActivity: %v", err)
			}
			defaults, ok := res["defaults"].(map[string]interface{})
			if !ok {
				t.Fatalf("response has no defaults map (got %T)", res["defaults"])
			}
			if got := defaults["result_status"]; got != tc.want {
				t.Errorf("defaults.result_status = %v, want %q", got, tc.want)
			}
		})
	}
}

// TestHandleRecentActivity_ResultStatus_ErrorAndAll exercises the two
// opt-in selectors through the surface, so the wiring — not just the
// query — is covered.
func TestHandleRecentActivity_ResultStatus_ErrorAndAll(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()
	seedOutcome(t, dm, "h-ok", "mpm_memory", "save", "success", now)
	seedOutcome(t, dm, "h-err", "mpm_memory", "save", "error", now+1)

	errRes, err := callRecentActivity(t, dm, map[string]interface{}{
		"limit": 50, "result_status": mpminternal.RecentActivityStatusError,
	})
	if err != nil {
		t.Fatalf("handleRecentActivity(error): %v", err)
	}
	errIDs := eventIDs(t, errRes)
	if !eventListHas(errIDs, "h-err") {
		t.Errorf("result_status=error omitted the failed row; got %v", errIDs)
	}
	if eventListHas(errIDs, "h-ok") {
		t.Errorf("result_status=error returned the successful row; got %v", errIDs)
	}

	allRes, err := callRecentActivity(t, dm, map[string]interface{}{
		"limit": 50, "result_status": mpminternal.RecentActivityStatusAll,
	})
	if err != nil {
		t.Fatalf("handleRecentActivity(all): %v", err)
	}
	allIDs := eventIDs(t, allRes)
	if !eventListHas(allIDs, "h-err") {
		t.Errorf("result_status=all omitted the failed row; got %v", allIDs)
	}
	if !eventListHas(allIDs, "h-ok") {
		t.Errorf("result_status=all omitted the successful row; got %v", allIDs)
	}
}

// TestHandleRecentActivity_ResultStatus_RejectsBadInput covers both
// rejection paths. A bad value must not be coerced, and a non-string
// must be a type error rather than being read as "unset" — silently
// treating `true` as unset would hand the caller the success view when
// they asked for something else.
func TestHandleRecentActivity_ResultStatus_RejectsBadInput(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()
	seedOutcome(t, dm, "h-err", "mpm_memory", "save", "error", now)

	t.Run("unknown value", func(t *testing.T) {
		_, err := callRecentActivity(t, dm, map[string]interface{}{
			"result_status": "failures",
		})
		if err == nil {
			t.Fatal(`result_status="failures" was accepted`)
		}
		for _, want := range []string{"success", "error", "all"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error does not name the accepted value %q: %v", want, err)
			}
		}
	})

	for _, bad := range []interface{}{true, 1, 1.5, []string{"error"}, map[string]interface{}{}} {
		t.Run("non-string", func(t *testing.T) {
			_, err := callRecentActivity(t, dm, map[string]interface{}{
				"result_status": bad,
			})
			if err == nil {
				t.Fatalf("result_status=%#v (%T) was accepted; it must be a "+
					"type error, not silently read as unset", bad, bad)
			}
			if !strings.Contains(err.Error(), "must be string") {
				t.Errorf("error does not report a type problem: %v", err)
			}
		})
	}
}

// TestHandleCriticFindings_UnaffectedByResultStatus is the
// unaffected-neighbour proof.
//
// handleCriticFindings has its own hand-written SQL
// (`WHERE tool_name = 'mpm_memory' AND action = 'challenge'`) with no
// result_status predicate, and it never calls RecentActivity or
// RecentActivityWithMeta. So the result_status selector cannot reach
// it — and it must keep returning BOTH outcomes, because an operator
// reading critic findings is precisely the person who needs to see the
// failures the critic recorded.
//
// This is asserted rather than assumed. The premise "critic findings
// only cares about successes" would be false, and the previous
// hardcoded success predicate in recent_activity.go is exactly the
// bug that made it tempting to believe.
func TestHandleCriticFindings_UnaffectedByResultStatus(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()

	// Two critic challenges: one succeeded, one failed.
	seedOutcome(t, dm, "cf-ok", "mpm_memory", "challenge", "success", now)
	seedOutcome(t, dm, "cf-err", "mpm_memory", "challenge", "error", now+1)
	// A non-challenge row that must stay out regardless.
	seedOutcome(t, dm, "cf-other", "mpm_memory", "save", "error", now+2)

	// Passing result_status here must not be silently honoured: this
	// action has no such parameter, and honouring it would narrow an
	// operator's view of critic output. It is ignored, as every
	// unknown parameter to this action is.
	res, err := handleCriticFindings(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"limit": 50, "result_status": mpminternal.RecentActivityStatusError,
	})
	if err != nil {
		t.Fatalf("handleCriticFindings: %v", err)
	}

	envelope, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("handleCriticFindings returned %T, want map[string]interface{}", res)
	}
	findings, ok := envelope["findings"].([]map[string]interface{})
	if !ok {
		t.Fatalf("findings is %T, want []map[string]interface{}", envelope["findings"])
	}

	seen := map[string]bool{}
	for _, f := range findings {
		id, _ := f["invocation_id"].(string)
		seen[id] = true
	}
	if !seen["cf-ok"] {
		t.Error("critic_findings dropped the successful challenge; it must " +
			"return both outcomes, not just failures")
	}
	if !seen["cf-err"] {
		t.Error("critic_findings dropped the failed challenge")
	}
	if seen["cf-other"] {
		t.Error("critic_findings returned a non-challenge row")
	}
}

// ── Schema contract ───────────────────────────────────────────────────

// mpmContextSchema returns the parsed mpm_context input schema straight
// out of the registry, so these tests assert against what the tool
// actually advertises rather than a copy that could drift.
func mpmContextSchema(t *testing.T) map[string]interface{} {
	t.Helper()
	tool, ok := ByName("mpm_context")
	if !ok {
		t.Fatal("registry must contain mpm_context")
	}
	var sch map[string]interface{}
	if err := json.Unmarshal(tool.Schema, &sch); err != nil {
		t.Fatalf("mpm_context schema is not valid JSON: %v", err)
	}
	return sch
}

// mpmContextParamSchema returns the declared schema for one param of the
// shared mpm_context params object.
func mpmContextParamSchema(t *testing.T, name string) (map[string]interface{}, bool) {
	t.Helper()
	props, ok := mpmContextSchema(t)["properties"].(map[string]interface{})["params"].(map[string]interface{})["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("mpm_context schema has no params.properties object")
	}
	p, ok := props[name].(map[string]interface{})
	return p, ok
}

// TestMpmContextSchema_ResultStatusDeclared pins the strict schema
// contract, not the prose. The tool description mentions result_status,
// but an MCP client does not read prose to decide whether a parameter is
// well-formed — it reads properties. An undeclared parameter reaches the
// handler only via additionalProperties:true, with no type and no value
// constraint, which is indistinguishable from a typo.
func TestMpmContextSchema_ResultStatusDeclared(t *testing.T) {
	p, ok := mpmContextParamSchema(t, "result_status")
	if !ok {
		t.Fatal("mpm_context schema does not declare result_status; " +
			"it is only documented in prose")
	}

	if got := p["type"]; got != "string" {
		t.Errorf("result_status type = %v, want string", got)
	}

	rawEnum, ok := p["enum"].([]interface{})
	if !ok {
		t.Fatalf("result_status has no enum; got %v", p["enum"])
	}
	got := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		s, _ := v.(string)
		got = append(got, s)
	}
	want := []string{"success", "error", "all"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("result_status enum = %v, want %v", got, want)
	}

	// No schema-level default, deliberately. This params object is
	// shared across twelve actions and eleven ignore result_status, so a
	// default here would advertise a value for a parameter that has no
	// meaning on the other eleven. It also matches the sibling
	// action-specific params (projection, format), which declare
	// type+enum and no default. The default is documented in the
	// description and enforced in the implementation.
	if _, has := p["default"]; has {
		t.Errorf("result_status declares a schema default (%v); the sibling "+
			"action-specific params declare none, and the shared params object "+
			"spans actions that ignore this field", p["default"])
	}
	if desc, _ := p["description"].(string); !strings.Contains(strings.ToLower(desc), "omit") {
		t.Errorf("result_status description does not state that omitting it "+
			"means success: %q", desc)
	}
}

// TestMpmContextSchema_ResultStatusEnumMatchesImplementation pins the
// schema against the constants the implementation enforces. A schema
// that drifts from the code is worse than no schema: it tells clients a
// value is valid and the server then rejects it.
func TestMpmContextSchema_ResultStatusEnumMatchesImplementation(t *testing.T) {
	p, _ := mpmContextParamSchema(t, "result_status")
	rawEnum := p["enum"].([]interface{})
	for _, v := range rawEnum {
		s := v.(string)
		switch s {
		case mpminternal.RecentActivityStatusSuccess,
			mpminternal.RecentActivityStatusError,
			mpminternal.RecentActivityStatusAll:
		default:
			t.Errorf("schema advertises %q, which is not one of the "+
				"implementation's constants", s)
		}
	}
}

// ── End-to-end through the real dispatch path ─────────────────────────

// dispatchRecentActivity sends a payload through the registry lookup and
// Handler invocation that `mpm call` and the MCP server both use, rather
// than calling handleRecentActivity directly. This is the chain the
// schema is actually attached to.
func dispatchRecentActivity(t *testing.T, dm mpminternal.CoreDB, payload map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	tool, ok := ByName("mpm_context")
	if !ok {
		t.Fatal("registry must contain mpm_context")
	}
	res, err := tool.Handler(dm, mpminternal.ActiveContext{}, payload)
	if err != nil {
		return nil, err
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		t.Fatalf("dispatch returned %T, want map[string]interface{}", res)
	}
	return m, nil
}

// TestRecentActivity_EndToEndThroughDispatch walks the whole chain —
// registry lookup, dispatch, handleMpmContext, handleRecentActivity,
// RecentActivityQueryParams, SQL, filterActivityPage — and pins all four
// selector spellings against one fixture holding a success and a
// failure.
//
// It goes through ByName + tool.Handler rather than calling the handler
// directly so that a future rename of the action, a dispatch-table edit,
// or a params-extraction change is caught here rather than in production.
func TestRecentActivity_EndToEndThroughDispatch(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()
	seedOutcome(t, dm, "e2e-ok", "mpm_memory", "save", "success", now)
	seedOutcome(t, dm, "e2e-err", "mpm_memory", "save", "error", now+1)

	cases := []struct {
		name       string
		params     map[string]interface{}
		wantOK     bool
		wantErrRow bool
	}{
		{"omitted", nil, true, false},
		{"success", map[string]interface{}{"result_status": "success"}, true, false},
		{"error", map[string]interface{}{"result_status": "error"}, false, true},
		{"all", map[string]interface{}{"result_status": "all"}, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]interface{}{"limit": 50}
			for k, v := range tc.params {
				params[k] = v
			}

			res, err := dispatchRecentActivity(t, dm, map[string]interface{}{
				"action": "recent_activity",
				"params": params,
			})
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			got := eventIDs(t, res)

			if eventListHas(got, "e2e-ok") != tc.wantOK {
				t.Errorf("success row present = %v, want %v (got %v)",
					eventListHas(got, "e2e-ok"), tc.wantOK, got)
			}
			if eventListHas(got, "e2e-err") != tc.wantErrRow {
				t.Errorf("error row present = %v, want %v (got %v)",
					eventListHas(got, "e2e-err"), tc.wantErrRow, got)
			}
		})
	}
}

// TestRecentActivity_EndToEndRejectsSchemaInvalidValue proves an invalid
// value is rejected rather than silently becoming success or all.
//
// The rejection comes from the handler, not the schema, and that is the
// correct layering: there is NO runtime JSON-Schema validator in the
// dispatch path, so the enum is advisory to MCP clients only. It must
// still hold for `mpm call`, which never sees a schema at all, and for
// any client that ignores the advertised enum. This test asserts the
// enforcement point actually exists.
func TestRecentActivity_EndToEndRejectsSchemaInvalidValue(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()
	seedOutcome(t, dm, "e2e-ok", "mpm_memory", "save", "success", now)
	seedOutcome(t, dm, "e2e-err", "mpm_memory", "save", "error", now+1)

	// Valid per the advertised enum: proves a schema-valid payload
	// really does reach the handler through dispatch.
	valid := map[string]interface{}{
		"action": "recent_activity",
		"params": map[string]interface{}{"result_status": "error"},
	}
	if _, err := dispatchRecentActivity(t, dm, valid); err != nil {
		t.Fatalf("schema-valid payload rejected: %v", err)
	}

	// Not in the enum: must fail rather than degrade.
	for _, bad := range []string{"failures", "ERRROR", "Success", ""} {
		t.Run("invalid="+bad, func(t *testing.T) {
			res, err := dispatchRecentActivity(t, dm, map[string]interface{}{
				"action": "recent_activity",
				"params": map[string]interface{}{"result_status": bad},
			})
			if bad == "" {
				// Empty string is the documented default path, not an
				// invalid value; assert it does NOT error.
				if err != nil {
					t.Fatalf(`result_status="" rejected: %v`, err)
				}
				got := eventIDs(t, res)
				if !eventListHas(got, "e2e-ok") || eventListHas(got, "e2e-err") {
					t.Errorf(`result_status="" returned %v; want the success-only `+
						"default view", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("result_status=%q accepted, returned %v; the enum is "+
					"advisory only, so the handler must reject it", bad, eventIDs(t, res))
			}
			for _, want := range []string{"success", "error", "all"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("rejection for %q does not name accepted value %q: %v",
						bad, want, err)
				}
			}
		})
	}
}

// TestRecentActivity_ResponseCompatibility pins that adding
// defaults.result_status was purely additive.
//
// Every defaults field that existed before the change must still be
// present with the same value, and the envelope's other keys must be
// unchanged in count and meaning. "Extra metadata is acceptable;
// changed existing semantics are not" — this is the test that says so.
func TestRecentActivity_ResponseCompatibility(t *testing.T) {
	dm := newTestDMForTools(t)
	now := time.Now().Unix()
	seedOutcome(t, dm, "rc-ok", "mpm_memory", "save", "success", now)

	res, err := dispatchRecentActivity(t, dm, map[string]interface{}{
		"action": "recent_activity",
		"params": map[string]interface{}{"limit": 50},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// Pre-existing defaults fields, verbatim from the envelope as it
	// stood before this feature.
	wantDefaults := map[string]string{
		"actor_scope":  "agent+human+unknown",
		"class_filter": "mutating",
		"ordering":     "newest-first",
	}
	defaults, ok := res["defaults"].(map[string]interface{})
	if !ok {
		t.Fatalf("response has no defaults map (got %T)", res["defaults"])
	}
	for k, want := range wantDefaults {
		got, present := defaults[k]
		if !present {
			t.Errorf("defaults.%s was removed", k)
			continue
		}
		if got != want {
			t.Errorf("defaults.%s = %v, want %q (unchanged)", k, got, want)
		}
	}
	if _, present := defaults["note"]; !present {
		t.Error("defaults.note was removed")
	}
	// Exactly one field added, and nothing else.
	if len(defaults) != len(wantDefaults)+2 { // +note +result_status
		t.Errorf("defaults has %d keys (%v), want %d: the change must be "+
			"purely additive", len(defaults), defaults, len(wantDefaults)+2)
	}

	// Envelope keys are unchanged apart from the defaults map itself.
	for _, k := range []string{"success", "action", "events", "count", "limit",
		"truncated", "history_exhausted", "scanned_rows", "scan_limit"} {
		if _, present := res[k]; !present {
			t.Errorf("envelope key %q disappeared", k)
		}
	}
	if res["success"] != true {
		t.Errorf("success = %v, want true", res["success"])
	}
	if res["action"] != "recent_activity" {
		t.Errorf("action = %v, want recent_activity", res["action"])
	}

	// count must still be derived from the events slice length.
	got := eventIDs(t, res)
	if res["count"] != float64(len(got)) && res["count"] != len(got) {
		t.Errorf("count = %v, want %d (derived from len(events))",
			res["count"], len(got))
	}
}
