package config

// timeout_test.go — the timeout policy's truth table.
//
// The policy is short enough to state in one line, and short enough to
// be reimplemented wrongly. These tests are the one place that states
// it, and they are the reference for every reader of a configured
// value.
//
// The policy:
//
//	a POSITIVE configured value is used exactly as configured;
//	ZERO, NEGATIVE or ABSENT falls back to DefaultTimeoutSeconds.

import "testing"

// TestEffectiveTimeoutSeconds_Policy is the truth table.
//
// The negative case is the one that matters most and is the one a
// naive `if configured > 0` at each call site would get wrong
// somewhere: a negative duration is not "no timeout configured", it is
// an INVALID value, and Go's http.Client treats a non-positive Timeout
// as UNLIMITED. So an operator who typed `-5` would silently get an
// unbounded request rather than the error they expected.
func TestEffectiveTimeoutSeconds_Policy(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		want       int
	}{
		{name: "absent (zero) falls back to the default", configured: 0, want: DefaultTimeoutSeconds},
		{name: "a negative value falls back to the default", configured: -5, want: DefaultTimeoutSeconds},
		{name: "a large negative value falls back to the default", configured: -1, want: DefaultTimeoutSeconds},
		{name: "one second is honoured", configured: 1, want: 1},
		{name: "an explicit value is honoured", configured: 45, want: 45},
		{name: "a large explicit value is honoured", configured: 3600, want: 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveTimeoutSeconds(tc.configured); got != tc.want {
				t.Errorf("EffectiveTimeoutSeconds(%d) = %d, want %d", tc.configured, got, tc.want)
			}
		})
	}
}

// TestDefaultTimeoutSeconds_Is300 pins the default itself.
//
// 300 is the value the example configuration in docs/CONFIGURATION.md
// carries, and the value a freshly-configured profile is written with.
// If the default changes, this test and the documentation change
// together — which is the point of pinning it.
func TestDefaultTimeoutSeconds_Is300(t *testing.T) {
	if DefaultTimeoutSeconds != 300 {
		t.Errorf("DefaultTimeoutSeconds = %d, want 300", DefaultTimeoutSeconds)
	}
}

// TestTimeoutSecondsIsValid pins the validity half of the policy.
//
// Only a positive value is valid. This is what the entry points consult
// to decide whether to REJECT a value the operator typed, and what the
// wizard consults to decide whether an existing value is worth
// preserving.
func TestTimeoutSecondsIsValid(t *testing.T) {
	cases := []struct {
		configured int
		want       bool
	}{
		{configured: 0, want: false},
		{configured: -1, want: false},
		{configured: -300, want: false},
		{configured: 1, want: true},
		{configured: 300, want: true},
	}
	for _, tc := range cases {
		if got := TimeoutSecondsIsValid(tc.configured); got != tc.want {
			t.Errorf("TimeoutSecondsIsValid(%d) = %v, want %v", tc.configured, got, tc.want)
		}
	}
}

// TestEffectiveTimeoutSeconds_AgreesWithValidity is the consistency
// check between the two halves of the policy.
//
// A value that the validity check rejects must be the value the
// resolver replaces. If these two ever disagree, some caller would
// preserve a value the runtime refuses to use — or reject a value the
// runtime would have honoured.
func TestEffectiveTimeoutSeconds_AgreesWithValidity(t *testing.T) {
	for _, configured := range []int{-300, -5, -1, 0, 1, 45, 300, 3600} {
		valid := TimeoutSecondsIsValid(configured)
		effective := EffectiveTimeoutSeconds(configured)
		if !valid && effective != DefaultTimeoutSeconds {
			t.Errorf("configured=%d is invalid but resolves to %d, want the default %d",
				configured, effective, DefaultTimeoutSeconds)
		}
		if valid && effective != configured {
			t.Errorf("configured=%d is valid but resolves to %d, want it unchanged",
				configured, effective)
		}
	}
}
