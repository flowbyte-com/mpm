// doctor_auth_report_test.go — the Doctor model row must tell the
// operator WHICH authentication problem they have.
//
// HAL-9001 Finding 1 produced a report that said "auth failed" for a
// configuration that was entirely correct. The corrective pass fixes
// the underlying request, but the report itself must also stop
// collapsing three distinct problems into one line:
//
//   - an invalid credential (fix: correct the key);
//   - an expired or unbilled subscription (fix: pay);
//   - a key without permission for the model (fix: change the model or
//     the key's scope).
//
// These assertions exercise probe fields introduced by the corrective
// pass, so against the previous behaviour they do not compile.

package main

import (
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm/cmd/mpm/probe"
)

// TestModelChecksFromProbes_ReportsTheAuthenticationSubKind pins that
// the authentication refusal sub-kind reaches the operator's report.
func TestModelChecksFromProbes_ReportsTheAuthenticationSubKind(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		want   string
		absent string
	}{
		{"invalid credential", "invalid_credential", "invalid credential", "subscription"},
		{"expired subscription", "subscription", "subscription expired or not billed", "invalid credential"},
		{"insufficient permission", "permission", "key not permitted for this model", "invalid credential"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := modelChecksFromProbes([]probe.ProbeResult{{
				Kind:         probe.ProbeKindGenerative,
				Provider:     "custom",
				Model:        "some-model",
				Status:       probe.ProbeAuthFailed,
				ErrorSummary: "provider refused the request",
				AuthFailure:  tc.kind,
			}})
			if len(checks) != 1 {
				t.Fatalf("got %d checks, want 1", len(checks))
			}
			if !strings.Contains(checks[0].Message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", checks[0].Message, tc.want)
			}
			if strings.Contains(checks[0].Message, tc.absent) {
				t.Errorf("message = %q, want it to distinguish the failure kind from %q", checks[0].Message, tc.absent)
			}
		})
	}
}

// TestModelChecksFromProbes_ReportsTheAuthenticationScheme pins that a
// healthy model row names the authentication scheme that worked.
//
// The operator's question after a first install is "how did it
// authenticate?". The answer should be visible without a debug log.
func TestModelChecksFromProbes_ReportsTheAuthenticationScheme(t *testing.T) {
	checks := modelChecksFromProbes([]probe.ProbeResult{{
		Kind:       probe.ProbeKindGenerative,
		Provider:   "custom",
		Model:      "MiniMax-M3",
		Status:     probe.ProbeHealthy,
		LatencyMs:  42,
		AuthScheme: "bearer",
	}})
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(checks))
	}
	if !strings.Contains(checks[0].Message, "bearer") {
		t.Errorf("message = %q, want it to name the bearer authentication scheme", checks[0].Message)
	}
}
