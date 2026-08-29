package internal

import (
	"strconv"
	"testing"
	"time"
)

// TestClassifyReferenceFreshness is the table-driven contract for the
// reference freshness classifier. Step 18 of the alpha discoverability
// pass — every case below corresponds to one of the spec's documented
// freshness states. Pure-function tests (no DB) so they remain hermetic.
func TestClassifyReferenceFreshness(t *testing.T) {
	// Deterministic reference time: 2025-01-01 UTC. All ages are computed
	// relative to this so tests are stable across clock drift.
	now := time.Unix(1735689600, 0)
	const day = int64(86400)

	tests := []struct {
		name string
		doc  *ReferenceDoc
		want Freshness
	}{
		// ---- Fresh / Current ----
		{
			name: "current: explicit verified tag",
			doc: &ReferenceDoc{
				LastIndexed: strconv.FormatInt(now.Unix()-1*day, 10),
				Tags:        []string{"verified"},
			},
			want: FreshnessCurrent,
		},
		{
			name: "current: explicit current tag overrides stale age",
			doc: &ReferenceDoc{
				LastIndexed: strconv.FormatInt(now.Unix()-200*day, 10),
				Tags:        []string{"current"},
			},
			want: FreshnessCurrent,
		},
		{
			name: "current: recent last_indexed with no tags",
			doc: &ReferenceDoc{
				LastIndexed: strconv.FormatInt(now.Unix()-7*day, 10),
			},
			want: FreshnessCurrent,
		},

		// ---- Stale ----
		{
			name: "stale: last_indexed older than threshold, no signals",
			doc: &ReferenceDoc{
				LastIndexed: strconv.FormatInt(now.Unix()-120*day, 10),
			},
			want: FreshnessStale,
		},
		{
			name: "stale: explicit stale tag overrides fresh age",
			doc: &ReferenceDoc{
				LastIndexed: strconv.FormatInt(now.Unix()-1*day, 10),
				Tags:        []string{"stale"},
			},
			want: FreshnessStale,
		},

		// ---- Version-Bound ----
		{
			name: "version-bound: tag version-bound:X",
			doc: &ReferenceDoc{
				Tags: []string{"version-bound:WordPress 6.7"},
			},
			want: FreshnessVersionBound,
		},
		{
			name: "version-bound: tag version:X",
			doc: &ReferenceDoc{
				Tags: []string{"version:6.7"},
			},
			want: FreshnessVersionBound,
		},
		{
			name: "version-bound: import_reason names a version (for X)",
			doc: &ReferenceDoc{
				ImportReason: "for WordPress 6.7",
				LastIndexed:  strconv.FormatInt(now.Unix()-1*day, 10),
			},
			want: FreshnessVersionBound,
		},

		// ---- Historical ----
		{
			name: "historical: explicit historical tag",
			doc: &ReferenceDoc{
				Tags: []string{"historical"},
			},
			want: FreshnessHistorical,
		},
		{
			name: "historical: tag freshness:historical",
			doc: &ReferenceDoc{
				Tags: []string{"freshness:historical"},
			},
			want: FreshnessHistorical,
		},

		// ---- Unknown ----
		{
			name: "unknown: empty last_indexed, no tags, no reason",
			doc:  &ReferenceDoc{},
			want: FreshnessUnknown,
		},
		{
			name: "unknown: future last_indexed (clock skew)",
			doc: &ReferenceDoc{
				LastIndexed: strconv.FormatInt(now.Unix()+5*day, 10),
			},
			want: FreshnessUnknown,
		},
		{
			name: "unknown: unparseable last_indexed",
			doc: &ReferenceDoc{
				LastIndexed: "not-a-number",
			},
			want: FreshnessUnknown,
		},

		{
			name: "priority: version-bound wins over historical when both present",
			doc: &ReferenceDoc{
				Tags: []string{"historical", "version-bound:foo"},
			},
			// First matching tag in scan order wins; historical is checked
			// before version-bound in ClassifyReferenceFreshness. This
			// documents the actual precedence.
			want: FreshnessHistorical,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyReferenceFreshness(tt.doc, now)
			if got != tt.want {
				t.Errorf("ClassifyReferenceFreshness() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClassifyReferenceFreshnessFromFields covers the JSON-tags-string
// wrapper used by ListReferences/SearchReferences/GetReference. SQLite
// stores tags as TEXT (JSON-encoded []string); the wire-format helpers
// must accept that format without requiring callers to json.Unmarshal
// before every list query.
func TestClassifyReferenceFreshnessFromFields(t *testing.T) {
	now := time.Unix(1735689600, 0)
	const day = int64(86400)

	tests := []struct {
		name         string
		tagsJSON     string
		importReason string
		lastIndexed  string
		want         Freshness
	}{
		{
			name:        "fromFields: empty tags string falls back to age",
			tagsJSON:    "",
			lastIndexed: strconv.FormatInt(now.Unix()-1*day, 10),
			want:        FreshnessCurrent,
		},
		{
			name:        "fromFields: json-encoded tag list",
			tagsJSON:    `["verified","wordpress"]`,
			lastIndexed: strconv.FormatInt(now.Unix()-200*day, 10),
			want:        FreshnessCurrent, // explicit verified wins over stale age
		},
		{
			name:         "fromFields: version-bound via tag in JSON string",
			tagsJSON:     `["version-bound:WP 6.7"]`,
			importReason: "imported for WordPress work",
			want:         FreshnessVersionBound,
		},
		{
			name:        "fromFields: stale when no signals and age past threshold",
			tagsJSON:    `[]`,
			lastIndexed: strconv.FormatInt(now.Unix()-200*day, 10),
			want:        FreshnessStale,
		},
		{
			name:        "fromFields: malformed tags JSON falls back to other fields",
			tagsJSON:    `not-valid-json`,
			lastIndexed: strconv.FormatInt(now.Unix()-1*day, 10),
			want:        FreshnessCurrent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyReferenceFreshnessFromFields(tt.tagsJSON, tt.importReason, tt.lastIndexed, now)
			if got != tt.want {
				t.Errorf("ClassifyReferenceFreshnessFromFields() = %q, want %q", got, tt.want)
			}
		})
	}
}
