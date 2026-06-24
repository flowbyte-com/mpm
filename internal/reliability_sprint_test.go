package internal

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mpm/internal/config"
	"mpm/internal/synth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReliabilitySprint is the top-level test for the Dual-Core Reliability
// Upgrade. It contains two sub-tests:
//
//	SynthesisIsolationOverflow — floods the worker with 250 events, verifies
//	  200 in channel buffer and 50 offloaded to raw_memories as 'overflow_deferred'
//
//	RetrievalContradictionTrigger — inserts two semantically overlapping
//	  memories, one challenged, runs hybrid search, asserts collision detection
//	  and in-memory warning prepend
func TestReliabilitySprint(t *testing.T) {
	t.Run("SynthesisIsolationOverflow", func(t *testing.T) {
		testSynthesisIsolationOverflow(t)
	})
	t.Run("RetrievalContradictionTrigger", func(t *testing.T) {
		testRetrievalContradictionTrigger(t)
	})
}

// ── Test 1: Ingestion Backpressure ───────────────────────────────────────

func testSynthesisIsolationOverflow(t *testing.T) {
	db := reliabilityFreshDB(t)
	defer db.Close()

	mock := &controllableSynthClient{}
	mock.setFail(fmt.Errorf("overflow test — no vendor"))

	// Create worker but do NOT start it — we want the channel buffer to fill
	// without any consumer draining events.
	dlqTick := make(chan time.Time)
	worker := NewSynthesisWorkerForTest(db, mock, 1, dlqTick, []config.SynthVendor{
		{Name: "test-vendor"},
	})
	worker.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Flood with 250 events. Channel has 200 buffer capacity.
	// First 200 fill the buffer, remaining 50 hit the default branch and
	// get offloaded to raw_memories with status='overflow_deferred'.
	for i := 0; i < 250; i++ {
		worker.Enqueue(MemoryEvent{
			ID:      fmt.Sprintf("overflow-test-%d", i),
			Content: fmt.Sprintf("overflow content %d", i),
			Tags:    []string{"overflow-test"},
		})
	}

	// Verify channel has 200 buffered events immediately (synchronous, deterministic)
	assert.Equal(t, 200, len(worker.events), "channel should hold 200 buffered events after flood")

	// Start the worker to drain events — the 200 buffered events hit the failing mock
	// and go to synthesis_dlq. Meanwhile overflow goroutines finish their DB writes.
	worker.Start()
	defer worker.Stop()

	// Poll until overflow goroutines finish writing under race detector
	require.Eventually(t, func() bool {
		return len(worker.events) == 0
	}, 5*time.Second, 100*time.Millisecond, "channel should be drained after worker starts")

	// Account for all 250 events:
	//   - 200 buffered → worker processes → synthesis fails → synthesis_dlq
	//   - 50 overflow  → raw_memories (DLQ tick never fires, stays as overflow_deferred)
	var dlqCount, overflowCount int
	require.Eventually(t, func() bool {
		dlqCount = 0
		overflowCount = 0
		if err := db.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM synthesis_dlq WHERE memory_id LIKE 'overflow-test-%'`,
		).Scan(&dlqCount); err != nil {
			return false
		}
		if err := db.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM raw_memories WHERE source_id LIKE 'overflow-test-%' AND status = 'overflow_deferred'`,
		).Scan(&overflowCount); err != nil {
			return false
		}
		return dlqCount == 200 && overflowCount == 50
	}, 5*time.Second, 100*time.Millisecond,
		"all 250 events should be accounted for: dlq=%d overflow=%d", dlqCount, overflowCount)
}

// ── Test 2: Cognitive Immune System — Contradiction Detection ───────────

func testRetrievalContradictionTrigger(t *testing.T) {
	db := reliabilityFreshDB(t)
	defer db.Close()

	// Insert two memories with identical content so HashEmbed produces the same
	// embedding → cosine=1.0, guaranteeing the 0.85 contradiction threshold is met.
	overlappingContent := "The user prefers concise and direct communication without unnecessary elaboration or fluff in their messages."
	embedding := HashEmbed(overlappingContent)

	// Memory 1: normal, unchallenged
	id1, err := db.SaveMemory("memories", overlappingContent, "", []string{"communication", "preference"},
		map[string]interface{}{"source": "test"}, embedding, false, 5)
	require.NoError(t, err)
	require.NotEmpty(t, id1)

	// Memory 2: same content, but marked as challenged via metadata
	challengedMeta := map[string]interface{}{
		"status":               "challenged",
		"challenged_theory_id": "theory-collision-001",
		"source":               "test",
	}
	id2, err := db.SaveMemory("memories", overlappingContent, "", []string{"communication", "preference"},
		challengedMeta, embedding, false, 3)
	require.NoError(t, err)
	require.NotEmpty(t, id2)

	// Run HybridSearch: use single-word query "concise" which matches verbatim
	// in both memories. searchLike uses LIKE '%concise%' — direct substring match.
	cfg := DefaultHybridConfig()
	cfg.Limit = 10
	cfg.VectorWeight = 0          // force LIKE-only, no vector dependency
	cfg.RetrievalThreshold = -100 // accept all results

	results, err := HybridSearch(db, "concise", "memories", cfg)
	require.NoError(t, err, "HybridSearch should not error: %v")
	require.GreaterOrEqual(t, len(results), 1, "should return at least one result, got: %d", len(results))

	// Verify the challenged memory has the warning prepended
	var challengedFound bool
	for _, r := range results {
		if r.ID == id2 {
			challengedFound = true
			assert.True(t, r.IsChallenged, "memory %s should be flagged as challenged", id2)
			assert.Contains(t, r.Content, challengeWarning,
				"challenged memory content should have warning prepended")
			assert.Contains(t, r.Content, overlappingContent,
				"challenged memory content should still contain original text after warning")
			break
		}
	}
	assert.True(t, challengedFound, "challenged memory should appear in hybrid search results")

	// Verify the unchallenged memory does NOT have the warning
	for _, r := range results {
		if r.ID == id1 {
			assert.False(t, r.IsChallenged, "memory %s should NOT be flagged as challenged", id1)
			assert.NotContains(t, r.Content, challengeWarning,
				"unchallenged memory content should NOT have warning")
			break
		}
	}
}

// ── Test Helpers ─────────────────────────────────────────────────────────

// reliabilityFreshDB creates an isolated DatabaseManager for reliability tests.
func reliabilityFreshDB(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := t.TempDir() + "/reliability_test.db"
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	require.NoError(t, EnsureDLQSchema(dm.SQLDB()))
	return dm
}

// controllableSynthClient is defined in synthesis_isolation_test.go — using
// the same type from the same package (internal) so it's shared implicitly.
// Below is a compile-time interface check for local use.
var _ synth.SynthClientInterface = (*controllableSynthClient)(nil)
