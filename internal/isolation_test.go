package internal

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSynthClient is a test double implementing SynthClientInterface.
type mockSynthClient struct {
	callCount     int
	failFirst     int
	failErr       error
	succeedResult *synthResult
}

func (m *mockSynthClient) Synthesize(ctx context.Context, fragments []string) (*synthResult, error) {
	return nil, nil
}

func (m *mockSynthClient) SynthesizeWithVendor(ctx context.Context, vendor SynthVendor, content string, tags []string) (*synthResult, error) {
	m.callCount++
	if m.callCount <= m.failFirst {
		return nil, m.failErr
	}
	return m.succeedResult, nil
}

var _ SynthClientInterface = (*mockSynthClient)(nil)

// freshDB creates a fully isolated DatabaseManager backed by a temp file.
// Uses NewDatabaseManagerForDB + InitSchema to avoid workspace path resolution.
func freshDB(t *testing.T) *DatabaseManager {
	t.Helper()
	tmp := t.TempDir() + "/test.db"
	db, err := sql.Open("sqlite3", tmp)
	require.NoError(t, err)
	dm := NewDatabaseManagerForDB(db)
	require.NoError(t, dm.InitSchema())
	require.NoError(t, EnsureDLQSchema(dm.SQLDB()))
	return dm
}

// TestSynthesisWorkerEnqueueThenDLQRetry proves the full isolation pipeline:
//  1. Enqueue → vendor fails → DLQ gets entry (attempt=0)
//  2. processDLQ() → vendor succeeds on retry → DLQ row removed
//  3. Synthesized memory created
func TestSynthesisWorkerEnqueueThenDLQRetry(t *testing.T) {
	db := freshDB(t)
	defer db.Close()

	// Mock: fail callCount <= 1, succeed from callCount >= 2
	mock := &mockSynthClient{
		failFirst: 1,
		failErr:   assert.AnError,
		succeedResult: &synthResult{
			Content: `{"content": "synthesized insight", "tags": ["synthesized"]}`,
			Tags:    []string{"synthesized"},
		},
	}

	// Use a manual-tick constructor with an explicit vendor chain.
	// This bypasses getSynthVendorChain() which returns nil without real API keys.
	dlqTick := make(chan time.Time) // never fires in this test
	worker := NewSynthesisWorkerForTest(db, mock, 1, dlqTick, []SynthVendor{{Name: "test-vendor"}})
	worker.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	worker.Start()
	defer worker.Stop()

	time.Sleep(50 * time.Millisecond)

	// ── Phase 1: Enqueue → first vendor call fails → DLQ entry written ──
	worker.Enqueue(MemoryEvent{
		ID:      "dlq-retry-test",
		Content: "test memory for dlq retry",
		Tags:    []string{"test"},
	})

	time.Sleep(500 * time.Millisecond)

	// Confirm DLQ entry exists with attempt=0
	var count int
	err := db.SQLDB().QueryRow(`SELECT COUNT(*) FROM synthesis_dlq WHERE memory_id = ?`, "dlq-retry-test").Scan(&count)
	require.NoError(t, err)
	t.Logf("DLQ count after failed attempt: %d (want 1)", count)
	assert.Equal(t, 1, count, "DLQ should have exactly 1 entry for the failed memory")

	var attempt int
	err = db.SQLDB().QueryRow(`SELECT attempt FROM synthesis_dlq WHERE memory_id = ?`, "dlq-retry-test").Scan(&attempt)
	require.NoError(t, err)
	t.Logf("DLQ attempt: %d (want 0)", attempt)
	assert.Equal(t, 0, attempt, "first failure → attempt=0")

	// ── Phase 2: Trigger processDLQ → retry succeeds → DLQ row removed ──
	// Use processDLQForced() which bypasses the next_retry time filter.
	// The real processDLQ() uses DLQReady() which filters by next_retry <= now,
	// but the DLQ entry was just created with next_retry = 1 minute in the future.
	worker.processDLQForced()
	time.Sleep(500 * time.Millisecond)

	// Confirm DLQ empty after successful retry
	err = db.SQLDB().QueryRow(`SELECT COUNT(*) FROM synthesis_dlq WHERE memory_id = ?`, "dlq-retry-test").Scan(&count)
	require.NoError(t, err)
	t.Logf("DLQ count after retry: %d (want 0)", count)
	assert.Equal(t, 0, count, "DLQ should be empty after successful retry")

	// Confirm synthesized memory was persisted.
	// persistSynthesizedMemory stores the raw JSON result string as content.
	var exists bool
	rows, err := db.SQLDB().Query(`SELECT 1 FROM memories WHERE content = ? AND is_long_term = 1 AND deleted_at IS NULL`,
		`{"content": "synthesized insight", "tags": ["synthesized"]}`)
	require.NoError(t, err)
	exists = rows.Next()
	rows.Close()
	assert.True(t, exists, "synthesized memory should be persisted")
}

// TestSynthesisWorkerNonBlockingEnqueue proves Enqueue never blocks,
// even when the event channel is at capacity (200 deep).
func TestSynthesisWorkerNonBlockingEnqueue(t *testing.T) {
	db := freshDB(t)
	defer db.Close()

	mock := &mockSynthClient{
		failFirst: 999,
		failErr:   context.DeadlineExceeded,
	}

	worker := NewSynthesisWorker(db, mock, 1)
	worker.logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	worker.Start()
	defer worker.Stop()

	// Fill channel to capacity (200 events)
	for i := 0; i < 200; i++ {
		worker.Enqueue(MemoryEvent{ID: "fill", Content: "filler", Tags: []string{}})
	}

	// Enqueue one more — should return immediately (non-blocking)
	start := time.Now()
	worker.Enqueue(MemoryEvent{ID: "overflow", Content: "should not block", Tags: []string{}})
	assert.Less(t, time.Since(start), 10*time.Millisecond,
		"Enqueue should return immediately even when channel is at capacity")
}
