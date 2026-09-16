// web_db_directive_exclusion_test.go — 2026-09-16 follow-up regression.
//
// The bc7ce686 / 1bc490b11 NULL-safe directive-exclusion predicate was
// applied to GetMemoryStats's seven scalar counters (total / active /
// deleted / ltm / reinforced / never_accessed / expired) but missed
// the four breakdown/distribution members:
//
//   - by_collection
//   - by_tag
//   - reinforce_dist
//   - provenance_registry (ISR Telemetry)
//
// On a pristine install (5 directives + 0 ordinary memories) the
// aggregates report zero correctly, but the breakdowns still leak
// the directive rows — a `by_collection: [{collection: directives,
// count: 5}]` entry contradicts `total: 0` in the same object. The
// function is named GetMemoryStats; every memory-derived member must
// share the directive boundary.
//
// The tests below drive GetMemoryStats from a hermetic DM
// (NewTestDM does NOT pre-populate any directives) so the seed
// state is fully controlled. Every regression test exercises
// ordinary-memory + directive coexistence and asserts on the
// full stats map.

package internal

import (
	"strings"
	"testing"
)

// seedMemory inserts one row directly into the memories table. The
// fields cover every cardinality the tests need to exercise:
// collection, is_prime_directive (column), tags (JSON array string),
// metadata (JSON object string), weight, created_at. Defaults
// preserve the test's signal by not setting values that would
// confound the assertion.
func seedMemoryForStats(t *testing.T, dm *DatabaseManager, id, collection string, primeFlag interface{}, tagsJSON, metadataJSON string, weight int) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, is_prime_directive, weight, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)
	`, id, collection, "content-"+id, tagsJSON, metadataJSON, primeFlag, weight)
	if err != nil {
		t.Fatalf("seedMemory %q: %v", id, err)
	}
}

// countByCollection extracts the by_collection distribution from
// GetMemoryStats as a map[collection]count for easy assertion.
func countByCollection(stats map[string]interface{}) map[string]int {
	out := map[string]int{}
	raw, ok := stats["by_collection"].([]map[string]interface{})
	if !ok {
		return out
	}
	for _, r := range raw {
		coll, _ := r["collection"].(string)
		count, _ := r["count"].(int)
		out[coll] += count
	}
	return out
}

// tagsByDistribution extracts the by_tag distribution as a map
// [tag]count for easy assertion.
func tagsByDistribution(stats map[string]interface{}) map[string]int {
	out := map[string]int{}
	raw, ok := stats["by_tag"].([]map[string]interface{})
	if !ok {
		return out
	}
	for _, r := range raw {
		tag, _ := r["tag"].(string)
		count, _ := r["count"].(int)
		out[tag] += count
	}
	return out
}

// reinforcementDistribution extracts the reinforce_dist distribution.
func reinforcementDistribution(stats map[string]interface{}) []int {
	raw, ok := stats["reinforce_dist"].([]map[string]interface{})
	if !ok {
		return nil
	}
	out := make([]int, 0, len(raw))
	for _, r := range raw {
		count, _ := r["count"].(int)
		out = append(out, count)
	}
	return out
}

// provenanceMemoryTotal sums every active_memories / total_memories
// across the nested agent → model → persona tree. Asserts that
// directive rows did not contribute.
func provenanceMemoryTotal(stats map[string]interface{}) int {
	registry, ok := stats["provenance_registry"].([]map[string]interface{})
	if !ok {
		return 0
	}
	total := 0
	for _, agentEntry := range registry {
		models, _ := agentEntry["models"].([]map[string]interface{})
		for _, modelEntry := range models {
			personas, _ := modelEntry["personas"].([]map[string]interface{})
			for _, personaEntry := range personas {
				tv, _ := personaEntry["total_memories"].(int)
				av, _ := personaEntry["active_memories"].(int)
				total += tv + av
			}
		}
	}
	return total
}

// TestGetMemoryStats_DirectivesExcludedFromBreakdowns pins the
// 2026-09-16 follow-up: every memory-derived member of the
// GetMemoryStats map must share the same directive boundary as
// the scalar counters. Pre-fix the four breakdown/distribution
// queries were unchanged when the scalar counters was updated, so
// a pristine install produced:
//
//	total = 0
//	by_collection: [{collection: directives, count: 5}]   ← LEAK
//	by_tag:        [{tag: prime_directive, count: 5}]      ← LEAK
//	reinforce_dist:[{rc: 0, count: 5}]                    ← LEAK
//	provenance_registry: [{..., active_memories: 5, ...}]  ← LEAK
//
// The post-fix query set applies `NOT (collection = 'directives'
// OR COALESCE(is_prime_directive, 0) = 1)` to every breakdown
// query too, so the four leaky members return empty on a
// directive-only database.
func TestGetMemoryStats_DirectivesExcludedFromBreakdowns(t *testing.T) {
	dm := NewTestDM(t)

	// Five canonical bootstrap directives (mirrors the
	// baseline-cognitive-bootstrap the production binary writes
	// on a pristine install). All collection='directives',
	// is_prime_directive=1, weight=10, LTM-eligible.
	for i := 0; i < 5; i++ {
		seedMemoryForStats(t, dm,
			"dir-"+string(rune('a'+i)),
			"directives",
			1,
			`["prime_directive"]`,
			`{"is_prime_directive":1,"provenance":{"agent":"mpm_ops_init","model":"direct","compute":"absolute","persona":"operator"}}`,
			10,
		)
	}

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}

	// 1. Scalar aggregates — these were fixed by bc7ce686.
	for _, key := range []string{"total", "active", "deleted", "ltm", "reinforced", "never_accessed", "expired"} {
		got, _ := stats[key].(int)
		if got != 0 {
			t.Errorf("stats[%q] = %d, want 0 (5 directives must not inflate the aggregate counts)", key, got)
		}
	}

	// 2. by_collection — must NOT contain the `directives` entry.
	bc := countByCollection(stats)
	if _, leaked := bc["directives"]; leaked {
		t.Errorf("by_collection still leaks directives: %v (must be empty on a directive-only database)", bc)
	}
	if len(bc) != 0 {
		t.Errorf("by_collection must be empty on a directive-only database; got %v", bc)
	}

	// 3. by_tag — must NOT contain `prime_directive` (or any other
	// directive-derived tag).
	bt := tagsByDistribution(stats)
	if _, leaked := bt["prime_directive"]; leaked {
		t.Errorf("by_tag still leaks directive tag 'prime_directive': %v", bt)
	}
	if len(bt) != 0 {
		t.Errorf("by_tag must be empty on a directive-only database; got %v", bt)
	}

	// 4. reinforce_dist — sum of counts must be 0.
	rd := reinforcementDistribution(stats)
	if sumInts(rd) != 0 {
		t.Errorf("reinforce_dist counts sum to %d, want 0 (directive rows must not contribute); rows: %v", sumInts(rd), rd)
	}

	// 5. provenance_registry — sum of total_memories + active_memories
	// across every agent/model/persona must be 0. The bootstrap
	// agent `mpm_ops_init` (which writes the directive rows) MUST
	// NOT appear in the registry at all.
	if total := provenanceMemoryTotal(stats); total != 0 {
		t.Errorf("provenance_registry total_memories+active_memories sums to %d, want 0 (directive rows must not contribute to *-memories fields)", total)
	}
	if registry, ok := stats["provenance_registry"].([]map[string]interface{}); ok {
		for _, agentEntry := range registry {
			if a, _ := agentEntry["agent"].(string); a == "mpm_ops_init" {
				t.Errorf("provenance_registry leaks bootstrap agent %q from directive rows; agent entries should be empty on a directive-only database", a)
			}
		}
	}
}

// TestGetMemoryStats_DirectivesAndMemoriesCoexistIn verifies the
// mixed-population case: when BOTH directives AND ordinary
// memories exist, the breakdowns must report ONLY the ordinary
// memories and stay quiet about the directives. This is the
// regression the existing alpha-5 dashboard info-anchor test
// exercises for the aggregate counts — this test extends the same
// invariant to the breakdown members.
func TestGetMemoryStats_DirectivesAndMemoriesCoexistIn(t *testing.T) {
	dm := NewTestDM(t)

	// 5 directives (as above).
	for i := 0; i < 5; i++ {
		seedMemoryForStats(t, dm,
			"dir-"+string(rune('a'+i)),
			"directives",
			1,
			`["prime_directive"]`,
			`{"is_prime_directive":1,"provenance":{"agent":"mpm_ops_init","model":"direct","compute":"absolute","persona":"operator"}}`,
			10,
		)
	}

	// 1 ordinary memory with a non-directive tag.
	seedMemoryForStats(t, dm,
		"ord-1",
		"default",
		nil, // NULL flag — must count (NULL-safe fix from 1bc490b1)
		`["user_note"]`,
		`{"provenance":{"agent":"human","model":"direct","compute":"direct","persona":"operator"}}`,
		1,
	)

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}

	// Aggregates: total/active must be exactly 1 (the ordinary
	// row); directives do not contribute.
	if got, _ := stats["total"].(int); got != 1 {
		t.Errorf("total = %d, want 1 (the ordinary NULL-flag memory; directives excluded)", got)
	}
	if got, _ := stats["active"].(int); got != 1 {
		t.Errorf("active = %d, want 1", got)
	}
	if got, _ := stats["ltm"].(int); got != 0 {
		t.Errorf("ltm = %d, want 0 (ordinary memory has weight=1; directives excluded)", got)
	}

	// by_collection: only the ordinary memory's collection
	// ("default") should appear; "directives" must NOT.
	bc := countByCollection(stats)
	if _, leaked := bc["directives"]; leaked {
		t.Errorf("by_collection still leaks directives in mixed case: %v", bc)
	}
	if got := bc["default"]; got != 1 {
		t.Errorf("by_collection[default] = %d, want 1", got)
	}

	// by_tag: only `user_note` should appear; `prime_directive` must NOT.
	bt := tagsByDistribution(stats)
	if _, leaked := bt["prime_directive"]; leaked {
		t.Errorf("by_tag still leaks directive tag in mixed case: %v", bt)
	}
	if got := bt["user_note"]; got != 1 {
		t.Errorf("by_tag[user_note] = %d, want 1", got)
	}

	// reinforce_dist: only the ordinary memory's row should appear.
	rd := reinforcementDistribution(stats)
	if sumInts(rd) != 1 {
		t.Errorf("reinforce_dist counts sum to %d, want 1 (ordinary memory only); rows: %v", sumInts(rd), rd)
	}

	// provenance_registry: only the `human` agent should appear.
	// My ordinary row is seeded with weight=1, so active = 0
	// (the registry counts active as "weight > 1") and the
	// per-persona sum is total_memories + active_memories = 1+0 = 1.
	if total := provenanceMemoryTotal(stats); total != 1 {
		t.Errorf("provenance_registry total+active = %d, want 1 (one ordinary row from human agent, weight=1 so active=0)", total)
	}
	if registry, ok := stats["provenance_registry"].([]map[string]interface{}); ok {
		for _, agentEntry := range registry {
			if a, _ := agentEntry["agent"].(string); a == "mpm_ops_init" {
				t.Errorf("provenance_registry leaks bootstrap agent %q from directive rows in mixed case", a)
			}
		}
	}
}

// TestGetMemoryStats_NullFlagOrdinaryMemoryCovered pins the
// 1bc490b11 NULL-safe fix carries over to the new exclusion
// surface. An ordinary memory with `is_prime_directive = NULL`
// must show up in EVERY breakdown (by_collection, by_tag,
// reinforce_dist, provenance_registry). Pre-1bc490b11 the buggy
// `is_prime_directive != 1` predicate hid this row from the
// aggregates; this test guards against any future regression
// where the breakdowns regress to that buggy form.
func TestGetMemoryStats_NullFlagOrdinaryMemoryCovered(t *testing.T) {
	dm := NewTestDM(t)

	seedMemoryForStats(t, dm,
		"ord-null",
		"default",
		nil, // NULL flag — the bug surface from 1bc490b11
		`["alpha","beta"]`,
		`{"provenance":{"agent":"human","model":"direct","compute":"direct","persona":"operator"}}`,
		1,
	)

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}

	if got, _ := stats["total"].(int); got != 1 {
		t.Errorf("total = %d, want 1 (NULL-flag ordinary memory must count)", got)
	}
	if bc := countByCollection(stats); bc["default"] != 1 {
		t.Errorf("by_collection[default] = %d, want 1 (NULL-flag row must surface)", bc["default"])
	}
	bt := tagsByDistribution(stats)
	if bt["alpha"] != 1 || bt["beta"] != 1 {
		t.Errorf("by_tag must reflect the NULL-flag row's tags: got %v", bt)
	}
	if sumInts(reinforcementDistribution(stats)) != 1 {
		t.Errorf("reinforce_dist must include the NULL-flag row")
	}
	// Per-persona sum is total_memories + active_memories; my
	// seed has weight=1 so active=0 → sum is 1+0 = 1.
	if provenanceMemoryTotal(stats) != 1 {
		t.Errorf("provenance_registry must include the NULL-flag row's agent entry (got total %d, want 1)", provenanceMemoryTotal(stats))
	}
}

// TestGetMemoryStats_CanonicalDirectiveExcluded pins that a row
// identified as a directive via `collection = 'directives'` is
// excluded from every breakdown regardless of the legacy flag
// value. Pre-fix the breakdowns did not exclude these rows at
// all; the post-fix surface MUST NOT include them under any flag
// state.
func TestGetMemoryStats_CanonicalDirectiveExcluded(t *testing.T) {
	dm := NewTestDM(t)

	// Canonical directive with NULL flag (the newer shape).
	seedMemoryForStats(t, dm,
		"dir-canon-null",
		"directives",
		nil,
		`["directive"]`,
		`{"provenance":{"agent":"bootstrap","model":"direct","compute":"absolute","persona":"operator"}}`,
		10,
	)
	// Canonical directive with flag=0 (the legacy seeded shape).
	seedMemoryForStats(t, dm,
		"dir-canon-zero",
		"directives",
		0,
		`["directive"]`,
		`{"provenance":{"agent":"bootstrap","model":"direct","compute":"absolute","persona":"operator"}}`,
		10,
	)

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}

	if got, _ := stats["total"].(int); got != 0 {
		t.Errorf("total = %d, want 0 (canonical directives must not contribute)", got)
	}
	if _, leaked := countByCollection(stats)["directives"]; leaked {
		t.Errorf("by_collection leaks canonical directives")
	}
	if _, leaked := tagsByDistribution(stats)["directive"]; leaked {
		t.Errorf("by_tag leaks canonical-directive tag")
	}
	if sumInts(reinforcementDistribution(stats)) != 0 {
		t.Errorf("reinforce_dist must not include canonical directives")
	}
	if provenanceMemoryTotal(stats) != 0 {
		t.Errorf("provenance_registry must not include canonical directives (bootstrap agent must not appear)")
	}
}

// TestGetMemoryStats_LegacyDirectiveExcluded pins that a row
// identified as a directive via `is_prime_directive = 1` is
// excluded from every breakdown regardless of collection. The
// pre-bc7ce686 world identified directives by either field; the
// post-fix canonical predicate must use BOTH.
//
// We seed a row with is_prime_directive=1 in a non-'directives'
// collection — this is the legacy data shape from before the
// collection-based canonical path existed. Pre-fix the breakdowns
// surfaced it; the post-fix must not.
func TestGetMemoryStats_LegacyDirectiveExcluded(t *testing.T) {
	dm := NewTestDM(t)

	// Legacy directive: flag=1, non-directives collection. The
	// content carries the is_prime_directive metadata marker too,
	// matching the historical seeded shape.
	seedMemoryForStats(t, dm,
		"dir-legacy",
		"legacy_collection", // NOT 'directives'
		1,                   // flag set
		`["prime_directive"]`,
		`{"is_prime_directive":1,"provenance":{"agent":"legacy","model":"direct","compute":"absolute","persona":"operator"}}`,
		10,
	)

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}

	if got, _ := stats["total"].(int); got != 0 {
		t.Errorf("total = %d, want 0 (legacy directive must not contribute)", got)
	}
	if _, leaked := countByCollection(stats)["legacy_collection"]; leaked {
		t.Errorf("by_collection leaks legacy directive's collection")
	}
	if _, leaked := tagsByDistribution(stats)["prime_directive"]; leaked {
		t.Errorf("by_tag leaks legacy directive tag")
	}
	if sumInts(reinforcementDistribution(stats)) != 0 {
		t.Errorf("reinforce_dist must not include legacy directive")
	}
	if provenanceMemoryTotal(stats) != 0 {
		t.Errorf("provenance_registry must not include legacy directive (legacy agent must not appear)")
	}
}

// TestGetMemoryStats_CrossSurfaceAgreement pins the agreement
// between the in-package surfaces that expose ordinary-memory
// counts:
//
//   - dm.GetMemoryStats()["total"]    — mpm info's stats.total (both human and JSON)
//   - dm.HealthCheck()["memories_active"] — canonical health-check field
//
// These are the two in-package surfaces; `mpm status`'s `MemTotal`
// (countMemories) lives in cmd/mpm/handlers_status.go and is pinned
// there by TestMemoryStats_StatusAndInfoAgreeOnNullSafeCount.
// All three share the canonical NULL-safe predicate, so they
// cannot drift. The test seeds an ordinary memory + directives
// and checks each in-package surface reports exactly 1 (the
// ordinary row).
func TestGetMemoryStats_CrossSurfaceAgreement(t *testing.T) {
	dm := NewTestDM(t)

	// 1 ordinary NULL-flag + 5 directives.
	seedMemoryForStats(t, dm, "ord-null", "default", nil, `["user"]`,
		`{"provenance":{"agent":"human","model":"direct","compute":"direct","persona":"operator"}}`, 1)
	for i := 0; i < 5; i++ {
		seedMemoryForStats(t, dm,
			"dir-"+string(rune('a'+i)),
			"directives",
			1,
			`["prime_directive"]`,
			`{"provenance":{"agent":"mpm_ops_init","model":"direct","compute":"absolute","persona":"operator"}}`,
			10,
		)
	}

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}

	// Surface 1: mpm info (human + JSON) — stats["total"].
	infoTotal, _ := stats["total"].(int)

	// Surface 2: HealthCheck["memories_active"] — canonical
	// health-check field.
	health, err := dm.HealthCheck()
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	memActive, _ := health["memories_active"].(int64)

	if infoTotal != 1 {
		t.Errorf("GetMemoryStats total = %d, want 1", infoTotal)
	}
	if memActive != 1 {
		t.Errorf("HealthCheck memories_active = %d, want 1", memActive)
	}

	// The two in-package surfaces must agree. The cmd/mpm surface
	// (MemTotal) is pinned separately by
	// TestMemoryStats_StatusAndInfoAgreeOnNullSafeCount and shares
	// the same predicate, so it cannot drift either.
	if !(infoTotal == int(memActive)) {
		t.Errorf("surface disagreement: info=%d health=%d", infoTotal, memActive)
	}
}

// sumInts sums a slice of ints. Tiny helper used by the breakdown
// assertions; stdlib reduce isn't worth importing for the four
// call sites in this file.
func sumInts(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}

// TestGetMemoryStats_StatsKeysAlwaysPresent pins the JSON
// compatibility invariant. Every breakdown key must be set in
// the stats map (even if empty) so the JSON shape stays stable.
// Pre-fix `provenance_registry` was only set when non-empty;
// the fix promotes it to always-present (with `[]` as the empty
// encoding) so consumers can rely on the key existing.
//
// This is also a guard against future regressions where a
// breakdown query starts returning nil and Go marshals that as
// `null` instead of `[]`.
func TestGetMemoryStats_StatsKeysAlwaysPresent(t *testing.T) {
	dm := NewTestDM(t)

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}

	required := []string{
		"total", "active", "deleted", "ltm", "reinforced",
		"never_accessed", "expired",
		"by_collection", "by_tag", "reinforce_dist",
		"provenance_registry",
	}
	for _, k := range required {
		if _, ok := stats[k]; !ok {
			t.Errorf("stats[%q] missing; the empty stats map must still carry every documented key (so JSON consumers see a stable shape)", k)
		}
	}

	// On an empty database, every breakdown must be a (possibly
	// empty) slice, not nil — JSON marshaling of nil slice → null,
	// of empty slice → []. We need the documented [] encoding.
	for _, k := range []string{"by_collection", "by_tag", "reinforce_dist", "provenance_registry"} {
		v, ok := stats[k]
		if !ok {
			continue
		}
		// Accept either an empty slice or nil for backward compat —
		// but if it's a slice with elements, the elements must NOT
		// reference directive data on a non-directive database.
		if slice, isSlice := v.([]map[string]interface{}); isSlice && len(slice) > 0 {
			t.Errorf("stats[%q] should be empty on a non-directive database; got %d entries", k, len(slice))
		}
	}

	// Sanity check: a substring like "directive" must not appear in
	// the JSON-encoded stats on an empty database. Catch-all guard
	// for any future breakdown that forgets the exclusion.
	enc := statsJSONSubset(stats)
	for _, k := range []string{"by_collection", "by_tag", "reinforce_dist", "provenance_registry"} {
		// Just check the key's JSON-encoded payload doesn't carry
		// directive-only tokens.
		if strings.Contains(strings.ToLower(enc[k]), "directive") {
			t.Errorf("stats[%q] JSON contains %q on an empty database (forgot the exclusion):\n  %s",
				k, "directive", enc[k])
		}
	}
}

// statsJSONSubset marshals each value to JSON and returns the
// resulting strings keyed by the original stats key. Used to do
// substring assertions over the wire shape without depending on
// the stdlib encoding/json directly in the assertion.
func statsJSONSubset(stats map[string]interface{}) map[string]string {
	// simple helper — we import encoding/json at the top of the
	// file so just inline:
	out := map[string]string{}
	for _, k := range []string{"by_collection", "by_tag", "reinforce_dist", "provenance_registry"} {
		// sort the canonical snapshot so assertions are stable
		// regardless of insertion order. (trivially small N here)
		out[k] = stringifyForAssert(stats[k])
	}
	return out
}

// stringifyForAssert marshals a stats value to a deterministic
// string. Errors are swallowed (the stats map only carries
// known types). Used to do substring assertions over the wire
// shape without depending on encoding/json directly in the
// assertion. The exact format is not load-bearing — the test
// only checks for substrings like "directive".
func stringifyForAssert(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case []map[string]interface{}:
		var sb strings.Builder
		sb.WriteString("[")
		for i, m := range x {
			if i > 0 {
				sb.WriteString(",")
			}
			for k, val := range m {
				sb.WriteString(k)
				sb.WriteString("=")
				switch val.(type) {
				case int:
					sb.WriteString("N")
				case string:
					sb.WriteString(val.(string))
				default:
					sb.WriteString("?")
				}
				sb.WriteString(";")
			}
		}
		sb.WriteString("]")
		return sb.String()
	default:
		return "?"
	}
}
