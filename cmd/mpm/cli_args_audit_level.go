// cli_args_audit_level.go — explicit CLI-side parser for the audit-level
// vocabulary.
//
// S4 explicitly deferred the audit-level vocabulary because the CLI
// surface (info|warn|warning|error|fatal|critical) is a strict superset
// of the canonical substrate vocabulary (info|warn|error|fatal|critical),
// and S4's parseEnum helper is exact-match / case-sensitive — it cannot
// host aliases like `warning` → `warn`.
//
// S7 extracts the inline switch in handlers_audit.go::handleAudit into a
// small, testable helper. The helper:
//
//   - validates CLI vocabulary (rejects unknown levels explicitly so a
//     typo doesn't silently no-op);
//   - explicitly maps `warning` → AuditWarn and `critical` → AuditCritical;
//   - preserves case-insensitive matching (matches the existing handler);
//   - returns the canonical AuditLevel constant so downstream code
//     doesn't carry the CLI alias vocabulary past the boundary.
//
// parseEnum is NOT modified. Audit is the only CLI verb that needs an
// alias layer; centralising it in a dedicated helper keeps the alias
// surface explicit and reviewable.

package main

import (
	"fmt"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// parseAuditLevel validates a CLI-supplied audit level string and
// returns the canonical AuditLevel constant. The CLI surface
// accepts: info, warn, warning, error, fatal, critical.
//
//   - empty input is invalid (caller should treat as "no filter");
//   - unknown levels return an explicit error listing the CLI vocabulary
//     so an operator can correct a typo without grepping the source;
//   - matching is case-insensitive, matching the prior inline-switch
//     behaviour.
//
// The helper deliberately does NOT consult parseEnum because the
// `warning` alias is a CLI-only extension to the substrate vocabulary;
// bake the alias mapping in one place rather than smuggling it through
// the strict enum parser.
func parseAuditLevel(raw string) (mpminternal.AuditLevel, error) {
	if raw == "" {
		return "", fmt.Errorf("audit level is empty (use info|warn|warning|error|fatal|critical)")
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "info":
		return mpminternal.AuditInfo, nil
	case "warn", "warning":
		return mpminternal.AuditWarn, nil
	case "error":
		return mpminternal.AuditError, nil
	case "fatal":
		return mpminternal.AuditFatal, nil
	case "critical":
		return mpminternal.AuditCritical, nil
	default:
		return "", fmt.Errorf("audit: unknown level %q (want info|warn|warning|error|fatal|critical)", raw)
	}
}
