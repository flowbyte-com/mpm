// wake_legacy_metadata_safety_test.go — Item 4.
//
// ResolveWake's CASE+json_extract matcher must be safe against every
// metadata shape an existing DB might contain, including
// arbitrarily-corrupted text. The matcher must:
//  1. treat NULL metadata as a wake row (legacy compat),
//  2. treat empty-string metadata as a wake row,
//  3. treat JSON `{}` as a wake row (json_extract returns NULL),
//  4. treat valid JSON with no `kind` key as a wake row,
//  5. treat malformed/non-JSON metadata as a wake row (json_extract
//     returns NULL on parse error rather than raising a SQL error
//     that becomes an internal failure),
//  6. preserve the cascade / cascade_summary / notification kind
//     values as wake rows,
//  7. reject `cron` and any unrecognized kind as
//     `(false, "not_a_wake")`.
package internal

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLegacyMetadataSafety exercises every realistic pre-fix metadata
// shape against ResolveWake. Each case inserts a row directly with the
// given metadata, then asserts that ResolveWake returns the
// documented outcome — wake row accepted with no error, or scheduled-
// task row rejected with not_a_wake.
func TestLegacyMetadataSafety(t *testing.T) {
	cases := []struct {
		name     string
		metadata any
		wantWake bool
	}{
		{"nil_metadata_legacy", nil, true},
		{"empty_string_metadata", "", true},
		{"empty_json_metadata", "{}", true},
		{"valid_json_no_kind", `{"source":"legacy-v1","weight":0.5}`, true},
		{"malformed_json", `not-actually-json{`, true},
		{"valid_json_kind_cascade", `{"kind":"cascade","source":"x"}`, true},
		{"valid_json_kind_cascade_summary", `{"kind":"cascade_summary"}`, true},
		{"valid_json_kind_notification", `{"kind":"notification"}`, true},
		{"valid_json_kind_cron", `{"kind":"cron","directive_id":"d-1"}`, false},
		{"valid_json_kind_other", `{"kind":"other-unknown"}`, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dm, err := NewDatabaseManager(t.TempDir())
			require.NoError(t, err)
			defer dm.Close()

			_, err = dm.db.Exec(
				`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
				 VALUES (?, ?, ?, NULL, NULL, 0, ?, ?)`,
				"legacy-meta-"+tc.name, timeUnix(t, 3600), "case: "+tc.name, "test-agent", tc.metadata,
			)
			require.NoError(t, err)

			resolved, status, err := dm.ResolveWake("legacy-meta-"+tc.name, "reconciled", "")
			if tc.wantWake {
				require.NoError(t, err,
					"legacy metadata %v must be accepted as a wake row; got status=%q",
					tc.metadata, status)
				require.True(t, resolved)
				require.Equal(t, "resolved", status)
			} else {
				require.Error(t, err,
					"non-wake kind metadata %v must be rejected",
					tc.metadata)
				require.False(t, resolved)
				require.Equal(t, "not_a_wake", status)
			}
		})
	}
}

// TestLegacyMetadataSafety_NoSQLPanic guards against a malformed
// metadata value living in the column. SQLite's json_extract is
// documented to return NULL on malformed JSON (per
// https://sqlite.org/json1.html section 6). The CASE wrapper
// additionally short-circuits on NULL/empty inputs. A SQL error
// here would surface as lookup_failed from ResolveWake, which is
// the failure mode this test pins closed.
func TestLegacyMetadataSafety_NoSQLPanic(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	dangerous := []string{
		`{`,
		`}`,
		`null`,
		`"unterminated`,
		`{"key":}`,
		"\x00\x01\x02",
	}
	for i, raw := range dangerous {
		_, err = dm.db.Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
			 VALUES (?, ?, ?, NULL, NULL, 0, ?, ?)`,
			fmt.Sprintf("legacy-danger-%d", i), timeUnix(t, 3600), fmt.Sprintf("danger: %d", i), "test-agent", raw,
		)
		require.NoError(t, err)
	}

	for i := range dangerous {
		_, _, err = dm.ResolveWake(fmt.Sprintf("legacy-danger-%d", i), "reconciled", "")
		if err != nil {
			msg := err.Error()
			require.NotContains(t, msg, "malformed JSON",
				"json_extract must tolerate malformed metadata, got: %v", err)
			require.NotContains(t, msg, "no such function",
				"json_extract must be available, got: %v", err)
		}
	}
}
