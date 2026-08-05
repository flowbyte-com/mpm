package capability

import (
	"testing"
)

// =============================================================================
// limits_test.go — ClampLimits + MemoryBytes tests (EX-4.1)
//
// Covers the contract documented in limits.go:
//
//   * ClampLimits reduces over-ceiling values to the ceiling.
//   * ClampLimits leaves under-ceiling values untouched.
//   * ClampLimits passes zero and negative values through
//     (the caller's responsibility to have applied defaults).
//   * ClampLimits is idempotent.
//   * MemoryBytes converts MB → bytes correctly, including
//     zero and negative inputs.
// =============================================================================

// TestClampLimits_ReducesOverCeiling verifies that absurd
// metadata values get reduced to the documented ceiling.
func TestClampLimits_ReducesOverCeiling(t *testing.T) {
	in := ResourceLimits{
		MaxRuntimeMs:   24 * 60 * 60 * 1000, // 24 hours
		MaxMemoryMB:    1024 * 1024,         // 1 PiB
		MaxFDs:         1_000_000_000,       // a billion
		MaxOutputBytes: 1 << 40,             // 1 TiB
	}
	got := ClampLimits(in)
	ceil := LimitsCeiling()

	if got.MaxRuntimeMs != ceil.MaxRuntimeMs {
		t.Errorf("MaxRuntimeMs = %d, want %d (ceiling)", got.MaxRuntimeMs, ceil.MaxRuntimeMs)
	}
	if got.MaxMemoryMB != ceil.MaxMemoryMB {
		t.Errorf("MaxMemoryMB = %d, want %d (ceiling)", got.MaxMemoryMB, ceil.MaxMemoryMB)
	}
	if got.MaxFDs != ceil.MaxFDs {
		t.Errorf("MaxFDs = %d, want %d (ceiling)", got.MaxFDs, ceil.MaxFDs)
	}
	if got.MaxOutputBytes != ceil.MaxOutputBytes {
		t.Errorf("MaxOutputBytes = %d, want %d (ceiling)", got.MaxOutputBytes, ceil.MaxOutputBytes)
	}
}

// TestClampLimits_PassesUnderCeiling verifies that legitimate
// (small) limits are untouched. The clamp must not rebase
// small values upward.
func TestClampLimits_PassesUnderCeiling(t *testing.T) {
	in := ResourceLimits{
		MaxRuntimeMs:   5_000,
		MaxMemoryMB:    128,
		MaxFDs:         64,
		MaxOutputBytes: 1 * 1024 * 1024,
	}
	got := ClampLimits(in)
	if got != in {
		t.Errorf("under-ceiling limits were modified:\n got: %+v\nwant: %+v", got, in)
	}
}

// TestClampLimits_PassesZeroAndNegative verifies that the
// clamp doesn't try to be smart about zero / negative values.
// Those are the caller's bug to fix — the clamp only defends
// against absurd-but-parseable values.
func TestClampLimits_PassesZeroAndNegative(t *testing.T) {
	in := ResourceLimits{
		MaxRuntimeMs:   0,
		MaxMemoryMB:    -1,
		MaxFDs:         0,
		MaxOutputBytes: -1024,
	}
	got := ClampLimits(in)
	if got != in {
		t.Errorf("zero/negative limits were modified:\n got: %+v\nwant: %+v", got, in)
	}
}

// TestClampLimits_Idempotent verifies that applying the clamp
// twice yields the same result as once. This matters because
// the clamp runs in resolveLimits (the Executor's pipeline)
// — if a future code path also clamps, double-application
// must be a no-op.
func TestClampLimits_Idempotent(t *testing.T) {
	in := ResourceLimits{
		MaxRuntimeMs:   999_999_999,
		MaxMemoryMB:    999_999_999,
		MaxFDs:         999_999_999,
		MaxOutputBytes: 999_999_999,
	}
	once := ClampLimits(in)
	twice := ClampLimits(once)
	if once != twice {
		t.Errorf("ClampLimits is not idempotent:\nonce:  %+v\ntwice: %+v", once, twice)
	}
}

// TestClampLimits_DefaultsUnchanged verifies that the
// DefaultResourceLimits() (which are well under every
// ceiling) pass through the clamp untouched. This is the
// regression guard for EX-1's limits-resolution tests — if a
// future change tightens the ceiling below the defaults,
// those tests would otherwise still pass but the EX-4 clamp
// would silently mutate the defaults.
func TestClampLimits_DefaultsUnchanged(t *testing.T) {
	d := DefaultResourceLimits()
	c := ClampLimits(d)
	if d != c {
		t.Errorf("defaults were modified by clamp:\n got: %+v\nwant: %+v", c, d)
	}
}

// TestLimitsCeiling_DefaultsUnderEveryCeiling verifies that
// the defaults sit below every ceiling. This is the invariant
// that makes ClampLimits safe to apply unconditionally — if a
// future change ever puts a default above a ceiling, the
// clamp would silently mutate it and break callers that
// passed defaults directly without going through resolveLimits.
func TestLimitsCeiling_DefaultsUnderEveryCeiling(t *testing.T) {
	d := DefaultResourceLimits()
	c := LimitsCeiling()

	if d.MaxRuntimeMs > c.MaxRuntimeMs {
		t.Errorf("default MaxRuntimeMs (%d) > ceiling (%d)", d.MaxRuntimeMs, c.MaxRuntimeMs)
	}
	if d.MaxMemoryMB > c.MaxMemoryMB {
		t.Errorf("default MaxMemoryMB (%d) > ceiling (%d)", d.MaxMemoryMB, c.MaxMemoryMB)
	}
	if d.MaxFDs > c.MaxFDs {
		t.Errorf("default MaxFDs (%d) > ceiling (%d)", d.MaxFDs, c.MaxFDs)
	}
	if d.MaxOutputBytes > c.MaxOutputBytes {
		t.Errorf("default MaxOutputBytes (%d) > ceiling (%d)", d.MaxOutputBytes, c.MaxOutputBytes)
	}
}

// TestMemoryBytes_ConversionCorrect verifies the MB→bytes
// arithmetic for boundary inputs.
func TestMemoryBytes_ConversionCorrect(t *testing.T) {
	cases := []struct {
		mb   int64
		want int64
	}{
		{0, 0},         // zero is "do not set" per MemoryBytes contract
		{-1, 0},        // negative is treated as zero
		{1, 1024 * 1024},
		{512, 512 * 1024 * 1024},
		{4096, 4096 * 1024 * 1024}, // 4 GiB ceiling
	}
	for _, tc := range cases {
		got := MemoryBytes(tc.mb)
		if got != tc.want {
			t.Errorf("MemoryBytes(%d) = %d, want %d", tc.mb, got, tc.want)
		}
	}
}
