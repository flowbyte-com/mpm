// cli_args_strict_int.go — strict bounded-integer CLI parser.
//
// Stage S3 of the approved CLI refactor (see
// docs/archive/mpm-cli-overhaul-investigation-2026-09-06.md §9). One
// place that defines how the CLI parses integer flags with strict
// bounds validation. This addresses the historical class where
// malformed CLI numeric input silently fell back to a default
// (documented at handlers_memory_d42_test.go and the audit
// summary at docs/full-tool-behavioural-audit-2026-09-05.md:697-705).
//
// Convention:
//   - Valid integer text in [lo, hi] inclusive → success.
//   - Empty, whitespace-padded, decimal, scientific, magnitude out
//     of range, non-integer text → deterministic error.
//   - The helper does NOT silently truncate, default, or clamp.
//
// Flag-omission semantics are handled by the caller. The helper
// receives only the value text (already extracted from the flag
// token). Empty input is rejected as an error because the helper
// cannot distinguish "caller had no value at all" from "caller had
// an explicit empty value"; both should be rejected. Callers that
// want the omission model must check flag presence before invoking
// the helper.
//
// Stage S2 invariants preserved:
//   - This helper is independent of ExtractJSONFlag (callers can
//     compose the two in any order).
//   - This helper is independent of splitOnDashDash; callers that
//     also honour the `--` separator must invoke splitOnDashDash
//     first and pass only the flags-bearing portion. The helper
//     does not know about separators.
//
// This stage (S3) migrates only the `--limit` family where the
// parsing shape is identical (see extractLimitFlag and the per-
// handler call sites). `--days` (duration), `--offset` (none in
// the codebase), `--delta` (positive-only by caller convention),
// `--token-budget` / `--weight-below` (different bounds), and
// version/semver/DB-value parsing are out of scope; see S4 handoff.
package main

import (
	"fmt"
	"strconv"
)

// parseBoundedInt parses s as a base-10 integer and validates that
// the result lies within [lo, hi] inclusive.
//
// All input forms other than a well-formed base-10 integer text in
// the inclusive range [lo, hi] are rejected with a deterministic
// error:
//
//   - empty string
//   - leading or trailing whitespace
//   - decimal point or scientific notation
//   - signed magnitude below lo or above hi
//   - non-numeric text
//
// The helper does NOT silently truncate, default, or clamp. Per the
// audit's "silent field loss / silent default" class (G.2 in
// docs/full-tool-behavioural-audit-2026-09-05.md), malformed input
// is rejected explicitly so the caller cannot accidentally store a
// "no-op success" outcome.
//
// The error message identifies the field name and the bounds, so
// direct emission via usererror.Error or respond(..., err.Error())
// produces a CLI message that names the failing field. Example:
//
//	parseBoundedInt("0", "limit", 1, 10000)
//	  → (0, nil)
//	parseBoundedInt("", "limit", 1, 10000)
//	  → (0, "--limit: empty value (must be an integer in [1, 10000])")
//	parseBoundedInt("abc", "limit", 1, 10000)
//	  → (0, "--limit: invalid integer \"abc\" (must be an integer in [1, 10000])")
//	parseBoundedInt("99999", "limit", 1, 10000)
//	  → (0, "--limit: 99999 out of range (must be in [1, 10000])")
//	parseBoundedInt("1.5", "limit", 1, 10000)
//	  → (0, "--limit: invalid integer \"1.5\" (must be an integer in [1, 10000])")
//
// The contract makes the omitted-vs-explicit-zero distinction at the
// caller level: callers MUST decide whether the flag was supplied
// before invoking the helper. A flag that was passed with an empty
// value (e.g. `--limit ""`) reaches this helper and is rejected;
// the caller cannot use `parseBoundedInt("", ...)` to mean
// "omitted" because that collapses two distinct semantic states
// into one. See the Plan §Part 3 for the omitted-vs-explicit
// invariant.
func parseBoundedInt(s, name string, lo, hi int) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("--%s: empty value (must be an integer in [%d, %d])", name, lo, hi)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("--%s: invalid integer %q (must be an integer in [%d, %d])", name, s, lo, hi)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("--%s: %d out of range (must be in [%d, %d])", name, n, lo, hi)
	}
	return n, nil
}