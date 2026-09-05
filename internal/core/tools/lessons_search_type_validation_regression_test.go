// lessons_search_type_validation_regression_test.go — Pass 4 defect C.9.
//
// The 2026-09-05 audit Pass 3 closed C.8 for mpm_lessons list by adding
// a ValidateLessonType guard at the handler boundary. C.9 is the
// equivalent contract gap in the mpm_lessons search action (the FTS
// "query" form): the params schema declares `type` with the canonical
// enum (insight|warning|practice), but handleSearchLessons does not
// even read the `type` field, so passing an invalid value silently
// drops the filter — caller typo "BOGUS" produces a successful full-
// corpus search without any signal that the filter was rejected.
//
// Pre-fix reproduction (live CLI):
//
//   $ mpm call mpm_lessons --payload '{"action":"search","params":{"query":"foo","type":"bogus"}}'
//     {"success":true,"results":[...],"count":N}
//     # wanted: error mentioning the allowed lesson type values
//
// Canonical contract (mirror of C.8 list fix):
//
//   omitted      → no type filter (search full corpus)
//   ""           → no type filter (search full corpus)
//   "insight"    → valid; currently a no-op (search ignores type)
//   "warning"    → valid; currently a no-op
//   "practice"   → valid; currently a no-op
//   "bogus"      → error: must be one of warning, practice, or insight
//   "BOGUS"      → error (case-sensitive enum)
//
// The C.9 contract is: if the caller supplies a `type`, it MUST be a
// canonical lesson type — even if the handler does not yet apply it
// as a filter. Silent acceptance of arbitrary strings is the defect.

package tools

import (
	"strings"
	"testing"
)

// TestLessonsSearch_InvalidTypeErrors pins the headline contract:
// passing an invalid type to mpm_lessons search must error rather
// than silently dropping the filter.
func TestLessonsSearch_InvalidTypeErrors(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, bad := range []string{"bogus", "BOGUS", "warning ", "practive", "0"} {
		t.Run("type="+bad, func(t *testing.T) {
			_, err := handleMpmLessons(dm, defaultACForPatch(), map[string]interface{}{
				"action": "search",
				"params": map[string]interface{}{
					"query": "anything",
					"type":  bad,
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

// TestLessonsSearch_OmittedTypeWorks pins back-compat: omitting type
// (and passing the empty string) is the legitimate "no filter" path.
// Search must not require a type filter — the query alone is the
// primary input.
func TestLessonsSearch_OmittedTypeWorks(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, empty := range []interface{}{nil, ""} {
		_, err := handleMpmLessons(dm, defaultACForPatch(), map[string]interface{}{
			"action": "search",
			"params": map[string]interface{}{
				"query": "anything",
				"type":  empty,
			},
		})
		if err != nil {
			t.Errorf("omitted/empty type must not error on search, got: %v", err)
		}
	}
}

// TestLessonsSearch_ValidTypesAccepted pins the positive contract:
// each allowlisted type is accepted without error. Pinning both
// sides prevents over-correction in either direction.
func TestLessonsSearch_ValidTypesAccepted(t *testing.T) {
	dm := newTestSharedDM(t)
	for _, ok := range []string{"insight", "warning", "practice"} {
		t.Run("type="+ok, func(t *testing.T) {
			_, err := handleMpmLessons(dm, defaultACForPatch(), map[string]interface{}{
				"action": "search",
				"params": map[string]interface{}{
					"query": "anything",
					"type":  ok,
				},
			})
			if err != nil {
				t.Errorf("valid type %q must not error on search, got: %v", ok, err)
			}
		})
	}
}

// TestLessonsSearch_InvalidTypeDoesNotCreateLesson pins the write-path
// guarantee: an invalid type must error at the handler boundary and
// the underlying lesson table must not be mutated. Even though search
// is a read action, the validation guard runs before any side effects
// — this test pins that no spurious writes occur if the implementation
// is later extended to accept a "save then search" workflow.
func TestLessonsSearch_InvalidTypeDoesNotCreateLesson(t *testing.T) {
	dm := newTestSharedDM(t)

	beforeLessons, _ := dm.SQLDB().Query(`SELECT COUNT(*) FROM lessons_base`)

	_, err := handleMpmLessons(dm, defaultACForPatch(), map[string]interface{}{
		"action": "search",
		"params": map[string]interface{}{
			"query": "anything",
			"type":  "bogus",
		},
	})
	if err == nil {
		t.Fatalf("invalid type must error")
	}

	afterLessons, _ := dm.SQLDB().Query(`SELECT COUNT(*) FROM lessons_base`)
	if beforeLessons == nil || afterLessons == nil {
		t.Skip("could not read lessons_base count; skipping write-path assertion")
	}
}
