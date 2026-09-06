// cli_args_enum.go — canonical CLI enum validator.
//
// Stage S4 of the approved CLI refactor (see
// docs/archive/mpm-cli-overhaul-investigation-2026-09-06.md §9). One
// place that defines how the CLI validates a value against a
// caller-supplied allowed-list. This addresses the historical class
// where enum-typed CLI input was silently accepted, ignored, or
// transformed into a default — the audit's G.2 silent-field-loss
// class (docs/full-tool-behavioural-audit-2026-09-05.md:697-705).
//
// Convention:
//   - Allowed values are case-sensitive. The helper does NOT
//     lowercase; CLI enum contracts are case-sensitive by design.
//   - Allowed values are NOT aliased. "warn" and "warning" are
//     distinct; if the CLI wishes to accept both, the mapping is
//     performed at the caller boundary, AFTER the helper has
//     confirmed the input is well-formed. This makes the alias
//     surface visible in code rather than buried in the helper.
//   - Empty input is rejected. The helper cannot distinguish
//     "caller had no value at all" from "caller had an explicit
//     empty"; both should be rejected. Callers that want to model
//     "omitted" must check flag presence before invoking the helper.
//
// Stage S4's vocabulary reconciliation explicitly DEFERRED the
// following mismatches (recorded in the S5 handoff):
//
//   - audit level: CLI accepts `info | warn | warning | error |
//     fatal | critical` (case-normalized, with aliases); the
//     tool side accepts `debug | info | warn | error`. The CLI
//     surface is intentionally richer than the tool surface;
//     domain mapping happens at the tool-boundary, not in this
//     helper. Migrating would erase the explicit translation.
//
//   - theory conclusion: CLI accepts `confirmed | proven |
//     disproven | refuted | invalidated`; canonical persistence
//     states are `proven | disproven`. The CLI surface is
//     intentionally richer; mapping happens at the storage
//     boundary.
//
// This helper is for vocabulary-canonical fields only. Domain
// mappings belong at the caller boundary, not in a shared enum
// validator.
package main

import (
	"fmt"
	"strings"
)

// parseEnum validates that s appears in allowed. On success returns
// (canonical, nil) where canonical equals s verbatim (no
// transformation, no lowercasing, no alias resolution). On any
// failure returns ("", error) with a deterministic message that
// identifies the field name, the supplied value, and the allowed
// values — suitable for direct emission via usererror.Error or
// respond(..., err.Error()).
//
// Empty s is rejected as an error because the helper cannot
// distinguish "caller had no value at all" from "caller had an
// explicit empty value"; both should be rejected. Callers that
// want to model "omitted" must check flag presence before invoking
// the helper.
//
// The helper does NOT perform:
//
//   - Lowercasing. CLI enum contracts are case-sensitive by design.
//     "Info" must NOT silently become "info".
//   - Alias mapping. "warning" must NOT silently become "warn".
//     The caller performs any required mapping explicitly after
//     parseEnum succeeds.
//   - Substring matching. "info" must NOT silently match an
//     allowed value of "information".
//
// The error message always includes the allowed list so the CLI
// surface can list valid options when an operator types a typo.
//
// Example:
//
//	parseEnum("json", "format", []string{"json", "csv"})
//	  → ("json", nil)
//	parseEnum("xml", "format", []string{"json", "csv"})
//	  → ("", "--format: invalid value \"xml\" (must be one of: json, csv)")
//	parseEnum("", "format", []string{"json", "csv"})
//	  → ("", "--format: empty value (must be one of: json, csv)")
//	parseEnum("JSON", "format", []string{"json", "csv"})
//	  → ("", "--format: invalid value \"JSON\" (must be one of: json, csv)")
func parseEnum(s, name string, allowed []string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("--%s: empty value (must be one of: %s)", name, strings.Join(allowed, ", "))
	}
	for _, v := range allowed {
		if s == v {
			return v, nil
		}
	}
	return "", fmt.Errorf("--%s: invalid value %q (must be one of: %s)", name, s, strings.Join(allowed, ", "))
}