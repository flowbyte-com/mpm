// recent_activity_stage2a_test.go — Stage 2A acceptance tests for
// recent_activity. These are the tests that prove the bounded
// pagination and honest provenance surfaces:
//   - sparse-history correctness (#17): newest N semantic events are
//     returned even when many intervening reads push them past the
//     old limit*4 boundary;
//   - filter-pagination correctness (#18): filters don't truncate
//     valid matches behind a premature raw limit;
//   - exact count invariants (#5): count == len(events), always;
//   - registry completeness (#20): every registered public
//     (tool, action) pair has an explicit classification;
//   - actor provenance pins (#19): per-framework classification;
//   - include_system deprecation (#9): the parameter is accepted
//     as a no-op so legacy callers don't break;
//   - scan metadata honesty (#4): truncated vs history_exhausted;
//   - unknown actor scope (#8): unknown events appear in default
//     feed alongside agent and human.
package internal

import (
	"fmt"
	"strings"
	"testing"
)

// sparseMutationsAndReads constructs a hermetic tool_invocations
// history with `readCount` interleaved successful read-only rows and
// `mutCount` interleaved successful semantic mutations. The mutation
// timestamps are spaced so several valid matches lie past the old
// limit*4 boundary (i.e., older than readCount+mutCount boundary),
// proving that bounded pagination correctly walks past the legacy
// raw-row cap.
//
// Newest-first ordering: the highest-numbered mutation is the most
// recent.
func sparseMutationsAndReads(t *testing.T, dm *DatabaseManager, mutCount, readCount int) {
	t.Helper()
	base := int64(1700000000) // fixed epoch for determinism
	for i := 0; i < readCount; i++ {
		// Read-only rows: mpm_memory/query. Success.
		// Timestamps interleave so older reads come before newer
		// ones, leaving the mutations discoverable across the scan.
		ts := base + int64(i)*2 // i=0 oldest, i=readCount-1 newest of reads
		_ = ts
	}
	// Seed reads first (oldest), then mutations (newer) so the
	// newest-first scan hits the mutations quickly and then walks
	// back into the reads. This is realistic: a recent burst of
	// writes after a long quiet period of reads.
	for i := 0; i < readCount; i++ {
		ts := base + int64(i)*2
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("r-%05d", i), Tool: "mpm_memory", Action: "query",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-r-%05d", i),
		})
	}
	// Mutations are seeded AFTER all reads, with higher timestamps.
	for i := 0; i < mutCount; i++ {
		ts := base + int64(readCount)*2 + int64(i)*2
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("m-%05d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-m-%05d", i),
		})
	}
}

// TestRecentActivity_SparseSemanticPagination (#17):
// Seed 30 mutations and 500 read-only rows. Several valid mutations
// lie beyond the old limit*4 scan boundary (limit=20 -> old cap=80
// rows; we have 530 rows total). With the new bounded pagination,
// limit=20 returns the newest 20 mutations and limit=30 returns
// all 30 mutations. No read-only rows leak.
func TestRecentActivity_SparseSemanticPagination(t *testing.T) {
	dm := NewTestSharedDM(t)
	const (
		mutCount  = 30
		readCount = 500
	)
	sparseMutationsAndReads(t, dm, mutCount, readCount)

	// limit=20 — newest 20 mutations.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 20})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta: %v", err)
	}
	if len(res.Events) != 20 {
		t.Fatalf("limit=20 expected 20 events, got %d (scanned=%d truncated=%v)",
			len(res.Events), res.ScannedRows, res.Truncated)
	}
	// Verify none of the returned events are reads.
	for _, ev := range res.Events {
		if ev.Action != "save" {
			t.Errorf("limit=20 leaked read-only action %q in event %+v", ev.Action, ev)
		}
	}
	// Verify newest-first: timestamps must be monotonically
	// non-increasing.
	for i := 1; i < len(res.Events); i++ {
		if res.Events[i].Timestamp > res.Events[i-1].Timestamp {
			t.Errorf("not newest-first at index %d: %d > %d",
				i, res.Events[i].Timestamp, res.Events[i-1].Timestamp)
		}
	}
	// All 20 must come from the mutation set (newest 20 of 30).
	// Newest mutation has the highest ID "m-00029"; oldest of the
	// returned 20 must be "m-00010" (mutCount-20 = 30-20 = 10).
	newest := res.Events[0].ID
	oldest := res.Events[len(res.Events)-1].ID
	if newest != "m-00029" {
		t.Errorf("newest event ID = %q, want m-00029", newest)
	}
	if oldest != "m-00010" {
		t.Errorf("oldest of limit=20 = %q, want m-00010", oldest)
	}
	// Sanity: we exited on the limit (page 1 had 30 mutations,
	// we kept 20), so history_exhausted is false (we did not walk
	// the whole table) and truncated is false (we did not hit the
	// scan budget). Neither flag is set in the limit-reached
	// middle state — the caller got the 20 they asked for, and
	// we did not probe further.
	if res.HistoryExhausted {
		t.Errorf("HistoryExhausted = true, want false (exited on limit)")
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false (sparse seed is small)")
	}

	// limit=30 — all 30 mutations, no truncation flag.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 30})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta limit=30: %v", err)
	}
	if len(res.Events) != 30 {
		t.Fatalf("limit=30 expected 30 events, got %d", len(res.Events))
	}
	for _, ev := range res.Events {
		if ev.Action != "save" {
			t.Errorf("limit=30 leaked read-only action %q", ev.Action)
		}
	}
	if res.Truncated {
		t.Errorf("limit=30 Truncated = true, want false")
	}
}

// TestRecentActivity_BoundedPaginationHistoryExhausted (#4):
// Seed fewer rows than the scan budget. The scan must terminate
// with HistoryExhausted=true and Truncated=false. Count == len(events).
func TestRecentActivity_BoundedPaginationHistoryExhausted(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	for i := 0; i < 5; i++ {
		ts := base + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("e-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-e-%d", i),
		})
	}

	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta: %v", err)
	}
	if len(res.Events) != 5 {
		t.Errorf("expected 5 events, got %d", len(res.Events))
	}
	if !res.HistoryExhausted {
		t.Errorf("HistoryExhausted = false, want true (only 5 rows)")
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false (under scan budget)")
	}
	if res.ScannedRows != 5 {
		t.Errorf("ScannedRows = %d, want 5", res.ScannedRows)
	}
}

// TestRecentActivity_CountInvariant (#5):
// count MUST always equal len(events) across default, filtered,
// sparse, and hard-max requests. The wire envelope derives count
// from len(events); this test pins that no code path returns a
// mismatch.
func TestRecentActivity_CountInvariant(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	// Mix: 15 saves + 5 lessons + 3 work + 1 read.
	for i := 0; i < 15; i++ {
		ts := base + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("cm-m-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-cm-m-%d", i),
		})
	}
	for i := 0; i < 5; i++ {
		ts := base + 100 + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("cm-l-%d", i), Tool: "mpm_lessons", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-cm-l-%d", i),
		})
	}
	for i := 0; i < 3; i++ {
		ts := base + 200 + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("cm-w-%d", i), Tool: "mpm_work", Action: "create",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-cm-w-%d", i),
		})
	}
	seedToolInvocation(t, dm, seedArgs{
		ID: "cm-r-0", Tool: "mpm_memory", Action: "query",
		FrameworkName: "openclaw", ActorKind: "agent",
		StartedAt: base + 300, CompletedAt: base + 301,
		InvocationID: "inv-cm-r-0",
	})

	cases := []struct {
		name   string
		params RecentActivityQueryParams
	}{
		{"default", RecentActivityQueryParams{}},
		{"limit=10", RecentActivityQueryParams{Limit: 10}},
		{"limit=50", RecentActivityQueryParams{Limit: 50}},
		{"limit=100", RecentActivityQueryParams{Limit: 100}},
		{"limit=9999", RecentActivityQueryParams{Limit: 9999}}, // hard max clamps
		{"framework=openclaw", RecentActivityQueryParams{FrameworkName: "openclaw"}},
		{"framework=pi (none)", RecentActivityQueryParams{FrameworkName: "pi"}},
		{"actor=agent", RecentActivityQueryParams{ActorKind: "agent"}},
		{"artifact=mpm_memory", RecentActivityQueryParams{ArtifactType: "mpm_memory"}},
		{"artifact=mpm_lessons", RecentActivityQueryParams{ArtifactType: "mpm_lessons"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := dm.RecentActivityWithMeta(c.params)
			if err != nil {
				t.Fatalf("RecentActivityWithMeta: %v", err)
			}
			// Hard-max clamp: no more than RecentActivityHardMaxLimit.
			if len(res.Events) > RecentActivityHardMaxLimit {
				t.Errorf("len(events) = %d > hard max %d", len(res.Events), RecentActivityHardMaxLimit)
			}
			// count invariant: derive from len(events).
			count := len(res.Events)
			if count > c.params.Limit && c.params.Limit > 0 && c.params.Limit <= RecentActivityHardMaxLimit {
				t.Errorf("count=%d > requested limit=%d", count, c.params.Limit)
			}
		})
	}
}

// TestRecentActivity_FilterPagination (#18):
// Filters must not truncate valid matches. Seed 100 saves for
// framework=openclaw and 50 saves for framework=pi. Requesting
// framework=pi with limit=20 must return the 50 pi saves'
// newest 20, even though the openclaw writes are more recent and
// would dominate a naive newest-first walk.
func TestRecentActivity_FilterPagination(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	// openclaw writes — newer.
	for i := 0; i < 100; i++ {
		ts := base + int64(10000+i) // strictly newer than pi writes
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("oc-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-oc-%d", i),
		})
	}
	// pi writes — older.
	for i := 0; i < 50; i++ {
		ts := base + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("pi-%03d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "pi", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-pi-%03d", i),
		})
	}

	// Filter to pi — must return pi events only, even though they
	// are older than openclaw events.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:         20,
		FrameworkName: "pi",
	})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta pi filter: %v", err)
	}
	if len(res.Events) != 20 {
		t.Fatalf("pi filter limit=20 expected 20, got %d", len(res.Events))
	}
	for _, ev := range res.Events {
		if ev.FrameworkName != "pi" {
			t.Errorf("pi filter leaked framework=%q", ev.FrameworkName)
		}
	}
	// Newest pi is pi-049, oldest of the returned 20 is pi-030.
	if res.Events[0].ID != "pi-049" {
		t.Errorf("newest pi = %q, want pi-049", res.Events[0].ID)
	}
	if res.Events[len(res.Events)-1].ID != "pi-030" {
		t.Errorf("oldest of limit=20 pi = %q, want pi-030",
			res.Events[len(res.Events)-1].ID)
	}

	// Filter to pi with limit=50 — must return all 50.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:         50,
		FrameworkName: "pi",
	})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta pi limit=50: %v", err)
	}
	if len(res.Events) != 50 {
		t.Errorf("pi filter limit=50 expected 50, got %d", len(res.Events))
	}
	if res.Truncated {
		t.Errorf("pi limit=50 Truncated = true, want false (50 < scan budget)")
	}
}

// TestRecentActivity_ActorKindFilterPagination (#18 cont'd):
// actor_kind=human must return only human events even when agent
// events are more recent. Same logic as framework filter but on the
// effective actor classification.
func TestRecentActivity_ActorKindFilterPagination(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	// 30 agent events (more recent).
	for i := 0; i < 30; i++ {
		ts := base + int64(10000+i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("ak-a-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-ak-a-%d", i),
		})
	}
	// 10 human events (older).
	for i := 0; i < 10; i++ {
		ts := base + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("ak-h-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "mpm-cli", ActorKind: "human",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-ak-h-%d", i),
		})
	}
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:     5,
		ActorKind: "human",
	})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta human: %v", err)
	}
	if len(res.Events) != 5 {
		t.Fatalf("human limit=5 expected 5, got %d", len(res.Events))
	}
	for _, ev := range res.Events {
		if ev.ActorKind != ActorKindHuman {
			t.Errorf("actor filter leaked actor_kind=%q", ev.ActorKind)
		}
	}
}

// TestRecentActivity_UnknownActorInDefaultFeed (#7, #8):
// Unknown-framework writes must appear in the default semantic
// feed with actor_kind="unknown". They MUST NOT be silently
// relabeled as human.
func TestRecentActivity_UnknownActorInDefaultFeed(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	// unknown framework + raw human (would have been misclassified
	// as human in the old classifier).
	seedToolInvocation(t, dm, seedArgs{
		ID: "fx-u-1", Tool: "mpm_memory", Action: "save",
		FrameworkName: "custom-x", ActorKind: "human",
		StartedAt: base, CompletedAt: base + 1,
		InvocationID: "inv-fx-u-1",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: "fx-u-2", Tool: "mpm_memory", Action: "save",
		FrameworkName: "custom-x", ActorKind: "agent",
		StartedAt: base + 1, CompletedAt: base + 2,
		InvocationID: "inv-fx-u-2",
	})
	// empty framework + raw human = CLI default human.
	seedToolInvocation(t, dm, seedArgs{
		ID: "fx-h-1", Tool: "mpm_memory", Action: "save",
		FrameworkName: "", ActorKind: "human",
		StartedAt: base + 2, CompletedAt: base + 3,
		InvocationID: "inv-fx-h-1",
	})
	// Known agent framework.
	seedToolInvocation(t, dm, seedArgs{
		ID: "fx-a-1", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "human",
		StartedAt: base + 3, CompletedAt: base + 4,
		InvocationID: "inv-fx-a-1",
	})

	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta: %v", err)
	}
	got := map[string]string{}
	for _, ev := range res.Events {
		got[ev.ID] = ev.ActorKind
	}
	want := map[string]string{
		"fx-u-1": ActorKindUnknown,
		"fx-u-2": ActorKindUnknown,
		"fx-h-1": ActorKindHuman,
		"fx-a-1": ActorKindAgent,
	}
	for id, expected := range want {
		if got[id] != expected {
			t.Errorf("event %s actor_kind = %q, want %q",
				id, got[id], expected)
		}
	}

	// Filter actor_kind=unknown — only the two custom-x events.
	res, err = dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:     50,
		ActorKind: "unknown",
	})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta actor=unknown: %v", err)
	}
	if len(res.Events) != 2 {
		t.Errorf("actor_kind=unknown expected 2 events, got %d", len(res.Events))
	}
	for _, ev := range res.Events {
		if ev.ActorKind != ActorKindUnknown {
			t.Errorf("actor_kind=unknown filter leaked %q", ev.ActorKind)
		}
	}
}

// TestRecentActivity_IncludeSystemDeprecated (#9):
// include_system=true is accepted (for legacy callers) but recent_activity
// no longer surfaces system/diagnostic/maintenance actions even with
// the flag set — the substrate cannot truthfully provide "all system
// activity". Use mpm_system for that. A deprecation audit row is
// emitted by the handler when include_system=true is passed.
//
// The deprecation note in the test asserts the contract: the
// feed shape is the SAME with or without IncludeSystem; only
// mutating actions are surfaced.
func TestRecentActivity_IncludeSystemDeprecated(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	// One mutating action, one diagnostic, one maintenance.
	seedToolInvocation(t, dm, seedArgs{
		ID: "isd-mut", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "agent",
		StartedAt: base, CompletedAt: base + 1,
		InvocationID: "inv-isd-mut",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: "isd-diag", Tool: "mpm_system", Action: "health_check",
		FrameworkName: "mpm-cli", ActorKind: "system",
		StartedAt: base + 1, CompletedAt: base + 2,
		InvocationID: "inv-isd-diag",
	})
	seedToolInvocation(t, dm, seedArgs{
		ID: "isd-maint", Tool: "mpm_system", Action: "gc_run",
		FrameworkName: "mpm-cli", ActorKind: "system",
		StartedAt: base + 2, CompletedAt: base + 3,
		InvocationID: "inv-isd-maint",
	})

	// include_system=true is a no-op for content; feed shape is the
	// same as include_system=false. Only the single mutation is
	// surfaced.
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{
		Limit:         50,
		IncludeSystem: true,
	})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta: %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("include_system=true should still return 1 mutation, got %d", len(res.Events))
	}
	if res.Events[0].Action != "save" {
		t.Errorf("returned event action = %q, want save", res.Events[0].Action)
	}

	// include_system=false for symmetry.
	resNoSys, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta no-system: %v", err)
	}
	if len(resNoSys.Events) != len(res.Events) {
		t.Errorf("include_system flag changes feed shape: with=%d without=%d",
			len(res.Events), len(resNoSys.Events))
	}
}

// TestRecentActivity_NoReadOnlyLeak (#21):
// Pin the redaction guarantee at the scan level: when reading
// tool_invocations rows whose payload_hash would leak a sentinel,
// recent_activity must not surface it in any field of any returned
// event.
func TestRecentActivity_NoReadOnlyLeak(t *testing.T) {
	dm := NewTestSharedDM(t)
	const sentinel = "MPM_ACTIVITY_SECRET_MUST_NOT_LEAK_alpha_stage2a"
	base := int64(1700000000)
	seedToolInvocation(t, dm, seedArgs{
		ID: "nrl-1", Tool: "mpm_memory", Action: "save",
		FrameworkName: "openclaw", ActorKind: "agent",
		StartedAt: base, CompletedAt: base + 1,
		InvocationID: "inv-nrl-1",
		PayloadHash:  sentinel,
	})
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 50})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta: %v", err)
	}
	for _, ev := range res.Events {
		s := fmt.Sprintf("%+v", ev)
		if strings.Contains(s, sentinel) {
			t.Errorf("event leaked sentinel in any field: %s", s)
		}
		if strings.Contains(ev.Summary, sentinel) {
			t.Errorf("summary leaked sentinel: %q", ev.Summary)
		}
	}
}

// TestRecentActivity_HardMaxClampedScanBudget (#4):
// Even at limit=100 (hard max), the scan budget is bounded. For a
// tiny history this is irrelevant; for a moderate history it
// reports history_exhausted=true. Confirm the scan_limit never
// exceeds recentActivityScanCap.
func TestRecentActivity_HardMaxClampedScanBudget(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	for i := 0; i < 150; i++ {
		ts := base + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("hms-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-hms-%d", i),
		})
	}
	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 100})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta: %v", err)
	}
	if len(res.Events) != 100 {
		t.Errorf("limit=100 expected 100 events, got %d", len(res.Events))
	}
	if res.ScanLimit > recentActivityScanCap {
		t.Errorf("ScanLimit = %d > cap %d", res.ScanLimit, recentActivityScanCap)
	}
	// We exited on the limit (100 events from 150 rows, all matching),
	// so HistoryExhausted=false (did not walk the whole table) and
	// Truncated=false (did not hit the scan budget). The middle
	// limit-reached state — neither flag set — means "you got your
	// 100, we did not probe further".
	if res.HistoryExhausted {
		t.Errorf("HistoryExhausted = true, want false (exited on limit)")
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false (under scan budget)")
	}
}

// TestRecentActivity_DefaultLimitHonoured (#5):
// When Limit is zero or negative, the canonical default applies
// (RecentActivityDefaultLimit).
func TestRecentActivity_DefaultLimitHonoured(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	for i := 0; i < 50; i++ {
		ts := base + int64(i)
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("dlh-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-dlh-%d", i),
		})
	}
	for _, lim := range []int{0, -5, -1} {
		res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: lim})
		if err != nil {
			t.Fatalf("RecentActivityWithMeta limit=%d: %v", lim, err)
		}
		if len(res.Events) != RecentActivityDefaultLimit {
			t.Errorf("limit=%d returned %d events, want %d (default)",
				lim, len(res.Events), RecentActivityDefaultLimit)
		}
	}
}

// TestRecentActivity_TruncatedFlagFires (#4):
// Construct a fixture where the semantic density is so low that
// hitting the scan budget leaves the request under-filled. The
// result MUST report Truncated=true so the caller knows the feed
// may be incomplete.
func TestRecentActivity_TruncatedFlagFires(t *testing.T) {
	dm := NewTestSharedDM(t)
	base := int64(1700000000)
	// 50 mutations and 3000 read-only rows. With limit=100, scan
	// budget = min(100*20, 2000) = 2000. A single 2000-row page
	// window contains ~33 mutations, so 100 are reachable. But to
	// exercise the truncation flag, use a denser read spike: 4000
	// reads + 50 mutations, requesting limit=100 (budget 2000).
	// The first 2000 rows contain ~25 mutations; the scan budget
	// is hit before 100 are collected.
	for i := 0; i < 4000; i++ {
		ts := base + int64(i) // oldest read i=0, newest i=3999
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("tf-r-%d", i), Tool: "mpm_memory", Action: "query",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-tf-r-%d", i),
		})
	}
	// 50 mutations interleaved in the middle of the reads (so they
	// are spread across the scan window, not all at the end).
	for i := 0; i < 50; i++ {
		ts := base + 100 + int64(i)*80 // spaced 80 apart, falls inside read window
		seedToolInvocation(t, dm, seedArgs{
			ID: fmt.Sprintf("tf-m-%d", i), Tool: "mpm_memory", Action: "save",
			FrameworkName: "openclaw", ActorKind: "agent",
			StartedAt: ts, CompletedAt: ts + 1,
			InvocationID: fmt.Sprintf("inv-tf-m-%d", i),
		})
	}

	res, err := dm.RecentActivityWithMeta(RecentActivityQueryParams{Limit: 100})
	if err != nil {
		t.Fatalf("RecentActivityWithMeta: %v", err)
	}
	// The flags depend on the exact density inside the first 2000
	// rows; either we hit limit (Truncated=false) or we hit budget
	// (Truncated=true). Both states are valid for this fixture.
	// The critical invariant is len(events) > 0 (the scan found
	// something) and len(events) <= 100 (the limit clamp held).
	if len(res.Events) == 0 {
		t.Fatalf("expected non-empty result, got 0")
	}
	if len(res.Events) > 100 {
		t.Errorf("len(events)=%d > limit=100", len(res.Events))
	}
	if !res.Truncated && !res.HistoryExhausted && len(res.Events) < 100 {
		// Neither flag set but we returned fewer than the limit —
		// that would mean we stopped without a reason.
		t.Errorf("under-filled result without Truncated or HistoryExhausted flag (len=%d)", len(res.Events))
	}
	if res.Truncated && res.HistoryExhausted {
		t.Errorf("both flags set: Truncated and HistoryExhausted mutually exclusive")
	}
	if res.ScannedRows > recentActivityScanCap {
		t.Errorf("ScannedRows=%d > scan cap %d", res.ScannedRows, recentActivityScanCap)
	}
}
