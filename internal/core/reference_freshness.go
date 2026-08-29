package internal

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Freshness classifies a reference document's authoritative state at a
// given point in time. Derived from existing schema fields (last_indexed,
// tags, import_reason) — no schema changes required. See
// mpm-agent-protocol.md §8 "REFERENCE FRESHNESS CONTRACT" for the
// canonical interpretation contract that agents must apply to each state.
type Freshness string

const (
	// FreshnessCurrent: safe to consult normally. Re-ingested recently or
	// explicitly tagged as verified/current by the operator.
	FreshnessCurrent Freshness = "current"

	// FreshnessStale: useful background; verify before relying. Either
	// explicitly tagged stale, or last_indexed older than
	// FreshnessAgeThreshold with no override signal.
	FreshnessStale Freshness = "stale"

	// FreshnessVersionBound: applicable only to the recorded version.
	// Either tagged version-bound:<X>/version:<X> or import_reason
	// identifies a specific upstream version.
	FreshnessVersionBound Freshness = "version-bound"

	// FreshnessHistorical: useful for understanding past state; not
	// current authority. Tagged historical/freshness:historical, or
	// import_reason indicates as-of date material.
	FreshnessHistorical Freshness = "historical"

	// FreshnessUnknown: no freshness signals available. Consult with
	// caution; do not treat as authoritative.
	FreshnessUnknown Freshness = "unknown"
)

// FreshnessAgeThreshold is the staleness cutoff for the age-based default
// path. References older than this without an explicit freshness signal
// are classified FreshnessStale. 90 days is the project default; see
// protocol §8 for the rationale.
const FreshnessAgeThreshold = 90 * 24 * 60 * 60 // 90 days in seconds

// ClassifyReferenceFreshness classifies a reference doc into a freshness
// state using only existing schema fields. Pure function — no DB access.
// Caller passes current time to keep tests deterministic.
//
// Signal precedence (first match wins):
//  1. Explicit tags: stale / current / verified / version-bound / version /
//     historical / freshness:*  (in scan order; tags are scanned in slice order)
//  2. import_reason: version-pattern / historical-pattern
//  3. Age fallback: last_indexed < FreshnessAgeThreshold → current;
//     older → stale. Future or unparseable → unknown.
//
// The function is the single source of truth for the freshness signal;
// the protocol §8 contract must remain consistent with this ordering.
func ClassifyReferenceFreshness(doc *ReferenceDoc, now time.Time) Freshness {
	// 1. Explicit operator tags win first.
	for _, raw := range doc.Tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case tag == "stale":
			return FreshnessStale
		case tag == "current" || tag == "verified" || tag == "freshness:current":
			return FreshnessCurrent
		case tag == "historical" || tag == "freshness:historical":
			return FreshnessHistorical
		case strings.HasPrefix(tag, "version-bound:") || strings.HasPrefix(tag, "version:"):
			return FreshnessVersionBound
		}
	}

	// 2. import_reason patterns.
	reason := strings.ToLower(strings.TrimSpace(doc.ImportReason))
	if reason != "" {
		if strings.HasPrefix(reason, "version:") || strings.HasPrefix(reason, "for ") || strings.Contains(reason, " v") {
			return FreshnessVersionBound
		}
		if strings.HasPrefix(reason, "historical") || strings.Contains(reason, "as-of ") {
			return FreshnessHistorical
		}
	}

	// 3. Age fallback on last_indexed (Unix epoch seconds, stored as
	// string in the struct). Empty / unparseable / future = unknown.
	if doc.LastIndexed != "" {
		idxSec, err := strconv.ParseInt(doc.LastIndexed, 10, 64)
		if err != nil {
			return FreshnessUnknown
		}
		age := now.Unix() - idxSec
		if age < 0 {
			return FreshnessUnknown // clock skew
		}
		if age > FreshnessAgeThreshold {
			return FreshnessStale
		}
		return FreshnessCurrent
	}

	return FreshnessUnknown
}

// ClassifyReferenceFreshnessFromFields is the wire-format convenience
// wrapper for ListReferences / SearchReferences / GetReference. SQLite
// stores tags as TEXT (JSON-encoded []string); this helper accepts that
// format directly so list-query handlers don't have to json.Unmarshal
// before each row. Malformed tags JSON falls back to import_reason +
// last_indexed without surfacing an error (best-effort, like the rest
// of the freshness pipeline).
func ClassifyReferenceFreshnessFromFields(tagsJSON, importReason, lastIndexed string, now time.Time) Freshness {
	var tags []string
	if tagsJSON != "" {
		// Tolerate malformed JSON; a real tag corruption will be visible
		// on the next full read. Don't surface it here.
		_ = json.Unmarshal([]byte(tagsJSON), &tags)
	}
	return ClassifyReferenceFreshness(&ReferenceDoc{
		Tags:         tags,
		ImportReason: importReason,
		LastIndexed:  lastIndexed,
	}, now)
}
