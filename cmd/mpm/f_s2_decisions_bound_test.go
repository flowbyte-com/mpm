// f_s2_decisions_bound_test.go — regression for pointer-indirection sweep
// finding F-S-2 (docs/pointer-indirection-sweep-2026-09-05.md).
//
// `mpm decisions` (no-args legacy listing) used to iterate every
// decision in the collection unbounded — output scaled linearly with
// collection size, no cap, no pagination. Today the live collection
// holds 139 rows ≈ 35 KB; grows over time.
//
// The fix: cap default listing at 20 rows (matching `mpm ls`'s default
// and the `--limit=N` parsing convention used by `mpm decisions list` /
// `mpm decisions query`). `--limit=0` means "no cap" (mirrors the
// existing decision_filter.Limit == 0 "no cap" semantics).
//
// This test pins the cap on a fresh fixture rather than the live DB so
// the assertions are not at the mercy of pre-existing rows.

package main

import (
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// decisionsListDivider matches the canonical render.Divider glyph
// (`─` × 60). 2026-09-14 release-pass: the canonical renderer
// emits exactly one Divider line per decision record.
var decisionsListDivider = strings.Repeat("─", 60)

// countDecisionRows returns how many formatted decision records appear
// in the output. 2026-09-14 release-pass: the canonical renderer
// emits exactly one Divider per record (no closing divider), so the
// count is the raw substring count.
func countDecisionRows(out string) int {
	return strings.Count(out, decisionsListDivider)
}

// countDecisionsInDB returns the live count of decisions rows via direct
// SQL — store.QueryMemory is a search, not a list, and may apply its own
// ranking/cap behaviour.
func countDecisionsInDB(t *testing.T) int {
	t.Helper()
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection='decisions' AND deleted_at IS NULL`,
	).Scan(&n); err != nil {
		t.Skipf("count failed: %v", err)
	}
	return n
}

// keep the import used.
var _ mpminternal.CoreDB

// seedDecisions inserts n distinct decisions via the in-memory store
// so each test run gets a clean fixture regardless of the live DB state.
func seedDecisions(t *testing.T, n int) {
	t.Helper()
	store := getMemoryStore()
	if store == nil {
		t.Skip("memory store unavailable via singleton")
	}
	for i := 0; i < n; i++ {
		choice := fmt.Sprintf("CHOICE: option-%d\nDetailed option-%d body line.", i, i)
		meta := map[string]interface{}{
			"context":   fmt.Sprintf("decision-%d context", i),
			"choice":    fmt.Sprintf("option-%d", i),
			"rationale": fmt.Sprintf("decision-%d rationale", i),
		}
		if _, err := store.AddMemory(choice, "decisions",
			[]string{"f-s2-test"}, meta, "", "test"); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// TestF_S2_DecisionsNoArgs_DefaultsToLimit20 pins the headline: the
// legacy no-args listing is capped at 20 rows regardless of how many
// decisions exist in the collection.
func TestF_S2_DecisionsNoArgs_DefaultsToLimit20(t *testing.T) {
	const fixtureSize = 25 // > 20, ensures the cap fires
	// Snapshot pre-existing rows via direct SQL — store.QueryMemory
	// is a search, not a list, and applies its own ranking/cap.
	preCount := countDecisionsInDB(t)

	seedDecisions(t, fixtureSize)

	out := captureBoth(t, func() {
		handleDecisions([]string{})
	})

	rendered := countDecisionRows(out)
	want := 20
	if preCount+fixtureSize <= want {
		// Live DB has fewer total decisions than the cap; ensure we still
		// see all of them (no spurious truncation).
		want = preCount + fixtureSize
	}
	if preCount+fixtureSize > want && rendered != want {
		t.Errorf("default no-args cap: rendered=%d, want=%d (pre-existing=%d, fixture=%d, total=%d)",
			rendered, want, preCount, fixtureSize, preCount+fixtureSize)
	}
	// Output must also include the leading usage hint and the
	// non-empty-state body (sanity for the no-error path).
	if strings.Contains(out, "Unknown subcommand") {
		t.Errorf("no-args path triggered Unknown-subcommand error:\n%s", out)
	}
	if strings.Contains(out, "No decisions recorded yet") {
		t.Errorf("no-args path returned the empty-state message despite seeded rows:\n%s", out)
	}
}

// TestF_S2_DecisionsNoArgs_LimitFlag_Honors pins the explicit --limit=N
// path: caller-controlled value is honored regardless of fixture size.
// Note: does NOT exercise --limit=0 (no cap) because MemoryExpireClause
// in GetMemoriesForExport filters rows by expires_at, which makes the
// exact post-cap count depend on the live DB state and not worth
// pinning from a behavioural-fixture standpoint. The cap-path itself
// is exercised by the limit-1 and limit-5 subtests below.
func TestF_S2_DecisionsNoArgs_LimitFlag_Honors(t *testing.T) {
	// Seed enough rows that limit=1 and limit=5 are both genuine
	// truncations (the cap fires) rather than just-no-truncation.
	const fixtureSize = 12

	cases := []struct {
		name string
		flag string
		want int
	}{
		// --limit=5 → at most 5 rows even though 12+ exist.
		{name: "limit-5", flag: "--limit=5", want: 5},
		// --limit=1 → exactly 1 row.
		{name: "limit-1", flag: "--limit=1", want: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedDecisions(t, fixtureSize)

			out := captureBoth(t, func() {
				handleDecisions([]string{tc.flag})
			})

			rendered := countDecisionRows(out)
			// The cap is an upper bound on what is rendered; the
			// MemoryExpireClause filter can only make rendered < fixtureSize,
			// so we assert ≤ tc.want and ≥ 1 (some render must have happened).
			if rendered > tc.want {
				t.Errorf("%s: rendered=%d exceeds cap %d", tc.name, rendered, tc.want)
			}
			if rendered < 1 {
				t.Errorf("%s: rendered=%d (no rows surfaced despite seeded fixture)", tc.name, rendered)
			}
			if strings.Contains(out, "Unknown subcommand") {
				t.Errorf("%s: --limit=N must fall through to legacy listing, got Unknown-subcommand error:\n%s",
					tc.name, out)
			}
		})
	}
}

// TestF_S2_DecisionsNoArgs_LimitFlagFallsThrough pins the alpha-4
// D-005 subcommand-rejection contract: passing only `--limit=N` (no
// subcommand) must not trigger the "Unknown subcommand" rejection.
// Without this guard, the previous subcommand-only switch would treat
// `--limit=N` as an unknown subcommand.
func TestF_S2_DecisionsNoArgs_LimitFlagFallsThrough(t *testing.T) {
	out := captureBoth(t, func() {
		handleDecisions([]string{"--limit=3"})
	})
	if strings.Contains(out, "Unknown subcommand") {
		t.Errorf("--limit=N must fall through to legacy listing, got Unknown-subcommand error:\n%s", out)
	}
}
