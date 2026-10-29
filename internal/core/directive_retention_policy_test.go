// directive_retention_policy_test.go — the Option B retention policy
// (issue #59), pinned end to end.
//
// THE POLICY UNDER TEST
// ---------------------
// An active registry-backed bootstrap directive is standing system policy,
// not an ordinary memory. Its retention follows REGISTRY MEMBERSHIP rather
// than age, access frequency or embedding status.
//
//  1. Active registry directives are protected from generic pruning.
//  2. Never-accessed directives never become automatic deletion candidates.
//  3. A directive removed from the registry becomes ELIGIBLE for explicit,
//     auditable cleanup.
//  4. Eligibility is not authorisation: MPM never reclaims a directive
//     automatically, whatever its registry status.
//  5. User-authored and customised directives are never at risk from a
//     registry change.
//
// These tests are in `package internal` because the generic-pruning
// protections live in the core package (AutoPrunePolicy, PruneExpired,
// ConsolidateMemories). The registry-membership model is in
// `package seed` and is tested there — see seed/retention_test.go.
//
// Disposable databases only.
package internal

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/flowbyte-com/mpm-core/seed"
)

// ── 1. The protections are real, and they are not accidental ──────────────

// TestRetentionPolicy_ExpiryDoesNotReclaimDirectives pins the expiry paths.
//
// Nothing in the substrate sets expires_at on a directive row, so these
// paths were DORMANT for the baseline — but dormant is not protected. An
// operator who put a TTL on a directive row would have had standing policy
// hard-deleted by the next maintenance cycle.
func TestRetentionPolicy_ExpiryDoesNotReclaimDirectives(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	// Give one directive a TTL in the past — as an operator's TTL edit
	// would.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET expires_at = 1 WHERE id = ?`, seed.SeedDirectives[0].StableID); err != nil {
		t.Fatalf("set expires_at: %v", err)
	}
	// And an ordinary expired memory, which MUST still be reclaimed.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, weight, reinforcement_count, is_long_term, expires_at, created_at, updated_at)
		VALUES ('mem-expired', 'memories', 'an expired memory', 1, 0, 0, 1, strftime('%s','now'), strftime('%s','now'))`); err != nil {
		t.Fatalf("insert expired memory: %v", err)
	}

	store := newRetentionStore(dm)
	pruned, err := store.AutoPrunePolicy(AutoPruneConfig{ExpiredEnabled: true})
	if err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1 (only the ordinary expired memory)", pruned)
	}
	if !isLive(t, dm, seed.SeedDirectives[0].StableID) {
		t.Errorf("directive %s was deleted by the expiry rule — a directive is "+
			"standing policy, not a memory awaiting expiry", seed.SeedDirectives[0].StableID)
	}
}

// TestRetentionPolicy_ConsolidationDoesNotReclaimDirectives is the
// protection that was MISSING.
//
// A seeded directive carries weight=10, so it satisfied the consolidation
// query's `weight > 1` and WAS selected. It survived only because
// directives hold no vector by design and clustering compares two vectors.
//
// The policy says retention follows registry membership, NOT embedding
// status. So the test gives the directives vectors: if the exclusion is
// missing, they become clusterable and a cluster larger than maxPerTopic
// soft-deletes the lowest-weight members.
func TestRetentionPolicy_ConsolidationDoesNotReclaimDirectives(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	// Give every directive a vector. This is the condition that used to
	// make standing policy eligible for cluster consolidation.
	//
	// The vector is stored the way the substrate stores one: JSON bytes,
	// with the provenance fields the vector-match filter needs.
	vec := []byte(`[0.1,0.2,0.3]`)
	for _, sd := range seed.SeedDirectives {
		if _, err := dm.SQLDB().Exec(`
			UPDATE memories SET embedding = ?, embedding_source = 'provider',
			                  embedding_dimension = 3, embedding_model = 'fixture:hashing-v1'
			WHERE id = ?`, vec, sd.StableID); err != nil {
			t.Fatalf("embed directive %s: %v", sd.StableID, err)
		}
	}
	// The five seeded directives share a collection and (now) a vector, so
	// they form ONE cluster. maxPerTopic=2 puts that cluster over the
	// limit, which is the condition that makes consolidation soft-delete
	// the lowest-weight members.
	//
	// The ordinary memories are the control: they must ALSO be
	// consolidated, so the test cannot pass by the consolidation path
	// simply not running.
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("mem-cluster-%d", i)
		if _, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, tags, embedding, embedding_source,
			                      embedding_dimension, embedding_model,
			                      weight, reinforcement_count, is_long_term, created_at, updated_at)
			VALUES (?, 'memories', 'the same content', '["same"]', ?, 'provider', 3, 'fixture:hashing-v1',
			        5, 1, 1, strftime('%s','now'), strftime('%s','now'))`,
			id, vec); err != nil {
			t.Fatalf("insert cluster member %s: %v", id, err)
		}
	}

	store := newRetentionStore(dm)
	consolidated, err := store.ConsolidateMemories(0.9, 2)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if consolidated == 0 {
		t.Error("consolidation consolidated nothing — the control group did not " +
			"cluster, so this test would pass for the wrong reason")
	}

	for _, sd := range seed.SeedDirectives {
		if !isLive(t, dm, sd.StableID) {
			t.Errorf("directive %s was soft-deleted by cluster consolidation — its "+
				"protection must not depend on whether it happens to hold a vector",
				sd.StableID)
		}
	}
}

// ── 2. Never-accessed directives are never deletion candidates ────────────

// TestRetentionPolicy_RepeatedPruningNeverSelectsADirective is the
// repeated-pruning case.
//
// A directive is permanently "never accessed", so a retention rule of that
// shape would select it on EVERY cycle. Before the exemption, each cycle
// soft-deleted it and the next boot revived it — churn that writes
// revisions and audit rows, and that can lose operator edits if the purge
// hard-deletes the row in between.
//
// Repeated pruning must be a NO-OP for directives, and must not lose an
// operator's edit.
func TestRetentionPolicy_RepeatedPruningNeverSelectsADirective(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	// The operator edits one directive.
	edited := seed.SeedDirectives[0].StableID
	const edit = "the operator's own words"
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET content = ? WHERE id = ?`, edit, edited); err != nil {
		t.Fatalf("edit directive: %v", err)
	}
	for _, sd := range seed.SeedDirectives {
		backdate(t, dm, sd.StableID, 400)
	}
	insertPlainMemory(t, dm, "mem-never-accessed", "ordinary never-accessed memory", 1)

	store := newRetentionStore(dm)
	for cycle := 1; cycle <= 5; cycle++ {
		if _, err := store.AutoPrunePolicy(AutoPruneConfig{
			NeverAccessedMaxDays: 30,
			LowWeightMaxDays:     90,
			ExpiredEnabled:       true,
		}); err != nil {
			t.Fatalf("auto-prune cycle %d: %v", cycle, err)
		}
		for _, sd := range seed.SeedDirectives {
			if !isLive(t, dm, sd.StableID) {
				t.Fatalf("cycle %d: directive %s was soft-deleted by retention", cycle, sd.StableID)
			}
		}
		if got := directiveContent(t, dm, edited); got != edit {
			t.Fatalf("cycle %d: the operator's edit was lost — content = %q", cycle, got)
		}
	}
}

// ── 3. Interrupted maintenance must not damage policy ─────────────────────

// TestRetentionPolicy_InterruptedMaintenanceLeavesDirectivesIntact models
// a maintenance cycle that is interrupted part-way.
//
// AutoPrunePolicy runs each rule as ONE statement, so a rule is atomic: it
// either ran or it did not. The test drives the cycle in a way that stops
// between rules and then re-runs it, which is what a crash and a restart
// produce.
//
// The requirement is that an interrupted maintenance leaves standing policy
// exactly as it was, and that the restarted cycle still does its job for
// ordinary memories.
func TestRetentionPolicy_InterruptedMaintenanceLeavesDirectivesIntact(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	for _, sd := range seed.SeedDirectives {
		backdate(t, dm, sd.StableID, 400)
	}
	insertPlainMemory(t, dm, "mem-never-accessed", "ordinary never-accessed memory", 1)

	store := newRetentionStore(dm)

	// First attempt: only the never-accessed rule runs, then the process
	// "dies" — the remaining rules never execute.
	if _, err := store.AutoPrunePolicy(AutoPruneConfig{NeverAccessedMaxDays: 30}); err != nil {
		t.Fatalf("first (interrupted) cycle: %v", err)
	}
	// At this point the ordinary memory may or may not have been pruned;
	// what matters is that every directive is intact.
	for _, sd := range seed.SeedDirectives {
		if !isLive(t, dm, sd.StableID) {
			t.Fatalf("directive %s was lost in the interrupted cycle", sd.StableID)
		}
	}

	// The restarted cycle runs the full rule set.
	pruned, err := store.AutoPrunePolicy(AutoPruneConfig{
		NeverAccessedMaxDays: 30,
		LowWeightMaxDays:     90,
		ExpiredEnabled:       true,
	})
	if err != nil {
		t.Fatalf("restarted cycle: %v", err)
	}
	// The ordinary memory was already reclaimed by the first cycle, so the
	// restarted cycle may have nothing left to do — but it must not have
	// selected a directive.
	if isLive(t, dm, "mem-never-accessed") {
		t.Error("the ordinary never-accessed memory was never reclaimed")
	}
	if pruned > len(seed.SeedDirectives) {
		t.Errorf("the restarted cycle pruned %d rows — more than the baseline holds, "+
			"which means it reached past the ordinary rows", pruned)
	}
	for _, sd := range seed.SeedDirectives {
		if !isLive(t, dm, sd.StableID) {
			t.Errorf("directive %s was lost across the interrupted maintenance", sd.StableID)
		}
	}
}

// ── 4. An upgrade must not lose policy ────────────────────────────────────

// TestRetentionPolicy_UpgradePathPreservesDirectives is the upgrade case:
// a database that already carries the baseline, opened by a newer build.
//
// The boot-time seed is idempotent and must not disturb an operator's
// edited directive, and retention must not reclaim it either.
func TestRetentionPolicy_UpgradePathPreservesDirectives(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	edited := seed.SeedDirectives[1].StableID
	const edit = "an edited directive"
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET content = ? WHERE id = ?`, edit, edited); err != nil {
		t.Fatalf("edit directive: %v", err)
	}
	for _, sd := range seed.SeedDirectives {
		backdate(t, dm, sd.StableID, 400)
	}

	// The upgrade: the seed runs again over an existing database.
	summary, err := seed.ApplyDirectives(dm)
	if err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if len(summary.Created) != 0 {
		t.Errorf("re-seed created %d rows on an already-seeded database", len(summary.Created))
	}
	if got := directiveContent(t, dm, edited); got != edit {
		t.Errorf("the operator's edit was lost across the re-seed — content = %q", got)
	}

	// And retention does not reclaim it.
	store := newRetentionStore(dm)
	if _, err := store.AutoPrunePolicy(AutoPruneConfig{
		NeverAccessedMaxDays: 30,
		LowWeightMaxDays:     90,
		ExpiredEnabled:       true,
	}); err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	for _, sd := range seed.SeedDirectives {
		if !isLive(t, dm, sd.StableID) {
			t.Errorf("directive %s was reclaimed across the upgrade", sd.StableID)
		}
	}
	if got := directiveContent(t, dm, edited); got != edit {
		t.Errorf("the operator's edit was lost to retention — content = %q", got)
	}
}

// ── 5. User-authored directives are not at risk from a registry change ────

// TestRetentionPolicy_UserAuthoredDirectiveIsNeverReclaimed is the
// "user-authored directives are not accidentally deleted merely because
// registry membership changes" requirement, at the pruning layer.
//
// A directive the operator wrote is not in the registry and never was, so
// registry membership says nothing about it. It is still policy, and it
// must survive every automatic reclamation path.
func TestRetentionPolicy_UserAuthoredDirectiveIsNeverReclaimed(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	// An operator-authored directive: collection='directives', but WITHOUT
	// the seeder's provenance marker.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight, reinforcement_count, is_long_term, created_at, updated_at)
		VALUES ('operator-authored', 'directives', 'the operator''s own policy',
		        '{"scope":"global"}', 10, 0, 0, strftime('%s','now','-400 days'), strftime('%s','now','-400 days'))`); err != nil {
		t.Fatalf("insert operator directive: %v", err)
	}
	// And an expired one, to exercise the expiry path too.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET expires_at = 1 WHERE id = 'operator-authored'`); err != nil {
		t.Fatalf("set expires_at: %v", err)
	}

	store := newRetentionStore(dm)
	if _, err := store.AutoPrunePolicy(AutoPruneConfig{
		NeverAccessedMaxDays: 30,
		LowWeightMaxDays:     90,
		ExpiredEnabled:       true,
	}); err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	if !isLive(t, dm, "operator-authored") {
		t.Error("the operator's own directive was reclaimed by retention — registry " +
			"membership says nothing about a directive the operator wrote")
	}
}

// ── 6. Non-vacuity: the exclusions are load-bearing ───────────────────────

// TestRetentionPolicy_DirectiveExclusionIsLoadBearing proves the exclusions
// are not decorative, by running the SAME query shape against rows that
// are NOT directives.
//
// If the exclusion clause were dropped from a prune, the directive would
// survive only by accident. This test shows the rule still reaches the
// rows it is supposed to reach, so a future edit that widens or narrows
// the exclusion is visible.
func TestRetentionPolicy_DirectiveExclusionIsLoadBearing(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	insertPlainMemory(t, dm, "mem-never-accessed", "ordinary never-accessed memory", 1)

	// Count what the exclusion clause selects.
	var ordinary, directives int
	if err := dm.SQLDB().QueryRow(`
		SELECT
			COUNT(CASE WHEN `+DirectiveExclusionClause+` THEN 1 END),
			COUNT(CASE WHEN NOT `+DirectiveExclusionClause+` THEN 1 END)
		FROM memories WHERE deleted_at IS NULL`).Scan(&ordinary, &directives); err != nil {
		t.Fatalf("count by exclusion clause: %v", err)
	}
	if ordinary != 1 {
		t.Errorf("the exclusion clause selected %d ordinary rows, want 1", ordinary)
	}
	if directives != len(seed.SeedDirectives) {
		t.Errorf("the exclusion clause excluded %d rows, want %d (the baseline)",
			directives, len(seed.SeedDirectives))
	}
}

// A directive row must not be selectable by the consolidation query at all.
func TestRetentionPolicy_ConsolidationSelectionExcludesDirectives(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	var selected int
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM memories
		WHERE deleted_at IS NULL
		  AND ` + DirectiveExclusionClause + `
		  AND (is_long_term = 1 OR reinforcement_count > 0 OR weight > 1)`).Scan(&selected); err != nil {
		t.Fatalf("count consolidation selection: %v", err)
	}
	if selected != 0 {
		t.Errorf("the consolidation selection would pick up %d directive rows", selected)
	}
}

// The directive predicate must still recognise BOTH identifiers — the
// collection column and the legacy column — or a directive written by an
// older path becomes reclaimable.
func TestRetentionPolicy_BothDirectiveIdentifiersAreRecognised(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := seed.ApplyDirectives(dm); err != nil {
		t.Fatalf("seed directives: %v", err)
	}
	// A legacy directive: is_prime_directive=1 but collection='memories'.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, is_prime_directive, weight, reinforcement_count, is_long_term, created_at, updated_at)
		VALUES ('legacy-directive', 'memories', 'a legacy directive', 1, 10, 0, 0, strftime('%s','now','-400 days'), strftime('%s','now','-400 days'))`); err != nil {
		t.Fatalf("insert legacy directive: %v", err)
	}
	store := newRetentionStore(dm)
	if _, err := store.AutoPrunePolicy(AutoPruneConfig{
		NeverAccessedMaxDays: 30,
		LowWeightMaxDays:     90,
		ExpiredEnabled:       true,
	}); err != nil {
		t.Fatalf("auto-prune: %v", err)
	}
	if !isLive(t, dm, "legacy-directive") {
		t.Error("a legacy directive (is_prime_directive=1) was reclaimed — the " +
			"exemption must recognise both identifiers")
	}
}

var _ = sql.ErrNoRows
