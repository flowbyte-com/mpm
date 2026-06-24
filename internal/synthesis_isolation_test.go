package internal

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mpm/internal/config"
	"mpm/internal/synth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// controllableSynthClient is a test double implementing synth.SynthClientInterface
// whose failure/success behavior can be toggled at runtime.
type controllableSynthClient struct {
	mu         sync.Mutex
	shouldFail bool
	failErr    error
	result     *synth.SynthResult
	callCount  int
}

func (c *controllableSynthClient) Synthesize(ctx context.Context, fragments []string) (*synth.SynthResult, error) {
	return nil, nil
}

func (c *controllableSynthClient) SynthesizeWithVendor(ctx context.Context, vendor config.SynthVendor, content string, tags []string) (*synth.SynthResult, error) {
	c.mu.Lock()
	c.callCount++
	should := c.shouldFail
	c.mu.Unlock()
	if should {
		return nil, c.failErr
	}
	return c.result, nil
}

func (c *controllableSynthClient) setFail(err error) {
	c.mu.Lock()
	c.shouldFail = true
	c.failErr = err
	c.mu.Unlock()
}

func (c *controllableSynthClient) setSucceed(content string, tags []string) {
	c.mu.Lock()
	c.shouldFail = false
	c.result = &synth.SynthResult{Content: content, Tags: tags}
	c.mu.Unlock()
}

var _ synth.SynthClientInterface = (*controllableSynthClient)(nil)

// synthesisFreshDB creates a fully isolated DatabaseManager for synthesis tests.
// Same as freshDB() but defined here to avoid import cycle (same package, different file).
func synthesisFreshDB(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	require.NoError(t, EnsureDLQSchema(dm.SQLDB()))
	return dm
}

// TestSynthesisIsolationPipeline is the top-level test for the E2E async
// synthesis pipeline. It contains three sub-tests:
//
//	NetworkFailure  — vendor always fails → DLQ entry with retry_count=1 + backoff
//	HeartbeatRecovery — vendor switches to success → DLQ cleared, memory persisted
//	GracefulShutdown — Stop() drains all in-flight events cleanly
func TestSynthesisIsolationPipeline(t *testing.T) {
	t.Run("NetworkFailure", func(t *testing.T) {
		testSynthesisNetworkFailure(t)
	})
	t.Run("HeartbeatRecovery", func(t *testing.T) {
		testSynthesisHeartbeatRecovery(t)
	})
	t.Run("GracefulShutdown", func(t *testing.T) {
		testSynthesisGracefulShutdown(t)
	})
}

// ── Scenario 1: Network Failure / Timeout ─────────────────────────────────

func testSynthesisNetworkFailure(t *testing.T) {
	db := synthesisFreshDB(t)
	defer db.Close()

	mock := &controllableSynthClient{}
	mock.setFail(fmt.Errorf("HTTP 504 Gateway Timeout"))

	dlqTick := make(chan time.Time) // never fires — we drive DLQ manually
	worker := NewSynthesisWorkerForTest(db, mock, 1, dlqTick, []config.SynthVendor{
		{Name: "test-vendor"},
	})
	worker.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	worker.Start()
	defer worker.Stop()

	time.Sleep(50 * time.Millisecond) // let the event loop start

	// ── Phase 1: Enqueue → vendor fails → DLQ entry written ──
	worker.Enqueue(MemoryEvent{
		ID:      "net-fail-test",
		Content: "memory content that will fail synthesis",
		Tags:    []string{"test"},
	})

	time.Sleep(500 * time.Millisecond) // wait for processing

	// Confirm DLQ entry exists (attempt=0 after first DLQEnqueue)
	var count int
	err := db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM synthesis_dlq WHERE memory_id = ?`, "net-fail-test",
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "DLQ should have 1 entry after first failure")

	// ── Phase 2: processDLQForced → retry → vendor still fails → attempt=1 ──
	worker.processDLQForced()
	time.Sleep(500 * time.Millisecond)

	var attempt int
	var nextRetry string
	err = db.SQLDB().QueryRow(
		`SELECT attempt, COALESCE(next_retry, '') FROM synthesis_dlq WHERE memory_id = ?`,
		"net-fail-test",
	).Scan(&attempt, &nextRetry)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, attempt, 1, "DLQ entry should have attempt>=1 after one retry failure (tuned by backoff config)")
	assert.NotEmpty(t, nextRetry, "DLQ entry must have an active backoff timestamp")
}

// ── Scenario 2: Heartbeat Recovery ────────────────────────────────────────

func testSynthesisHeartbeatRecovery(t *testing.T) {
	db := synthesisFreshDB(t)
	defer db.Close()

	mock := &controllableSynthClient{}
	mock.setFail(assert.AnError)

	dlqTick := make(chan time.Time)
	worker := NewSynthesisWorkerForTest(db, mock, 1, dlqTick, []config.SynthVendor{
		{Name: "test-vendor"},
	})
	worker.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	worker.Start()
	defer worker.Stop()

	time.Sleep(50 * time.Millisecond)

	// Pre-seed a source memory so persistSynthesizedMemory has a row to soft-delete
	sourceID, err := db.SaveMemory("memories", "memory content for recovery test",
		"", []string{"test"}, map[string]interface{}{}, nil, false, 0)
	require.NoError(t, err)
	require.NotEmpty(t, sourceID)

	// ── Phase 1: Enqueue → vendor fails → DLQ ──
	worker.Enqueue(MemoryEvent{
		ID:      sourceID,
		Content: "memory content for recovery test",
		Tags:    []string{"test"},
	})
	time.Sleep(500 * time.Millisecond)

	var count int
	err = db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM synthesis_dlq WHERE memory_id = ?`, sourceID,
	).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "DLQ must have entry before recovery")

	// ── Phase 2: Mutate mock to succeed, trigger retry ──
	mock.setSucceed("synthesized recovery content", []string{"synthesized", "ltm"})

	worker.processDLQForced()
	time.Sleep(500 * time.Millisecond)

	// Assert DLQ row was cleaned
	err = db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM synthesis_dlq WHERE memory_id = ?`, sourceID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "DLQ should be empty after successful retry")

	// Assert synthesized memory persisted (is_long_term=1, deleted_at IS NULL)
	var found bool
	rows, err := db.SQLDB().Query(
		`SELECT 1 FROM memories WHERE content = ? AND is_long_term = 1 AND deleted_at IS NULL`,
		"synthesized recovery content",
	)
	require.NoError(t, err)
	found = rows.Next()
	rows.Close()
	assert.True(t, found, "synthesized memory should exist in memories table")

	// Assert source memory was soft-deleted
	var deletedAt *string
	err = db.SQLDB().QueryRow(
		`SELECT deleted_at FROM memories WHERE id = ?`, sourceID,
	).Scan(&deletedAt)
	require.NoError(t, err)
	assert.NotNil(t, deletedAt, "source memory should have deleted_at set after synthesis")
}

// ── Scenario 3: Graceful Shutdown ─────────────────────────────────────────

func testSynthesisGracefulShutdown(t *testing.T) {
	db := synthesisFreshDB(t)
	defer db.Close()

	mock := &controllableSynthClient{}
	mock.setFail(assert.AnError)

	dlqTick := make(chan time.Time)
	worker := NewSynthesisWorkerForTest(db, mock, 2, dlqTick, []config.SynthVendor{
		{Name: "test-vendor"},
	})
	worker.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	worker.Start()

	// Enqueue several items
	for i := 0; i < 5; i++ {
		worker.Enqueue(MemoryEvent{
			ID:      fmt.Sprintf("shutdown-test-%d", i),
			Content: fmt.Sprintf("shutdown content %d", i),
			Tags:    []string{"test"},
		})
	}

	// Give events time to be picked up by the worker
	time.Sleep(100 * time.Millisecond)

	// Stop must drain all in-flight items and complete within the deadline
	done := make(chan struct{})
	go func() {
		worker.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not complete within 10s — goroutine or channel leak")
	}

	// All 5 items should have been processed and ended up in DLQ
	var dlqCount int
	err := db.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM synthesis_dlq WHERE memory_id LIKE 'shutdown-test-%'`,
	).Scan(&dlqCount)
	require.NoError(t, err)
	assert.Equal(t, 5, dlqCount,
		"all 5 enqueued events should have been processed and written to DLQ")
}
