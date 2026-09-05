// skills_list_scope_validation_regression_test.go — Residual pass §I-C.15.
//
// The 2026-09-05 audit found mpm_skills.list silently coerced
// invalid `scope` values to "all" via the previous shape
// (ParseStringOr defaulting to "all" plus a DM `default:` branch
// that dropped the filter). A caller asking `scope="bogus"`
// received every skill instead of an error.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_skills --payload '{"action":"list","params":{"scope":"bogus"}}'
//     # wanted: error mentioning the scope enum
//     # actual: success, every skill returned (broadened query)
//
// Canonical contract (per the registry schema enum
// [all|local|shared] and §I-C.15 brief):
//
//   omitted / nil / "" → scope = "all" (legitimate default)
//   "all"              → all skills
//   "local"            → local skills only
//   "shared"           → global skills only
//   "bogus"            → ERROR: scope must be one of [all, local, shared]
//   123 / array / bool → ERROR (wrong type)

package tools

import (
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestSkillsList_OmittedScopeDefaultsToAll pins: omitting scope
// is the documented default — caller sees all skills.
func TestSkillsList_OmittedScopeDefaultsToAll(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, payload := range []interface{}{
		nil,                              // omitted (key absent)
		map[string]interface{}{},         // omitted (params empty)
		map[string]interface{}{"scope": ""}, // explicit empty
		map[string]interface{}{"scope": nil}, // explicit null
	} {
		t.Run("payload", func(t *testing.T) {
			params := map[string]interface{}{}
			if payload != nil {
				params = payload.(map[string]interface{})
			}
			_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "list",
				"params": params,
			})
			if err != nil {
				t.Errorf("omitted/empty/null scope must not error: %v", err)
			}
		})
	}
}

// TestSkillsList_CanonicalScopesAccepted pins: each of the three
// documented enum values is accepted without error.
func TestSkillsList_CanonicalScopesAccepted(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, scope := range []string{"all", "local", "shared"} {
		t.Run("scope="+scope, func(t *testing.T) {
			_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "list",
				"params": map[string]interface{}{"scope": scope},
			})
			if err != nil {
				t.Errorf("canonical scope %q must not error: %v", scope, err)
			}
		})
	}
}

// TestSkillsList_InvalidScopeRejected pins the headline §I-C.15
// invariant: an invalid scope value errors rather than silently
// broadening the query.
func TestSkillsList_InvalidScopeRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, bad := range []string{"bogus", "ALL", "global", "everyone", "0"} {
		t.Run("scope="+bad, func(t *testing.T) {
			_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "list",
				"params": map[string]interface{}{"scope": bad},
			})
			if err == nil {
				t.Fatalf("invalid scope %q must error", bad)
			}
			if !strings.Contains(err.Error(), "scope") {
				t.Errorf("error must mention 'scope', got: %v", err)
			}
			if !strings.Contains(err.Error(), "all") ||
				!strings.Contains(err.Error(), "local") ||
				!strings.Contains(err.Error(), "shared") {
				t.Errorf("error must list allowed values, got: %v", err)
			}
		})
	}
}

// TestSkillsList_NonStringScopeRejected pins: a non-string value
// for scope errors with a clear type message.
func TestSkillsList_NonStringScopeRejected(t *testing.T) {
	dm := newTestSharedDM(t)

	for _, bad := range []interface{}{123, true, []interface{}{"all"}} {
		t.Run("type", func(t *testing.T) {
			_, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
				"action": "list",
				"params": map[string]interface{}{"scope": bad},
			})
			if err == nil {
				t.Fatalf("non-string scope (%T) must error", bad)
			}
			if !strings.Contains(err.Error(), "scope") {
				t.Errorf("error must mention 'scope', got: %v", err)
			}
		})
	}
}

// TestSkillsList_InvalidScopeDoesNotBroadenQuery pins the
// no-side-effect invariant: an invalid scope returns an error and
// no skills, matching the caller's intent that they had narrowed
// the query (and the system refused the narrowing).
func TestSkillsList_InvalidScopeDoesNotBroadenQuery(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleMpmSkills(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{"scope": "bogus"},
	})
	if err == nil {
		t.Fatal("invalid scope must error")
	}
	if res != nil {
		t.Errorf("invalid scope must return nil result, got %v", res)
	}
}
