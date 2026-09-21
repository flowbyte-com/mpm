// tool_outcome.go — Outcome classification contract for tool dispatch
// results.
//
// Two fields compose every tool outcome:
//
//   outcome_class — general category (small closed vocabulary).
//                   Wide enough to gate fall-through in tool-call
//                   aggregation queries and to surface in Doctor.
//                   Bounded to lowercase ASCII identifiers.
//
//   outcome_code  — bounded subtype inside the class. Diagnostically
//                   meaningful; safe to consume in routing/CI/metrics.
//                   Bounded to lowercase ASCII identifiers with a
//                   single underscore between segments.
//
// Both are stored verbatim on tool_invocations (this module adds the
// schema columns; see migration_tool_outcome_class_v1).
//
// Stability rules:
//
//   - outcome_class vocabulary is the public enum (ClassOk ..). Adding
//     a class is a schema change (migrateToolOutcomeClassV1_v2 etc).
//     Never silently broaden or narrow.
//   - outcome_code is free-form within its class but UNCONDITIONALLY
//     must be a non-empty ASCII identifier (no SQL, no payload, no
//     path). Callers classify by it; auditors aggregate on it.
//   - Successful invocation without a classification collapses to
//     (outcome_class=ok, outcome_code="").
//   - Per-tool actions keep their existing `error_message` text. The
//     outcome fields NEVER inherit user content.
//
// Classification source: typed at the error boundary where possible
// (see Classify), string-matched against well-known validation
// phrases as fallback. NEVER regex-match full message bodies wholesale.

package internal

import (
	"context"
	"errors"
	"strings"
)

// ToolOutcomeClass is the closed set of outcome categories that apply
// to tool dispatch. Small on purpose so Doctor / dashboards can plot
// the distribution without normalization.
type ToolOutcomeClass string

const (
	// OutcomeClassOk — invocation succeeded normally. outcome_code is
	// empty for this class.
	OutcomeClassOk ToolOutcomeClass = "ok"

	// OutcomeClassValidation — caller-supplied input failed schema or
	// business validation. Agent's fault, not MPM's. Subcodes:
	// "missing_required_field", "unknown_action", "invalid_type",
	// "invalid_enum_value", "missing_param".
	OutcomeClassValidation ToolOutcomeClass = "validation"

	// OutcomeClassNotFound — well-formed request, target absent.
	// Subcodes: "artifact_not_found", "profile_not_found",
	// "artifact_type_not_found", "topic_not_found", "session_not_found".
	OutcomeClassNotFound ToolOutcomeClass = "not_found"

	// OutcomeClassConflict — request conflicts with current state.
	// Subcodes: "duplicate_artifact", "state_precondition_unsatisfied",
	// "version_mismatch".
	OutcomeClassConflict ToolOutcomeClass = "conflict"

	// OutcomeClassDegraded — invocation succeeded with reduced
	// fidelity; data returned is usable but incomplete. Subcodes:
	// "contextual_focus_degraded", "partial_materialization",
	// "fallback_substrate".
	//
	// Degraded outcomes STILL record result_status=success. The
	// outcome_class column makes the degradation queryable without
	// parsing error text.
	OutcomeClassDegraded ToolOutcomeClass = "degraded"

	// OutcomeClassSubstrate — MPM's own storage layer failed. Worth
	// surfacing in Doctor. Subcodes: "sqlite_*", "fts_missing",
	// "migration_failed", "integrity_check_failed",
	// "wal_busy_timeout".
	OutcomeClassSubstrate ToolOutcomeClass = "substrate"

	// OutcomeClassIntegration — external party (LLM provider, embed
	// provider, watcher, scheduler) refused or was unreachable.
	// Subcodes: "provider_unreachable", "provider_4xx",
	// "provider_5xx", "provider_auth_failed", "watchdog_unreachable".
	OutcomeClassIntegration ToolOutcomeClass = "integration"

	// OutcomeClassTimeout — exceeded bounded deadline. Subcodes:
	// "context_deadline", "probe_timeout", "watchdog_timeout".
	OutcomeClassTimeout ToolOutcomeClass = "timeout"

	// OutcomeClassInternal — unclassified error. NOT a license to
	// emit "internal" freely: this is the fallback ONLY when no
	// signal is available. Subcode: "unclassified".
	OutcomeClassInternal ToolOutcomeClass = "internal"
)

// AllOutcomeClasses lists the closed vocabulary in declaration order.
// Used by schema CHECK constraint and by Doctor rollups.
var AllOutcomeClasses = []ToolOutcomeClass{
	OutcomeClassOk,
	OutcomeClassValidation,
	OutcomeClassNotFound,
	OutcomeClassConflict,
	OutcomeClassDegraded,
	OutcomeClassSubstrate,
	OutcomeClassIntegration,
	OutcomeClassTimeout,
	OutcomeClassInternal,
}

// ToolOutcome is the structured classification layered next to
// `result_status` (success|error) and the human-readable
// `error_message`. Callers populate it; readers MUST NOT trust
// outcome_code for routing decisions (it's diagnostic only).
type ToolOutcome struct {
	Class ToolOutcomeClass
	Code  string
}

// NewOutcome constructs a ToolOutcome. Code is bounded: empty
// allowed only when Class == OutcomeClassOk. Other classes require a
// non-empty diagnostic code.
func NewOutcome(class ToolOutcomeClass, code string) ToolOutcome {
	if class == OutcomeClassOk && code == "" {
		return ToolOutcome{Class: class, Code: ""}
	}
	if code == "" {
		code = "unclassified"
	}
	return ToolOutcome{Class: class, Code: code}
}

// IsSuccess reports whether the outcome represents a successful
// operation for aggregate counting purposes. ok + degraded are both
// "something useful was returned" so count as success.
func (o ToolOutcome) IsSuccess() bool {
	return o.Class == OutcomeClassOk || o.Class == OutcomeClassDegraded
}

// EqualString returns true when the outcome is logically equivalent
// for query purposes (matches on class, ignores code).
func (o ToolOutcome) EqualString(other ToolOutcome) bool {
	return o.Class == other.Class
}

// ClassifyFn is the contract for the per-tool classification seam.
// Each tool may register a classifier that recognises its specific
// failure modes without forcing every error to bubble through a
// common switch.
type ClassifyFn func(err error) (ToolOutcomeClass, string)

// ValidClass reports whether class is in the closed vocabulary. Used
// by the schema CHECK constraint and by readers that trust the
// writer's classification.
func ValidClass(class ToolOutcomeClass) bool {
	for _, c := range AllOutcomeClasses {
		if c == class {
			return true
		}
	}
	return false
}

// ClassifyError walks a small cascade of typed signals to assign
// the most specific class available. The fallback path is
// heuristic on the message text; that is intentional for the final
// safety net but not the primary classification source.
//
// In practice the dispatcher path should pass through a ClassifyFn
// that recognises the local vocabulary. ClassifyError here handles
// the small set of go-stdlib error sentinels plus the usererror
// prefix characters emitted by usererror.Error / Warn.
func ClassifyError(err error) (ToolOutcomeClass, string) {
	if err == nil {
		return OutcomeClassOk, ""
	}
	// RUNTIME OUTCOME WIRING (2026-09-21): use real stdlib sentinels
	// (context.Canceled / context.DeadlineExceeded) so wrapping
	// preserves the typed signature. The previous local sentinels
	// only matched against themselves, not against wrapped
	// production errors.
	if errors.Is(err, context.Canceled) {
		return OutcomeClassTimeout, "context_deadline"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeClassTimeout, "context_deadline"
	}
	if errors.Is(err, sqlNoRowsSentinel) {
		return OutcomeClassNotFound, "artifact_not_found"
	}
	// RUNTIME OUTCOME WIRING (2026-09-21): typed sentinel recognition
	// BEFORE the string-prefix fallback. Brief §3 priority: typed
	// error → sentinel → typed → string fallback. ErrInvalidWorkTransition
	// is the canonical typed sentinel for state-machine rejections;
	// promoting it to a typed class here means future conflict sites
	// anywhere in the codebase classify correctly without re-typing
	// message strings.
	if errors.Is(err, ErrInvalidWorkTransition) {
		return OutcomeClassConflict, "state_transition_invalid"
	}
	msg := err.Error()
	// Bound checks — only inspect the head so we don't speculate on
	// the variable tail of a stack trace. Each check is intentionally
	// narrow.
	switch {
	case strings.HasPrefix(msg, "❌ "):
		// usererror.Error — caller-facing message; treat as validation
		// by default since the canonical use in handlers is to reject
		// a malformed call. Tools that emit substrate errors via
		// usererror.Error override via ClassifyFn.
		return OutcomeClassValidation, "usererror_rejected"
	case strings.HasPrefix(msg, "[!] "):
		return OutcomeClassInternal, "warn_unclassified"
	case strings.Contains(msg, "missing required field"):
		return OutcomeClassValidation, "missing_required_field"
	case strings.Contains(msg, "Valid actions include"):
		return OutcomeClassValidation, "unknown_action"
	case strings.Contains(msg, " is required"):
		// RUNTIME OUTCOME WIRING (2026-09-21): "<param> is required"
		// is the most common validation form across tools that
		// don't use the JSON-schema "missing required field" path.
		// Capturing it here keeps unclassified noise down without
		// depending on per-tool override registration.
		return OutcomeClassValidation, "missing_required_field"
	case strings.Contains(msg, "not found"), strings.HasSuffix(msg, "does not exist"):
		return OutcomeClassNotFound, "artifact_not_found"
	case strings.Contains(msg, "SQLITE_BUSY"), strings.Contains(msg, "database is locked"):
		return OutcomeClassSubstrate, "wal_busy_timeout"
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "Client.Timeout"):
		return OutcomeClassTimeout, "context_deadline"
	case strings.Contains(msg, "no such table"), strings.Contains(msg, "no such column"):
		return OutcomeClassSubstrate, "substrate_schema"
	case strings.Contains(msg, "integrity check"):
		return OutcomeClassSubstrate, "integrity_check_failed"
	}
	return OutcomeClassInternal, "unclassified"
}

// contextCanceledSentinel is the canonical stdlib error for
// context-cancelled operations. Used to map context.Canceled into
// the timeout class without parsing message text.
var contextCanceledSentinel = errors.New("context canceled")

// contextDeadlineSentinel — likewise for context.DeadlineExceeded.
// Both declared as values (not errors.Is against the stdlib package)
// to keep this module testable in isolation.
var contextDeadlineSentinel = errors.New("context deadline exceeded")

// sqlNoRowsSentinel — database/sql.ErrNoRows equivalent for tests
// of the classification. The handler path uses errors.Is against
// the actual sql.ErrNoRows via the imports that wrap this; the
// sentinel here is for the standalone classification tests.
var sqlNoRowsSentinel = errors.New("sql: no rows in result set")
