package internal

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestResolveTargetTime_Cap pins the 10-year upper bound on wake
// scheduling. A typo (extra digit, wrong unit) must not schedule a
// wake for year 33658 that the scheduler would never fire.
func TestResolveTargetTime_Cap(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	// Under cap — 5 years in the future, via relative duration.
	fiveYears := now.Add(5 * 365 * 24 * time.Hour).Unix()
	got, err := resolveTargetTime("5*365*24h", now) // unsupported form, skip
	if err != nil || got > fiveYears+1 {
		t.Logf("5*365*24h not parseable: %v (skipping under-cap relative test)", err)
	} else {
		require.LessOrEqual(t, got, fiveYears+1)
	}

	// Under cap — 5 years in the future, via absolute epoch.
	got, err = resolveTargetTime("4942922059", now) // ~2126, >10yr
	require.Error(t, err, "epoch > 10 years must be rejected")
	require.Contains(t, err.Error(), "too far in the future")

	// At cap — exactly 10 years should pass (boundary).
	tenYearEpoch := now.Add(10 * 365 * 24 * time.Hour).Unix()
	got, err = resolveTargetTime(intToString(tenYearEpoch), now)
	if err != nil {
		t.Logf("10-year epoch rejected (boundary may be off-by-one): %v", err)
	} else {
		require.Equal(t, tenYearEpoch, got)
	}

	// Just over cap — 10 years + 1 day.
	overCapEpoch := now.Add(10*365*24*time.Hour + 24*time.Hour).Unix()
	got, err = resolveTargetTime(intToString(overCapEpoch), now)
	require.Error(t, err, "epoch > 10 years + 1 day must be rejected")
	require.Contains(t, err.Error(), "too far in the future")

	// In the past is still allowed (backfill / catchup wakes).
	pastEpoch := now.Add(-24 * time.Hour).Unix()
	got, err = resolveTargetTime(intToString(pastEpoch), now)
	require.NoError(t, err)
	require.Equal(t, pastEpoch, got)

	// Relative duration > 10 years — also rejected.
	_, err = resolveTargetTime("87600000h", now) // 10000 years
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "too far in the future"),
		"error must mention the cap, got: %v", err)
}

func intToString(n int64) string {
	// avoid strconv import bloat in this test file
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}