// parity_lock_all_tools_regression_test.go — 2026-09-05 audit
// remediation pass 2.
//
// Audit C.19 (P3) found seven tools lacking the existing
// assertParityForTool registry/dispatcher parity lock. Without this
// lock, future schema drift (the schema enum shorter than the
// dispatcher's case list) is not caught at `make test` time — the
// exact class of drift that surfaced in the alpha-4 audit D-006
// (mpm_theories schema enum missing show/list/query) and D-4.1
// (mpm_topics schema enum missing list/show).
//
// This file pins the lock for every remaining public tool with an
// action enum, so future drift in any of them triggers a test
// failure. The lock uses the existing assertParityForTool helper —
// no new infrastructure is introduced.

package tools

import (
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestParity_AllActionTools_LockEverySurface iterates every registry
// tool with an action enum and asserts registry/dispatcher parity.
// Pre-fix (alpha-4 era) only mpm_theories and mpm_topics were
// locked. The audit identified seven others — this test makes the
// coverage gap visible to CI rather than to manual audit.
//
// The seven (from the audit):
//   - mpm_memory
//   - mpm_lessons
//   - mpm_decisions
//   - mpm_skills
//   - mpm_references
//   - mpm_evidence
//   - mpm_confidence
//   - mpm_context
//   - mpm_wakes
//   - mpm_handoff
//   - mpm_scratchpad
//   - mpm_system
//   - mpm_work
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
		"mpm_theories", // already locked separately; iterated for completeness
		"mpm_skills",
		"mpm_topics",    // already locked separately; iterated for completeness
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
