// handlers_helpers.go — strict type-assertion helpers for DB-derived maps.
//
// Why this file exists (alpha-4 audit D-001 / W-008):
// `internal/core/web_db.go:GetMemoriesForExport` returns `[]map[string]interface{}`
// where numeric fields have Go types fixed by the SQL column type:
//   - INTEGER columns (id counts, reinforcement_count, is_long_term) → int64
//   - REAL columns (weight)                                    → float64
//
// Prior to this fix, the projection used `int` and `int(weight)` casts,
// which forced every consumer to write three-way type assertions
// (`int`/`int64`/`float64`). The assertions silently fell through to their
// default values, masking the bug that `mpm ls` printed weight=1 for every
// row regardless of the stored value.
//
// These helpers centralize the contract: a numeric field coming out of a
// `GetMemoriesForExport`-style projection must have the documented Go
// type. A type mismatch is logged and surfaced as the documented default;
// we deliberately do NOT silently fall through to 0 — that pattern is what
// hid the bug in the first place.
package main

import (
	"fmt"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// int64FromMap extracts an int64 from a DB-projection map.
//
// Returns (value, true) on a clean type match. Returns (default, false) when
// the key is absent or has the wrong Go type, and emits a single user-visible
// warning so the mismatch is surfaced instead of being silently hidden.
//
// Per the D-001 / W-008 contract from `GetMemoriesForExport`, INTEGER
// columns are returned as `int64`. Callers MUST use this helper instead of
// `m[key].(int64)` so a future projection change surfaces the regression
// instead of silently printing zeros.
func int64FromMap(m map[string]interface{}, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		usererror.Warn("int64FromMap: key %q absent from projection (would default to 0); check GetMemoriesForExport contract", key)
		return 0, false
	}
	switch val := v.(type) {
	case int64:
		return val, true
	case int:
		return int64(val), true
	case int32:
		return int64(val), true
	default:
		usererror.Warn("int64FromMap: key %q has unexpected type %T (want int64); projection contract violated", key, v)
		return 0, false
	}
}

// float64FromMap extracts a float64 from a DB-projection map.
//
// Same contract as int64FromMap: warns on type mismatch instead of silently
// defaulting. Per the D-001 / W-008 contract, REAL columns (weight) are
// returned as `float64`.
func float64FromMap(m map[string]interface{}, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		usererror.Warn("float64FromMap: key %q absent from projection (would default to 0); check GetMemoriesForExport contract", key)
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int64:
		// Some projections (tests, hand-built maps) may pass integers for
		// fields that are conceptually real-valued. Accept the conversion
		// but emit a debug note.
		return float64(val), true
	case int:
		return float64(val), true
	default:
		usererror.Warn("float64FromMap: key %q has unexpected type %T (want float64); projection contract violated", key, v)
		return 0, false
	}
}

// stringFromMap extracts a string from a DB-projection map.
//
// Logs and returns empty string on type mismatch. This is intentionally more
// permissive than int64FromMap/float64FromMap because string projections are
// passed through JSON in many places and the cost of a missing field is
// lower than the cost of surfacing every minor encoding difference.
func stringFromMap(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		usererror.Warn("stringFromMap: key %q has unexpected type %T (want string); projection contract violated", key, v)
		return ""
	}
	return s
}

// describeMapValue is a debug aid used in regression tests. It returns the
// concrete Go type of a map value, useful for asserting that the projection
// matches the helper contract.
func describeMapValue(v interface{}) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", v)
}
