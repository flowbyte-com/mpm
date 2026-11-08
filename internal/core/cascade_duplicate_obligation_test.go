// cascade_duplicate_obligation_test.go — the cascade cap bounds the
// BATCH, not the OBLIGATION.
//
// WHAT THIS FILE PINS.
//
// internal/core/cascade_materializer.go justified not deleting a wake
// when its bookkeeping update failed with "the duplication risk is
// bounded by the per-check delivery cap". That justification was
// FALSE and the comment now says so.
//
// The cap bounds how many cascade rows reach the agent in one call.
// It does not deduplicate. Two cascade wakes carrying the same
// theory_id AND the same invalidation_event_id are ONE obligation
// that the reconciler duplicated, and both of them are delivered in a
// single call (2 <= MaxCascadeWakePerCheck = 3).
//
// WHY THE DUPLICATE EXISTS.
//
// The reconciler schedules the wake FIRST and claims the outbox row
// second. Two overlapping passes both SELECT the same outbox row
// (status='materialized', wake_scheduled=0), both schedule a wake,
// and one of them loses the claim. The loser reclaims its own wake —
// but only while that wake is still PENDING. If the agent's fold
// settled it first, the reclaim is skipped (the delivery record
// survives) and the obligation has been delivered TWICE.
//
// WHY NO DEDUPLICATION IS ATTEMPTED HERE.
//
// A wake row carries no obligation key the substrate could dedupe on
// without a new column, and a dedupe that "cancels" the duplicate
// would settle a wake without delivering it — trading a duplicate
// delivery for a silent loss. The durable fix is a claim with a
// lease, which belongs to the delivery-state work (Stage B/C). This
// file makes the current behaviour EXPLICIT so a future fix is
// visible rather than silent.
//
// Every test uses a disposable in-memory database (NewTestDM) and
// never touches the operator's.
package internal

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedCascadeObligationWake inserts a due cascade-kind wake for an
// obligation identified by (theory_id, invalidation_event_id).
func seedCascadeObligationWake(t *testing.T, dm *DatabaseManager, id, theoryID, eventID string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, 'cascade-reconciler', ?)`,
		id, time.Now().Add(-time.Minute).Unix(),
		fmt.Sprintf("cascade: theory %s requires review", theoryID),
		fmt.Sprintf(`{"kind":"cascade","theory_id":%q,"invalidation_event_id":%q,"source":"cascade-reconciler"}`,
			theoryID, eventID),
	)
	require.NoError(t, err, "seed cascade wake %s", id)
}

// TestCascadeDuplicate_SameObligationIsDeliveredTwice is the
// MEDIUM-2 evidence.
//
// The reconciler's lost-claim race leaves two pending cascade wakes
// for ONE obligation — the same theory_id and the same
// invalidation_event_id. The fold delivers BOTH, because the cap
// bounds the batch and 2 is under the cap of 3.
//
// This is the CURRENT behaviour and it is pinned deliberately: a
// future deduplication must change this test loudly, not silently.
func TestCascadeDuplicate_SameObligationIsDeliveredTwice(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	seedCascadeObligationWake(t, dm, "wk-dup-a", "th-1", "ev-1")
	seedCascadeObligationWake(t, dm, "wk-dup-b", "th-1", "ev-1")

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, 2,
		"the cap bounds the BATCH, not the OBLIGATION: the duplicate pair for one "+
			"obligation is delivered together (2 <= MaxCascadeWakePerCheck)")

	ids := map[string]bool{}
	for _, w := range due {
		ids[w["id"].(string)] = true
	}
	require.True(t, ids["wk-dup-a"])
	require.True(t, ids["wk-dup-b"])
}

// TestCascadeDistinct_ObligationsAreNotDuplicates is the other half
// of the MEDIUM-2 question: are two cascade wakes for the same
// theory_id always duplicates?
//
// NO. The obligation's identity is (theory_id, invalidation_event_id).
// The same theory invalidated by TWO different events is TWO distinct
// obligations — the theory needs review against each invalidation —
// and both must reach the agent.
//
// This is why a dedupe on theory_id alone would be wrong, and why the
// reconciler's duplicate is specifically a duplicate of the PAIR.
func TestCascadeDistinct_ObligationsAreNotDuplicates(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now()

	// The same theory, two DIFFERENT invalidation events.
	seedCascadeObligationWake(t, dm, "wk-dist-a", "th-1", "ev-1")
	seedCascadeObligationWake(t, dm, "wk-dist-b", "th-1", "ev-2")

	due, err := dm.CheckPendingWakes(now, nil)
	require.NoError(t, err)
	require.Len(t, due, 2,
		"two invalidation events for one theory are two DISTINCT obligations and both "+
			"must reach the agent: a dedupe on theory_id alone would lose one")
}
