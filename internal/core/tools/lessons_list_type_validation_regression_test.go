// lessons_list_type_validation_regression_test.go — Pass 3 defect C.8.
//
// The 2026-09-05 audit found mpm_lessons list with an invalid `type`
// silently returns success with an empty array, masking caller typos.
// handleListLessons forwarded the raw type string to
// dm.ListLessonsFiltered without consulting the existing
// ValidateLessonType allowlist (insight|warning|practice).
//
// Canonical contract (per registry schema enum and ValidateLessonType):
//
//   omitted      → no type filter (return all types)
//   ""           → no type filter (return all types)
//   "insight"    → only insight lessons
//   "warning"    → only warning lessons
//   "practice"   → only practice lessons
//   "bogus"      → error: must be one of warning, practice, or insight
//   "BOGUS"      → error (case-sensitive enum)
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_lessons list --payload '{"action":"list","params":{"type":"bogus"}}'
//     {"count":0,"lessons":[],"mode":"summary","success":true}
//     # wanted: error mentioning the allowed values

package tools

import (
	"strings"
	"testing"
)

// TestLessonsList_InvalidTypeErrors pins the canonical contract:
// invalid type is rejected loudly rather than silently returning an
// empty result. The error must list the allowed values.
func TestLessonsList_InvalidTypeErrors(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, bad := range []string{"bogus", "BOGUS", "warning ", "practive", "0"} {
		t.Run("type="+bad, func(t *testing.T) {
			_, err := handleMpmLessons(dm, defaultACForPatch(), map[string]interface{}{
				"action": "list",
				"params": map[string]interface{}{
					"type": bad,
				},
			})
			if err == nil {
				t.Fatalf("type=%q must error", bad)
			}
			if !strings.Contains(err.Error(), "lesson type") {
				t.Errorf("error must mention 'lesson type', got: %v", err)
			}
		})
	}
}

// TestLessonsList_OmittedTypeReturnsAll pins the documented contract:
// omitting `type` (or passing the empty string) returns all lesson
// types, matching the existing CLI behaviour. The audit flagged the
// silent-empty case for invalid types; this test pins that omission
// remains the legitimate "no filter" path.
func TestLessonsList_OmittedTypeReturnsAll(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, empty := range []interface{}{nil, ""} {
		_, err := handleMpmLessons(dm, defaultACForPatch(), map[string]interface{}{
			"action": "list",
			"params": map[string]interface{}{
				"type": empty,
			},
		})
		if err != nil {
			t.Errorf("omitted/empty type must not error, got: %v", err)
		}
	}
}

// TestLessonsList_ValidTypesAccepted pins the positive contract: each
// allowlisted type is accepted without error. The audit flagged the
// silent-empty class, not the valid-types class, but pinning both
// sides prevents over-correction.
func TestLessonsList_ValidTypesAccepted(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, ok := range []string{"insight", "warning", "practice"} {
		t.Run("type="+ok, func(t *testing.T) {
			_, err := handleMpmLessons(dm, defaultACForPatch(), map[string]interface{}{
				"action": "list",
				"params": map[string]interface{}{
					"type": ok,
				},
			})
			if err != nil {
				t.Errorf("valid type %q must not error, got: %v", ok, err)
			}
		})
	}
}
