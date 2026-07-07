// ltm.go — single canonical "is this memory LTM?" check.
//
// Before this file existed, the same boolean expression appeared in
// at least four places:
//   - internal/gc_tools.go (RunGC's per-row LTM classification)
//   - cmd/mpm/handlers.go (handleGC's identical per-row classification)
//   - cmd/mpm/simple_cmds.go (SaveMemory flag computation)
//   - SQL queries inline `WHERE is_long_term = 1 OR weight >= 10`
//
// The two-flag "LTM" definition is load-bearing: a memory is LTM if
// either (a) is_long_term=1 was explicitly set (the durable flag), or
// (b) weight >= 10 (the threshold the scoring model treats as LTM).
// Drifting one site to drop the second clause silently re-enables decay
// on memory that's been promoted by weight alone — a quiet regression.
//
// IsLTMMemory is the single source of truth. SQL queries still inline
// the WHERE clause (no clean way to call a Go function from SQL), but
// every Go-side check routes through here.
package internal

// IsLTMMemory returns true when the memory qualifies as LTM by either
// the explicit is_long_term flag or the weight >= 10 threshold. The
// threshold is 10 — matching the schema comment ("weight >= 10 means
// LTM") and the scoring-model behavior ("LTM memories are immune to
// decay"). Both inputs are scalar so callers can pass either from the
// raw SQL row or from a struct field without conversion.
func IsLTMMemory(isLTMFlag bool, weight int) bool {
	return isLTMFlag || weight >= 10
}