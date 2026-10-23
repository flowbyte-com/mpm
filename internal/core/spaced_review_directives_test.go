// spaced_review_directives_test.go — HAL-9001 Finding 5.
//
// The spaced-reinforcement review query counted directive rows as
// review candidates.
//
// A directive is standing policy, not a memory the agent learned: it
// is read directly at wake time and is never a vector-search
// candidate. The seeded baseline directives carry weight=10 and never
// have `last_accessed_at` written — the directive read path does not
// touch it — so they matched the review query forever. On a clean
// install that produced a spaced-review backlog that could never be
// cleared: the only way to clear it would be to "review" the bootstrap
// policy.
//
// The exclusion must not silence the queue. The genuine high-weight
// memory alongside the directive has to stay visible.
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpacedReinforcementReview_ExcludesDirectives pins the exclusion
// on both directive paths (the canonical collection and the legacy
// flag) and pins that a genuine candidate survives.
func TestSpacedReinforcementReview_ExcludesDirectives(t *testing.T) {
	dm := newTestDM(t)

	// Canonical directive path: collection='directives'.
	_, err := dm.ExecTracked(`INSERT INTO memories
		(id, collection, content, tags, weight, is_long_term, is_prime_directive, reinforcement_count)
		VALUES (?, 'directives', 'bootstrap policy', '[]', 10, 0, 1, 0)`, 0, "dir-canonical")
	require.NoError(t, err)

	// Legacy directive path: is_prime_directive=1 on an ordinary collection.
	_, err = dm.ExecTracked(`INSERT INTO memories
		(id, collection, content, tags, weight, is_long_term, is_prime_directive, reinforcement_count)
		VALUES (?, 'memories', 'legacy bootstrap policy', '[]', 10, 0, 1, 0)`, 0, "dir-legacy")
	require.NoError(t, err)

	// A genuine candidate: high weight, never accessed.
	_, err = dm.ExecTracked(`INSERT INTO memories
		(id, collection, content, tags, weight, is_long_term, is_prime_directive, reinforcement_count)
		VALUES (?, 'memories', 'a real memory worth reinforcing', '[]', 10, 0, 0, 0)`, 0, "mem-real")
	require.NoError(t, err)

	items, err := dm.GetSpacedReinforcementReview(14, 100)
	require.NoError(t, err)
	require.Len(t, items, 1,
		"the review queue must hold only the genuine candidate; got %v", items)
	assert.Equal(t, "mem-real", items[0]["id"])
}

// TestSpacedReinforcementReview_DirectivesOnlyYieldsNothing pins the
// clean-install case: a bootstrapped store with only directives has no
// review backlog.
func TestSpacedReinforcementReview_DirectivesOnlyYieldsNothing(t *testing.T) {
	dm := newTestDM(t)

	_, err := dm.ExecTracked(`INSERT INTO memories
		(id, collection, content, tags, weight, is_long_term, is_prime_directive, reinforcement_count)
		VALUES (?, 'directives', 'bootstrap policy', '[]', 10, 0, 1, 0)`, 0, "dir-canonical")
	require.NoError(t, err)

	items, err := dm.GetSpacedReinforcementReview(14, 100)
	require.NoError(t, err)
	assert.Empty(t, items,
		"bootstrap directives must not appear as spaced-review work")
}
