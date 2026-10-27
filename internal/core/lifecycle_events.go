// lifecycle_events.go — the durable mpm-event/0.1 store.
//
// WHY THIS FILE EXISTS.
//
// mpm-event/0.1 (docs/dev/event-contract.md) defines an observation
// envelope: a frozen field set, a closed event-type vocabulary, a strict
// per-event identity, a structural allowlist for the only field bag
// (`meta`), and an ingest contract (§3.10): validate → classify → dedup →
// stamp → store → receipt, with an observable rejection counter.
//
// The substrate had NOWHERE to put those observations. §6.1 deferred the
// storage question until it could be answered against a measured gap; the
// gap is now measured (P4 produces real envelopes) and no existing table
// fits. The reasoning per candidate is in
// docs/dev/session-a-p5-lifecycle-ingest.md §1.1; the short version is
// that `tool_invocations` is the substrate's audit of its OWN dispatch
// (and its CHECK forbids the envelope's `unknown`), `evidence` carries
// epistemic weight an observation has not earned, `system_audit_log` has no
// unique event identity and therefore no dedup, and `memories` would be
// automatic promotion — which M5 forbids.
//
// WHAT THIS FILE DOES NOT DO.
//
//   * It does not promote. Capturing an observation creates no memory, no
//     lesson, no decision and no theory (§3.9 rule 2). The promotion path
//     is unchanged and still runs through review.
//   * It does not decide what is worth remembering.
//   * It does not prune. §3.8 forbids a retention policy until storage and
//     scheduler behaviour have been demonstrated.
//
// THE TRUST INVARIANT.
//
// `trust` is 'observation' and only 'observation' (§3.7). There is no
// 'trusted' value at the event layer: a trusted artifact is a memory, a
// lesson or a decision, not an event. The CHECK constraint enforces it.
//
// THE host_call_id RULE.
//
// A host correlation id is NEVER an event identity and NEVER an
// independence signal. Two stored envelopes that share a `host_call_id` are
// ONE observation, not two independent corroborations. The value is stored
// verbatim inside `meta` so that a reader can pair a "before" with an
// "after"; it is never used as a key, a uniqueness constraint, or a
// confidence input. This is the core-side half of amendment A1.

package internal

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
	"unicode/utf8"
)

// ── The frozen schema version (§3.1) ────────────────────────────────

// LifecycleEventSchemaVersion is the only schema version the store
// accepts. The field set of mpm-event/0.1 is frozen on acceptance; any
// change to it — including an additive one — is mpm-event/0.2.
const LifecycleEventSchemaVersion = "mpm-event/0.1"

// ── The closed event-type vocabulary (§4) ────────────────────────────

// LifecycleEventTypes is the closed vocabulary of §4. Adding a type is a
// MINOR version bump and is additive.
//
// The vocabulary is enforced in Go, not by a SQL CHECK, because a CHECK
// would make every minor bump require a table rebuild — and §4 says a
// minor bump is additive.
var LifecycleEventTypes = []string{
	"session.started",
	"session.observed",
	"context.changed",
	"tool.outcome",
	"task.boundary",
	"compaction.started",
	"compaction.completed",
	"handoff.written",
	"critic.proposal",
}

// LifecycleEventTypeKnown reports whether t is in the closed vocabulary.
//
// An unknown type is REJECTED and counted — never mapped onto a
// neighbouring type (§3.9 rule 6, M2).
func LifecycleEventTypeKnown(t string) bool {
	for _, k := range LifecycleEventTypes {
		if k == t {
			return true
		}
	}
	return false
}

// ── The rejection reasons (§3.10 step 8) ─────────────────────────────

// The four reasons the CORE can observe. The fifth reason named by the
// contract — `unsupported_host_event` — is ADAPTER-side: the core never
// sees an unmapped host event, because the mapping happens in the adapter
// (§4, M2). The adapter counts it and reports it through its own counter
// surface. That is a split of responsibility, not a gap.
const (
	LifecycleRejectUnknownVersion   = "unknown_version"
	LifecycleRejectUnknownEventType = "unknown_event_type"
	LifecycleRejectInvalidEnvelope  = "invalid_envelope"
	LifecycleRejectLimitExceeded    = "limit_exceeded"
)

// LifecycleEventRejectionReasons is the closed set of reasons a core-side
// rejection can carry.
var LifecycleEventRejectionReasons = []string{
	LifecycleRejectUnknownVersion,
	LifecycleRejectUnknownEventType,
	LifecycleRejectInvalidEnvelope,
	LifecycleRejectLimitExceeded,
}

// LifecycleEventRejection is the typed error a rejected envelope produces.
//
// It is a TYPED sentinel rather than a formatted string so that the
// rejection reason survives into the substrate's dispatch audit as a
// machine-readable `outcome_code`: both audit hooks (cmd/mpm and
// cmd/mpm-mcp) classify through internal.ClassifyError, so the reason is
// countable without parsing prose.
//
// The message is "<reason>: <detail>" — the reason is the FIRST
// colon-delimited segment, so a caller can read it without a parser and a
// counter can group by it without a LIKE.
type LifecycleEventRejection struct {
	// Reason is one of LifecycleEventRejectionReasons.
	Reason string
	// Detail names the offending FIELD, never its content. A rejection
	// message must not become a channel through which envelope content
	// escapes into logs.
	Detail string
}

func (e *LifecycleEventRejection) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

// newLifecycleRejection builds a typed rejection.
func newLifecycleRejection(reason, detail string) *LifecycleEventRejection {
	return &LifecycleEventRejection{Reason: reason, Detail: detail}
}

// ── The meta allowlist (§3.8) ─────────────────────────────────────────

// lifecycleMetaStringBounds is the §3.8 meta allowlist: the ONLY keys that
// may cross the adapter boundary, and the byte bound for each.
//
// An allowlist, not a denylist. Nothing is collected and then
// pattern-matched for secrets — that ordering is what makes secret-scanning
// unreliable in the first place (§3.8).
//
// The keys whose bound is 0 are not strings: `overflow` is a boolean and
// `changed_paths_count` / `duration_ms` are non-negative integers.
var lifecycleMetaStringBounds = map[string]int{
	"host_event_type":     64,
	"host_tool_name":      128,
	"host_action":         64,
	"host_result":         64,
	"host_agent":          64,
	"host_model_provider": 64,
	"host_model_id":       128,
	"host_message_kind":   64,
	"host_error_code":     64,
	"compaction_kind":     32,
	"diff_stat_summary":   256,
	"payload_hash":        80,
	"host_call_id":        128,
	"host_message_id":     128,
	"overflow":            0,
	"changed_paths_count": 0,
	"duration_ms":         0,
}

// LifecycleEventMetaKeys returns the allowlisted meta keys, sorted, for
// discovery surfaces.
func LifecycleEventMetaKeys() []string {
	keys := make([]string, 0, len(lifecycleMetaStringBounds))
	for k := range lifecycleMetaStringBounds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── The hard limits (§3.8) ───────────────────────────────────────────

const (
	// lifecycleMetaFieldsMax is the maximum number of meta fields.
	lifecycleMetaFieldsMax = 16
	// lifecycleStringFieldMax is the per-field byte ceiling. The
	// per-key meta bounds are tighter; this catches the unbounded
	// top-level fields.
	lifecycleStringFieldMax = 512
	// lifecycleMetaBytesMax is the total encoded size of meta.
	lifecycleMetaBytesMax = 2048
	// lifecycleEnvelopeBytesMax is the total encoded size of the whole
	// envelope.
	lifecycleEnvelopeBytesMax = 8192
)

// ── The frozen envelope field set (§3.2) ─────────────────────────────

// lifecycleEnvelopeFields is the contract's frozen field set. A key
// outside it is a malformed envelope, not an unknown extension: §3.1 says
// the field set is frozen on acceptance and any change — even an additive
// one — is a new schema version.
//
// The `ingested_at_ms` key is present here because the core SETS it; an
// adapter that sets it is malformed (§3.2), and the validator rejects it
// on the way in.
var lifecycleEnvelopeFields = map[string]bool{
	"schema_version":       true,
	"event_id":             true,
	"idempotency":          true,
	"event_type":           true,
	"observation":          true,
	"observed_at_ms":       true,
	"ingested_at_ms":       true,
	"host_seq":             true,
	"host":                 true,
	"host_version":         true,
	"source":               true,
	"adapter_version":      true,
	"project_id":           true,
	"project_id_source":    true,
	"framework_name":       true,
	"framework_session_id": true,
	"mpm_session_id":       true,
	"invocation_id":        true,
	"task_id":              true,
	"parent_event_id":      true,
	"operation":            true,
	"result_status":        true,
	"outcome_class":        true,
	"outcome_code":         true,
	"trust":                true,
	"meta":                 true,
	"payload":              true,
}

// ── The formats ──────────────────────────────────────────────────────

var (
	// lifecycleStrictEventIDRe is the strict-mode id: "ev1_" + 64
	// lowercase hex characters (§3.5).
	lifecycleStrictEventIDRe = regexp.MustCompile(`^ev1_[0-9a-f]{64}$`)
	// lifecycleBestEffortEventIDRe is the best-effort id: "ev1b_" + 32
	// lowercase hex characters (§3.5).
	lifecycleBestEffortEventIDRe = regexp.MustCompile(`^ev1b_[0-9a-f]{32}$`)
	// lifecycleProjectIDRe is a DERIVED project id (§3.3).
	lifecycleProjectIDRe = regexp.MustCompile(`^pj_[0-9a-f]{32}$`)
	// lifecycleExplicitIDRe is an EXPLICITLY configured project id.
	lifecycleExplicitIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	// lifecycleOutcomeCodeRe is a bounded ASCII identifier (§3.7).
	lifecycleOutcomeCodeRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
)

// lifecycleProjectIDSources is the closed set of project-id sources
// (§3.3).
var lifecycleProjectIDSources = map[string]bool{
	"explicit-env":    true,
	"explicit-file":   true,
	"git-remote":      true,
	"host-git-remote": true,
	"unresolved":      true,
}

// ── The validated envelope ───────────────────────────────────────────

// LifecycleEnvelope is a validated mpm-event/0.1 envelope.
//
// A nil pointer means "not a usable envelope"; the error that accompanies
// it is always a *LifecycleEventRejection, so the caller can count the
// rejection by reason.
type LifecycleEnvelope struct {
	SchemaVersion string
	EventID       string
	Idempotency   string
	EventType     string
	Observation   string
	ObservedAtMs  int64
	HostSeq       *int64

	Host            string
	HostVersion     string
	Source          string
	AdapterVersion  string
	ProjectID       string
	ProjectIDSource string
	FrameworkName   string
	// FrameworkSessionID is never synthesized (§3.9 rule 4). Empty means
	// "the adapter reported none".
	FrameworkSessionID string
	// The explicit causality carriers (§3.6). Empty means "the adapter
	// reported none".
	MPMSessionID  string
	InvocationID  string
	TaskID        string
	ParentEventID string

	Operation    string
	ResultStatus string
	OutcomeClass string
	OutcomeCode  string

	Trust string

	// Meta is the adapter's allowlisted field bag, verbatim.
	Meta map[string]interface{}
	// MetaJSON is Meta re-encoded deterministically (Go's json encoder
	// sorts map keys), ready for the column.
	MetaJSON string
}

// ── Validation ───────────────────────────────────────────────────────

// ValidateLifecycleEnvelope validates a candidate mpm-event/0.1 envelope.
//
// The contract's rules, in the contract's order:
//
//  1. the field set is the frozen one — an unknown key is malformed;
//  2. `ingested_at_ms` must be absent (the core sets it; an adapter that
//     sets it is malformed);
//  3. `payload` must be null;
//  4. the identity is well formed for its declared idempotency mode;
//  5. the event type is in the closed vocabulary — never a
//     nearest-neighbour mapping;
//  6. the outcomes are coherent (§3.7): an observation with no operation
//     behind it carries no outcome fields, and an observation with an
//     operation behind it never claims `unknown`;
//  7. the meta bag is allowlisted and within its bounds;
//  8. the hard limits hold. An over-limit envelope is REJECTED WHOLE — it
//     is never truncated into a valid-looking event.
//
// The schema version is checked by the caller, because "this version is
// not recognised" is a different rejection from "this envelope is
// malformed" and the two are counted separately.
func ValidateLifecycleEnvelope(raw map[string]interface{}) (*LifecycleEnvelope, error) {
	if raw == nil {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope, "the event must be an object")
	}

	// 1. The frozen field set.
	for k := range raw {
		if !lifecycleEnvelopeFields[k] {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"unknown envelope field "+quoteField(k))
		}
	}

	// 2. The core sets ingested_at_ms; an adapter must not.
	if _, ok := raw["ingested_at_ms"]; ok {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"ingested_at_ms is set by the core and must be absent from an emitted envelope")
	}

	// 3. payload is reserved and always null in 0.1.
	if v, ok := raw["payload"]; ok && v != nil {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"payload must be null in mpm-event/0.1")
	}

	env := &LifecycleEnvelope{}

	// The schema version is carried in the envelope; the caller checks it
	// separately so that an unrecognised version is not reported as a
	// malformed envelope.
	env.SchemaVersion, _ = raw["schema_version"].(string)

	// 4. Identity.
	idempotency, err := reqString(raw, "idempotency")
	if err != nil {
		return nil, err
	}
	env.Idempotency = idempotency

	eventID, err := reqString(raw, "event_id")
	if err != nil {
		return nil, err
	}
	env.EventID = eventID

	switch idempotency {
	case "strict":
		if !lifecycleStrictEventIDRe.MatchString(eventID) {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"event_id must be ev1_<64 lowercase hex> in strict mode")
		}
	case "best_effort":
		if !lifecycleBestEffortEventIDRe.MatchString(eventID) {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"event_id must be ev1b_<32 lowercase hex> in best_effort mode")
		}
	default:
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"idempotency must be strict or best_effort")
	}

	// 5. The closed event-type vocabulary.
	eventType, err := reqString(raw, "event_type")
	if err != nil {
		return nil, err
	}
	if !LifecycleEventTypeKnown(eventType) {
		return nil, newLifecycleRejection(LifecycleRejectUnknownEventType,
			"event_type is not in the closed vocabulary")
	}
	env.EventType = eventType

	// observation: the adapter emits observed events only (§3.4).
	observation, err := reqString(raw, "observation")
	if err != nil {
		return nil, err
	}
	if observation != "observed" {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"observation must be observed in mpm-event/0.1")
	}
	env.Observation = observation

	// trust: observation and only observation (§3.7).
	trust, err := reqString(raw, "trust")
	if err != nil {
		return nil, err
	}
	if trust != "observation" {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"trust must be observation")
	}
	env.Trust = trust

	// observed_at_ms: a positive integer of unix milliseconds.
	observedAt, ok := asInt64(raw["observed_at_ms"])
	if !ok || observedAt <= 0 {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"observed_at_ms must be a positive integer")
	}
	env.ObservedAtMs = observedAt

	// host_seq: optional; when present it must be an integer.
	if v, present := raw["host_seq"]; present && v != nil {
		seq, ok := asInt64(v)
		if !ok {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"host_seq must be an integer when present")
		}
		env.HostSeq = &seq
	}

	// The bounded, non-empty top-level strings.
	for _, key := range []string{"host", "source", "adapter_version", "framework_name"} {
		val, err := reqString(raw, key)
		if err != nil {
			return nil, err
		}
		if len(val) > lifecycleStringFieldMax {
			return nil, newLifecycleRejection(LifecycleRejectLimitExceeded,
				key+" exceeds the per-field byte limit")
		}
		switch key {
		case "host":
			env.Host = val
		case "source":
			env.Source = val
		case "adapter_version":
			env.AdapterVersion = val
		case "framework_name":
			env.FrameworkName = val
		}
	}

	// host_version is OPTIONAL. The contract's required-field list (§3.2)
	// names `host`, `source` and `adapter_version` but not `host_version`,
	// so the store must not reject a conforming envelope that omits it.
	// The reference adapter always supplies it.
	if v, present := raw["host_version"]; present && v != nil {
		val, ok := v.(string)
		if !ok {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"host_version must be a string when present")
		}
		if len(val) > lifecycleStringFieldMax {
			return nil, newLifecycleRejection(LifecycleRejectLimitExceeded,
				"host_version exceeds the per-field byte limit")
		}
		env.HostVersion = val
	}

	// project_id: never fabricated (§3.9 rule 5).
	if v, present := raw["project_id"]; present && v != nil {
		val, ok := v.(string)
		if !ok {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"project_id must be a string or null")
		}
		if !lifecycleProjectIDRe.MatchString(val) && !lifecycleExplicitIDRe.MatchString(val) {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"project_id is not a known project-id form")
		}
		env.ProjectID = val
	}
	if v, present := raw["project_id_source"]; present && v != nil {
		val, ok := v.(string)
		if !ok {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"project_id_source must be a string or null")
		}
		if !lifecycleProjectIDSources[val] {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"project_id_source is not a known source")
		}
		env.ProjectIDSource = val
	}
	// A reported project id must say where it came from, and a reported
	// source must agree with the id's presence.
	if env.ProjectID != "" && env.ProjectIDSource == "" {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"project_id was reported without a project_id_source")
	}
	if env.ProjectIDSource != "" {
		if (env.ProjectID == "") != (env.ProjectIDSource == "unresolved") {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"project_id and project_id_source disagree")
		}
	}

	// The nullable identity strings.
	for _, key := range []string{"framework_session_id", "mpm_session_id", "invocation_id", "task_id", "parent_event_id"} {
		v, present := raw[key]
		if !present || v == nil {
			continue
		}
		val, ok := v.(string)
		if !ok {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				key+" must be a string or null")
		}
		if val == "" {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				key+" must be absent rather than empty")
		}
		if len(val) > lifecycleStringFieldMax {
			return nil, newLifecycleRejection(LifecycleRejectLimitExceeded,
				key+" exceeds the per-field byte limit")
		}
		switch key {
		case "framework_session_id":
			env.FrameworkSessionID = val
		case "mpm_session_id":
			env.MPMSessionID = val
		case "invocation_id":
			env.InvocationID = val
		case "task_id":
			env.TaskID = val
		case "parent_event_id":
			env.ParentEventID = val
		}
	}

	// 6. Outcomes (§3.7).
	operation, err := reqString(raw, "operation")
	if err != nil {
		return nil, err
	}
	if operation != "present" && operation != "absent" {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"operation must be present or absent")
	}
	env.Operation = operation

	resultStatus, err := reqString(raw, "result_status")
	if err != nil {
		return nil, err
	}
	switch resultStatus {
	case "success", "error", "unknown":
	default:
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"result_status is not a known value")
	}
	env.ResultStatus = resultStatus

	outcomeClass, hasOutcomeClass, err := optString(raw, "outcome_class")
	if err != nil {
		return nil, err
	}
	outcomeCode, hasOutcomeCode, err := optString(raw, "outcome_code")
	if err != nil {
		return nil, err
	}
	if hasOutcomeClass {
		env.OutcomeClass = outcomeClass
	}
	if hasOutcomeCode {
		env.OutcomeCode = outcomeCode
	}

	if operation == "absent" {
		// An observation with no operation behind it must not assert an
		// outcome (§3.7).
		if resultStatus != "unknown" {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"an observation with no operation must not assert an outcome")
		}
		if hasOutcomeClass || hasOutcomeCode {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"outcome_class/outcome_code must be absent when operation is absent")
		}
	} else {
		if resultStatus == "unknown" {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"unknown result_status is legal only when operation is absent")
		}
		if !hasOutcomeClass {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"outcome_class must be present when operation is present")
		}
		if !ValidClass(ToolOutcomeClass(outcomeClass)) {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"outcome_class is not a member of the ToolOutcomeClass enum")
		}
		if outcomeClass == string(OutcomeClassOk) && hasOutcomeCode {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"outcome_code must be absent when outcome_class is ok")
		}
		if hasOutcomeCode && !lifecycleOutcomeCodeRe.MatchString(outcomeCode) {
			return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"outcome_code must be a bounded identifier")
		}
	}

	// 7. The meta bag.
	meta, metaJSON, err := validateLifecycleMeta(raw["meta"])
	if err != nil {
		return nil, err
	}
	env.Meta = meta
	env.MetaJSON = metaJSON

	// 8. The whole-envelope limit.
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"the envelope is not encodable")
	}
	if len(encoded) > lifecycleEnvelopeBytesMax {
		return nil, newLifecycleRejection(LifecycleRejectLimitExceeded,
			"the envelope exceeds the whole-envelope byte limit")
	}

	return env, nil
}

// validateLifecycleMeta validates the only field bag (§3.8) and returns
// it with its deterministic JSON encoding.
func validateLifecycleMeta(v interface{}) (map[string]interface{}, string, error) {
	if v == nil {
		return map[string]interface{}{}, "{}", nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, "", newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"meta must be an object")
	}
	if len(m) > lifecycleMetaFieldsMax {
		return nil, "", newLifecycleRejection(LifecycleRejectLimitExceeded,
			"meta has more fields than the limit allows")
	}
	for k, val := range m {
		bound, allowed := lifecycleMetaStringBounds[k]
		if !allowed {
			// The allowlist is enforced at the boundary. A key that is
			// not on it is not "mostly harmless": it is a field that
			// was not permitted to cross.
			return nil, "", newLifecycleRejection(LifecycleRejectInvalidEnvelope,
				"meta contains a key that is not allowlisted")
		}
		switch k {
		case "overflow":
			if _, ok := val.(bool); !ok {
				return nil, "", newLifecycleRejection(LifecycleRejectInvalidEnvelope,
					"meta.overflow must be a boolean")
			}
		case "changed_paths_count", "duration_ms":
			n, ok := asInt64(val)
			if !ok || n < 0 {
				return nil, "", newLifecycleRejection(LifecycleRejectInvalidEnvelope,
					"meta."+k+" must be a non-negative integer")
			}
		default:
			s, ok := val.(string)
			if !ok {
				return nil, "", newLifecycleRejection(LifecycleRejectInvalidEnvelope,
					"meta."+k+" must be a string")
			}
			if len(s) > bound {
				return nil, "", newLifecycleRejection(LifecycleRejectLimitExceeded,
					"meta."+k+" exceeds its byte bound")
			}
			if !utf8.ValidString(s) {
				return nil, "", newLifecycleRejection(LifecycleRejectInvalidEnvelope,
					"meta."+k+" is not valid UTF-8")
			}
		}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, "", newLifecycleRejection(LifecycleRejectInvalidEnvelope,
			"meta is not encodable")
	}
	if len(encoded) > lifecycleMetaBytesMax {
		return nil, "", newLifecycleRejection(LifecycleRejectLimitExceeded,
			"meta exceeds its total byte limit")
	}
	return m, string(encoded), nil
}

// ── The ingest (§3.10) ───────────────────────────────────────────────

// LifecycleEventReceipt is the read-backed receipt the ingest returns
// (§3.10 step 7).
type LifecycleEventReceipt struct {
	// EventID is the STORED event id, read back from the row.
	EventID string `json:"event_id"`
	// IngestedAtMs is the STORED ingestion timestamp, read back from
	// the row. For a duplicate it is the ORIGINAL timestamp, not the
	// timestamp of the second delivery.
	IngestedAtMs int64 `json:"ingested_at_ms"`
	// Duplicate is true when the event_id was already stored.
	Duplicate bool `json:"duplicate"`
}

// IngestLifecycleEvent validates, deduplicates, stamps and stores one
// mpm-event/0.1 observation, and returns a READ-BACK receipt.
//
// The receipt's new-or-duplicate determination comes from a read-back of
// the stored row, never from the INSERT's own result (§6.4, Substrate
// Defense Triad rule 3). The mechanism is a single statement:
//
//	INSERT … ON CONFLICT(event_id) DO NOTHING RETURNING event_id, ingested_at_ms
//
// A returned row IS the stored row's content — the write and the read-back
// are the same statement — so the receipt's timestamp is what the row
// actually holds. When nothing comes back the event_id already existed,
// and the ORIGINAL timestamp is read back with a plain SELECT.
//
// If the INSERT neither stored a row nor found a pre-existing one, that is
// a FAILURE and it is returned as an error. A silent write is never
// reported as success.
//
// No counter rows are persisted and no promotion happens: capturing an
// observation creates no memory, no lesson, no decision and no theory
// (§3.9 rule 2).
func IngestLifecycleEvent(db *sql.DB, raw map[string]interface{}) (LifecycleEventReceipt, error) {
	if db == nil {
		return LifecycleEventReceipt{}, errors.New("lifecycle ingest: no database")
	}

	// A nil event is a malformed envelope, not an unknown version. It is
	// checked before the version so the reason names the real fault.
	if raw == nil {
		return LifecycleEventReceipt{}, newLifecycleRejection(
			LifecycleRejectInvalidEnvelope, "the event must be an object")
	}

	// The version check lives here, in the version authority, and it is a
	// REJECTION rather than an ignore: the receipt is the only success
	// signal in this protocol, so returning success for an event that was
	// not stored would be exactly the silent drop §3.10 step 8 exists to
	// prevent. "Ignore and count" remains correct for a CONSUMER that
	// meets a version it does not recognise — that is the adapter.
	schemaVersion, _ := raw["schema_version"].(string)
	if schemaVersion != LifecycleEventSchemaVersion {
		return LifecycleEventReceipt{}, newLifecycleRejection(
			LifecycleRejectUnknownVersion,
			"schema_version must be "+LifecycleEventSchemaVersion)
	}

	env, err := ValidateLifecycleEnvelope(raw)
	if err != nil {
		return LifecycleEventReceipt{}, err
	}

	ingestedAtMs := time.Now().UnixMilli()

	var (
		hostSeq    interface{}
		projectID  interface{}
		projSource interface{}
		frameSess  interface{}
		mpmSess    interface{}
		invocation interface{}
		taskID     interface{}
		parentEv   interface{}
		hostVer    interface{}
		outClass   interface{}
		outCode    interface{}
	)
	if env.HostSeq != nil {
		hostSeq = *env.HostSeq
	}
	nullIfEmpty := func(s string) interface{} {
		if s == "" {
			return nil
		}
		return s
	}
	projectID = nullIfEmpty(env.ProjectID)
	projSource = nullIfEmpty(env.ProjectIDSource)
	frameSess = nullIfEmpty(env.FrameworkSessionID)
	mpmSess = nullIfEmpty(env.MPMSessionID)
	invocation = nullIfEmpty(env.InvocationID)
	taskID = nullIfEmpty(env.TaskID)
	parentEv = nullIfEmpty(env.ParentEventID)
	hostVer = nullIfEmpty(env.HostVersion)
	outClass = nullIfEmpty(env.OutcomeClass)
	outCode = nullIfEmpty(env.OutcomeCode)

	rows, err := db.Query(`
		INSERT INTO lifecycle_events (
			event_id, idempotency, schema_version, event_type, observation, trust,
			observed_at_ms, ingested_at_ms, host_seq,
			host, host_version, source, adapter_version,
			project_id, project_id_source, framework_name, framework_session_id,
			mpm_session_id, invocation_id, task_id, parent_event_id,
			operation, result_status, outcome_class, outcome_code, meta
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO NOTHING
		RETURNING event_id`,
		env.EventID, env.Idempotency, env.SchemaVersion, env.EventType,
		env.Observation, env.Trust,
		env.ObservedAtMs, ingestedAtMs, hostSeq,
		env.Host, hostVer, env.Source, env.AdapterVersion,
		projectID, projSource, env.FrameworkName, frameSess,
		mpmSess, invocation, taskID, parentEv,
		env.Operation, env.ResultStatus, outClass, outCode, env.MetaJSON,
	)
	if err != nil {
		return LifecycleEventReceipt{}, fmt.Errorf("lifecycle ingest: store the event: %w", err)
	}

	// The statement tells us whether it wrote the row or the event_id
	// already existed. It is NOT the receipt's source of truth: the
	// receipt is read back from the row below, with an independent
	// SELECT.
	wrote := false
	for rows.Next() {
		var ignored string
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return LifecycleEventReceipt{}, fmt.Errorf("lifecycle ingest: read back the stored event: %w", err)
		}
		wrote = true
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return LifecycleEventReceipt{}, fmt.Errorf("lifecycle ingest: read back the stored event: %w", rowsErr)
	}

	// The READ-BACK. This is an independent SELECT of the stored row —
	// not the INSERT's own output. It is the receipt's source of truth
	// for BOTH the event id and the ingestion timestamp, and it is what
	// makes the new-or-duplicate determination a READ rather than an
	// inference (§6.4, Substrate Defense Triad rule 3).
	//
	// For a duplicate, the row read back is the ORIGINAL one, so the
	// receipt carries the original ingestion timestamp rather than the
	// timestamp of the second delivery.
	var (
		storedID   string
		storedAtMs int64
	)
	if err := db.QueryRow(
		`SELECT event_id, ingested_at_ms FROM lifecycle_events WHERE event_id = ?`,
		env.EventID,
	).Scan(&storedID, &storedAtMs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Neither a write nor a pre-existing row. The insert did not
			// persist. Reporting "duplicate" here would be a lie, and
			// reporting success at all would be worse.
			return LifecycleEventReceipt{}, fmt.Errorf(
				"lifecycle ingest: the event %s was neither stored nor found pre-existing", env.EventID)
		}
		return LifecycleEventReceipt{}, fmt.Errorf("lifecycle ingest: read back the stored event: %w", err)
	}

	return LifecycleEventReceipt{
		EventID:      storedID,
		IngestedAtMs: storedAtMs,
		Duplicate:    !wrote,
	}, nil
}

// ── The counters (§3.10 step 8) ──────────────────────────────────────

// LifecycleEventCounters computes the observable counters.
//
// NOTHING IS PERSISTED FOR THE COUNTERS. Both families are computed from
// authoritative state at read time:
//
//   - the stored counts come from lifecycle_events — the events
//     themselves;
//   - the rejection counts come from tool_invocations — the substrate's
//     own dispatch audit. A rejected ingest IS a failed dispatch, and it
//     is already recorded with a typed outcome_code, so the reason is
//     countable without parsing prose and without a second source of
//     truth.
//
// Duplicates are deliberately NOT counted: a duplicate leaves no row (the
// event_id is the primary key), so a durable duplicate total is not
// derivable from authoritative state. The receipt tells the caller
// `duplicate: true` at the moment it matters.
func LifecycleEventCounters(db *sql.DB) (map[string]interface{}, error) {
	if db == nil {
		return nil, errors.New("lifecycle counters: no database")
	}
	out := map[string]interface{}{}

	// The stored events.
	byType := map[string]int64{}
	total := int64(0)
	rows, err := db.Query(`SELECT event_type, COUNT(*) FROM lifecycle_events GROUP BY event_type`)
	if err != nil {
		return nil, fmt.Errorf("lifecycle counters: count stored events: %w", err)
	}
	for rows.Next() {
		var t string
		var n int64
		if err := rows.Scan(&t, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("lifecycle counters: count stored events: %w", err)
		}
		byType[t] = n
		total += n
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return nil, fmt.Errorf("lifecycle counters: count stored events: %w", rowsErr)
	}
	out["stored_total"] = total
	out["stored_by_event_type"] = byType

	byVersion := map[string]int64{}
	rows2, err := db.Query(`SELECT schema_version, COUNT(*) FROM lifecycle_events GROUP BY schema_version`)
	if err != nil {
		return nil, fmt.Errorf("lifecycle counters: count stored versions: %w", err)
	}
	for rows2.Next() {
		var v string
		var n int64
		if err := rows2.Scan(&v, &n); err != nil {
			rows2.Close()
			return nil, fmt.Errorf("lifecycle counters: count stored versions: %w", err)
		}
		byVersion[v] = n
	}
	rows2Err := rows2.Err()
	rows2.Close()
	if rows2Err != nil {
		return nil, fmt.Errorf("lifecycle counters: count stored versions: %w", rows2Err)
	}
	out["stored_by_schema_version"] = byVersion

	// The rejections: failed ingest dispatches, grouped by their typed
	// outcome code. The four contract reasons are a subset of what can
	// appear here — a storage failure classifies as 'internal', and
	// reporting it is more honest than hiding it behind a lifecycle
	// reason.
	byReason := map[string]int64{}
	rejectedTotal := int64(0)
	rows3, err := db.Query(`
		SELECT outcome_code, COUNT(*)
		FROM tool_invocations
		WHERE tool_name = 'mpm_system'
		  AND action = 'event_ingest'
		  AND result_status = 'error'
		GROUP BY outcome_code`)
	if err != nil {
		return nil, fmt.Errorf("lifecycle counters: count rejections: %w", err)
	}
	for rows3.Next() {
		var code string
		var n int64
		if err := rows3.Scan(&code, &n); err != nil {
			rows3.Close()
			return nil, fmt.Errorf("lifecycle counters: count rejections: %w", err)
		}
		if code == "" {
			code = "unclassified"
		}
		byReason[code] = n
		rejectedTotal += n
	}
	rows3Err := rows3.Err()
	rows3.Close()
	if rows3Err != nil {
		return nil, fmt.Errorf("lifecycle counters: count rejections: %w", rows3Err)
	}
	out["rejected_total"] = rejectedTotal
	out["rejected_by_reason"] = byReason

	return out, nil
}

// ── Small helpers ────────────────────────────────────────────────────

// reqString reads a required, non-empty string field.
func reqString(m map[string]interface{}, key string) (string, error) {
	v, present := m[key]
	if !present || v == nil {
		return "", newLifecycleRejection(LifecycleRejectInvalidEnvelope, key+" is required")
	}
	s, ok := v.(string)
	if !ok {
		return "", newLifecycleRejection(LifecycleRejectInvalidEnvelope, key+" must be a string")
	}
	if s == "" {
		return "", newLifecycleRejection(LifecycleRejectInvalidEnvelope, key+" must not be empty")
	}
	return s, nil
}

// optString reads an optional string field. The second return value is
// false when the key is absent or null.
func optString(m map[string]interface{}, key string) (string, bool, error) {
	v, present := m[key]
	if !present || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, newLifecycleRejection(LifecycleRejectInvalidEnvelope, key+" must be a string or null")
	}
	return s, true, nil
}

// asInt64 coerces a JSON number to an int64, refusing a fractional value.
func asInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// quoteField quotes a field name for an error message.
func quoteField(s string) string {
	return `"` + s + `"`
}
